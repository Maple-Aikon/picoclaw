package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// retryExecuteToolChainCallCount is a package-level counter incremented
// every time retryExecuteToolChain is entered. Used by tests to verify
// that Path 1 (handleGoalRecovery) and Path 2 (retryLLMForBlockedTool)
// delegate to retryExecuteToolChain as required by Phase 12.28.
// This is test instrumentation only; production callers ignore it.
var (
	retryExecuteToolChainCallCount     int
	retryExecuteToolChainWrongToolHits int
)

// ResetRetryExecuteToolChainTestCounters resets the package-level test
// instrumentation counters to zero. Tests that exercise retryExecuteToolChain
// should call this at the start to ensure they observe only the calls made
// during their own execution.
func ResetRetryExecuteToolChainTestCounters() {
	retryExecuteToolChainCallCount = 0
	retryExecuteToolChainWrongToolHits = 0
}

// retryExecuteToolChain unifies the same-iteration retry chain used by
// goal-recovery (handleGoalRecovery), tool-exec-recovery (Path 4),
// and tool-block recovery (retryLLMForBlockedTool).
//
// Phase 12.28 contract (plan §3.1):
//   - Step 1: recall LLM with hint
//   - Step 2: check first tool against allowedTools
//   - Step 3: if selected tool is in allowlist → execute it
//   - Step 4: check tool result
//   - Step 5: if error → new hint + retry (up to ToolExecErrorRetryCap)
//   - Step 6: on success → ControlToolLoop; on exhaustion → ControlBreak + archive
//
// Task 3 ships only the stub (Step 1 + Step 2 branch + Step 3-6 stubs
// for compile-only). Tasks 6-7 fill in Step 3-5.

// toolExecutor is the swappable ExecuteTools dependency (Phase 12.28.1 Task 2).
// Path 2 (retryLLMForBlockedTool) and Path 4 (turn_coord.go:373+) use this to
// inject a fake in tests, avoiding the cost of full Pipeline.ExecuteTools
// which depends on AgentLoop + tool registry + session storage.
//
// Interface contract matches Pipeline.ExecuteTools signature verbatim:
//
//	ExecuteTools(ctx, turnCtx context.Context, ts *turnState,
//	            exec *turnExecution, iteration int) ToolControl
//
// Breaking changes to Pipeline.ExecuteTools require a deliberate interface
// update — the compile-time assertion below will catch any drift.
type toolExecutor interface {
	ExecuteTools(
		ctx context.Context,
		turnCtx context.Context,
		ts *turnState,
		exec *turnExecution,
		iteration int,
	) ToolControl
}

// Compile-time check: *Pipeline must satisfy toolExecutor. Silent breakage
// forces a compile error here, surfacing the contract drift.
var _ toolExecutor = (*Pipeline)(nil)

func (p *Pipeline) retryExecuteToolChain(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
	recoveryHint string,
	allowedTools []string,
	phase string,
) (Control, error) {
	retryExecuteToolChainCallCount++

	// Step 5 (Task 5 — this commit): wrap Steps 1-4 in
	// BoundedRetry(MaxAttempts=ToolExecErrorRetryCap=3) so transient
	// tool-execution errors get same-iter retry budget (Phase 12.22
	// retryLLMForBlockedTool pattern + Phase 12.11 BoundedRetry shape).
	//
	// Mapping from inner Control to BoundedRetry decision:
	//   ControlToolLoop  → RetryDecisionRetry (caller-loop OR retry-on-error)
	//   ControlBreak    → RetryDecisionDone  (terminal: wrong tool / archive / LLM down)
	//
	// After BoundedRetry exits we check the exhausted flag (set by
	// OnExhausted) because BoundedRetry returns RetryDecisionRetry on
	// exhaustion — NOT RetryDecisionAbort (Phase 12.22 lesson). Without
	// the flag we'd never distinguish "caller broke loop naturally" from
	// "we burned all attempts".
	// Inner result preserved across the BoundedRetry loop so the outer
	// caller sees the same Control enum Path 2 / Path 4 originally
	// expected. Without this, ControlBreak from Step 2/Step 4 would be
	// collapsed into ControlToolLoop on the way out.
	var innerResult Control
	exhausted := false
	_, err := BoundedRetry(ctx, RetryConfig{
		Name:        "retryExecuteToolChain",
		MaxAttempts: ToolExecErrorRetryCap,
		OnExhausted: func(rc RetryContext) {
			exhausted = true
			logger.InfoCF("agent", "retryExecuteToolChain: attempts exhausted",
				map[string]any{
					"agent_id":   ts.agent.ID,
					"max":        rc.MaxAttempts,
					"elapsed_ms": rc.Elapsed.Milliseconds(),
					"phase":      phase,
				})
		},
		}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		ctrl, err := p.retryExecuteToolChainOnce(ctx, turnCtx, ts, exec, iteration,
			recoveryHint, allowedTools, phase)
		innerResult = ctrl
		if err != nil {
			return RetryDecisionDone, err
		}
		if ctrl == ControlBreak {
			return RetryDecisionDone, nil
		}
		// ctrl == ControlToolLoop → caller-loop or retry-on-error.
		// Phase 12.28 Task 6 contract: BoundedRetry should only continue
		// when the LLM/timing warrants another attempt. The
		// distinguishing signal is ts.pendingRecoveryMessage:
		//   - empty: success path. Propagate ControlToolLoop to caller
		//     (the caller-loop is OUTSIDE this helper) and exit
		//     BoundedRetry immediately so we don't burn retry budget.
		//   - non-empty: a retry was triggered (Step 2 wrong-tool or
		//     Step 4 tool-exec transient). Continue BoundedRetry until
		//     retryMsg is cleared or the cap is hit.
		if ts.pendingRecoveryMessage == "" {
			return RetryDecisionDone, nil
		}
		return RetryDecisionRetry, nil
	})
	if err != nil {
		ts.goalArchiveRequested = true
		return ControlBreak, err
	}
	if exhausted {
		// Cap hit while still retrying. Mirror the canonical
		// handleGoalRecovery OnExhausted (pipeline_llm.go:1173): archive
		// the goal so the caller finalizes cleanly, then break out.
		ts.goalArchiveRequested = true
		logger.InfoCF("agent", "retryExecuteToolChain: archive after exhaustion",
			map[string]any{
				"agent_id": ts.agent.ID,
				"phase":    phase,
			})
		return ControlBreak, nil
	}
	// Return whatever the inner step produced. If Step 2/Step 4 hit
	// a terminal condition (wrong tool / archive) the inner returned
	// ControlBreak and BoundedRetry exited via Done — propagate that
	// to the caller instead of flattening it to ControlToolLoop.
	return innerResult, nil
}

// retryExecuteToolChainOnce runs Steps 1-4 ONCE. Split out from the BoundedRetry
// wrapper above so the per-attempt semantics stay readable and testable in
// isolation (Task 4 tests target this entry point). Returns the same Control
// semantics that the outer helper exposes to Path 2 / Path 4 callers.
func (p *Pipeline) retryExecuteToolChainOnce(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
	recoveryHint string,
	allowedTools []string,
	phase string,
) (Control, error) {
	// Step 1: recall LLM with hint. RecallLLM only invokes callLLMCore;
	// it does NOT re-build exec.callMessages from ts.pendingRecoveryMessage
	// the way CallLLM does (that consumption lives in pipeline_llm.go
	// line 108 inside CallLLM, which we are bypassing). We therefore
	// inject the hint directly into exec.callMessages AND arm the
	// pendingRecoveryMessage field for any observer. The hook order
	// in pipeline_llm.go's CallLLM (interruptHintMessage +
	// pendingRecoveryMessage) is mirrored here.
	hint := recoveryHint
	setupFunc := func() {
		ts.pendingRecoveryMessage = hint
		if hint == "" {
			return
		}
		base := exec.messages
		if base == nil {
			base = exec.callMessages
		}
		exec.callMessages = append([]providers.Message{}, base...)
		exec.callMessages = append(exec.callMessages, providers.Message{
			Role:    "user",
			Content: hint,
		})
	}
	resp, err := p.RecallLLM(ctx, turnCtx, ts, exec, iteration,
		"retryExecuteToolChain", setupFunc)
	if err != nil || resp == nil {
		// LLM unavailable — archive the goal so the caller finalizes
		// it cleanly, and break out of the loop.
		ts.goalArchiveRequested = true
		return ControlBreak, err
	}

	// RecallLLM does NOT mutate exec.response internally — Phase 12.26
	// design contract. Caller MUST assign the returned value to
	// exec.response so subsequent Step 2/Step 3 logic can read the
	// tool call set. Without this assignment, exec.response stays nil
	// from the prior call and the entire retry chain silently aborts.
	exec.response = resp

	// Step 2: check first tool selection against the phase allowlist.
	firstTool := ""
	if exec.response != nil && len(exec.response.ToolCalls) > 0 {
		firstTool = exec.response.ToolCalls[0].Name
	}
	if firstTool == "" || !allowlistContains(allowedTools, firstTool) {
		// Wrong or no tool selected. Re-arm the recovery hint with a
		// fresh, phase-aware message so the caller (Path 4 / Path 3)
		// sees a useful pendingRecoveryMessage on the next turn.
		hint = buildRecoveryHint(firstTool, allowedTools, phase)
		ts.pendingRecoveryMessage = hint
		retryExecuteToolChainWrongToolHits++
		return ControlBreak, nil
	}

	// Step 3 (Tasks 4-7 wiring): call ExecuteTools via toolExecLazy().
	// Production self-binding uses p.ExecuteTools; tests inject *fakeExecutor
	// via SetToolExecutor (Task 3). The tool results land in exec.messages
	// (last entries with Role="tool"); checkToolExecErrorRecovery inspects
	// them after this call.
	toolCtrl := p.toolExecLazy().ExecuteTools(ctx, turnCtx, ts, exec, iteration)
	if toolCtrl == ToolControlBreak {
		// Executor broke early (e.g. approval rejected, hard abort). Mirror
		// the caller-side handling: return ControlBreak so the coordinator
		// exits the loop without re-trying in the same iteration.
		return ControlBreak, nil
	}

	// Step 4 (Task 4 — this commit): inspect the tool results for executor
	// errors that warrant same-iter retry (Phase 12.22 retryLLMForBlockedTool
	// pattern). checkToolExecErrorRecovery returns (toolName, msg):
	//   - toolName != "" → counter exhausted → archive goal + break
	//   - toolName == "" && msg != "" → counter not exhausted → retry
	//   - both "" → no error detected → continue normally
	archiveTool, retryMsg := checkToolExecErrorRecovery(ts, exec)
	if archiveTool != "" {
		ts.goalArchiveRequested = true
		logger.InfoCF("agent", "retryExecuteToolChain: tool-exec retries exhausted",
			map[string]any{
				"agent_id": ts.agent.ID,
				"tool":     archiveTool,
				"message":  retryMsg,
			})
		return ControlBreak, nil
	}
	if retryMsg != "" {
		// Same-iter retry. Re-arm the recovery hint with the executor's
		// retry message and return ControlToolLoop so the caller may
		// loop back into Step 1 of the next attempt. (BoundedRetry
		// wrapping lands in Task 5.)
		ts.pendingRecoveryMessage = retryMsg
		return ControlToolLoop, nil
	}

	// No error detected — proceed with the tool loop normally. The caller
	// will either continue iterating (ControlToolLoop returned above is
	// already handled) or exit when the iteration cap is hit.
	return ControlToolLoop, nil
}

// allowlistContains reports whether name appears in allowlist. Equivalent to
// slices.Contains but kept local to avoid an extra import in this small file.
func allowlistContains(allowlist []string, name string) bool {
	for _, t := range allowlist {
		if t == name {
			return true
		}
	}
	return false
}

// buildRecoveryHint constructs a phase-aware recovery message. When the LLM
// picked the wrong tool (wrongTool != ""), we name it explicitly; otherwise
// we just list the allowed tools. The trailing "Pick one of these or call
// complete_goal" gives the LLM an unambiguous next action.
func buildRecoveryHint(wrongTool string, allowedTools []string, phase string) string {
	var b strings.Builder
	if wrongTool != "" {
		fmt.Fprintf(&b, "Tool %q is not available in current phase (%s). ", wrongTool, phase)
	} else {
		fmt.Fprintf(&b, "No tool selected; current phase is %s. ", phase)
	}
	b.WriteString("Allowed tools: ")
	for i, t := range allowedTools {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(t)
	}
	b.WriteString(". Pick one of these or call complete_goal.")
	return b.String()
}
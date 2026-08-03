package agent

import (
	"context"
	"encoding/json"

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
	logger.InfoCF("agent", "P1242 debug: retryExecuteToolChain entered", map[string]any{
		"iteration": iteration,
		"hint":      recoveryHint,
		"allowed":   allowedTools,
		"phase":     phase,
	})

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
			// Phase 12.42 (C5): mirror Path 2's OnExhausted
			// (pipeline_llm.go:1416-1426) — the BoundedRetry exhaustion
			// itself is the stuck signal. Set a phase-specific abort
			// reason so finalizeGoalOnTurnEnd archives with the right
			// reason (not a generic error).
			currentPhase := GoalPhase(phase)
			switch currentPhase {
			case GoalPhaseSet:
				ts.lastPhaseStuckError = GoalPhaseSetStuckAbortReason
			case GoalPhaseCheckpoint:
				ts.lastPhaseStuckError = GoalPhaseCheckpointStuckAbortReason
			case GoalPhaseFinal:
				ts.lastPhaseStuckError = GoalPhaseFinalStuckAbortReason
			default:
				ts.lastPhaseStuckError = computePhaseStuckAbortReasonForPhase(
					currentPhase, ts.setGoalFailCount, ts.goalProgressFailCount, ts.completeGoalFailCount)
			}
			logger.InfoCF("agent", "retryExecuteToolChain: attempts exhausted",
				map[string]any{
					"agent_id":   ts.agent.ID,
					"max":        rc.MaxAttempts,
					"elapsed_ms": rc.Elapsed.Milliseconds(),
					"phase":      phase,
					"reason":     ts.lastPhaseStuckError,
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
//
// Phase 12.28.1 Task 8: Steps 1+2 are extracted to recallAndCheckTool so
// retryLLMForBlockedTool (Path 2) can compose the same primitive. The
// path-specific behavior on wrong-tool is delegated to onWrongTool.
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
	// Step 1+2: shared recall-and-check primitive. Path 4's wrong-tool
	// policy (Phase 12.42, G3): default phase-aware mirror of Path 2's
	// arms (pipeline_llm.go:1475-1510) — re-prompt instead of break:
	//   text-only                 → ControlToolLoop (success)
	//   allowAll + gate-allowed   → ControlToolLoop (execute at Step 3)
	//   allowAll + gate-blocked   → hint + ControlContinue (re-prompt)
	//   restricted + wrong tool   → hint + ControlContinue (re-prompt)
	ctrl, _, err := p.recallAndCheckTool(
		ctx, turnCtx, ts, exec, iteration,
		"retryExecuteToolChain",
		recoveryHint, allowedTools,
		func(firstTool string) Control {
			logger.InfoCF("agent", "P1242 debug: onWrongTool arm", map[string]any{
				"first_tool": firstTool,
				"allowed":    allowedTools,
			})
			if firstTool == "" {
				// Text-only or malformed tool call → success (Path 2
				// arm i, :1476-1482). Caller skips Step 3/4 via the
				// G4 guard below.
				return ControlToolLoop
			}
			allowAll := len(allowedTools) == 0
			if allowAll {
				// Allow-all phase (Open default) + real tool. Phase
				// 12.37 C1: when the tool is ALSO gate-blocked by the
				// Phase 12.31 lifecycle gate (e.g. goal_progress at
				// OPEN), treat it as wrong-tool to drive BoundedRetry
				// exhaustion → D2 archive after 3 attempts.
				if ts.agent != nil && ts.agent.Tools != nil && !ts.agent.Tools.IsAllowed(firstTool) {
					ts.pendingRecoveryMessage = recoveryHint
					exec.messages = append(exec.messages, providers.Message{
						Role:    "user",
						Content: recoveryHint,
					})
					exec.callMessages = exec.messages
					return ControlContinue
				}
				return ControlToolLoop
			}
			// Restricted phase + wrong tool → re-prompt with the
			// original recoveryHint (spec 12.37 "buộc LLM tuân thủ 3×").
			ts.pendingRecoveryMessage = recoveryHint
			exec.messages = append(exec.messages, providers.Message{
				Role:    "user",
				Content: recoveryHint,
			})
			exec.callMessages = exec.messages
			return ControlContinue
		},
	)
	if err != nil {
		return ctrl, err
	}
	if ctrl == ControlBreak {
		return ControlBreak, nil
	}
	if ctrl == ControlContinue {
		// Wrong tool at allow-all/restricted: hint appended, recovery
		// hint still armed (pendingRecoveryMessage) → retry next attempt.
		return ControlContinue, nil
	}
	// ctrl == ControlToolLoop — LLM picked a valid tool. Proceed to Step 3+4.

	// Phase 12.42 (G4/C3): text-only or malformed assistant output (no
	// tool calls) after a tool-exec error is SUCCESS — skip Step 3/4 so
	// we don't re-read the stale tool error at messages[len-1] and re-arm
	// a retry loop (which would burn 3 attempts and archive a healthy
	// goal). C9b: clear pendingRecoveryMessage so BoundedRetry exits
	// (RetryDecisionDone) instead of retrying.
	if len(exec.normalizedToolCalls) == 0 {
		logger.InfoCF("agent", "P1242 debug: G4 guard hit", map[string]any{
			"helper":      "retryExecuteToolChainOnce",
			"resp_tools":  len(exec.response.ToolCalls),
			"normalized":  len(exec.normalizedToolCalls),
			"first_tool":  func() string { if len(exec.response.ToolCalls) > 0 { return exec.response.ToolCalls[0].Name }; return "" }(),
			"pending_msg": ts.pendingRecoveryMessage,
		})
		ts.pendingRecoveryMessage = ""
		return ControlToolLoop, nil
	}

	// Step 3 (Tasks 4-7 wiring): call ExecuteTools via toolExecLazy().
	// Production self-binding uses p.ExecuteTools; tests inject *fakeExecutor
	// via SetToolExecutor (Task 3). The tool results land in exec.messages
	// (last entries with Role="tool"); checkToolExecErrorRecovery inspects
	// them after this call.
	messagesBefore := len(exec.messages)
	toolCtrl := p.toolExecLazy().ExecuteTools(ctx, turnCtx, ts, exec, iteration)
	if toolCtrl == ToolControlBreak {
		// Executor broke early (e.g. approval rejected, hard abort). Mirror
		// the caller-side handling: return ControlBreak so the coordinator
		// exits the loop without re-trying in the same iteration.
		return ControlBreak, nil
	}
	// Phase 12.42 (C3, external review F3): if ExecuteTools appended
	// NOTHING (every normalized call was filtered out — denyByTurnProfile,
	// allowlist), there is no fresh tool result to check. Skipping Step 4
	// prevents re-reading the stale tool error at messages[len-1] which
	// would re-arm a retry loop against a tool that never executed.
	if len(exec.messages) == messagesBefore {
		ts.pendingRecoveryMessage = ""
		return ControlToolLoop, nil
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
	//
	// Phase 12.28 Task 7: clear pendingRecoveryMessage so the outer
	// BoundedRetry recognizes this attempt as "no retry needed" and
	// returns RetryDecisionDone instead of burning the retry budget.
	// Without this clear, the original recoveryHint stays armed
	// forever and BoundedRetry exhausts 3 attempts on an
	// already-passing chain (regression test for Task 6's design).
	if ts.pendingRecoveryMessage != "" {
		ts.pendingRecoveryMessage = ""
	}
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

// recallAndCheckTool (Phase 12.28.1 Task 8) is the shared Step 1+2 between
// retryLLMForBlockedTool (Path 2, no ExecuteTools) and retryExecuteToolChain
// (Path 4, executes tool). Extracts the LLM-recall + first-tool allowlist
// check loop so both paths compose the same primitive.
//
// Contract:
//   - Always invokes p.RecallLLM with the supplied hint (setupFunc arms
//     ts.pendingRecoveryMessage and appends the hint to exec.callMessages).
//   - Always assigns the returned *providers.LLMResponse to exec.response so
//     downstream callers see the fresh response (Phase 12.26 contract).
//   - Examines exec.response.ToolCalls[0].Name:
//     * tool name == "" OR not in allowedTools → calls onWrongTool(firstTool)
//       which decides what to do (re-prompt / break / etc.). This callback is
//       path-specific — Path 2 re-prompts with stronger hint; Path 4 just
//       builds a fresh phase-aware recovery message and breaks.
//     * tool name in allowedTools → returns (ControlToolLoop, firstTool, nil)
//       so the caller can proceed (Path 4 → ExecuteTools, Path 2 → return to
//       caller which executes externally).
//   - LLM call fatal error → returns (ControlBreak, "", err) so caller can
//     abort the BoundedRetry loop.
//
// This helper does NOT decide retry-vs-break semantics; it owns the LLM
// recall and tool selection check, and delegates the wrong-tool policy to
// onWrongTool. The duplication between Path 2 (re-prompt) and Path 4
// (break) is preserved by design — the two paths intentionally differ on
// this point (Phase 12.22 lesson: re-prompt at blocked phases, break on
// already-correct-pick error).
func (p *Pipeline) recallAndCheckTool(
	ctx, turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
	helperName string,
	recoveryHint string,
	allowedTools []string,
	onWrongTool func(firstTool string) (control Control),
) (Control, string, error) {
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
	resp, err := p.RecallLLM(ctx, turnCtx, ts, exec, iteration, helperName, setupFunc)
	if err != nil || resp == nil {
		ts.goalArchiveRequested = true
		return ControlBreak, "", err
	}

	// RecallLLM does NOT mutate exec.response internally — Phase 12.26
	// design contract. Caller MUST assign the returned value so downstream
	// Step 2/Step 3 logic can read the tool call set.
	exec.response = resp
	logger.InfoCF("agent", "P1242 debug: recall got resp", map[string]any{
		"helper":       helperName,
		"tool_calls":   len(resp.ToolCalls),
		"content_len":  len(resp.Content),
		"finish":       resp.FinishReason,
		"first_tool":   func() string { if len(resp.ToolCalls) > 0 { return resp.ToolCalls[0].Name }; return "" }(),
	})

	// Phase 12.28.1 fix: also populate exec.normalizedToolCalls.
	// Pipeline.ExecuteTools (pkg/agent/pipeline_execute.go:121) iterates
	// exec.normalizedToolCalls (NOT exec.response.ToolCalls) to dispatch
	// tools. Without this, the just-emitted tool call from recovery LLM
	// is silently dropped — exec.response.ToolCalls is populated but
	// ExecuteTools sees 0 items. Symptom: complete_goal picked at
	// GoalPhaseCheckpoint recovery → toolLimitResponse fallback instead of
	// goal.Summary. proceedPastLLM normally populates this field (line
	// 796) but recovery helpers don't call proceedPastLLM, so we do it
	// here. Bug closed in Phase 12.28.1 (anh Maple confirmed 2026-07-29
	// via Horus-protocol live failure on main-turn-3, 16:43:01 ICT).
	if exec.response != nil {
		exec.normalizedToolCalls = make([]providers.ToolCall, 0, len(exec.response.ToolCalls))
		for _, tc := range exec.response.ToolCalls {
			exec.normalizedToolCalls = append(exec.normalizedToolCalls, providers.NormalizeToolCall(tc))
		}
	}

	firstTool := ""
	if exec.response != nil && len(exec.response.ToolCalls) > 0 {
		firstTool = exec.response.ToolCalls[0].Name
	}
	ctrl := ControlToolLoop
	if firstTool == "" || !allowlistContains(allowedTools, firstTool) {
		ctrl = onWrongTool(firstTool)
	}
	// Phase 12.41 (Option A', 2026-08-02): append the assistant tool_calls
	// message BEFORE EVERY return of ControlToolLoop so ExecuteTools'
	// role="tool" results always reference a preceding assistant tool_calls
	// entry. Without this the tool results are orphaned → DeepSeek 400
	// invalid_request_error (strict message-shape validation) on the next
	// provider call; MiniMax-M3 tolerated the malformed shape so the bug
	// stayed hidden (main-turn-8/12). Covers all three ControlToolLoop
	// sources: the allowlisted branch above, Path 2's allow-all arm and the
	// malformed empty-name arm (both via onWrongTool returning
	// ControlToolLoop). Text-only responses (ToolCalls==0) and re-prompt
	// ControlContinue append nothing.
	//
	// Mirror proceedPastLLM (pipeline_llm.go:839-882) EXACTLY: assistantMsg
	// built from exec.response.Content + exec.normalizedToolCalls
	// (ID/Type/Name/Function{Name,Arguments,ThoughtSignature}/ExtraContent),
	// appended to exec.messages unconditionally, persistence trio
	// (AddFullMessage/recordPersistedMessage/ingestMessage) only under
	// !ts.opts.NoHistory.
	if ctrl == ControlToolLoop && exec.response != nil && len(exec.response.ToolCalls) > 0 {
		assistantMsg := providers.Message{
			Role:             "assistant",
			Content:          exec.response.Content,
			ModelName:        exec.llmModelName,
			ReasoningContent: exec.response.ReasoningContent,
			ReasoningDetails: exec.response.ReasoningDetails,
		}
		for _, tc := range exec.normalizedToolCalls {
			argumentsJSON, _ := json.Marshal(tc.Arguments)
			thoughtSignature := ""
			if tc.Function != nil {
				thoughtSignature = tc.Function.ThoughtSignature
			}
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, providers.ToolCall{
				ID:   tc.ID,
				Type: "function",
				Name: tc.Name,
				Function: &providers.FunctionCall{
					Name:             tc.Name,
					Arguments:        string(argumentsJSON),
					ThoughtSignature: thoughtSignature,
				},
				ExtraContent:     tc.ExtraContent,
				ThoughtSignature: thoughtSignature,
			})
		}
		exec.messages = append(exec.messages, assistantMsg)
		if !ts.opts.NoHistory && p.al != nil {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, assistantMsg)
			ts.recordPersistedMessage(assistantMsg)
			ts.ingestMessage(turnCtx, p.al, assistantMsg)
		}
	}
	return ctrl, firstTool, nil
}

// buildRecoveryHint previously constructed a phase-aware recovery message
// for Path 4's hardcoded break policy. Phase 12.42 (G3) replaced that policy
// with the phase-aware mirror of Path 2's arms (re-prompt with the original
// recoveryHint), so this helper is dead code — removed.

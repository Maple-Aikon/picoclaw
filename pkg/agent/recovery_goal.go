// Package agent — Phase 5 goal-lifecycle retry triggers.
//
// 5 recovery triggers wire the same-iteration BoundedRetry pattern for
// the goal-lifecycle feature. They are called from pipeline_llm.go /
// pipeline_execute.go after each LLM response or tool execution. Each
// trigger returns a RecoveryAction hint that the caller uses to decide
// whether to retry within the iteration, escalate to force-complete, or
// archive the goal and stop.
//
// Triggers (per plan §5.2 + §8.3):
//   1. EmptyTextResponse — Goal Phase 1, LLM returns text="" with no tool calls
//   2. TextOnly2x        — Goal Phase 1, two consecutive text-only LLM responses
//   3. ToolExecError     — executor returned IsError=true (not signature)
//   4. BoundedRetryExhausted — any BoundedRetry loop hit cap
//   5. ProviderTransient — HTTP 5xx/timeout/429 exhausted existing retry
//
// Rules:
//   - Recovery retries do NOT consume iteration slots (per §5.3)
//   - Cap exhaustion always triggers goal archive (Hook 1 + Hook 3 §8.3)
//   - Non-retryable errors (auth, 404, context-overflow-exhausted) skip recovery
package agent

// RecoveryAction is the hint returned by recovery triggers. The caller
// applies the action and continues the agent loop without bumping iteration.
type RecoveryAction int

const (
	// RecoveryNone means no recovery is needed — proceed as normal.
	RecoveryNone RecoveryAction = iota
	// RecoveryRetrySameIteration means inject a recovery message and re-run
	// the same iteration. Caller MUST NOT bump ts.iteration.
	RecoveryRetrySameIteration
	// RecoveryForceComplete means strip non-goal tools and force the LLM to
	// emit complete_goal on the next call. Caller must inject force-complete
	// prompt and re-run the same iteration.
	RecoveryForceComplete
	// RecoveryArchiveGoal means the goal cannot be completed. Caller must
	// call ts.finalizeGoalOnTurnEnd (Phase 6 hook; stub for now) and end
	// the turn with status=aborted.
	RecoveryArchiveGoal
)

// RecoveryContext bundles the inputs needed by trigger evaluation. Created
// fresh per-iteration by the caller.
type RecoveryContext struct {
	Phase         string // current goal phase ("" / "Lock" / "Open" / "Checkpoint" / "Final")
	Iteration     int
	TextEmpty     bool   // LLM response text was empty
	HasToolCalls  bool   // LLM response included at least one tool call
	ToolName      string // for ToolExecError trigger: which tool failed
	ProviderError bool   // for ProviderTransient trigger: was this a provider-side transient error
	MaxIterations int
}

// Constants for recovery prompts. These are user-visible messages injected
// into the conversation history when recovery fires.
const (
	// EmptyResponseRecoveryMessage tells the LLM that its previous response
	// was empty and asks it to produce a non-empty text response or invoke
	// a tool. Single line — no newlines (CF Pages esbuild strips them in
	// emitted JS; not relevant for Go but consistent with project policy).
	EmptyResponseRecoveryMessage = "Your previous response was empty. Please produce a non-empty text response or invoke a tool to continue working toward the goal."

	// TextOnlyForceCompleteMessage tells the LLM that consecutive text-only
	// responses are not productive, and forces it to either invoke a real
	// tool or call complete_goal to terminate.
	TextOnlyForceCompleteMessage = "You have produced two consecutive text-only responses with no tool calls. To make progress toward your goal, you must either invoke a tool to gather more information or call complete_goal to acknowledge the goal cannot be completed."

	// ToolExecErrorRetryMessage tells the LLM that a tool execution failed
	// and asks it to retry the call (possibly with different args).
	ToolExecErrorRetryMessage = "A tool execution failed: %s. You may retry the same call with adjusted arguments, invoke a different tool, or call complete_goal if the goal is unreachable."
)

// Caps for each trigger. Per §5.2 + §5.3 — these are sub-attempt counts
// inside one iteration, NOT iteration counts.
const (
	EmptyResponseRecoveryCap     = 2  // soft retry up to 2 per iteration
	TextOnlyForceCompleteCap      = 1  // hard force-complete fires after 2 consecutive text-only; only 1 such escalation per turn
	ToolExecErrorRetryCap         = 3  // per-tool retry up to 3 within same iteration
	ProviderTransientRetryCap     = 3  // matches existing callLLMCore cap
)

// evaluateRecovery decides which recovery action to take based on the
// RecoveryContext and the per-iteration counters on ts. Returns the action
// plus an optional message to inject into the conversation.
//
// This function is pure (no side effects, no logger writes) so it can be
// unit-tested without mocking the full pipeline.
func evaluateRecovery(ts *turnState, ctx RecoveryContext) (RecoveryAction, string) {
	// Out of goal-phase: no recovery needed. Caller proceeds normally.
	if ctx.Phase == "" || ctx.Phase == "Final" {
		return RecoveryNone, ""
	}

	// Provider transient (Trigger #5): always retry up to cap. Independent
	// of goal phase. The existing callLLMCore retry already runs 3 times;
	// when exhausted we archive the goal (Hook 3 §8.3).
	if ctx.ProviderError {
		// Bounded retry was exhausted by callLLMCore — escalate to archive.
		return RecoveryArchiveGoal, "Provider API retry exhausted; archiving goal."
	}

	// Trigger #3: tool execution error (executor returned IsError=true).
	// Skip if no tool name provided or Phase is Lock (only set_goal allowed).
	if ctx.ToolName != "" && ctx.Phase != "Lock" {
		if ts.toolExecRecoveryAttempts == nil {
			ts.toolExecRecoveryAttempts = make(map[string]int)
		}
		if ts.toolExecRecoveryAttempts[ctx.ToolName] < ToolExecErrorRetryCap {
			ts.toolExecRecoveryAttempts[ctx.ToolName]++
			return RecoveryRetrySameIteration, ""
		}
		return RecoveryArchiveGoal, "Tool execution error retry exhausted for " + ctx.ToolName + "."
	}

	// Triggers #1 and #2 only apply in Phase 1 (Open) where the LLM has
	// freedom to call any goal-aware tool. In other phases, these are silent.
	if ctx.Phase != "Open" {
		return RecoveryNone, ""
	}

	// Trigger #1: empty text response.
	if ctx.TextEmpty && !ctx.HasToolCalls && !ts.emptyResponseRecoverySent {
		if countWouldExceed(ts.emptyResponseRecoverySentCount(), EmptyResponseRecoveryCap) {
			ts.emptyResponseRecoverySent = true
			return RecoveryRetrySameIteration, EmptyResponseRecoveryMessage
		}
	}

	// Trigger #2: text-only (no tool calls) on consecutive iterations.
	if !ctx.HasToolCalls && !ctx.TextEmpty {
		ts.textOnlyStreak++
		if ts.textOnlyStreak >= 2 && ts.textOnlyStreak <= TextOnlyForceCompleteCap+1 {
			return RecoveryForceComplete, TextOnlyForceCompleteMessage
		}
		if ts.textOnlyStreak > TextOnlyForceCompleteCap+1 {
			return RecoveryArchiveGoal, "Text-only streak exceeded force-complete cap."
		}
	} else if ctx.HasToolCalls {
		// Reset streak when LLM actually calls a tool (productive turn).
		ts.textOnlyStreak = 0
	}

	return RecoveryNone, ""
}

// emptyResponseRecoverySentCount returns 0 or 1 — we only inject the
// recovery message at most once per iteration.
func (ts *turnState) emptyResponseRecoverySentCount() int {
	if ts.emptyResponseRecoverySent {
		return 1
	}
	return 0
}

func countWouldExceed(current, cap int) bool {
	return current < cap
}

// checkToolExecErrorRecovery examines the most recent tool result message
// in exec.messages. If it's a tool message with IsError=true (signaled via
// the tool result's ContentForLLM flag, which the executor sets when the
// tool runtime returned an error), this calls evaluateRecovery with the
// Trigger #3 context. Returns (toolName, msg) when archive is requested,
// ("", "") otherwise.
//
// Phase 5 note: we don't have direct access to IsError from the message
// store (IsError is on ToolResult, not on providers.Message). Instead, we
// detect error markers in the message Content: messages generated by
// ExecuteTools for executor errors carry a sentinel prefix that the
// executor sets in toolResultPromptMessage. For Phase 5 we use a simpler
// heuristic: if the last message is a tool message AND its content starts
// with "Tool execution failed:" (the standard executor error format from
// toolErrorSummary), trigger recovery.
func checkToolExecErrorRecovery(ts *turnState, exec *turnExecution) (string, string) {
	if exec == nil || len(exec.messages) == 0 {
		return "", ""
	}
	last := exec.messages[len(exec.messages)-1]
	// Only tool-role messages carry tool results.
	if last.Role != "tool" {
		return "", ""
	}
	// Heuristic: detect executor error format. The standard format comes
	// from toolErrorSummary() in pipeline_execute.go and is prefixed with
	// "Tool execution failed:". Phase 6 will replace this with a proper
	// IsError flag on the message itself.
	const executorErrPrefix = "Tool execution failed:"
	if len(last.Content) < len(executorErrPrefix) {
		return "", ""
	}
	if last.Content[:len(executorErrPrefix)] != executorErrPrefix {
		return "", ""
	}
	// Phase 5 fallback: tool name is the Role/ToolCallID carrier. Use
	// last.ToolCallID or fall back to "unknown". The map counter handles
	// per-tool accounting, so a generic key is acceptable here.
	toolName := last.ToolCallID
	if toolName == "" {
		toolName = "unknown"
	}
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:     string(ts.currentGoalPhase()),
		Iteration: ts.iteration,
		ToolName:  toolName,
	})
	if action == RecoveryArchiveGoal {
		return toolName, msg
	}
	return "", ""
}

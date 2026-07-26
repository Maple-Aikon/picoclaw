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

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// RecoveryAction is the hint returned by recovery triggers. The caller
// applies the action and continues the agent loop without bumping iteration.
type RecoveryAction int

const (
	// RecoveryNone means no recovery is needed — proceed as normal.
	RecoveryNone RecoveryAction = iota
	// RecoveryRetrySameIteration means inject a recovery message and re-run
	// the LLM call WITHIN THE SAME iteration. History:
	//   - Phase 5: original name (intent: sub-attempt within one iter)
	//   - Phase 12.10: renamed to RecoveryRetryNextIteration to match the
	//     broken iter-bump pattern (loop top re-entered, setIteration bumped
	//     ts.iteration, so the recovery prompt actually fired in iter N+1)
	//   - Phase 12.11: renamed BACK to RecoveryRetrySameIteration after
	//     replacing the iter-bump pattern with a same-iteration BoundedRetry
	//     loop (see handleGoalRecovery in pipeline_llm.go). Retries no
	//     longer bump the iteration counter.
	//
	// Design intent (Phase 5 plan §5.3): recovery is a sub-attempt within
	// one iteration, not a new iteration. The iter-bump pattern broke the
	// boundary case where a tool exec error at iter=iterationCap-1 forced
	// an early Checkpoint phase transition and stripped the failing tool
	// from the allowlist.
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
	Phase         string // current goal phase ("" / "set" / "open" / "checkpoint" / "final") — matches GoalPhase* constants in tool_allowlist_phase.go
	Iteration     int
	TextEmpty     bool   // LLM response text was empty
	HasToolCalls  bool   // LLM response included at least one tool call
	ToolName      string // for ToolExecError trigger: which tool failed
	ToolExecError string // for ToolExecError trigger: the error message from the tool executor
	ProviderError bool   // for ProviderTransient trigger: was this a provider-side transient error

	// IsTransient (Phase 12.6.1): for ToolExecError trigger only — was the
	// failure classified as transient (timeout / rate-limit / network)?
	// When true, the retry prompt appends ToolExecErrorTransientHint to
	// suggest wait-then-retry without arg changes. Distinct from
	// ProviderError (which gates whether recovery fires); IsTransient
	// changes the SUGGESTED action, not whether recovery fires.
	IsTransient bool

	MaxIterations int

	// ToolKnowledgeRegistry (Phase 12) — when set and a tool execution
	// error triggers retry, the relevant tool_knowledge section is fetched
	// and appended to the recovery message so the LLM gets lessons
	// learned from prior calls. May be nil (feature disabled, default).
	ToolKnowledgeRegistry *tools.ToolRegistry
}

// Constants for recovery prompts. These are user-visible messages injected
// into the conversation history when recovery fires.
const (
	// EmptyResponseRecoveryMessage tells the LLM that its previous response
	// was empty and asks it to produce a non-empty text response or invoke
	// a tool. Single line — no newlines (CF Pages esbuild strips them in
	// emitted JS; not relevant for Go but consistent with project policy).
	EmptyResponseRecoveryMessage = "Your previous response was empty. Please produce a non-empty text response or invoke a tool to continue working toward the goal."

	// TextOnlySoftRetryMessage (Phase 12) — fires on the first text-only
	// retry within an iteration. Asks the LLM to make a decision: complete
	// the goal, complete + ask user, or continue with a tool call. Single
	// line; Vietnamese; soft tone.
	TextOnlySoftRetryMessage = "Your last response was text-only with no tool call. Has the goal been completed? If yes, call `complete_goal`. If a critical decision needs user approval, call `complete_goal` with a question for the user. Otherwise, continue working with an appropriate tool."

	// TextOnlyHardRetryMessage (Phase 12) — fires on the second text-only
	// retry within an iteration. Firm tone: LLM MUST pick one of three
	// paths or the turn will be archived.
	TextOnlyHardRetryMessage = "⚠️ Second consecutive text-only response with no tool call. You MUST decide in your next response: (1) call `complete_goal` if the goal is finished; (2) call `complete_goal` + a question for the user if a critical decision needs user approval; (3) call a tool to continue working. If your next response is still text-only, this turn will be archived."

	// ToolExecErrorRetryMessage tells the LLM that a tool execution failed
	// and asks it to retry the call (possibly with different args). Phase 12
	// rewrites this as a builder function (buildToolExecErrorRetryMessage)
	// so it can include the relevant tool_knowledge section when available.
	//
	// Phase 12.6.1: %s placeholder is TWO-shot — first %s is the tool name,
	// second %s is the error message. The builder inserts BOTH so the LLM
	// knows which tool failed (was just `errMsg` before, which was unhelpful
	// when the LLM had multiple tool calls in flight).
	ToolExecErrorRetryMessage = "Tool %q failed: %s. You may retry the same call with adjusted arguments, invoke a different tool, or call complete_goal if the goal is unreachable."

	// ToolExecErrorTransientHint is appended to the retry message when the
	// tool failure is classified as transient (timeout / 5xx / 429 / connection
	// refused / etc.). Tells the LLM that a brief retry is likely to succeed
	// without changing arguments. English per USER.md preference.
	ToolExecErrorTransientHint = " The error looks transient (timeout, rate-limit, or network). Wait briefly and retry the SAME call — argument changes are unlikely to help."

	// Phase 12.15: ToolExecErrorSetPhaseHint is appended when a tool call
	// was rejected at GoalPhaseSet (iter 1, no active goal). The allowlist
	// is `[set_goal]` only — every other tool call is blocked at the
	// execution gate. Before Phase 12.15, GoalPhaseSet SKIPPED Trigger #3
	// (tool-exec recovery) entirely, so a MiniMax-M3 `unknown({})` quirk
	// silently looped into the `max_tool_iterations` fallback. With this
	// hint, the LLM receives a concrete redirect: call set_goal with
	// top-level fields.
	ToolExecErrorSetPhaseHint = " In the current goal phase (set), only `set_goal` is available — every other tool call is blocked. Call `set_goal` with top-level fields (name, objective, success_criteria[]) to start the goal: the tool name is `set_goal`, the field names are `name`, `objective`, `success_criteria[]` (array), and optional `in_scope[]`, `out_of_scope[]`, `cadence`. Do NOT wrap the arguments inside a `{\"raw\":\"...\"}` envelope — provide each field as a top-level key."

	// Phase 12.15: ToolExecErrorFinalPhaseHint is appended when a tool call
	// was rejected at GoalPhaseFinal (post-complete_goal final-report iter).
	// Allowlist is `[complete_goal]` only. If the LLM emits a broken tool
	// call here (e.g. MiniMax-M3 `unknown({})` quirk), the message redirects
	// back to complete_goal (the only allowed tool) until the goal is fully
	// closed. Skipped when postCompleteGoalReportSent=true (the final report
	// has already been published — no more iterations happen).
	ToolExecErrorFinalPhaseHint = " In the current goal phase (final), only `complete_goal` is available — every other tool call is blocked. The goal is already finalized; call `complete_goal` with a non-empty `summary` (1-500 chars) to close out the turn, or reply with the final user-facing report directly."

	// Phase 12.18: ToolExecErrorCheckpointPhaseHint is appended when a tool
	// call was rejected at GoalPhaseCheckpoint (iter == iterationCap, hit the
	// cap). Allowlist is `[goal_progress, complete_goal]` only. The LLM has
	// hit the iteration cap and must either extend (goal_progress) or wrap
	// up (complete_goal) — calling any other tool is blocked at the
	// execution gate. Before Phase 12.18, recovery (Trigger #3) silently
	// MISSED this rejection because checkToolExecErrorRecovery only matched
	// the legacy "Tool execution failed:" prefix and the Phase 12.3
	// execution gate uses a different format ("tool %q is not available in
	// the current phase..."). Combined effect was a silent recovery blind
	// spot that ended the turn with a canned "max_tool_iterations" string.
	// Telegram user feedback 2026-07-26: main-turn-4 hit this at iter 25.
	ToolExecErrorCheckpointPhaseHint = " This is the final iteration (goal phase: checkpoint). Only `goal_progress` (to extend with another step) and `complete_goal` (to wrap up the goal) are available. Every other tool call is blocked. If your current work is incomplete, call `goal_progress` to extend the turn; otherwise call `complete_goal` with a non-empty `summary` (1-500 chars) to close out the goal."
)

// Caps for each trigger. Per §5.2 + §5.3 — these are sub-attempt counts
// inside one iteration, NOT iteration counts.
const (
	EmptyResponseRecoveryCap     = 2  // soft retry up to 2 per iteration
	// Phase 12 redesign: text-only retries fire 2x per iteration with escalation.
	// Soft prompt (TextOnlySoftRetryMessage) fires first, then hard prompt
	// (TextOnlyHardRetryMessage). If both fire and LLM still produces
	// text-only, archive the goal.
	//
	// Both caps are PER-ITERATION counts (not cross-iteration streak). The
	// cross-iteration textOnlyStreak field still tracks consecutive text-only
	// iterations but is no longer the gate — recovery fires within iteration.
	TextOnlySoftRetryCap         = 1  // 1 soft prompt per iteration (fires on first text-only)
	TextOnlyHardRetryCap         = 1  // 1 hard prompt per iteration (fires on second consecutive text-only in same iter)
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
	// Out of goal-phase or in post-complete_goal final-report iter (Phase 12.7):
	// no recovery needed. Caller proceeds normally. We skip recovery because:
	//   - Phase=Final: tool allowlist is empty; nothing to retry.
	//   - postCompleteGoalReportSent: the LLM has already completed the goal;
	//     a text-only retry prompt would be redundant and could spam the user.
	// Phase 12.15: GoalPhaseFinal is now eligible for tool-exec recovery
	// (line below), but ONLY when postCompleteGoalReportSent=false. The
	// post-complete_goal final-report iter is the rare case where the LLM
	// has already published the user-facing report, so no further retry
	// prompts should fire. Bare GoalPhaseFinal skips Trigger #3 in the
	// pre-line check (no `return RecoveryNone` here).
	if ctx.Phase == "" || ts.postCompleteGoalReportSent {
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
	// Skip if no tool name provided.
	// Phase 12.15: GoalPhaseSet and GoalPhaseFinal are NOW eligible for
	// tool-exec recovery (was previously skipped). The recovery prompt
	// appends a phase-specific redirect hint (ToolExecErrorSetPhaseHint /
	// ToolExecErrorFinalPhaseHint) so the LLM is told which tool is the
	// only one allowed in the current phase. This closes the recovery-
	// blind-spot where MiniMax-M3 `unknown({})` quirk at GoalPhaseSet
	// would silently loop into the `max_tool_iterations` fallback without
	// any feedback to the LLM (Telegram turn 16:06 ICT 2026-07-25).
	// Phase 12: when about to retry, fetch tool_knowledge for that tool
	// (lessons learned from prior calls) and append to the prompt so the
	// LLM gets relevant guidance instead of repeating the same mistake.
	if ctx.ToolName != "" {
		if ts.toolExecRecoveryAttempts == nil {
			ts.toolExecRecoveryAttempts = make(map[string]int)
		}
		if ts.toolExecRecoveryAttempts[ctx.ToolName] < ToolExecErrorRetryCap {
			ts.toolExecRecoveryAttempts[ctx.ToolName]++
			// Phase 12.6.1: thread `IsTransient` so the prompt can suggest
			// wait-then-retry (transient) vs diagnose-or-recomplete (permanent).
			// Caller (checkToolExecErrorRecovery / pipeline) sets this from
			// the tool result's error text + circuit-breaker state.
			msg := buildToolExecErrorRetryMessage(ctx.ToolName, ctx.ToolExecError, ctx.IsTransient, ctx.ToolKnowledgeRegistry, ctx.Phase)
			return RecoveryRetrySameIteration, msg
		}
		return RecoveryArchiveGoal, "Tool execution error retry exhausted for " + ctx.ToolName + "."
	}

	// Triggers #1 and #2 only apply in Open phase where the LLM has
	// freedom to call any goal-aware tool. In other phases, these are silent.
	if ctx.Phase != string(GoalPhaseOpen) {
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
	// Phase 12 redesign: fire soft prompt first, then hard prompt, then
	// archive. All counters are per-iteration (reset on iteration bump
	// elsewhere). The cross-iteration textOnlyStreak field still tracks
	// for observability but is not the gating signal any more.
	if !ctx.HasToolCalls && !ctx.TextEmpty {
		ts.textOnlyStreak++
		// Increment within-iteration escalation counters in order.
		if ts.textOnlySoftRetriesDone < TextOnlySoftRetryCap {
			ts.textOnlySoftRetriesDone++
			var agentID string
			if ts.agent != nil {
				agentID = ts.agent.ID
			}
			logger.InfoCF("agent", "Text-only soft retry fired", map[string]any{
				"agent_id": agentID,
				"iteration": ctx.Iteration,
				"soft_done": ts.textOnlySoftRetriesDone,
				"hard_done": ts.textOnlyHardRetriesDone,
			})
			return RecoveryRetrySameIteration, TextOnlySoftRetryMessage
		}
		if ts.textOnlyHardRetriesDone < TextOnlyHardRetryCap {
			ts.textOnlyHardRetriesDone++
			var agentID string
			if ts.agent != nil {
				agentID = ts.agent.ID
			}
			logger.InfoCF("agent", "Text-only hard retry fired (escalation)", map[string]any{
				"agent_id": agentID,
				"iteration": ctx.Iteration,
				"soft_done": ts.textOnlySoftRetriesDone,
				"hard_done": ts.textOnlyHardRetriesDone,
			})
			return RecoveryRetrySameIteration, TextOnlyHardRetryMessage
		}
		// Both soft + hard fired this iteration; archive the goal.
		var agentID string
		if ts.agent != nil {
			agentID = ts.agent.ID
		}
		logger.WarnCF("agent", "Text-only retry cap exhausted — archiving goal", map[string]any{
			"agent_id": agentID,
			"iteration": ctx.Iteration,
			"streak":    ts.textOnlyStreak,
		})
		return RecoveryArchiveGoal, "Text-only retry cap exhausted (1 soft + 1 hard per iteration)."
	} else if ctx.HasToolCalls {
		// Reset streak + per-iteration escalation counters when LLM calls
		// a tool (productive turn). Counters are now useless for the
		// current iteration but keep them clean for the next iteration
		// boundary (defensive — they will be reset at iteration bump
		// anyway, but resetting here documents intent).
		ts.textOnlyStreak = 0
		ts.textOnlySoftRetriesDone = 0
		ts.textOnlyHardRetriesDone = 0
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
// in exec.messages. If it's a tool message with an executor error
// (signaled via the legacy "Tool execution failed:" prefix OR via the
// Phase 12.3 execution gate's "tool %q is not available in the current
// phase..." format), this calls evaluateRecovery with the Trigger #3
// context. Returns (toolName, msg) when archive is requested, ("", "")
// otherwise.
//
// Phase 12.18: added Phase 12.3 execution gate format to the matcher.
// The execution gate in pkg/tools/registry.go rejects non-allowlist tool
// calls at runtime with `tool %q is not available in the current phase
// (allowed tools: %v)` — this format is what reaches the LLM but the
// legacy heuristic only matched `Tool execution failed:`. As a result,
// tool-exec recovery (Trigger #3) silently MISSED the most common
// checkpoint-phase error pattern (LLM calls a non-lifecycle tool after
// hitting iteration cap), leading to the canned "max_tool_iterations"
// fallback. We now also match the execution gate format and extract the
// failing tool name from the quoted name in the message.
//
// Tool-name extraction: `tool "X" is not available` -> X via prefix
// matching. Falls back to last.ToolCallID when format doesn't yield a
// quoted name (e.g. tool-not-found errors use `tool %q not found` which
// also starts with `tool "` so the same parser works).
func checkToolExecErrorRecovery(ts *turnState, exec *turnExecution) (string, string) {
	if exec == nil || len(exec.messages) == 0 {
		return "", ""
	}
	last := exec.messages[len(exec.messages)-1]
	// Only tool-role messages carry tool results.
	if last.Role != "tool" {
		return "", ""
	}
	// Heuristic: detect either executor error format.
	//
	// (a) Legacy: toolErrorSummary() format prefixed with
	//     "Tool execution failed:" — used by post-execution error
	//     wrapping in pipeline_execute.go.
	//
	// (b) Phase 12.3: execution gate rejection in pkg/tools/registry.go
	//     with "tool %q is not available..." (also matches "tool %q not
	//     found" from the same registry).
	const executorErrPrefix = "Tool execution failed:"
	const executionGatePrefix = `tool "`
	isExecutorErr := len(last.Content) >= len(executorErrPrefix) && last.Content[:len(executorErrPrefix)] == executorErrPrefix
	isExecutionGateErr := len(last.Content) >= len(executionGatePrefix) && last.Content[:len(executionGatePrefix)] == executionGatePrefix
	if !isExecutorErr && !isExecutionGateErr {
		return "", ""
	}
	// Phase 12.18: extract tool name from the message when execution
	// gate rejected. Format: `tool "X" is not available...` — parse X
	// from the leading `tool "` quote. Falls back to last.ToolCallID
	// when the format doesn't yield a quoted name (legacy executor
	// error uses %q'd tool name in different positions, so
	// last.ToolCallID is the safer fallback there).
	toolName := last.ToolCallID
	if isExecutionGateErr && toolName == "" {
		// Strip `tool "` prefix and read until next `"`.
		rest := last.Content[len(executionGatePrefix):]
		if idx := strings.Index(rest, `"`); idx > 0 {
			toolName = rest[:idx]
		}
	}
	if toolName == "" {
		toolName = "unknown"
	}
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:         string(ts.currentGoalPhase()),
		Iteration:     ts.iteration,
		TextEmpty:     false,
		HasToolCalls:  true,
		ToolName:      toolName,
		ToolExecError: last.Content,
		// Phase 12.6.1: classify transient vs permanent by scanning error
		// text for known transient markers. Heuristic only — a false
		// transient classification just appends the transient-hint
		// suffix; LLM still gets the standard retry prompt either way.
		//
		// Phase 12.18: execution gate rejections (tool not in allowlist)
		// are PERMANENT (arg changes can't help — the tool is blocked
		// by phase policy). Mark as non-transient so the standard
		// retry prompt asks the LLM to "invoke a different tool or
		// call complete_goal" rather than waiting and retrying.
		IsTransient:   !isExecutionGateErr && isTransientErrorText(last.Content),
		MaxIterations: ts.iterationCap,
	})
	if action == RecoveryArchiveGoal {
		return toolName, msg
	}
	// Phase 12.18: also propagate msg for retry actions so callers
	// (tests + future same-iter recovery) can verify the injected hint
	// matches the current phase. Production callers (turn_coord.go:361
	// and pipeline_llm.go:728) only inspect toolName to decide between
	// archive vs continue, so returning "" here is safe.
	return "", msg
}

// buildToolExecErrorRetryMessage constructs the retry message for the
// tool-execution-error recovery trigger. Phase 12: when a tool knowledge
// store is configured and a lesson body exists for the failing tool, that
// body is appended to the message so the LLM gets concrete guidance from
// prior calls (avoid the same mistake / pick the right argument shape /
// surface a known workaround).
//
// Phase 12.6.1: now takes `isTransient` flag — when true, appends the
// transient-hint suffix (suggests wait-then-retry without arg changes).
// When false, the base message asks for diagnose-or-recomplete logic.
//
// Phase 12.18: now takes `phase` string — when Phase==GoalPhaseSet it
// appends ToolExecErrorSetPhaseHint (tells the LLM the only allowed tool
// is set_goal), when Phase==GoalPhaseFinal it appends
// ToolExecErrorFinalPhaseHint (only complete_goal is allowed), and when
// Phase==GoalPhaseCheckpoint it appends ToolExecErrorCheckpointPhaseHint
// (only goal_progress + complete_goal are allowed). Other phases (Open)
// get no suffix — the base message + transient hint + tool knowledge are
// sufficient for those.
//
// Format (non-transient, phase=Open/Checkpoint/empty):
//
//	"Tool "view_goal" failed: <errMsg>. You may retry the same call with
//	 adjusted arguments, invoke a different tool, or call complete_goal
//	 if the goal is unreachable.\n\n<Tool knowledge for <toolName>>:\n
//	 <body>"
//
// Format (transient) — appends ToolExecErrorTransientHint:
//
//	"Tool "view_goal" failed: <errMsg>. You may retry the same call with
//	 adjusted arguments, invoke a different tool, or call complete_goal
//	 if the goal is unreachable. The error looks transient (timeout,
//	 rate-limit, or network). Wait briefly and retry the SAME call —
//	 argument changes are unlikely to help.\n\n<Tool knowledge>:\n<body>"
//
// Format (phase=Set) — appends ToolExecErrorSetPhaseHint AFTER the base
// message but BEFORE tool knowledge: the LLM is told "set_goal is the
// only allowed tool, here is the argument shape".
//
// Returns just the standard message when registry is nil or no knowledge
// exists for the tool. Never returns an empty string.
func buildToolExecErrorRetryMessage(toolName, errMsg string, isTransient bool, registry *tools.ToolRegistry, phase string) string {
	base := fmt.Sprintf(ToolExecErrorRetryMessage, toolName, errMsg)
	if isTransient {
		base += ToolExecErrorTransientHint
	}
	// Phase 12.15: append phase-specific redirect hint BEFORE tool knowledge
	// so the LLM sees the corrective guidance first.
	// Phase 12.18: extended to GoalPhaseCheckpoint so iter-cap LLM calls
	// receive explicit guidance to wrap up via goal_progress or
	// complete_goal (instead of silently looping into cap-hit canned
	// fallback).
	switch phase {
	case string(GoalPhaseSet):
		base += ToolExecErrorSetPhaseHint
	case string(GoalPhaseFinal):
		base += ToolExecErrorFinalPhaseHint
	case string(GoalPhaseCheckpoint):
		base += ToolExecErrorCheckpointPhaseHint
	}
	if registry == nil {
		return base
	}
	store := registry.ToolKnowledgeStore()
	if store == nil {
		return base
	}
	knowledge := store.LoadForEscalation(toolName)
	if knowledge == "" {
		return base
	}
	return base + "\n\n" + tools.AppendKnowledgeSection(knowledge)
}

// isTransientErrorText classifies a tool-execution error message as
// transient or permanent based on substring markers. Phase 12.6.1 — when
// true, the retry prompt appends ToolExecErrorTransientHint suggesting
// wait-then-retry without arg changes.
//
// Markers (intentionally substring matches — error wording varies across
// tools):
//
//   - "connection"      (refused / reset / closed) — network failures
//   - "timeout"         (i/o / handshake) — network failures
//   - "rate limit"      — provider-side throttle (HTTP 429)
//   - "429" / "502" / "503" / "504" — HTTP transient codes
//   - "no such host"    — DNS failures
//
// Heuristic — false positives and false negatives are both acceptable.
// Conservative bias: prefer false-negative (say permanent when actually
// transient) so the LLM gets the standard retry prompt instead of the
// wait-then-retry hint. The standard prompt still allows the LLM to retry
// the same call.
func isTransientErrorText(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	transientMarkers := []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"timeout",
		"rate limit",
		"http 429",
		"http 502",
		"http 503",
		"http 504",
		"no such host",
		"tls handshake",
	}
	for _, m := range transientMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

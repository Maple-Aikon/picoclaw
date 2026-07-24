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
	if ctx.Phase == "" || ctx.Phase == string(GoalPhaseFinal) || ts.postCompleteGoalReportSent {
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
	// Phase 12: when about to retry, fetch tool_knowledge for that tool
	// (lessons learned from prior calls) and append to the prompt so the
	// LLM gets relevant guidance instead of repeating the same mistake.
	if ctx.ToolName != "" && ctx.Phase != string(GoalPhaseSet) { // GoalPhaseLock aliases to GoalPhaseSet per Phase 11
		if ts.toolExecRecoveryAttempts == nil {
			ts.toolExecRecoveryAttempts = make(map[string]int)
		}
		if ts.toolExecRecoveryAttempts[ctx.ToolName] < ToolExecErrorRetryCap {
			ts.toolExecRecoveryAttempts[ctx.ToolName]++
			// Phase 12.6.1: thread `IsTransient` so the prompt can suggest
			// wait-then-retry (transient) vs diagnose-or-recomplete (permanent).
			// Caller (checkToolExecErrorRecovery / pipeline) sets this from
			// the tool result's error text + circuit-breaker state.
			msg := buildToolExecErrorRetryMessage(ctx.ToolName, ctx.ToolExecError, ctx.IsTransient, ctx.ToolKnowledgeRegistry)
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
		IsTransient:   isTransientErrorText(last.Content),
		MaxIterations: ts.iterationCap,
	})
	if action == RecoveryArchiveGoal {
		return toolName, msg
	}
	return "", ""
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
// Format (non-transient):
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
// Returns just the standard message when registry is nil or no knowledge
// exists for the tool. Never returns an empty string.
func buildToolExecErrorRetryMessage(toolName, errMsg string, isTransient bool, registry *tools.ToolRegistry) string {
	base := fmt.Sprintf(ToolExecErrorRetryMessage, toolName, errMsg)
	if isTransient {
		base += ToolExecErrorTransientHint
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

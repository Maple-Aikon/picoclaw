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
	Phase         string // current goal phase ("" / "Lock" / "Open" / "Checkpoint" / "Final")
	Iteration     int
	TextEmpty     bool   // LLM response text was empty
	HasToolCalls  bool   // LLM response included at least one tool call
	ToolName      string // for ToolExecError trigger: which tool failed
	ToolExecError string // for ToolExecError trigger: the error message from the tool executor
	ProviderError bool   // for ProviderTransient trigger: was this a provider-side transient error
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
	TextOnlySoftRetryMessage = "Bạn vừa trả lời text-only không gọi tool. Bạn đã hoàn thành goal chưa? Nếu xong rồi, hãy gọi `complete_goal`. Nếu có vấn đề quan trọng cần user phê duyệt, hãy gọi `complete_goal` kèm câu hỏi cho user. Nếu có thể tự quyết định, hãy tiếp tục làm việc với tool phù hợp."

	// TextOnlyHardRetryMessage (Phase 12) — fires on the second text-only
	// retry within an iteration. Firm tone: LLM MUST pick one of three
	// paths or the turn will be archived.
	TextOnlyHardRetryMessage = "⚠️ Lần thứ 2 liên tiếp text-only không gọi tool. Bạn PHẢI đưa ra quyết định ngay trong response tới: (1) gọi `complete_goal` nếu goal đã hoàn thành; (2) gọi `complete_goal` + câu hỏi user nếu cần user phê duyệt quyết định quan trọng; (3) gọi tool để tiếp tục thực hiện. Nếu response tới vẫn text-only, turn sẽ bị archive."

	// ToolExecErrorRetryMessage tells the LLM that a tool execution failed
	// and asks it to retry the call (possibly with different args). Phase 12
	// rewrites this as a builder function (buildToolExecErrorRetryMessage)
	// so it can include the relevant tool_knowledge section when available.
	ToolExecErrorRetryMessage = "A tool execution failed: %s. You may retry the same call with adjusted arguments, invoke a different tool, or call complete_goal if the goal is unreachable."
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
	// Phase 12: when about to retry, fetch tool_knowledge for that tool
	// (lessons learned from prior calls) and append to the prompt so the
	// LLM gets relevant guidance instead of repeating the same mistake.
	if ctx.ToolName != "" && ctx.Phase != "Lock" {
		if ts.toolExecRecoveryAttempts == nil {
			ts.toolExecRecoveryAttempts = make(map[string]int)
		}
		if ts.toolExecRecoveryAttempts[ctx.ToolName] < ToolExecErrorRetryCap {
			ts.toolExecRecoveryAttempts[ctx.ToolName]++
			msg := buildToolExecErrorRetryMessage(ctx.ToolName, ctx.ToolExecError, ctx.ToolKnowledgeRegistry)
			return RecoveryRetrySameIteration, msg
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
// Format:
//
//	"A tool execution failed: <errMsg>. You may retry the same call with
//	 adjusted arguments, invoke a different tool, or call complete_goal
//	 if the goal is unreachable.\n\n<Tool knowledge for <toolName>>:\n
//	 <body>"
//
// Returns just the standard message when registry is nil or no knowledge
// exists for the tool. Never returns an empty string.
func buildToolExecErrorRetryMessage(toolName, errMsg string, registry *tools.ToolRegistry) string {
	base := fmt.Sprintf(ToolExecErrorRetryMessage, errMsg)
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

package agent

import (
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Phase 12.33 + 12.67b: rebuild system prompt when the prompt STATE
// (goal phase OR iteration) changes mid-turn.
//
// Without this hook, messages[0] persists from iter 0 across all iters
// of a turn, even when GoalPhase changes (e.g., Open → Checkpoint at
// iter=MaxIter). The LLM at iter 5 (phase=Checkpoint) sees the iter 0
// SET-phase prompt with "call set_goal" instruction, contradicting the
// actual CHECKPOINT-phase allowlist ([goal_progress, complete_goal]).
//
// Phase 12.67b extends the trigger from phase-only to (phase, iteration):
// the dynamic compass line ("Goal phase: open (iter N / total M)",
// formatIterCompass) is rendered from req.Iteration at build time, but
// the same phase can span many iterations (OPEN: iters 2..25). Without
// the iteration dimension, the compass froze at the first iter of each
// phase — the LLM read "iter 2" at iter 12, so compass claims went stale
// while the wire (allowlist, phase resolver) moved on. Rebuilding on
// every iter change keeps the compass the latest; the cost is one
// system-prompt rebuild per iteration (OPEN consults the iteration-
// keyed cache, non-Open phases rebuild directly per Phase 12.16.1).
//
// Wire trace evidence (2026-07-31 06:55 ICT): HORUS Protocol 6-iter
// turn showed only 2 prompt_build events (both at iter 0). LLM at
// iter 5 was observed calling goal_progress correctly because the
// tool allowlist gate (tools_visible=2) overrode the contradictory
// prompt text — but this is fragile.
//
// IMPORTANT: even though goalPhaseSetHintTextTemplate (Phase 12.16.1)
// dynamically fills the "(iter N)" reference from req.Iteration, the
// hint TEXT BODY ("only set_goal is available") still belongs to the
// SET phase. If the cache returns the iter-0 build at iter 5 phase=
// Checkpoint, the LLM reads SET-specific instructions even with the
// correct iter number. Phase 12.16.1 only fixed the iter number drift,
// not the phase drift. Phase 12.33 closes the phase drift gap.
//
// Detection: compare current GoalPhase to lastBuiltPromptPhase. Empty
// string (first iter) always triggers rebuild. Set/Checkpoint/Final
// phases use the no-cache rebuild path via BuildSystemPrompt
// (Phase 12.16.1 followup: isCacheableGoalPhase returns false for non-
// Open, so non-Open builds bypass cache). Open phase rebuilds via the
// same path but may consult cache (cache key encodes phase, so fresh
// key on phase change forces rebuild).
//
// Implementation note: use BuildSystemPrompt (returns string, not
// []providers.Message) so we replace ONLY messages[0] without
// duplicating the user message. BuildMessagesFromPrompt would append
// history+currentUserMessage to newMessages, which would DUPLICATE
// the user message when we append messages[1:] on top.

// maybeRebuildPromptForStateChange returns the messages array with
// messages[0] replaced if the prompt state (goal phase OR iteration)
// changed since the last build. If the state hasn't changed, returns the
// input messages unchanged (no rebuild cost). Caller is responsible for
// assigning the returned value back to `exec.messages` and `messages`
// (the local).
//
// The hook does NOT depend on exec (history/summary) — BuildSystemPrompt
// only needs the phase + postCompleteGoalReport + iteration to produce
// the system prompt string.
func (ts *turnState) maybeRebuildPromptForStateChange(
	messages []providers.Message,
	exec *turnExecution,
	cfg *config.Config,
	iteration int,
) []providers.Message {
	currentPhase := string(ts.currentGoalPhase())
	stateChanged := ts.lastBuiltPromptPhase != currentPhase ||
		ts.lastBuiltPromptIteration != iteration

	if IsAgentDebugEnabled() {
		agentDebugf("prompt_cache", map[string]any{
			"event_stage":               "phase_change_check",
			"iteration":                 iteration,
			"ts_current_goal_phase":     currentPhase,
			"ts_last_built_prompt_phase": ts.lastBuiltPromptPhase,
			"ts_last_built_prompt_iter":  ts.lastBuiltPromptIteration,
			"rebuild_needed":            stateChanged,
		})
	}
	if !stateChanged {
		return messages // no rebuild needed
	}

	// NOTE: do NOT call recordSkillContextSnapshot here. The skill snapshot
	// was already recorded at the initial build (pipeline_setup.go:55). On
	// rebuild we provide the SAME skills to the LLM — recording again would
	// duplicate entries in AttemptedSkills (Phase 12.33 regression: observed
	// [observe-skill missing-skill] in TestEvolutionBridge).

	rebuildPromptReq := promptBuildRequestForTurn(
		ts,
		exec.history,
		exec.summary,
		ts.userMessage,
		ts.media,
		cfg,
	)

	// Build new messages via BuildMessagesFromPrompt (preserves summary +
	// history + current user). We take only newMessages[0] (the rebuilt
	// system prompt with summary injected) and then append the original
	// messages[1:] (which already contains history + current user with
	// media-resolution metadata). This avoids duplicating the user message
	// that BuildMessagesFromPrompt would otherwise add at newMessages[len-1].
	rebuiltAll := ts.agent.ContextBuilder.BuildMessagesFromPrompt(rebuildPromptReq)
	if len(rebuiltAll) == 0 {
		// Edge case: BuildMessagesFromPrompt returned empty (shouldn't happen
		// for a valid phase+iteration, but defensively guard). Return original
		// messages with a stub system prompt so the turn can continue.
		newMessages := []providers.Message{{Role: "system", Content: ""}}
		if len(messages) > 1 {
			newMessages = append(newMessages, messages[1:]...)
		}
		ts.lastBuiltPromptPhase = currentPhase
		ts.lastBuiltPromptIteration = iteration
		return newMessages
	}
	rebuiltSystem := rebuiltAll[0]
	newMessages := make([]providers.Message, 0, len(messages))
	newMessages = append(newMessages, rebuiltSystem)
	if len(messages) > 1 {
		newMessages = append(newMessages, messages[1:]...)
	}
	ts.lastBuiltPromptPhase = currentPhase
	ts.lastBuiltPromptIteration = iteration
	if IsAgentDebugEnabled() {
		agentDebugf("prompt_cache", map[string]any{
			"event_stage":                "phase_change_rebuild",
			"iteration":                  iteration,
			"ts_last_built_prompt_phase": ts.lastBuiltPromptPhase,
			"ts_last_built_prompt_iter":  ts.lastBuiltPromptIteration,
			"new_prompt_len":             len(rebuiltSystem.Content),
		})
	}
	return newMessages
}

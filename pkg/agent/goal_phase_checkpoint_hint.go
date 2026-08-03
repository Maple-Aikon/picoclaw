package agent

import "fmt"

// Phase 12.21: GoalPhaseCheckpoint hint contributor (analog
// goalPhaseSetHintContributor Phase 12.3, extended to Checkpoint phase).
//
// GoalPhaseCheckpoint fires when the per-turn loop hits the
// iterationCap (default = `agent.MaxIterations = max_tool_iterations`)
// AND `iterationCap < maxIterationsCap` (i.e. NOT GoalPhaseFinal).
// Tool allowlist = `[goal_progress, complete_goal]` only.
//
// The hint tells the LLM:
//   - WHICH phase it is in (Checkpoint, iter N).
//   - WHICH 2 tools are currently usable and what each is for.
//   - That ALL other tools are temporarily locked (IsAllowed will reject
//     them with "tool X is not available in the current phase" errors).
//   - Concrete arg-shape for goal_progress + complete_goal so the LLM
//     avoids retrying with malformed args (Phase 12.20 wire guard:
//     remaining_steps REQUIRED, summary REQUIRED for complete_goal).
//   - When to call complete_goal with wait-state summary instead of
//     goal_progress (Phase 12.20.4 deferred → wait-state via summary).
//
// Fires ONLY when the per-turn goal lifecycle is in CHECKPOINT phase.
// Returns nil for Open, Set, or Final so the hint does not bleed
// across the rest of the turn's lifecycle.
//
// Note: this fires INDEPENDENTLY of postCompleteGoalReport (Phase 12.7)
// and goalPhaseSetHintContributor (Phase 12.3). All 3 hint contributors
// are registered separately so the prompt build composes them as
// distinct PromptParts in the Capability layer / Tooling slot.

// goalPhaseCheckpointHintTextTemplate is the GoalPhaseCheckpoint hint
// text with a single %d placeholder filled with the current iter number.
// Do NOT hardcode an iter value — it confuses the LLM when the same
// hint appears at later iters (Phase 12.16.1 cache-bypass lesson).
//
// Phase 12.39: replaced Phase 12.21's Decision tree (a) wording "The system
// will extend the iteration cap by 1" with a lifecycle continue clause.
// Root cause: main-turn-3 2026-08-01 — LLM saw "extend by 1" while config
// actually extends by MaxIterations (5 live, 50 default), reasoned "+1 not
// enough" → called complete_goal prematurely → archived goal. New wording
// omits the extend amount entirely — only tells LLM "use goal_progress to
// continue the lifecycle".
const goalPhaseCheckpointHintTextTemplate = `Goal phase: CHECKPOINT (iter %d).

You are now at a CHECKPOINT in this turn. Only 2 tools are now available — all other tools are temporarily locked and will be rejected with a "not available in the current phase" error if you call them.

The 2 available tools:

1. goal_progress — self-evaluate progress and (optionally) continue the goal lifecycle to keep working on this goal for more iterations. Required fields: completed_steps[], blockers[], remaining_steps[] (at least 1), next_action, drift_detected.

2. complete_goal — finalize this goal now. Required field: summary (1-500 chars, written to the goal archive and used as the final user-facing report if your turn produces no other text).

Decision tree:

(a) If you have more tool work to do for this goal (e.g., need to read more files, run more commands, query APIs further), call goal_progress with remaining_steps listing the steps you still need to do. Use goal_progress to continue the goal lifecycle when work remains.

(b) If your work for this goal is done, call complete_goal with a concise summary of what was accomplished. The summary is recorded as the goal archive entry.

(c) If you need to wait for user input/approval/review before continuing (cannot make further progress without that external signal), call complete_goal with a summary like "Waiting for <user> to review/approve <X> before continuing". Do NOT call goal_progress with empty remaining_steps — it will be rejected.

Multi-turn goal guidance (Phase 12.34):

- Do NOT use complete_goal as a pause mechanism for a multi-turn goal. calling complete_goal finalizes the goal — it does NOT pause it. The goal ends, and the next user turn starts a fresh goal.
- "Waiting for next turn" is not a reason to call complete_goal with a summary like "Waiting for next turn". The next turn will be a NEW goal, not a continuation of this one. If you want to keep working on the same goal, use goal_progress.
- Case (c) is only appropriate when you need an external signal (user input, approval, review) before continuing. It is NOT appropriate for "let me think about it" or "I'll pick this up later" — those are multi-turn goals that need goal_progress.
- Multi-turn goal: when the goal's objective describes work that spans multiple turns (e.g. "upgrade uv and verify tests pass", "implement feature X with multiple sub-steps"), use goal_progress at every checkpoint. ONLY use complete_goal when the goal is genuinely finished.

DO NOT call any other tool (e.g. read_file, write_file, web_search, etc.) while in CHECKPOINT. They will be rejected and you will waste iterations.

When goal_progress fires, your remaining_steps MUST contain at least 1 item — empty remaining_steps is rejected by the wire guard. When complete_goal fires, your summary must be 1-500 chars.`

// goalPhaseCheckpointHintContributor returns a Capability-layer / Tooling-slot
// PromptPart when the request is in GoalPhaseCheckpoint phase. Returns nil
// for any other phase (Open, Set, Final) so the hint does not bleed.
//
// Phase 12.39: header now comes from formatIterCompass (event-marker style)
// instead of the Phase 12.21 static "Goal phase: CHECKPOINT (iter N)" line.
// The Decision tree + multi-turn guidance below the header are unchanged.
// Phase 12.34: prepends goal context (objective + success criteria)
// so the LLM can decide goal_progress vs complete_goal based on actual
// goal state. GoalSnapshot is the output of goal.RenderHeader for the
// active goal — empty when no active goal, in which case the hint
// fires unchanged (backward compat).
func goalPhaseCheckpointHintContributor(req PromptBuildRequest) *PromptPart {
	if req.GoalPhase != string(GoalPhaseCheckpoint) {
		return nil
	}
	// Dynamic header from helper (Phase 12.39). Falls back to legacy
	// template when MaxIterationsCap=0 (backward compat).
	var header string
	if compass := formatIterCompass(req, GoalPhaseCheckpoint, false); compass != "" {
		header = compass + "\n"
	} else {
		// Legacy fallback: use the original template (with iter placeholder).
		header = fmt.Sprintf(goalPhaseCheckpointHintTextTemplate, req.Iteration)
	}
	if req.GoalSnapshot != "" {
		header = req.GoalSnapshot + "\n" + header
	}
	return &PromptPart{
		ID:      "capability.goal_phase_checkpoint_hint",
		Layer:   PromptLayerCapability,
		Slot:    PromptSlotTooling,
		Source:  PromptSource{ID: PromptSourceGoalPhaseCheckpointHint, Name: "goal_phase_checkpoint_hint"},
		Title:   "Goal Phase Checkpoint Hint",
		Content: header + "\n",
		Stable:  false,
		Cache:   PromptCacheNone,
	}
}

// Phase 12.43 (Q4-A, DOUBT-3 rephrased) — instruction for LLM when writing
// Blockers in goal_progress. Describes consequences (which tool, what
// alternative paths) instead of state references. State descriptors may
// become stale after phase transitions and mislead future reasoning.
const goalProgressBlockersHintText = `When writing Blockers in goal_progress: describe consequences (which tool was rejected, what alternative paths exist) instead of state references. State descriptors (e.g., describing where you are in the lifecycle) may become stale after transitions and mislead future reasoning. Frame continuation needs as tool/information requirements, not as state assertions.`

// goalProgressBlockersHintContributor — Phase 12.43 contributor that fires
// at GoalPhaseCheckpoint to inject the Blockers guidance text. Wired via
// phase-aware contributor registration (Q4-A inline, no new file).
func goalProgressBlockersHintContributor(req PromptBuildRequest) *PromptPart {
	if req.GoalPhase != string(GoalPhaseCheckpoint) {
		return nil
	}
	return &PromptPart{
		ID:      "capability.goal_progress_blockers_hint",
		Layer:   PromptLayerCapability,
		Slot:    PromptSlotTooling,
		Source:  PromptSource{ID: PromptSourceGoalPhaseCheckpointHint, Name: "goal_progress_blockers_hint"},
		Title:   "Goal Progress Blockers Hint",
		Content: goalProgressBlockersHintText + "\n",
		Stable:  false,
		Cache:   PromptCacheNone,
	}
}

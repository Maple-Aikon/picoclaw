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
const goalPhaseCheckpointHintTextTemplate = `Goal phase: CHECKPOINT (iter %d).

You have hit the iteration cap for this turn. Only 2 tools are now available — all other tools are temporarily locked and will be rejected with a "not available in the current phase" error if you call them.

The 2 available tools:

1. goal_progress — self-evaluate progress and (optionally) extend the iteration cap to keep working on this goal for more iterations. Required fields: completed_steps[], blockers[], remaining_steps[] (at least 1), next_action, drift_detected.

2. complete_goal — finalize this goal now. Required field: summary (1-500 chars, written to the goal archive and used as the final user-facing report if your turn produces no other text).

Decision tree:

(a) If you have more tool work to do for this goal (e.g., need to read more files, run more commands, query APIs further), call goal_progress with remaining_steps listing the steps you still need to do. The system will extend the iteration cap by 1.

(b) If your work for this goal is done, call complete_goal with a concise summary of what was accomplished. The summary is recorded as the goal archive entry.

(c) If you need to wait for user input/approval/review before continuing (cannot make further progress without that external signal), call complete_goal with a summary like "Waiting for <user> to review/approve <X> before continuing". Do NOT call goal_progress with empty remaining_steps — it will be rejected.

DO NOT call any other tool (e.g. read_file, write_file, web_search, etc.) while in CHECKPOINT. They will be rejected and you will waste iterations.

When goal_progress fires, your remaining_steps MUST contain at least 1 item — empty remaining_steps is rejected by the wire guard. When complete_goal fires, your summary must be 1-500 chars.`

// goalPhaseCheckpointHintContributor returns a Capability-layer / Tooling-slot
// PromptPart when the request is in GoalPhaseCheckpoint phase. Returns nil
// for any other phase (Open, Set, Final) so the hint does not bleed.
func goalPhaseCheckpointHintContributor(req PromptBuildRequest) *PromptPart {
	if req.GoalPhase != string(GoalPhaseCheckpoint) {
		return nil
	}
	iter := req.Iteration
	if iter <= 0 {
		iter = 1
	}
	header := fmt.Sprintf(goalPhaseCheckpointHintTextTemplate, iter)
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

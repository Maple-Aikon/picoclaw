package agent

// Phase 12.21: GoalPhaseFinal hint contributor (analog
// goalPhaseSetHintContributor, extended to Final phase).
//
// GoalPhaseFinal fires when iterationCap reaches or exceeds
// maxIterationsCap (Phase 11 4-phase model: iter >= max_iterations_cap).
// In this phase, ONLY complete_goal is available — all other tools
// (including set_goal/goal_progress) are locked.
//
// In normal operation, GoalPhaseFinal is only reached AFTER
// complete_goal has already been called (post-complete_goal final-report
// iter; see goalCompleteReportHintContributor Phase 12.7). The
// post-complete_goal hint fires on top of this one — the layers are
// distinct (one for final-report, one for the locked-tools affordance).
//
// Edge case: GoalPhaseFinal can also fire WITHOUT a prior complete_goal
// when max_iterations_cap is hit in a turn where the LLM never reached
// goal_progress either (rare but possible — Phase 12.20.1: terminal-state
// handling). In that case, the hint serves as a single way out:
// call complete_goal with a "partial work" summary.
//
// Hint tells the LLM:
//   - WHICH phase it is in (Final, iter N).
//   - ONLY 1 tool is available: complete_goal (idempotent + summary 1-500).
//   - All OTHER tools are temporarily locked (Phase 11).
//
// Returns nil for any non-Final phase so the hint does not bleed.

const goalPhaseFinalHintText = `Goal phase: FINAL (terminal iter).

You have hit the absolute maximum iteration cap. Only 1 tool is now available — complete_goal. All other tools (including set_goal and goal_progress) are permanently locked for this turn.

If you have not already completed this goal, call complete_goal now with a summary describing what was accomplished. The summary field is required (1-500 chars) and is the user-facing final report for this turn.

If you have already called complete_goal earlier in this turn, calling it again is safe and idempotent — it returns success without changing state. Do NOT call set_goal or goal_progress; they will be rejected.

DO NOT call any other tool (read_file, write_file, web_search, etc.). They will be rejected and you will waste this final iter.`

// goalPhaseFinalHintContributor returns a Capability-layer / Tooling-slot
// PromptPart when the request is in GoalPhaseFinal phase. Returns nil
// for any other phase (Open, Set, Checkpoint).
//
// Phase 12.21: GoalPhaseFinal is the terminal phase of the per-turn
// loop. The hint fires INDEPENDENTLY of postCompleteGoalReport
// (Phase 12.7) — both are layered into the Capability / Tooling slot.
// When both fire in the same prompt build, the post-complete_goal
// hint dominates because it carries the 5-section structured template.
func goalPhaseFinalHintContributor(req PromptBuildRequest) *PromptPart {
	if req.GoalPhase != string(GoalPhaseFinal) {
		return nil
	}
	return &PromptPart{
		ID:      "capability.goal_phase_final_hint",
		Layer:   PromptLayerCapability,
		Slot:    PromptSlotTooling,
		Source:  PromptSource{ID: PromptSourceGoalPhaseFinalHint, Name: "goal_phase_final_hint"},
		Title:   "Goal Phase Final Hint",
		Content: goalPhaseFinalHintText + "\n",
		Stable:  false,
		Cache:   PromptCacheNone,
	}
}

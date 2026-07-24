package agent

// Phase 12.7: Post-complete_goal final-report hint.
//
// After complete_goal is called, the per-turn loop re-enters once with
// phase = GoalPhaseFinal, tool allowlist = []. This hint tells the LLM:
// "This is your LAST CHANCE to provide a final report to the user for this
// goal. Provide it now or add additional info." See plan file
// ~/.picoclaw/workspace/memory/plan/picoclaw-phase12.7-post-complete-goal-final-report-iter-20260724.md §3.2.
//
// Owner decision (2026-07-24 08:50 ICT, anh Maple): Always emit this hint
// after complete_goal — even if the LLM already emitted text in the same
// iteration. The LLM can supplement or skip; either is fine. This guarantees
// the LLM always has one last chance to provide a final user-facing report.

// goalCompleteReportHintText is the static hint injected at the
// post-complete_goal final-report iter (Phase 12.7). English per
// USER.md preference (saves tokens vs VN for recovery prompts).
const goalCompleteReportHintText = `Goal complete. The final summary has been recorded.

This is your LAST CHANCE to provide a final report to the user for this goal. If you have not yet given a complete user-facing report, provide it now. You may also add any additional info or supplementary details you want the user to know.

Tools are now locked. Do NOT call any tools.`

// goalCompleteReportHintContributor returns a Capability-layer / Tooling-slot
// PromptPart when PostCompleteGoalReport=true (the post-complete_goal
// final-report iter in Phase 12.7). Returns nil otherwise.
//
// Layer:   Capability (system-level directive)
// Slot:    Tooling (groups with other tool-usage rules)
// Source:  PromptSourceGoalCompleteReportHint
func goalCompleteReportHintContributor(req PromptBuildRequest) *PromptPart {
	if !req.PostCompleteGoalReport {
		return nil
	}
	return &PromptPart{
		ID:      string(PromptSourceGoalCompleteReportHint),
		Layer:   PromptLayerCapability,
		Slot:    PromptSlotTooling,
		Content: goalCompleteReportHintText,
	}
}

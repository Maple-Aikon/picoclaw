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
//
// Phase 12.20.1: structured 5-section template. Owner decision (anh Maple,
// 2026-07-27 06:24 ICT): final reports were sometimes too short / missing
// sections. New template enforces 5 sections: (1) full answer, (2) done so
// far with concrete artifacts, (3) remaining / not done, (4) approach
// pros/cons, (5) open notes going forward. Anchors via "Tools are now
// locked. Do NOT call any tools." preserves Phase 12.7 behavior.
//
// Phase 12.68 Option B (anh Maple, 2026-08-10): section 1 is now FULL
// ANSWER — the LLM must reproduce the complete answer to the user's
// original request, because earlier-iteration text (incl. the complete_goal
// explanation) is never delivered to the user. Root cause of main-turn-13
// (2026-08-10 06:29): a 2515-char explanation was swallowed at iter 2 and
// the post-final iter only produced a 302-char wrap-up because the hint
// never told the LLM the user saw nothing yet.
const goalCompleteReportHintText = `Goal phase: POST-FINAL.

Goal complete. The final summary has been recorded.

IMPORTANT: your reply from the previous iteration was NOT delivered to the user — only the goal summary was recorded. This iteration is the ONLY one whose text reaches the user. Treat it as your FIRST and only answer.

Tools are now locked — do NOT call any tools (including set_goal or complete_goal again). Output a single user-facing reply in 5 sections, in this order:

1. FULL ANSWER — restate the user's original request in 1-2 sentences, then reproduce your COMPLETE answer to it in full detail, as if answering for the first time. The user has not seen any earlier text — do NOT reference or summarize it. Include all details: lists, names, numbers, file paths, examples. This is the main content of your reply.

2. DONE SO FAR — list what was accomplished. Include concrete artifacts: file paths, commit hashes, binary md5 sums, API endpoints shipped, migrations applied, etc. Use bullet points.

3. REMAINING / NOT DONE — list anything left incomplete or out of scope. Be honest about gaps. If everything is done, say "Nothing remaining."

4. APPROACH: PROS / CONS — brief honest tradeoff analysis of the chosen approach (1-2 short paragraphs or bullets). Why this approach? What did it cost? What did it buy? When would a different approach have been better?

5. OPEN NOTES — anything the user should know going forward: caveats, follow-ups, gotchas to remember next time, deferred items, recommended next-step direction.

Keep the reply focused and scannable. Skip a section ONLY if it genuinely does not apply (and say so explicitly); otherwise cover all 5.`

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
		Source:  PromptSource{ID: PromptSourceGoalCompleteReportHint, Name: "goal_complete_report_hint"},
		Content: goalCompleteReportHintText,
	}
}

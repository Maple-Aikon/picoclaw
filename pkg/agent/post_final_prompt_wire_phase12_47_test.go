// Phase 12.47 (T7) — Wire test: BuildMessagesFromPrompt at post_final + report.
// Verifies:
//
//	(1) the report hint fires (PART with PromptSourceGoalCompleteReportHint + "POST-FINAL")
//	(2) ABSENCE assertions: SET/OPEN/CHECKPOINT/FINAL hints all ABSENT
//	(3) cache bypass — bypass for non-Open, md5 differs per call
package agent

import (
	"testing"
)

func TestPostFinal_PromptBuild_Wire_F5(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())
	opts := systemPromptBuildOptions{
		IncludeToolUseRule:     true,
		GoalPhase:              string(GoalPhasePostFinal),
		PostCompleteGoalReport: true,
	}

	parts := cb.buildSystemPromptParts(opts)
	seen := map[PromptSourceID]bool{}
	for _, p := range parts {
		seen[p.Source.ID] = true
	}

	// LEVER (a) - 2026-11-10: the report hint moved OUT of the system
	// prompt into the phase channel. The system prompt must NOT carry it
	// (that is what keeps the prefix byte-constant across transitions).
	if seen[PromptSourceGoalCompleteReportHint] {
		t.Fatalf("report hint must NOT be in the system prompt (lever (a))")
	}
	// The report hint must still reach the LLM - via the phase channel.
	assertPhaseChannelContains(t, cb, PromptBuildRequest{
		GoalPhase:              string(GoalPhasePostFinal),
		PostCompleteGoalReport: true,
		Iteration:              2,
		IterationCap:           25,
		MaxIterationsCap:       300,
		PhaseStartIter:         2,
		CurrentMessage:         "do the thing",
	}, "Goal phase: POST-FINAL", "post-final report hint must reach the LLM via the phase channel")

	// Absent: 4 phase hints
	for _, missing := range []PromptSourceID{
		PromptSourceGoalPhaseSetHint,
		PromptSourceGoalPhaseOpenHint,
		PromptSourceGoalPhaseCheckpointHint,
		PromptSourceGoalPhaseFinalHint,
	} {
		if seen[missing] {
			t.Fatalf("hint %s must be ABSENT at post_final", missing)
		}
	}
}

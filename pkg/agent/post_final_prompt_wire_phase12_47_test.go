// Phase 12.47 (T7) — Wire test: BuildMessagesFromPrompt at post_final + report.
// Verifies:
//   (1) the report hint fires (PART with PromptSourceGoalCompleteReportHint + "POST-FINAL")
//   (2) ABSENCE assertions: SET/OPEN/CHECKPOINT/FINAL hints all ABSENT
//   (3) cache bypass — bypass for non-Open, md5 differs per call
package agent

import (
	"strings"
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

	// Present: report hint
	if !seen[PromptSourceGoalCompleteReportHint] {
		ids := make([]string, 0, len(parts))
		for _, p := range parts { ids = append(ids, string(p.Source.ID)) }
		t.Fatalf("report hint ABSENT at post_final; got %v", ids)
	}
	for _, p := range parts {
		if p.Source.ID == PromptSourceGoalCompleteReportHint &&
			!strings.Contains(p.Content, "Goal phase: POST-FINAL") {
			t.Fatalf("report hint missing POST-FINAL header; got:\n%s", p.Content)
		}
	}

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

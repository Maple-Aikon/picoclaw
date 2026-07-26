package agent

import (
	"strings"
	"testing"
)

// Phase 12.7 — Post-complete_goal final-report hint contributor tests.
// See plan file:
// ~/.picoclaw/workspace/memory/plan/picoclaw-phase12.7-post-complete-goal-final-report-iter-20260724.md §4.1

func TestGoalCompleteReportHint_FiresWhenPostFlagSet(t *testing.T) {
	got := goalCompleteReportHintContributor(PromptBuildRequest{
		GoalPhase:               string(GoalPhaseFinal),
		PostCompleteGoalReport: true,
	})
	if got == nil {
		t.Fatalf("expected non-nil PromptPart when PostCompleteGoalReport=true")
	}
	if got.ID != string(PromptSourceGoalCompleteReportHint) {
		t.Errorf("ID = %q, want %q", got.ID, PromptSourceGoalCompleteReportHint)
	}
	if got.Content != goalCompleteReportHintText {
		t.Errorf("Content mismatch")
	}
}

func TestGoalCompleteReportHint_SuppressedWhenFlagFalse(t *testing.T) {
	got := goalCompleteReportHintContributor(PromptBuildRequest{
		GoalPhase:               string(GoalPhaseFinal),
		PostCompleteGoalReport: false,
	})
	if got != nil {
		t.Fatalf("expected nil PromptPart when PostCompleteGoalReport=false")
	}
}

func TestGoalCompleteReportHint_ContentMentionsLastChance(t *testing.T) {
	got := goalCompleteReportHintContributor(PromptBuildRequest{PostCompleteGoalReport: true})
	if got == nil {
		t.Fatalf("expected non-nil PromptPart")
	}
	// Owner decision (2026-07-24 08:50 ICT, anh Maple): hint should clearly
	// state "LAST CHANCE" so LLM knows this is the final opportunity.
	if !strings.Contains(got.Content, "LAST CHANCE") {
		t.Errorf("hint missing 'LAST CHANCE' marker; got: %q", got.Content)
	}
	if !strings.Contains(got.Content, "Tools are now locked") {
		t.Errorf("hint missing 'Tools are now locked' marker; got: %q", got.Content)
	}
}

func TestGoalCompleteReportHint_LayerSlotCapabilityTooling(t *testing.T) {
	got := goalCompleteReportHintContributor(PromptBuildRequest{PostCompleteGoalReport: true})
	if got == nil {
		t.Fatalf("expected non-nil PromptPart")
	}
	if got.Layer != PromptLayerCapability {
		t.Errorf("Layer = %q, want %q (Capability-layer groups system directives)", got.Layer, PromptLayerCapability)
	}
	if got.Slot != PromptSlotTooling {
		t.Errorf("Slot = %q, want %q (Tooling-slot groups tool-usage rules)", got.Slot, PromptSlotTooling)
	}
}

// Phase 12.20.1: enhanced hint must include all 5 structured-section markers
// so LLM has an explicit template to fill. Owner decision (anh Maple,
// 2026-07-27 06:24 ICT): final reports were sometimes too short or missing
// sections — 5-section template enforces task recap / done / remaining /
// approach pros-cons / open notes.
func TestGoalCompleteReportHint_StructuredFiveSections_Phase12_20_1(t *testing.T) {
	got := goalCompleteReportHintContributor(PromptBuildRequest{PostCompleteGoalReport: true})
	if got == nil {
		t.Fatalf("expected non-nil PromptPart")
	}
	sections := []struct {
		name    string
		keyword string
	}{
		{"task-recap", "TASK RECAP"},
		{"done", "DONE SO FAR"},
		{"remaining", "REMAINING"},
		{"approach", "APPROACH"},
		{"open-notes", "OPEN NOTES"},
	}
	for _, s := range sections {
		if !strings.Contains(got.Content, s.keyword) {
			t.Errorf("hint missing section %q (expected keyword %q) — see Phase 12.20.1 5-section template", s.name, s.keyword)
		}
	}
}

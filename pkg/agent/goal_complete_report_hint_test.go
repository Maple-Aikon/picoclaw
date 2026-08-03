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

// Phase 12.45.1 — Wire test: the hint must actually SURVIVE stack.Add
// (ValidatePart) and reach the rendered prompt parts. Phase 12.45 live
// trace (main-turn-2, 2026-08-03 23:16) found the contributor was called
// but its part was rejected with "prompt part ... has empty source id" —
// goalCompleteReportHintContributor never set Source, so the hint was
// silently skipped from every prompt since Phase 12.7 (2026-07-24).
// Unit tests above assert contributor output only — they cannot catch
// a ValidatePart rejection. Lesson: Phase 12.32 "code grep != rendered
// prompt" applies to stack.Add survival too.
func TestGoalCompleteReportHint_WirePath_ReachesPromptParts(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
		IncludeToolUseRule:     true,
		GoalPhase:              string(GoalPhaseFinal),
		PostCompleteGoalReport: true,
	})
	found := false
	for _, p := range parts {
		if p.Source.ID == PromptSourceGoalCompleteReportHint {
			found = true
			if !strings.Contains(p.Content, "LAST CHANCE") {
				t.Errorf("wired hint missing 'LAST CHANCE' marker; got:\n%s", p.Content)
			}
			if !strings.Contains(p.Content, "TASK RECAP") {
				t.Errorf("wired hint missing 5-section template; got:\n%s", p.Content)
			}
			break
		}
	}
	if !found {
		ids := make([]string, 0, len(parts))
		for _, p := range parts {
			ids = append(ids, string(p.Source.ID))
		}
		t.Fatalf("expected hint part %q in prompt parts (rejected by ValidatePart?); got parts: %v",
			PromptSourceGoalCompleteReportHint, ids)
	}
}

// Wire test companion: hint must NOT reach parts when the post-report
// flag is off (no bleed into normal Final-phase prompts).
func TestGoalCompleteReportHint_WirePath_SuppressedWithoutFlag(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
		IncludeToolUseRule:     true,
		GoalPhase:              string(GoalPhaseFinal),
		PostCompleteGoalReport: false,
	})
	for _, p := range parts {
		if p.Source.ID == PromptSourceGoalCompleteReportHint {
			t.Fatalf("hint part must not appear when PostCompleteGoalReport=false")
		}
	}
}

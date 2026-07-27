package agent

import (
	"strings"
	"testing"
)

// Phase 12.21 — GoalPhaseCheckpoint hint contributor tests. Analogous to
// goalPhaseSetHintContributor (Phase 12.3) but for Checkpoint phase.

// TestGoalPhaseCheckpointHint_FiresOnlyInCheckpointPhase verifies that the
// hint contributor only returns a non-nil PromptPart when
// req.GoalPhase == string(GoalPhaseCheckpoint) and returns nil for all
// other phases (Open/Set/Final + empty phase).
func TestGoalPhaseCheckpointHint_FiresOnlyInCheckpointPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase string
		want  bool
	}{
		{"Checkpoint_fires", string(GoalPhaseCheckpoint), true},
		{"Open_silent", string(GoalPhaseOpen), false},
		{"Set_silent", string(GoalPhaseSet), false},
		{"Final_silent", string(GoalPhaseFinal), false},
		{"Empty_silent", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := PromptBuildRequest{GoalPhase: tt.phase, Iteration: 25}
			got := goalPhaseCheckpointHintContributor(req)
			if (got != nil) != tt.want {
				t.Fatalf("CheckpointHint(%q) non-nil=%v want=%v", tt.phase, got != nil, tt.want)
			}
		})
	}
}

// TestGoalPhaseCheckpointHint_ContentMentionsLifecycleTools ensures the
// hint text explicitly names goal_progress + complete_goal so the LLM
// does not retry with malformed args (Phase 12.20 wire guard).
func TestGoalPhaseCheckpointHint_ContentMentionsLifecycleTools(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint), Iteration: 25}
	got := goalPhaseCheckpointHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	mustContain := []string{
		"goal_progress",
		"complete_goal",
		"remaining_steps",
		"summary",
		"iter 25",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got.Content, sub) {
			t.Errorf("CheckpointHint missing %q\n--- content ---\n%s", sub, got.Content)
		}
	}
}

// TestGoalPhaseCheckpointHint_PlacementCapabilityTooling ensures the hint
// lands in the Capability / Tooling slot (matches goal_phase_set_hint
// convention from Phase 12.3).
func TestGoalPhaseCheckpointHint_PlacementCapabilityTooling(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint), Iteration: 25}
	got := goalPhaseCheckpointHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	if got.Layer != PromptLayerCapability {
		t.Errorf("Layer=%q want=%q", got.Layer, PromptLayerCapability)
	}
	if got.Slot != PromptSlotTooling {
		t.Errorf("Slot=%q want=%q", got.Slot, PromptSlotTooling)
	}
	if got.Source.ID != PromptSourceGoalPhaseCheckpointHint {
		t.Errorf("Source.ID=%q want=%q", got.Source.ID, PromptSourceGoalPhaseCheckpointHint)
	}
}

// TestGoalPhaseFinalHint_FiresOnlyInFinalPhase verifies hint scoping.
func TestGoalPhaseFinalHint_FiresOnlyInFinalPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase string
		want  bool
	}{
		{"Final_fires", string(GoalPhaseFinal), true},
		{"Checkpoint_silent", string(GoalPhaseCheckpoint), false},
		{"Open_silent", string(GoalPhaseOpen), false},
		{"Set_silent", string(GoalPhaseSet), false},
		{"Empty_silent", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := PromptBuildRequest{GoalPhase: tt.phase}
			got := goalPhaseFinalHintContributor(req)
			if (got != nil) != tt.want {
				t.Fatalf("FinalHint(%q) non-nil=%v want=%v", tt.phase, got != nil, tt.want)
			}
		})
	}
}

// TestGoalPhaseFinalHint_ContentMentionsCompleteGoal ensures the hint
// text explicitly names complete_goal + summary + idempotency so the LLM
// does not retry or call disallowed tools.
func TestGoalPhaseFinalHint_ContentMentionsCompleteGoal(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseFinal)}
	got := goalPhaseFinalHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Final")
	}
	mustContain := []string{
		"complete_goal",
		"summary",
		"idempotent",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got.Content, sub) {
			t.Errorf("FinalHint missing %q\n--- content ---\n%s", sub, got.Content)
		}
	}
}

// TestGoalPhaseFinalHint_PlacementCapabilityTooling ensures the hint
// lands in the Capability / Tooling slot.
func TestGoalPhaseFinalHint_PlacementCapabilityTooling(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseFinal)}
	got := goalPhaseFinalHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Final")
	}
	if got.Layer != PromptLayerCapability {
		t.Errorf("Layer=%q want=%q", got.Layer, PromptLayerCapability)
	}
	if got.Slot != PromptSlotTooling {
		t.Errorf("Slot=%q want=%q", got.Slot, PromptSlotTooling)
	}
	if got.Source.ID != PromptSourceGoalPhaseFinalHint {
		t.Errorf("Source.ID=%q want=%q", got.Source.ID, PromptSourceGoalPhaseFinalHint)
	}
}

// TestGoalPhaseHint_AllThreeAreLayerSeparated verifies that all 3
// phase hints (Set/Checkpoint/Final) return different Source.IDs so
// the prompt-build can compose all 3 simultaneously if a phase
// transition happens mid-turn (Phase 12.16.1 cache-bypass lesson).
func TestGoalPhaseHint_AllThreeAreLayerSeparated(t *testing.T) {
	set := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	cph := goalPhaseCheckpointHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint)})
	fin := goalPhaseFinalHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseFinal)})
	if set == nil || cph == nil || fin == nil {
		t.Fatal("expected all 3 hints to fire in their own phase")
	}
	ids := map[PromptSourceID]bool{
		set.Source.ID:  true,
		cph.Source.ID:  true,
		fin.Source.ID:  true,
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 distinct Source.IDs, got %v", ids)
	}
}

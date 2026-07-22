package agent

import (
	"strings"
	"testing"
)

func TestGoalPhaseSetHint_FiresOnlyInSetPhase(t *testing.T) {
	tests := []struct {
		name      string
		goalPhase string
		want      bool
	}{
		{name: "Set phase — fires", goalPhase: string(GoalPhaseSet), want: true},
		{name: "Open phase — silent", goalPhase: string(GoalPhaseOpen), want: false},
		{name: "Checkpoint phase — silent", goalPhase: string(GoalPhaseCheckpoint), want: false},
		{name: "Final phase — silent", goalPhase: string(GoalPhaseFinal), want: false},
		{name: "Empty phase — silent", goalPhase: "", want: false},
		{name: "Bogus phase — silent", goalPhase: "unknown", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: tt.goalPhase})
			if tt.want && got == nil {
				t.Errorf("expected hint part for phase %q, got nil", tt.goalPhase)
			}
			if !tt.want && got != nil {
				t.Errorf("expected nil hint for phase %q, got part %q", tt.goalPhase, got.ID)
			}
		})
	}
}

func TestGoalPhaseSetHint_ContentMentionsSetGoal(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	mustContain(t, part.Content, "set_goal",
		"hint must reference set_goal so LLM knows which tool is allowed")
}

func TestGoalPhaseSetHint_ContentMentionsLockedTools(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	mustContain(t, part.Content, "locked",
		"hint must state that other tools are temporarily locked")
}

func TestGoalPhaseSetHint_ContentMentionsTwoForwardPaths(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	// Both paths must be reachable in the text so the LLM picks based on
	// turn context, not defaults to one path only.
	mustContain(t, part.Content, "set_goal",
		"hint path 1: call set_goal first")
	mustContain(t, part.Content, "respond directly",
		"hint path 2: no-tool reply is allowed")
}

func TestGoalPhaseSetHint_PlacementCapabilityTooling(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	if part.Layer != PromptLayerCapability {
		t.Errorf("expected Layer=Capability, got %q", part.Layer)
	}
	if part.Slot != PromptSlotTooling {
		t.Errorf("expected Slot=Tooling, got %q", part.Slot)
	}
	if part.Source.ID != PromptSourceGoalPhaseSetHint {
		t.Errorf("expected Source.ID=%q, got %q", PromptSourceGoalPhaseSetHint, part.Source.ID)
	}
}

func mustContain(t *testing.T, haystack, needle, rationale string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected haystack to contain %q (%s), got:\n%s", needle, rationale, haystack)
	}
}
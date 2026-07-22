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

// TestGoalPhaseSetHint_Integration_BuildSystemPromptParts verifies the hint
// is injected into the actual prompt parts emitted by the context builder when
// GoalPhase=Set. Phase 12.3 wiring: promptBuildRequestForTurn populates
// req.GoalPhase from ts.currentGoalPhase(); BuildMessagesFromPrompt passes
// it through to systemPromptBuildOptions.GoalPhase; buildSystemPromptParts
// fires goalPhaseSetHintContributor when GoalPhase==Set.
func TestGoalPhaseSetHint_Integration_BuildSystemPromptParts(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
		IncludeToolUseRule: true,
		GoalPhase:           string(GoalPhaseSet),
	})
	// Hunt for our hint part by ID (stable identifier, no race on text edits).
	found := false
	for _, p := range parts {
		if p.Source.ID == PromptSourceGoalPhaseSetHint {
			found = true
			if !strings.Contains(p.Content, "set_goal") {
				t.Errorf("GoalPhaseSet hint part missing set_goal reference; got:\n%s", p.Content)
			}
			if !strings.Contains(p.Content, "respond directly") {
				t.Errorf("GoalPhaseSet hint part missing no-tool reply path; got:\n%s", p.Content)
			}
			break
		}
	}
	if !found {
		// Print part IDs to aid debugging if the wiring breaks.
		ids := make([]string, 0, len(parts))
		for _, p := range parts {
			ids = append(ids, string(p.Source.ID))
		}
		t.Fatalf("expected hint part %q in prompt parts; got parts: %v", PromptSourceGoalPhaseSetHint, ids)
	}
}

// TestGoalPhaseSetHint_Integration_OpenPhase_NotInjected confirms the hint is
// gated to Set phase only and does NOT bleed into Open/Checkpoint/Final turns.
func TestGoalPhaseSetHint_Integration_OpenPhase_NotInjected(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	for _, phase := range []string{
		string(GoalPhaseOpen),
		string(GoalPhaseCheckpoint),
		string(GoalPhaseFinal),
	} {
		parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
			IncludeToolUseRule: true,
			GoalPhase:           phase,
		})
		for _, p := range parts {
			if p.Source.ID == PromptSourceGoalPhaseSetHint {
				t.Errorf("GoalPhaseSet hint should NOT appear in phase %q; got part: %q", phase, p.ID)
			}
		}
	}
}
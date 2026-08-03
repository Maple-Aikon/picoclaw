package agent

import (
	"strings"
	"testing"
)

// T1 — Fires only at OPEN phase, silent at Set/Checkpoint/Final/empty/bogus.
// Mirrors existing TestGoalPhaseSetHint_FiresOnlyInSetPhase pattern.
func TestGoalPhaseOpenHint_FiresOnlyInOpenPhase(t *testing.T) {
	tests := []struct {
		name      string
		goalPhase string
		want      bool
	}{
		{name: "Set phase — silent", goalPhase: string(GoalPhaseSet), want: false},
		{name: "Open phase — fires", goalPhase: string(GoalPhaseOpen), want: true},
		{name: "Checkpoint phase — silent", goalPhase: string(GoalPhaseCheckpoint), want: false},
		{name: "Final phase — silent", goalPhase: string(GoalPhaseFinal), want: false},
		{name: "Empty phase — silent", goalPhase: "", want: false},
		{name: "Bogus phase — silent", goalPhase: "unknown", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := goalPhaseOpenHintContributor(PromptBuildRequest{GoalPhase: tt.goalPhase})
			if tt.want && got == nil {
				t.Errorf("expected hint part for phase %q, got nil", tt.goalPhase)
			}
			if !tt.want && got != nil {
				t.Errorf("expected nil hint for phase %q, got part %q", tt.goalPhase, got.ID)
			}
		})
	}
}

// T2 — Mentions set_goal is LOCKED at OPEN phase.
func TestGoalPhaseOpenHint_MentionsSetGoalLocked(t *testing.T) {
	part := goalPhaseOpenHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseOpen)})
	if part == nil {
		t.Fatal("expected hint part for Open phase")
	}
	mustContain(t, part.Content, "set_goal",
		"hint must reference set_goal so LLM knows which tool is locked at OPEN")
	mustContain(t, part.Content, "LOCKED",
		"hint must state set_goal is LOCKED at OPEN")
}

// T3 — Mentions goal_progress is CHECKPOINT-only.
func TestGoalPhaseOpenHint_MentionsGoalProgressCheckpointOnly(t *testing.T) {
	part := goalPhaseOpenHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseOpen)})
	if part == nil {
		t.Fatal("expected hint part for Open phase")
	}
	mustContain(t, part.Content, "goal_progress",
		"hint must reference goal_progress")
	mustContain(t, part.Content, "CHECKPOINT",
		"hint must state goal_progress is CHECKPOINT-only")
}

// T4 — Mentions complete_goal is available at OPEN phase with summary shape.
func TestGoalPhaseOpenHint_MentionsCompleteGoalAvailable(t *testing.T) {
	part := goalPhaseOpenHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseOpen)})
	if part == nil {
		t.Fatal("expected hint part for Open phase")
	}
	mustContain(t, part.Content, "complete_goal",
		"hint must reference complete_goal as available at OPEN")
	mustContain(t, part.Content, "1-1000 char",
		"hint must document the complete_goal summary length constraint")
}

// T5 — Placement: Capability layer / Tooling slot / PromptSourceGoalPhaseOpenHint.
func TestGoalPhaseOpenHint_PlacementCapabilityTooling(t *testing.T) {
	part := goalPhaseOpenHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseOpen)})
	if part == nil {
		t.Fatal("expected hint part for Open phase")
	}
	if part.Layer != PromptLayerCapability {
		t.Errorf("expected Layer=Capability, got %q", part.Layer)
	}
	if part.Slot != PromptSlotTooling {
		t.Errorf("expected Slot=Tooling, got %q", part.Slot)
	}
	if part.Source.ID != PromptSourceGoalPhaseOpenHint {
		t.Errorf("expected Source.ID=%q, got %q", PromptSourceGoalPhaseOpenHint, part.Source.ID)
	}
}

// Phase 12.39 — Dynamic header when cap dims are non-zero.
// Verifies the OPEN hint now uses formatIterCompass (event-marker style):
// "Goal phase: open (iter N / total M turn iters)" + "Next CHECKPOINT
// at iter X" / "FINAL phase will be at iter M" (replaced Phase 12.38 v2's
// static "Iteration cap: M" + ceiling warning).
func TestGoalPhaseOpenHint_DynamicHeaderWithCap(t *testing.T) {
	part := goalPhaseOpenHintContributor(PromptBuildRequest{
		GoalPhase:         string(GoalPhaseOpen),
		Iteration:         5,
		IterationCap:      15,
		MaxIterationsCap:  15,
	})
	if part == nil {
		t.Fatal("hint must fire at OPEN phase")
	}
	mustContain(t, part.Content, "Goal phase: open (iter 5 / total 15 turn iters)", "dynamic header must appear when cap dims are set")
	mustContain(t, part.Content, "FINAL phase will be at iter 15", "FINAL marker must appear when iterCap == maxCap (no more CHECKPOINTs)")
}

// Phase 12.38 §4 T2 — Legacy zero-cap caller keeps static text (no
// dynamic header). Backward-compat: callers that don't thread the
// new IterationCap/MaxIterationsCap fields still get the static text.
func TestGoalPhaseOpenHint_LegacyZeroCapKeepsStaticText(t *testing.T) {
	part := goalPhaseOpenHintContributor(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseOpen),
		Iteration:        5,
		IterationCap:     0,
		MaxIterationsCap: 0,
	})
	if part == nil {
		t.Fatal("hint must fire")
	}
	if strings.Contains(part.Content, "Goal phase: OPEN (iter 5)") {
		t.Fatalf("must NOT include dynamic header when cap=0, got: %s", part.Content)
	}
	if !strings.Contains(part.Content, "set_goal") {
		t.Fatalf("must keep static content (set_goal mention), got: %s", part.Content)
	}
}

// T6 — Integration: hint is wired through buildSystemPromptParts.
// Without this, a contributor-function fix could pass unit tests but the
// wiring could still be broken (hint never reaches the LLM). Code grep !=
// rendered prompt.
func TestGoalPhaseOpenHint_Integration_BuildSystemPromptParts(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
		IncludeToolUseRule: true,
		GoalPhase:           string(GoalPhaseOpen),
		Iteration:           2,
	})
	found := false
	for _, p := range parts {
		if p.Source.ID == PromptSourceGoalPhaseOpenHint {
			found = true
			if !strings.Contains(p.Content, "goal_progress") {
				t.Errorf("GoalPhaseOpen hint part missing goal_progress reference; got:\n%s", p.Content)
			}
			if !strings.Contains(p.Content, "CHECKPOINT") {
				t.Errorf("GoalPhaseOpen hint part missing CHECKPOINT reference; got:\n%s", p.Content)
			}
			break
		}
	}
	if !found {
		ids := make([]string, 0, len(parts))
		for _, p := range parts {
			ids = append(ids, string(p.Source.ID))
		}
		t.Fatalf("expected hint part %q in prompt parts; got parts: %v", PromptSourceGoalPhaseOpenHint, ids)
	}
}

// T7 — Integration: hint must NOT bleed into other phases.
// Catches: hint accidentally registered multiple times → bleeds into
// Set/Checkpoint/Final prompts.
func TestGoalPhaseOpenHint_Integration_OtherPhases_NotInjected(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	for _, phase := range []string{
		string(GoalPhaseSet),
		string(GoalPhaseCheckpoint),
		string(GoalPhaseFinal),
	} {
		parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
			IncludeToolUseRule: true,
			GoalPhase:           phase,
			Iteration:           0,
		})
		for _, p := range parts {
			if p.Source.ID == PromptSourceGoalPhaseOpenHint {
				t.Errorf("GoalPhaseOpen hint should NOT appear in phase %q; got part: %q", phase, p.ID)
			}
		}
	}
}
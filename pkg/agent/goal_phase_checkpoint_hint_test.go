// Placeholder file — Phase 12.43 tests added inline below.

package agent

import (
	"strings"
	"testing"
)

// TestGoalProgressBlockersHintText_NoPhaseClaim — Phase 12.43 (Q4-A +
// DOUBT-3 rephrased). The Blockers guidance text must NOT contain phase
// enum literals (per invariant: no phase claims in LLM-visible text).
func TestGoalProgressBlockersHintText_NoPhaseClaim(t *testing.T) {
	assertNoPhaseClaim(t, goalProgressBlockersHintText)
}

// TestGoalProgressBlockersHintContributor_FiresOnlyAtCheckpoint — wire
// verification: contributor returns nil at non-Checkpoint phases.
func TestGoalProgressBlockersHintContributor_FiresOnlyAtCheckpoint(t *testing.T) {
	for _, phase := range []string{
		string(GoalPhaseSet),
		string(GoalPhaseOpen),
		string(GoalPhaseFinal),
		"",
	} {
		got := goalProgressBlockersHintContributor(PromptBuildRequest{GoalPhase: phase})
		if got != nil {
			t.Errorf("expected nil at phase %q, got contributor with content %q", phase, got.Content)
		}
	}

	// Checkpoint phase: contributor fires.
	got := goalProgressBlockersHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint)})
	if got == nil {
		t.Fatal("expected non-nil at GoalPhaseCheckpoint, got nil")
	}
	if !strings.Contains(got.Content, "describe consequences") {
		t.Errorf("expected Blockers guidance text, got %q", got.Content)
	}
}

// TestCheckpointHint_DecisionBodyRendersWithCompass — Phase 12.67
// regression catch (F8): since Phase 12.39 (b2340eca) the contributor
// rendered ONLY the compass header when MaxIterationsCap>0 (production),
// dropping the entire decision body that lived in the legacy template.
// This test uses production-like cap dims and asserts the body renders.
// FAILS on HEAD (regression present) — RED before fix.
func TestCheckpointHint_DecisionBodyRendersWithCompass(t *testing.T) {
	got := goalPhaseCheckpointHintContributor(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseCheckpoint),
		MaxIterationsCap: 250,
		Iteration:        25,
	})
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	for _, want := range []string{
		"Goal phase: checkpoint (iter 25 / total 250",
		"Decision rule",
		"CANNOT be resumed",
		"goal_progress",
	} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("checkpoint hint must contain %q (regression F8: decision body dropped); got:\n%s", want, got.Content)
		}
	}
	assertNoPhaseClaim(t, got.Content)
}

// TestCheckpointHint_LegacyFallbackStillRenders — Phase 12.67 T8b:
// zero cap dims (MaxIterationsCap=0) must still render the legacy
// template body (backward compat, MaxIterationsCap=0 configs).
func TestCheckpointHint_LegacyFallbackStillRenders(t *testing.T) {
	got := goalPhaseCheckpointHintContributor(PromptBuildRequest{
		GoalPhase: string(GoalPhaseCheckpoint),
		Iteration: 25,
	})
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	if !strings.Contains(got.Content, "Goal phase: CHECKPOINT (iter 25)") {
		t.Errorf("legacy fallback must render template header; got:\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "Decision tree") {
		t.Errorf("legacy fallback must render legacy decision tree; got:\n%s", got.Content)
	}
}

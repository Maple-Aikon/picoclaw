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

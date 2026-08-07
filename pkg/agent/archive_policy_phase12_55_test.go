// PicoClaw - Ultra-lightweight personal AI agent

package agent

import "testing"

// Phase 12.55 T7: shouldArchiveToolExecExhausted — the single source of
// truth for "does tool-exec retry exhaustion archive the goal at this
// phase?" (Q2/Q3). True ONLY for Set/Checkpoint/Final. Open = no archive
// (error result goes into history for the next iteration); PostFinal = no
// tools, no archive.
func TestShouldArchiveToolExecExhausted_PhaseMatrix(t *testing.T) {
	cases := []struct {
		phase GoalPhase
		want  bool
	}{
		{GoalPhaseSet, true},
		{GoalPhaseOpen, false},
		{GoalPhaseCheckpoint, true},
		{GoalPhaseFinal, true},
		{GoalPhasePostFinal, false},
	}
	for _, tc := range cases {
		if got := shouldArchiveToolExecExhausted(tc.phase); got != tc.want {
			t.Errorf("shouldArchiveToolExecExhausted(%q) = %v, want %v", tc.phase, got, tc.want)
		}
	}
}

// GoalPhaseLock is an alias of GoalPhaseSet — must behave identically.
func TestShouldArchiveToolExecExhausted_GoalPhaseLockAlias(t *testing.T) {
	if got := shouldArchiveToolExecExhausted(GoalPhaseLock); !got {
		t.Error("shouldArchiveToolExecExhausted(GoalPhaseLock) = false, want true (Lock == Set alias)")
	}
}

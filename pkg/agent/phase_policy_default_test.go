//go:build !strict_phases

// Default-build regression tests — Phase 12.49.

package agent

import "testing"
//
// Verifies default (no `strict_phases` build tag) behavior unchanged:
// PhasePolicyFor(GoalPhase("bogus")) returns nil (fail-OPEN semantics).
// Lock the contract so adding strict-mode dual-file split doesn't
// accidentally regress the default path.

func TestDefaultBuild_PhasePolicyFor_KnownPhasesReturnRows(t *testing.T) {
	cases := map[GoalPhase]*AgentPhasePolicy{
		GoalPhaseSet:       nil,
		GoalPhaseOpen:      nil,
		GoalPhaseCheckpoint: nil,
		GoalPhaseFinal:     nil,
		GoalPhasePostFinal: nil,
	}
	for gp := range cases {
		p := PhasePolicyFor(gp)
		if p == nil {
			t.Errorf("PhasePolicyFor(%q) returned nil", string(gp))
			continue
		}
		cases[gp] = p
	}
}

func TestDefaultBuild_PhasePolicyFor_UnknownReturnsNil(t *testing.T) {
	cases := []GoalPhase{"", "bogus", "lock_legacy_typo"}
	for _, c := range cases {
		if p := PhasePolicyFor(c); p != nil {
			t.Errorf("PhasePolicyFor(%q) = %+v, want nil (fail-OPEN default)", string(c), p)
		}
	}
}

// TestDefaultBuild_PhasePolicyFor_LockAliasResolves — DDD-1 cell.
// Phase 12.49 §T9 cell (d): PhasePolicyFor(GoalPhaseLock) returns non-nil at
// DEFAULT build (alias resolves to SET row, no panic, no error). If someone
// deletes alias resolution in v3, this test catches it without needing strict.
func TestDefaultBuild_PhasePolicyFor_LockAliasResolves(t *testing.T) {
	p := PhasePolicyFor(GoalPhaseLock)
	if p == nil {
		t.Fatalf("PhasePolicyFor(GoalPhaseLock) = nil, want non-nil (Lock must alias to Set row)")
	}
	if p.Phase != GoalPhaseSet {
		t.Errorf("PhasePolicyFor(GoalPhaseLock).Phase = %q, want %q (Set alias)", p.Phase, GoalPhaseSet)
	}
}

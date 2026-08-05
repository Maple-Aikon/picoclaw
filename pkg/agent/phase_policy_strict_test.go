//go:build strict_phases

package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/phases"
)

// TestStrict_PhasePolicyFor_PanicsOnUnknown verifies that with the
// strict_phases build tag, unknown GoalPhase values trigger UnknownPhaseError.
//
// Note: lock-alias resolution runs BEFORE MustBeKnown (per R8 / F4 fact-check
// 2026-08-05). GoalPhaseLock(value="lock" stringwise same as Set) → must NOT
// panic; it normalizes to Set row.
func TestStrict_PhasePolicyFor_PanicsOnUnknown(t *testing.T) {
	cases := []GoalPhase{"bogus", "checkpoint_typo", "postFinal", "phase_unknown"}
	for _, c := range cases {
		t.Run("panic_"+string(c), func(t *testing.T) {
			var r any
			func() {
				defer func() { r = recover() }()
				PhasePolicyFor(c)
			}()
			if r == nil {
				t.Errorf("PhasePolicyFor(%q) did not panic in strict mode", string(c))
				return
			}
			if _, ok := r.(*phases.UnknownPhaseError); !ok {
				t.Errorf("PhasePolicyFor(%q) panic = %T, want *phases.UnknownPhaseError", string(c), r)
			}
		})
	}
}

// TestStrict_PhasePolicyFor_LockAliasResolves — locks alias resolution at strict build.
func TestStrict_PhasePolicyFor_LockAliasResolves(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("PhasePolicyFor(GoalPhaseLock) panicked in strict mode (alias must resolve): %v", r)
		}
	}()
	p := PhasePolicyFor(GoalPhaseLock)
	if p == nil {
		t.Fatalf("PhasePolicyFor(GoalPhaseLock) = nil, want non-nil (Lock must alias to Set row)")
	}
	if p.Phase != GoalPhaseSet {
		t.Errorf("PhasePolicyFor(GoalPhaseLock).Phase = %q, want %q (Set alias)", p.Phase, GoalPhaseSet)
	}
}

// TestStrict_PhasePolicyFor_KnownReturnsRow verifies known GoalPhase values
// still return their policy row.
func TestStrict_PhasePolicyFor_KnownReturnsRow(t *testing.T) {
	cases := []GoalPhase{
		GoalPhaseSet,
		GoalPhaseOpen,
		GoalPhaseCheckpoint,
		GoalPhaseFinal,
		GoalPhasePostFinal,
		GoalPhaseLock, // alias to Set
	}
	for _, c := range cases {
		t.Run("known_"+string(c), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("PhasePolicyFor(%q) panicked in strict mode for known phase: %v", string(c), r)
				}
			}()
			if p := PhasePolicyFor(c); p == nil {
				t.Errorf("PhasePolicyFor(%q) = nil, want non-nil", string(c))
			}
		})
	}
}

// TestStrict_PhasePolicyFor_EmptyReturnsNil — empty phase is "no phase set"
// sentinel; passes through without panic even in strict mode.
func TestStrict_PhasePolicyFor_EmptyReturnsNil(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("PhasePolicyFor(\"\") panicked in strict mode: %v", r)
		}
	}()
	if p := PhasePolicyFor(""); p != nil {
		t.Errorf("PhasePolicyFor(\"\") = %+v, want nil", p)
	}
}

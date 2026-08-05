//go:build strict_phases

package tools

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/phases"
)

// TestStrict_ToolPolicyForPhase_PanicsOnUnknown verifies that with the
// strict_phases build tag, unknown non-empty phases trigger UnknownPhaseError.
func TestStrict_ToolPolicyForPhase_PanicsOnUnknown(t *testing.T) {
	cases := []string{"bogus", "Set", " open", "set "}
	for _, c := range cases {
		t.Run("panic_"+c, func(t *testing.T) {
			var r any
			func() {
				defer func() { r = recover() }()
				ToolPolicyForPhase(c)
			}()
			if r == nil {
				t.Errorf("ToolPolicyForPhase(%q) did not panic in strict mode", c)
				return
			}
			if _, ok := r.(*phases.UnknownPhaseError); !ok {
				t.Errorf("ToolPolicyForPhase(%q) panic = %T, want *phases.UnknownPhaseError", c, r)
			}
		})
	}
}

// TestStrict_ToolPolicyForPhase_EmptyReturnsNil verifies empty phase
// keeps the historic "no phase set" contract — nil, no panic. Even in
// strict mode.
func TestStrict_ToolPolicyForPhase_EmptyReturnsNil(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("ToolPolicyForPhase(\"\") panicked: %v", r)
		}
	}()
	if p := ToolPolicyForPhase(""); p != nil {
		t.Errorf("ToolPolicyForPhase(\"\") = %+v, want nil", p)
	}
}

// TestStrict_ToolPolicyForPhase_KnownReturnsRow verifies the 5 canonical
// tokens still return their policy row (no behavior change in steady state).
func TestStrict_ToolPolicyForPhase_KnownReturnsRow(t *testing.T) {
	for _, tok := range []string{"set", "open", "checkpoint", "final", "post_final"} {
		t.Run("known_"+tok, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ToolPolicyForPhase(%q) panicked in strict mode for known phase: %v", tok, r)
				}
			}()
			p := ToolPolicyForPhase(tok)
			if p == nil {
				t.Errorf("ToolPolicyForPhase(%q) = nil, want non-nil row", tok)
			}
		})
	}
}

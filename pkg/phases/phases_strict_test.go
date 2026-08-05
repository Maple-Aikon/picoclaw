// Code Phase 12.49 — strict-mode enforcement tests (build tag `strict_phases`).
//
//go:build strict_phases

package phases

import (
	"strings"
	"testing"
)

// TestStrict_MustBeKnown_KnownPhasesAccept verifies the 5 canonical tokens
// pass through MustBeKnown unchanged (no panic, no error return).
func TestStrict_MustBeKnown_KnownPhasesAccept(t *testing.T) {
	for _, tok := range AllTokens() {
		t.Run("known_"+tok, func(t *testing.T) {
			var r any
			func() {
				defer func() { r = recover() }()
				MustBeKnown(tok)
			}()
			if r != nil {
				t.Errorf("MustBeKnown(%q) panicked unexpectedly: %v", tok, r)
			}
		})
	}
}

// TestStrict_MustBeKnown_EmptyPasses verifies the empty string is NOT
// treated as unknown — it's the "no phase set" sentinel and must not
// panic. This matches the historic default behavior of ToolPolicyForPhase
// and PhasePolicyFor.
func TestStrict_MustBeKnown_EmptyPasses(t *testing.T) {
	var r any
	func() {
		defer func() { r = recover() }()
		MustBeKnown("")
	}()
	if r != nil {
		t.Errorf("MustBeKnown(\"\") panicked: %v (empty must pass)", r)
	}
}

// TestStrict_MustBeKnown_PanicsOnUnknown verifies a non-empty unknown phase
// triggers UnknownPhaseError panic.
func TestStrict_MustBeKnown_PanicsOnUnknown(t *testing.T) {
	cases := []string{
		"bogus",
		"set ",
		" open", // leading whitespace — currently stripped by callers
		"Set",   // case mismatch — strictly different token
		"lock_legacy", // old GoalPhaseLock raw (must NEVER reach here per F4)
	}
	for _, c := range cases {
		t.Run("unknown_"+c, func(t *testing.T) {
			var r any
			func() {
				defer func() { r = recover() }()
				MustBeKnown(c)
			}()
			if r == nil {
				t.Errorf("MustBeKnown(%q) did not panic", c)
				return
			}
			err, ok := r.(*UnknownPhaseError)
			if !ok {
				t.Errorf("MustBeKnown(%q) panic value %T, want *UnknownPhaseError", c, r)
				return
			}
			if err.Phase != c {
				t.Errorf("UnknownPhaseError.Phase = %q, want %q", err.Phase, c)
			}
			if !strings.Contains(err.Error(), c) {
				t.Errorf("UnknownPhaseError.Error() = %q, should contain %q", err.Error(), c)
			}
		})
	}
}

// TestStrict_UnknownPhaseError_ErrorMessage verifies the panic value's
// Error() string includes the offending phase so test failures and
// panic stacks are informative.
func TestStrict_UnknownPhaseError_ErrorMessage(t *testing.T) {
	err := &UnknownPhaseError{Phase: "wat"}
	if err.Phase != "wat" {
		t.Errorf("Phase field: got %q want %q", err.Phase, "wat")
	}
	msg := err.Error()
	if !strings.Contains(msg, "wat") {
		t.Errorf("Error() = %q, must contain phase string", msg)
	}
	if !strings.Contains(msg, "pkg/phases") {
		t.Errorf("Error() = %q, must contain package name for caller localization", msg)
	}
}

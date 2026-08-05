// Code Phase 12.49 — default-build regression tests for pkg/tools/phase_policy.go.
//
// Verifies that default (no `strict_phases` build tag) behavior is UNCHANGED:
// ToolPolicyForPhase("bogus") returns nil (fail-OPEN semantics preserved for
// legacy callers). Lock the contract so adding strict-mode dual-file split
// doesn't accidentally regress the default path.
//
//go:build !strict_phases

package tools

import "testing"

// TestDefaultBuild_KnownPhasesReturnRows verifies the 5 canonical phases
// return non-nil rows in the default build (sanity).
func TestDefaultBuild_KnownPhasesReturnRows(t *testing.T) {
	for _, tok := range []string{"set", "open", "checkpoint", "final", "post_final"} {
		t.Run("known_"+tok, func(t *testing.T) {
			p := ToolPolicyForPhase(tok)
			if p == nil {
				t.Errorf("ToolPolicyForPhase(%q) returned nil, want non-nil", tok)
			}
		})
	}
}

// TestDefaultBuild_UnknownReturnsNil verifies the OLD fail-OPEN contract is
// preserved: unknown phase → nil (NOT panic). Strict build does panic; this
// test pins the default-build lenient semantics.
//
// Note: trim + lowercase is intentionally applied (Phase 12.1 regression guard),
// so "Set" / " open" / "set " resolve to the lowercase "set" row, NOT nil.
// Strict build rejects these (case-sensitive strict match per MustBeKnown).
func TestDefaultBuild_UnknownReturnsNil(t *testing.T) {
	cases := []string{"", "bogus", "postFinal", "checkpoint_typo"}
	for _, c := range cases {
		t.Run("nil_"+c, func(t *testing.T) {
			p := ToolPolicyForPhase(c)
			if p != nil {
				t.Errorf("ToolPolicyForPhase(%q) = %+v, want nil (default build fail-OPEN)", c, p)
			}
		})
	}
}

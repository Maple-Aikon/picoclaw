//go:build !strict_phases

package agent

import "testing"

// TestPhaseContextSuffix_UnknownPhaseEmpty is the default-build-only variant.
// Stricter asserts that unknown non-empty phase yields empty suffix. Strict mode
// panics on unknown phase lookup, so this test cannot run there.
func TestPhaseContextSuffix_UnknownPhaseEmpty(t *testing.T) {
	if got := phaseContextSuffix(""); got != "" {
		t.Fatalf("empty phase must yield empty suffix (fail-closed), got: %s", got)
	}
	if got := phaseContextSuffix("bogus"); got != "" {
		t.Fatalf("unknown phase must yield empty suffix, got: %s", got)
	}
}

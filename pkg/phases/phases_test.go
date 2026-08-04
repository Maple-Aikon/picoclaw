package phases

import "testing"

// TestTokens_AllLocked — locked in plan §3.1 + R3-F6: 5 phase tokens, fail-safe typo.
func TestTokens_AllLocked(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"PhaseSet", PhaseSet, "set"},
		{"PhaseOpen", PhaseOpen, "open"},
		{"PhaseCheckpoint", PhaseCheckpoint, "checkpoint"},
		{"PhaseFinal", PhaseFinal, "final"},
		{"PhasePostFinal", PhasePostFinal, "post_final"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s = %q, want %q", c.name, string(c.got), c.want)
		}
	}
}

// TestTokens_NoAlias — GoalPhaseLock is a pkg/agent synonym, NOT in pkg/phases.
// pkg/phases exposes NO synonyms (only canonical 5 tokens). Cross-package alias
// resolution lives at pkg/agent/tool_allowlist_phase.go (GoalPhaseLock = GoalPhaseSet).
func TestTokens_NoAlias(t *testing.T) {
	// Lock is NOT in pkg/phases — there must be no PhaseLock constant.
	defer func() {
		if r := recover(); r != nil {
			// Will only fire if someone aliases Lock here later.
			t.Errorf("pkg/phases should not expose PhaseLock alias: %v", r)
		}
	}()
	// Compile-time check: accessing non-existent PhaseLock SHOULD not compile,
	// but at runtime we can verify the 5-token string set is exact.
	if len(AllTokens()) != 5 {
		t.Errorf("AllTokens() size = %d, want 5", len(AllTokens()))
	}
	want := map[string]bool{"set": true, "open": true, "checkpoint": true, "final": true, "post_final": true}
	for _, tok := range AllTokens() {
		if !want[string(tok)] {
			t.Errorf("unexpected token %q in AllTokens()", string(tok))
		}
		delete(want, string(tok))
	}
	for k := range want {
		t.Errorf("missing token %q in AllTokens()", k)
	}
}

// TestTokens_PhaseSetUniqueAcrossTables — pkg/tools (string-keyed policy table)
// and pkg/agent (GoalPhase-keyed policy table) MUST agree on the set of phase
// tokens. Cross-table equality test lives at pkg/agent (P2-F1, R3-F6); this
// checks pkg/phases alone is internally consistent.
func TestTokens_InternalConsistency(t *testing.T) {
	for _, tok := range AllTokens() {
		if !IsKnown(string(tok)) {
			t.Errorf("AllTokens() returned %q but IsKnown=false", string(tok))
		}
	}
	if IsKnown("bogus") {
		t.Errorf("IsKnown(bogus) should be false")
	}
	if IsKnown("") {
		t.Errorf("IsKnown(empty) should be false")
	}
}

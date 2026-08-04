// Phase 12.47 (T13, P6) — Text-policy invariant: every string emitted at the
// post_final path (gate-skip message) must NOT claim phase/iter/cap state
// (Phase 12.43 assertNoPhaseClaim pattern, extended by invariant).
package agent

import (
	"regexp"
	"testing"
)

var postFinalForbiddenClaims = []*regexp.Regexp{
	regexp.MustCompile(`(?i)goal (is )?finaliz`),
	regexp.MustCompile(`(?i)(last|final) iter`),
	regexp.MustCompile(`(?i)in this iter`),
	regexp.MustCompile(`(?i)cap( is|:|\s+\d)`),
}

func TestPostFinal_TextPolicy_NoStateClaims(t *testing.T) {
	msg := gateSkipMessageForPhase("some_tool", GoalPhasePostFinal)
	for _, re := range postFinalForbiddenClaims {
		if re.MatchString(msg) {
			t.Errorf("gate-skip message violates invariant %q:\n%s", re.String(), msg)
		}
	}
	if len(msg) == 0 {
		t.Fatal("gate-skip message must be non-empty")
	}
}

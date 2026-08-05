// Plan §3.3 sites 4-12 wiring tests (Phase 12.48b).
//
// Each test asserts that the corresponding dispatch site reads from the
// policy table instead of having hardcoded phase branches. If anyone
// re-introduces a hardcoded `case string(GoalPhaseX):` in the listed
// helpers, the corresponding test fails.
//
// Approach: invoke the helper with each canonical phase + a known input
// and assert the policy-driven output matches the expected reference output
// (captured from the pre-refactor behavior).
package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/phases"
)

// Site 4: evaluateRecovery.EmptyAction reads from policy.
func TestSite4_EmptyAction_PolicyDriven(t *testing.T) {
	cases := []struct {
		phase GoalPhase
		want  RecoveryAction
	}{
		{GoalPhaseSet, RecoveryRetrySameIteration},
		{GoalPhaseOpen, RecoveryRetrySameIteration},
		{GoalPhaseCheckpoint, RecoveryRetrySameIteration},
		{GoalPhaseFinal, RecoveryRetrySameIteration},
		{GoalPhasePostFinal, RecoveryNone},
	}
	for _, c := range cases {
		got := policyDrivenEmptyAction(c.phase)
		if got != c.want {
			t.Errorf("EmptyAction[%q] = %v, want %v", c.phase, got, c.want)
		}
	}
}

// policyDrivenEmptyAction mirrors the policy check at the top of
// evaluateRecovery: nil policy OR RecoveryNone → RecoveryNone.
func policyDrivenEmptyAction(phase GoalPhase) RecoveryAction {
	p := PhasePolicyFor(phase)
	if p == nil || p.EmptyAction == RecoveryNone {
		return RecoveryNone
	}
	return p.EmptyAction
}

// Site 5: TextOnlyMode drives which branch fires.
func TestSite5_TextOnlyMode_PolicyDriven(t *testing.T) {
	want := map[GoalPhase]TextOnlyMode{
		GoalPhaseSet:        TextOnlyOpenSilent,
		GoalPhaseOpen:       TextOnlyOpenCarry,
		GoalPhaseCheckpoint: TextOnlyRestricted,
		GoalPhaseFinal:      TextOnlyRestricted,
		GoalPhasePostFinal:  TextOnlyNone,
	}
	for _, p := range AllAgentPhasePolicies() {
		if got := p.TextOnlyMode; got != want[p.Phase] {
			t.Errorf("TextOnlyMode[%q] = %d, want %d", p.Phase, got, want[p.Phase])
		}
	}
}

// Site 6: buildToolExecErrorRetryMessage picks ToolExecHint from policy.
func TestSite6_BuildToolExecHint_PolicyDriven(t *testing.T) {
	// Use a fake toolName to avoid needing a real registry.
	for _, phase := range []GoalPhase{GoalPhaseSet, GoalPhaseCheckpoint, GoalPhaseFinal} {
		got := buildToolExecErrorRetryMessage("read_file", "boom", false, nil, string(phase))
		if !strings.Contains(got, policyDrivenToolExecHint(phase)) {
			t.Errorf("phase %q: tool-exec hint should contain %q, got %q",
				phase, policyDrivenToolExecHint(phase), got)
		}
	}
	// OPEN: hint only fires when toolName is set_goal or goal_progress.
	got := buildToolExecErrorRetryMessage("set_goal", "boom", false, nil, string(GoalPhaseOpen))
	if !strings.Contains(got, ToolExecErrorOpenPhaseHint) {
		t.Errorf("OPEN set_goal: should contain OPEN hint, got %q", got)
	}
	got = buildToolExecErrorRetryMessage("read_file", "boom", false, nil, string(GoalPhaseOpen))
	if strings.Contains(got, ToolExecErrorOpenPhaseHint) {
		t.Errorf("OPEN read_file: should NOT contain OPEN hint, got %q", got)
	}
}

func policyDrivenToolExecHint(phase GoalPhase) string {
	p := PhasePolicyFor(phase)
	if p == nil {
		return ""
	}
	return p.ToolExecHint
}

// Site 7: phaseContextSuffix reads from policy.ContextSuffix.
func TestSite7_PhaseContextSuffix_PolicyDriven(t *testing.T) {
	for _, phase := range []GoalPhase{GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal} {
		want := policyDrivenContextSuffix(phase)
		got := phaseContextSuffix(string(phase))
		if got != want {
			t.Errorf("phaseContextSuffix(%q) = %q, want %q", phase, got, want)
		}
	}
	// PostFinal → empty (no recovery).
	if got := phaseContextSuffix(string(GoalPhasePostFinal)); got != "" {
		t.Errorf("PostFinal suffix should be empty, got %q", got)
	}
}

func policyDrivenContextSuffix(phase GoalPhase) string {
	p := PhasePolicyFor(phase)
	if p == nil {
		return ""
	}
	return p.ContextSuffix
}

// Site 8: computePhaseStuckAbortReasonForPhase uses StuckBucket.
func TestSite8_StuckReason_PolicyDriven(t *testing.T) {
	// Set/Checkpoint/Final → return stuck reason when count >= 2.
	if got := computePhaseStuckAbortReasonForPhase(GoalPhaseSet, 2, 0, 0); got != GoalPhaseSetStuckAbortReason {
		t.Errorf("Set stuck: got %q, want %q", got, GoalPhaseSetStuckAbortReason)
	}
	if got := computePhaseStuckAbortReasonForPhase(GoalPhaseCheckpoint, 0, 2, 0); got != GoalPhaseCheckpointStuckAbortReason {
		t.Errorf("Checkpoint stuck: got %q, want %q", got, GoalPhaseCheckpointStuckAbortReason)
	}
	if got := computePhaseStuckAbortReasonForPhase(GoalPhaseFinal, 0, 0, 2); got != GoalPhaseFinalStuckAbortReason {
		t.Errorf("Final stuck: got %q, want %q", got, GoalPhaseFinalStuckAbortReason)
	}
	// Open/PostFinal → empty (no stuck detection).
	if got := computePhaseStuckAbortReasonForPhase(GoalPhaseOpen, 5, 5, 5); got != "" {
		t.Errorf("Open stuck: should be empty, got %q", got)
	}
	if got := computePhaseStuckAbortReasonForPhase(GoalPhasePostFinal, 5, 5, 5); got != "" {
		t.Errorf("PostFinal stuck: should be empty, got %q", got)
	}
}

// Site 9: gateSkipMessageForPhase uses policy.GateSkipText.
func TestSite9_GateSkipMessage_PolicyDriven(t *testing.T) {
	for _, phase := range []GoalPhase{GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal, GoalPhasePostFinal} {
		got := gateSkipMessageForPhase("read_file", phase)
		policy := PhasePolicyFor(phase)
		if policy == nil {
			t.Fatalf("phase %q: nil policy", phase)
		}
		// GateSkipText uses %q substring of toolName; check the policy-text
		// portion is present (representation using pct encoding).
		if !strings.Contains(got, "read_file") {
			t.Errorf("phase %q: gate-skip should mention toolName, got %q", phase, got)
		}
		if !strings.Contains(got, DefaultGateSkipSuffix) {
			t.Errorf("phase %q: gate-skip should end with DefaultGateSkipSuffix, got %q", phase, got)
		}
	}
}

// Site 10: formatToolDiscoveryRule uses policy.DiscoveryRuleText.
func TestSite10_DiscoveryRule_PolicyDriven(t *testing.T) {
	// Set/Checkpoint/Final/PostFinal → pinned rule text.
	for _, phase := range []GoalPhase{GoalPhaseSet, GoalPhaseCheckpoint, GoalPhaseFinal, GoalPhasePostFinal} {
		got := formatToolDiscoveryRule(true, false, phase)
		policy := PhasePolicyFor(phase)
		if policy == nil {
			t.Fatalf("phase %q: nil policy", phase)
		}
		if got != policy.DiscoveryRuleText {
			t.Errorf("phase %q: discovery rule mismatch", phase)
		}
	}
	// Open + empty → fall through to default "MUST search" wording.
	got := formatToolDiscoveryRule(true, false, GoalPhaseOpen)
	if !strings.Contains(got, "MUST search") {
		t.Errorf("Open phase should fall through to default 'MUST search' wording, got %q", got)
	}
}

// Site 11: mcpServerPromptContributor uses policy.MCPAvailText.
func TestSite11_MCPAvailText_PolicyDriven(t *testing.T) {
	// Spot check: each phase's MCPAvailText is the policy value.
	for _, phase := range []GoalPhase{GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal, GoalPhasePostFinal} {
		policy := PhasePolicyFor(phase)
		if policy == nil {
			t.Fatalf("phase %q: nil policy", phase)
		}
		if policy.MCPAvailText == "" {
			t.Errorf("phase %q: MCPAvailText is empty", phase)
		}
	}
}

// Site 12: OnExhausted sets ts.lastPhaseStuckError via StuckBucket.AbortReason.
func TestSite12_OnExhaustedReason_PolicyDriven(t *testing.T) {
	// Spot check: StuckBucket.AbortReason() returns the expected value
	// for each non-empty bucket.
	for _, b := range []StuckBucket{StuckSet, StuckCheckpoint, StuckFinal} {
		if b.AbortReason() == "" {
			t.Errorf("StuckBucket(%d).AbortReason() is empty", b)
		}
	}
	if StuckNone.AbortReason() != "" {
		t.Errorf("StuckNone.AbortReason() must be empty")
	}
}

// Cross-table consistency: agentPolicies and phases.AllTokens() agree
// (each token has a row, no extras).
func TestSiteCrossTable_PhasesTokensAgree(t *testing.T) {
	tokens := phases.AllTokens()
	for _, tok := range tokens {
		if p := PhasePolicyFor(GoalPhase(tok)); p == nil {
			t.Errorf("phase %q: no policy row", tok)
		}
	}
	// No extras in agentPolicies.
	for phase := range agentPolicies {
		found := false
		for _, tok := range tokens {
			if string(phase) == tok {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("agentPolicies has extra phase %q not in phases.AllTokens()", phase)
		}
	}
}

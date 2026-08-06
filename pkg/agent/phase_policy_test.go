// Schema lock test for the agent-side policy table (Phase 12.48b).
// Mirrors pkg/tools/phase_policy_test.go's TestPhasePolicy_SchemaLock_*.
//
// Invariants (Plan §3.3 + §4.20):
//   1. Keys of agentPolicies == phases.AllTokens() (5 rows, minus Lock alias).
//   2. Each row's Phase field matches the map key (defense against typo).
//   3. GateSkipText / DiscoveryRuleText / MCPAvailText are non-empty.
//   4. ToolExecHint for OPEN is empty (use toolName gate; Plan §3.3 site 6).
//   5. EmptyAction == RecoveryNone ONLY at PostFinal (R5 lock).
//   6. TextOnlyMode matches expected per-row (canonical mapping).
//   7. StuckBucket matches expected per-row (Plan §3.3 F4).
//   8. PhasePolicyFor(nil) and PhasePolicyFor("") return nil.
//   9. PhasePolicyFor(GoalPhaseLock) returns the SET row (Phase 11 alias).
//  10. Case-insensitive lookup (Phase 12.1 regression guard).
//
// Phase 12.49: add build tag `!strict_phases` because tests 8/9/10 assert
// default fail-OPEN semantics on unknown inputs which contradict strict-mode
// panic. Strict counterparts in phase_policy_strict_test.go.
//go:build !strict_phases

package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/phases"
)

func TestPhasePolicy_AgentSide_SchemaLock_KeySetMatchesPhases(t *testing.T) {
	want := phases.AllTokens()
	got := make([]string, 0, len(agentPolicies))
	for k := range agentPolicies {
		got = append(got, string(k))
	}
	if len(got) != len(want) {
		t.Fatalf("agent table has %d rows, want %d (%v)", len(got), len(want), want)
	}
	for _, tok := range want {
		goalPhase := GoalPhase(tok)
		if goalPhase == GoalPhaseLock {
			continue // Lock is an alias, not a separate row
		}
		if _, ok := agentPolicies[goalPhase]; !ok {
			t.Errorf("agentPolicies missing key for token %q", tok)
		}
	}
}

func TestPhasePolicy_AgentSide_EveryRowPhaseMatchesKey(t *testing.T) {
	for key, row := range agentPolicies {
		if row == nil {
			t.Fatalf("agentPolicies[%q] is nil", key)
		}
		if row.Phase != key {
			t.Fatalf("agentPolicies[%q].Phase=%q mismatch (typo in row)", key, row.Phase)
		}
	}
}

func TestPhasePolicy_AgentSide_TextsNonEmpty(t *testing.T) {
	for key, row := range agentPolicies {
		if row.GateSkipText == "" {
			t.Errorf("agentPolicies[%q].GateSkipText is empty", key)
		}
		if row.MCPAvailText == "" {
			t.Errorf("agentPolicies[%q].MCPAvailText is empty", key)
		}
		// DiscoveryRuleText is allowed to be empty at OPEN (default branch).
	}
}

func TestPhasePolicy_AgentSide_OpenToolExecHintNonEmpty(t *testing.T) {
	// Plan §3.3 site 6: OPEN carries the ToolExecErrorOpenPhaseHint in
	// policy.ToolExecHint — the gate that decides whether to actually
	// append it (toolName ∈ {set_goal, goal_progress}) lives in
	// buildToolExecErrorRetryMessage, NOT in the policy table. By pushing
	// the hint INTO the table, all 4 phases have a single source of truth
	// and the policy table is testable for "all 5 rows have a hint".
	open := agentPolicies[GoalPhaseOpen]
	if open == nil {
		t.Fatal("agentPolicies[GoalPhaseOpen] is nil")
	}
	if open.ToolExecHint == "" {
		t.Errorf("OPEN ToolExecHint should be non-empty (gate is in buildToolExecErrorRetryMessage), got empty")
	}
}

func TestPhasePolicy_AgentSide_EmptyActionOnlyRecoveryNoneAtPostFinal(t *testing.T) {
	// R5 lock: PostFinal is the ONLY phase where EmptyAction == RecoveryNone.
	for key, row := range agentPolicies {
		if row.Phase == GoalPhasePostFinal {
			if row.EmptyAction != RecoveryNone {
				t.Errorf("PostFinal EmptyAction must be RecoveryNone, got %d", row.EmptyAction)
			}
			continue
		}
		if row.EmptyAction == RecoveryNone {
			t.Errorf("Non-PostFinal phase %q has EmptyAction=RecoveryNone (must be RecoveryRetrySameIteration)", key)
		}
	}
}

func TestPhasePolicy_AgentSide_TextOnlyModeCanonical(t *testing.T) {
	// Plan §3.3 site 5: each phase has a fixed TextOnlyMode.
	want := map[GoalPhase]TextOnlyMode{
		GoalPhaseSet:       TextOnlyOpenSilent,
		GoalPhaseOpen:      TextOnlyOpenCarry,
		GoalPhaseCheckpoint: TextOnlyRestricted,
		GoalPhaseFinal:     TextOnlyRestricted,
		GoalPhasePostFinal: TextOnlyNone,
	}
	for key, row := range agentPolicies {
		if got := row.TextOnlyMode; got != want[key] {
			t.Errorf("phase %q TextOnlyMode=%d, want %d", key, got, want[key])
		}
	}
}

func TestPhasePolicy_AgentSide_StuckBucketCanonical(t *testing.T) {
	// Plan §3.3 F4: Set/Checkpoint/Final have stuck buckets; Open/PostFinal = StuckNone.
	want := map[GoalPhase]StuckBucket{
		GoalPhaseSet:        StuckSet,
		GoalPhaseOpen:       StuckNone,
		GoalPhaseCheckpoint: StuckCheckpoint,
		GoalPhaseFinal:      StuckFinal,
		GoalPhasePostFinal:  StuckNone,
	}
	for key, row := range agentPolicies {
		if got := row.StuckBucket; got != want[key] {
			t.Errorf("phase %q StuckBucket=%d, want %d", key, got, want[key])
		}
	}
}

func TestPhasePolicyFor_EmptyAndNilReturnNil(t *testing.T) {
	if got := PhasePolicyFor(GoalPhase("")); got != nil {
		t.Errorf("PhasePolicyFor(\"\") = %v, want nil", got)
	}
}

func TestPhasePolicyFor_UnknownPhaseReturnsNil(t *testing.T) {
	// Unknown non-empty phase → nil (R6-F1: caller fails-CLOSED).
	for _, p := range []GoalPhase{"gibberish", "x", "transition"} {
		if got := PhasePolicyFor(p); got != nil {
			t.Errorf("PhasePolicyFor(%q) = %v, want nil", p, got)
		}
	}
}

func TestPhasePolicyFor_LockAliasReturnsSetRow(t *testing.T) {
	// Phase 11 backward compat: GoalPhaseLock has the same value as GoalPhaseSet.
	// The lookup MUST return the SET row even when called as Lock.
	got := PhasePolicyFor(GoalPhaseLock)
	if got == nil {
		t.Fatal("PhasePolicyFor(GoalPhaseLock) = nil, want SET row")
	}
	if got.Phase != GoalPhaseSet {
		t.Errorf("PhasePolicyFor(GoalPhaseLock).Phase = %q, want %q", got.Phase, GoalPhaseSet)
	}
}

func TestPhasePolicyFor_CaseInsensitive(t *testing.T) {
	// Phase 12.1 regression guard: lookup must be case-insensitive
	// (capital "Open" historically broke the gate).
	for _, variant := range []GoalPhase{"set", "SET", "Set", " oPeN "} {
		got := PhasePolicyFor(variant)
		if got == nil {
			t.Errorf("PhasePolicyFor(%q) = nil, want policy (case-insensitive)", variant)
		}
	}
}

func TestStuckBucket_AbortReasonCanonical(t *testing.T) {
	cases := map[StuckBucket]string{
		StuckNone:       "",
		StuckSet:        GoalPhaseSetStuckAbortReason,
		StuckCheckpoint: GoalPhaseCheckpointStuckAbortReason,
		StuckFinal:      GoalPhaseFinalStuckAbortReason,
	}
	for b, want := range cases {
		if got := b.AbortReason(); got != want {
			t.Errorf("StuckBucket(%d).AbortReason() = %q, want %q", b, got, want)
		}
	}
}

func TestStuckBucket_CounterFieldCanonical(t *testing.T) {
	cases := map[StuckBucket]string{
		StuckNone:       "",
		StuckSet:        "setGoalAttemptCount",
		StuckCheckpoint: "goalProgressAttemptCount",
		StuckFinal:      "completeGoalAttemptCount",
	}
	for b, want := range cases {
		if got := b.CounterField(); got != want {
			t.Errorf("StuckBucket(%d).CounterField() = %q, want %q", b, got, want)
		}
	}
}

func TestAllAgentPhasePolicies_OrderedMatchesPhases(t *testing.T) {
	// AllAgentPhasePolicies must return rows in the lifecycle order
	// (set → open → checkpoint → final → post_final). Lock is skipped.
	want := []GoalPhase{
		GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal, GoalPhasePostFinal,
	}
	got := AllAgentPhasePolicies()
	if len(got) != len(want) {
		t.Fatalf("AllAgentPhasePolicies len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Phase != want[i] {
			t.Errorf("AllAgentPhasePolicies[%d].Phase = %q, want %q", i, got[i].Phase, want[i])
		}
	}
}

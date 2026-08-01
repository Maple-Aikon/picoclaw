// Phase 12.37 — Recovery spec alignment. Tests for the new GAP #2
// (all-phase empty trigger) + Deviation #3 (count-based cap, 3 same-iter
// attempts) behavior. See plan §1 (D1-D4) + Task 1.
//
// These tests overwrite pre-Phase 12.37 expectations from recovery_goal_test.go:
//   - EmptyResponseRecoveryCap is now 3 (was 2/one-shot bool).
//   - All phases (set/checkpoint/final) fire Trigger #1 (was Open-only).
//   - 3rd consecutive fire returns EmptyResponseHardMessage.
//   - 4th evaluation archives the goal (was: silently None after one-shot).
//   - Final+postReport silent (unchanged from spec 8e).

package agent

import (
	"testing"
)

// GAP #2: empty response at restricted phases must retry same-iter.
func TestEmptyResponse_FiresAtRestrictedPhases_Phase12_37(t *testing.T) {
	for _, phase := range []string{"set", "checkpoint", "final"} {
		ts := newPhase5TurnState(t)
		ts.emptyResponseRecoveryCount = 0
		ctx := RecoveryContext{
			Phase:        phase,
			Iteration:    5,
			TextEmpty:    true,
			HasToolCalls: false,
		}
		action, msg := evaluateRecovery(ts, ctx)
		if action != RecoveryRetrySameIteration {
			t.Fatalf("phase=%s: want RecoveryRetrySameIteration, got %v", phase, action)
		}
		if msg != EmptyResponseRecoveryMessage {
			t.Fatalf("phase=%s: want soft msg, got %q", phase, msg)
		}
		if ts.emptyResponseRecoveryCount != 1 {
			t.Fatalf("phase=%s: count want 1, got %d", phase, ts.emptyResponseRecoveryCount)
		}
	}
}

// Deviation #3: 3rd consecutive empty fire returns the hard message.
func TestEmptyResponse_ThirdAttempt_HardMessage_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.emptyResponseRecoveryCount = 2
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:        "checkpoint",
		Iteration:    5,
		TextEmpty:    true,
		HasToolCalls: false,
	})
	if action != RecoveryRetrySameIteration {
		t.Fatalf("want RetrySameIteration, got %v", action)
	}
	if msg != EmptyResponseHardMessage {
		t.Fatalf("want hard msg on 3rd attempt, got %q", msg)
	}
	if ts.emptyResponseRecoveryCount != 3 {
		t.Fatalf("count want 3, got %d", ts.emptyResponseRecoveryCount)
	}
}

// Deviation #3: 4th evaluation (cap reached) archives the goal.
func TestEmptyResponse_CapExhausted_ArchivesGoal_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.emptyResponseRecoveryCount = 3
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:        "final",
		Iteration:    9,
		TextEmpty:    true,
		HasToolCalls: false,
	})
	if action != RecoveryArchiveGoal {
		t.Fatalf("want RecoveryArchiveGoal, got %v", action)
	}
	if msg == "" {
		t.Fatal("want non-empty archive message")
	}
}

// Final + post-report + empty stays silent (spec 8e, unchanged).
func TestEmptyResponse_FinalPostReport_Silent_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.emptyResponseRecoveryCount = 0
	action, _ := evaluateRecovery(ts, RecoveryContext{
		Phase:                 "final",
		Iteration:             10,
		TextEmpty:             true,
		HasToolCalls:          false,
		PostCompleteGoalReport: true,
	})
	if action != RecoveryNone {
		t.Fatalf("want RecoveryNone for final+postReport silent, got %v", action)
	}
}

// Open phase still works (regression-proof for pre-12.37 Open behavior).
func TestEmptyResponse_Open_StillFires_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.emptyResponseRecoveryCount = 0
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:        "open",
		Iteration:    2,
		TextEmpty:    true,
		HasToolCalls: false,
	})
	if action != RecoveryRetrySameIteration {
		t.Fatalf("want RetrySameIteration, got %v", action)
	}
	if msg != EmptyResponseRecoveryMessage {
		t.Fatalf("want soft msg, got %q", msg)
	}
	if ts.emptyResponseRecoveryCount != 1 {
		t.Fatalf("count want 1, got %d", ts.emptyResponseRecoveryCount)
	}
}

// Const equality: EmptyResponseRecoveryCap must be 3 (spec 9).
func TestEmptyResponse_CapIsThree_Phase12_37(t *testing.T) {
	if EmptyResponseRecoveryCap != 3 {
		t.Fatalf("want cap=3, got %d", EmptyResponseRecoveryCap)
	}
}

// Task 2 — Deviation #3 restricted text-only = 2 soft + 1 hard = 3 attempts.

// 1st text-only at restricted phase: soft message, soft counter→1.
func TestTextOnlyRestricted_FirstAttempt_SoftMessage_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.textOnlySoftRetriesDone = 0
	ts.textOnlyHardRetriesDone = 0
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:        "checkpoint",
		Iteration:    5,
		TextEmpty:    false,
		HasToolCalls: false,
	})
	if action != RecoveryRetrySameIteration {
		t.Fatalf("want RetrySameIteration, got %v", action)
	}
	if msg != TextOnlySoftRetryMessage {
		t.Fatalf("want soft msg, got %q", msg)
	}
	if ts.textOnlySoftRetriesDone != 1 {
		t.Fatalf("soft count want 1, got %d", ts.textOnlySoftRetriesDone)
	}
	if ts.textOnlyHardRetriesDone != 0 {
		t.Fatalf("hard count want 0, got %d", ts.textOnlyHardRetriesDone)
	}
}

// 2nd text-only at restricted phase: still soft, soft counter→2.
func TestTextOnlyRestricted_SecondAttempt_StillSoft_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.textOnlySoftRetriesDone = 1 // simulate 1st fire
	ts.textOnlyHardRetriesDone = 0
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:        "checkpoint",
		Iteration:    5,
		TextEmpty:    false,
		HasToolCalls: false,
	})
	if action != RecoveryRetrySameIteration {
		t.Fatalf("want RetrySameIteration, got %v", action)
	}
	if msg != TextOnlySoftRetryMessage {
		t.Fatalf("want soft msg on 2nd attempt, got %q", msg)
	}
	if ts.textOnlySoftRetriesDone != 2 {
		t.Fatalf("soft count want 2, got %d", ts.textOnlySoftRetriesDone)
	}
}

// 3rd text-only at restricted phase: hard message, hard counter→1.
func TestTextOnlyRestricted_ThirdAttempt_HardMessage_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.textOnlySoftRetriesDone = 2 // simulate 2 soft fires
	ts.textOnlyHardRetriesDone = 0
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:        "checkpoint",
		Iteration:    5,
		TextEmpty:    false,
		HasToolCalls: false,
	})
	if action != RecoveryRetrySameIteration {
		t.Fatalf("want RetrySameIteration, got %v", action)
	}
	if msg != TextOnlyHardRetryMessage {
		t.Fatalf("want hard msg on 3rd attempt, got %q", msg)
	}
	if ts.textOnlyHardRetriesDone != 1 {
		t.Fatalf("hard count want 1, got %d", ts.textOnlyHardRetriesDone)
	}
}

// 4th text-only at restricted phase (cap exhausted): archive.
func TestTextOnlyRestricted_CapExhausted_ArchivesGoal_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.textOnlySoftRetriesDone = 2
	ts.textOnlyHardRetriesDone = 1
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:        "checkpoint",
		Iteration:    5,
		TextEmpty:    false,
		HasToolCalls: false,
	})
	if action != RecoveryArchiveGoal {
		t.Fatalf("want RecoveryArchiveGoal, got %v", action)
	}
	if msg == "" {
		t.Fatal("want non-empty archive message")
	}
}

// Final + post-report + text-only stays silent (spec 8e, unchanged).
func TestTextOnlyRestricted_FinalPostReport_Silent_Phase12_37(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.textOnlySoftRetriesDone = 0
	action, _ := evaluateRecovery(ts, RecoveryContext{
		Phase:                 "final",
		Iteration:             10,
		TextEmpty:             false,
		HasToolCalls:          false,
		PostCompleteGoalReport: true,
	})
	if action != RecoveryNone {
		t.Fatalf("want RecoveryNone for final+postReport silent, got %v", action)
	}
}

// Const equality: TextOnlySoftRetryCap=2, TextOnlyHardRetryCap=1 (spec 9).
func TestTextOnlyRestricted_CapsAre2And1_Phase12_37(t *testing.T) {
	if TextOnlySoftRetryCap != 2 {
		t.Fatalf("want soft cap=2, got %d", TextOnlySoftRetryCap)
	}
	if TextOnlyHardRetryCap != 1 {
		t.Fatalf("want hard cap=1, got %d", TextOnlyHardRetryCap)
	}
}

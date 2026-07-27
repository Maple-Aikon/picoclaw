package agent

import (
	"strings"
	"testing"
)

// Phase 12.21 — Fix B tests for recordPhaseStuckToolAllowedBlock* + Fix D
// integration test for computePhaseStuckAbortReasonForPhase (the new
// static helper that lets us verify threshold logic without a full
// AgentLoop).
//
// Verifies that when a tool is blocked by the runtime allowlist while
// the agent is in a restricted-allowlist phase (Set/Checkpoint/Final),
// the corresponding phase-stuck counter is incremented even though the
// blocked tool is NOT one of the 3 lifecycle tools.
//
// This test class documents the bug class Phase 12.21 closes:
// previously the counter only incremented when a LIFECYCLE tool
// (set_goal/goal_progress/complete_goal) failed with ErrInvalidInput;
// an IsAllowed-block on a non-lifecycle tool (write_file, web_search,
// read_file, etc.) was silently dropped from the phase-stuck path.

// TestRecordPhaseStuckToolAllowedBlockInPhase_Checkpoint_IncrementsGoalProgressCounter
// is the regression-proof for Phase 12.21 Fix B: blocking write_file at
// GoalPhaseCheckpoint increments goalProgressFailCount (not silently dropped).
func TestRecordPhaseStuckToolAllowedBlockInPhase_Checkpoint_IncrementsGoalProgressCounter(t *testing.T) {
	ts := &turnState{}
	if ts.goalProgressFailCount != 0 {
		t.Fatalf("precondition: counter should be 0, got %d", ts.goalProgressFailCount)
	}
	enrichedMsg := `called tool "write_file" but checkpoint only allows the phase-specific lifecycle tools — tool "write_file" is not available in the current phase`
	recordPhaseStuckToolAllowedBlockInPhase(ts, GoalPhaseCheckpoint, "write_file", enrichedMsg)
	if ts.goalProgressFailCount != 1 {
		t.Fatalf("goalProgressFailCount=%d want=1 — IsAllowed block at Checkpoint did NOT increment counter", ts.goalProgressFailCount)
	}
	if !strings.Contains(ts.lastPhaseStuckError, "write_file") {
		t.Errorf("lastPhaseStuckError missing tool name: %q", ts.lastPhaseStuckError)
	}
	if !strings.Contains(ts.lastPhaseStuckError, "checkpoint") {
		t.Errorf("lastPhaseStuckError missing phase name: %q", ts.lastPhaseStuckError)
	}
}

// TestRecordPhaseStuckToolAllowedBlockInPhase_Set_IncrementsSetGoalCounter covers
// the Set-phase variant of Fix B.
func TestRecordPhaseStuckToolAllowedBlockInPhase_Set_IncrementsSetGoalCounter(t *testing.T) {
	ts := &turnState{}
	enrichedMsg := `called tool "web_search" but set only allows the phase-specific lifecycle tools — tool "web_search" is not available in the current phase`
	recordPhaseStuckToolAllowedBlockInPhase(ts, GoalPhaseSet, "web_search", enrichedMsg)
	if ts.setGoalFailCount != 1 {
		t.Fatalf("setGoalFailCount=%d want=1 — IsAllowed block at Set did NOT increment counter", ts.setGoalFailCount)
	}
	if !strings.Contains(ts.lastPhaseStuckError, "set") {
		t.Errorf("lastPhaseStuckError missing phase name: %q", ts.lastPhaseStuckError)
	}
}

// TestRecordPhaseStuckToolAllowedBlockInPhase_Final_IncrementsCompleteGoalCounter
// covers the Final-phase variant of Fix B.
func TestRecordPhaseStuckToolAllowedBlockInPhase_Final_IncrementsCompleteGoalCounter(t *testing.T) {
	ts := &turnState{}
	enrichedMsg := `called tool "read_file" but final only allows the phase-specific lifecycle tools — tool "read_file" is not available in the current phase`
	recordPhaseStuckToolAllowedBlockInPhase(ts, GoalPhaseFinal, "read_file", enrichedMsg)
	if ts.completeGoalFailCount != 1 {
		t.Fatalf("completeGoalFailCount=%d want=1 — IsAllowed block at Final did NOT increment counter", ts.completeGoalFailCount)
	}
	if !strings.Contains(ts.lastPhaseStuckError, "final") {
		t.Errorf("lastPhaseStuckError missing phase name: %q", ts.lastPhaseStuckError)
	}
}

// TestRecordPhaseStuckToolAllowedBlockInPhase_Open_NoIncrement documents that
// Fix B does NOT apply at GoalPhaseOpen: full tool set is allowed, and
// any allowlist rejection is unexpected — the recovery trigger fires
// via checkToolExecErrorRecovery (Phase 12.11) without phase-stuck
// escalation.
func TestRecordPhaseStuckToolAllowedBlockInPhase_Open_NoIncrement(t *testing.T) {
	ts := &turnState{}
	recordPhaseStuckToolAllowedBlockInPhase(ts, GoalPhaseOpen, "any_tool",
		`called tool "any_tool" but open only allows the phase-specific lifecycle tools`)
	if ts.setGoalFailCount != 0 || ts.goalProgressFailCount != 0 || ts.completeGoalFailCount != 0 {
		t.Fatalf("Open phase should NOT increment any phase-stuck counter, got set=%d prog=%d comp=%d",
			ts.setGoalFailCount, ts.goalProgressFailCount, ts.completeGoalFailCount)
	}
}

// TestRecordPhaseStuckToolAllowedBlockInPhase_TwiceTriggersStuckArchiveMessage
// is the Fix-D integration-flavored test for Fix B + computePhaseStuckAbortReasonForPhase:
// after 2 IsAllowed-blocks at Checkpoint phase, the threshold check
// returns GoalPhaseCheckpointStuckAbortReason. This is what main-turn-5
// specifically needed: 2 write_file blocks at checkpoint should land on
// a user-visible stuck message rather than the generic toolLimitResponse
// fallback.
//
// Uses the static helpers rather than the AgentLoop-bound method so we
// can verify the threshold logic in isolation. The integration test for
// the full pipeline path lives in
// turn_coord_phase12_21_archive_integration_test.go (or equivalent, see
// plan §2.5 for the live-verify target).
func TestRecordPhaseStuckToolAllowedBlockInPhase_TwiceTriggersStuckArchiveMessage(t *testing.T) {
	ts := &turnState{}
	recordPhaseStuckToolAllowedBlockInPhase(ts, GoalPhaseCheckpoint, "write_file", "block 1")
	recordPhaseStuckToolAllowedBlockInPhase(ts, GoalPhaseCheckpoint, "web_search", "block 2")
	if ts.goalProgressFailCount != 2 {
		t.Fatalf("goalProgressFailCount=%d want=2", ts.goalProgressFailCount)
	}
	got := computePhaseStuckAbortReasonForPhase(GoalPhaseCheckpoint, ts.setGoalFailCount, ts.goalProgressFailCount, ts.completeGoalFailCount)
	if got != GoalPhaseCheckpointStuckAbortReason {
		t.Fatalf("computePhaseStuckAbortReasonForPhase=%q want=%q — 2 IsAllowed blocks at Checkpoint should escalate to stuck-message",
			got, GoalPhaseCheckpointStuckAbortReason)
	}
}

// TestComputePhaseStuckAbortReasonForPhase_AllPhasesAtThreshold covers
// the threshold-flap matrix: counter < 2 → empty, counter == 2 → stuck
// reason, counter > 2 → still stuck reason (no upper cap).
func TestComputePhaseStuckAbortReasonForPhase_AllPhasesAtThreshold(t *testing.T) {
	tests := []struct {
		name       string
		phase      GoalPhase
		set, prog, comp int
		wantReason string
	}{
		{"Set_0", GoalPhaseSet, 0, 0, 0, ""},
		{"Set_1", GoalPhaseSet, 1, 0, 0, ""},
		{"Set_2", GoalPhaseSet, 2, 0, 0, GoalPhaseSetStuckAbortReason},
		{"Set_3", GoalPhaseSet, 3, 0, 0, GoalPhaseSetStuckAbortReason},
		{"Checkpoint_1", GoalPhaseCheckpoint, 0, 1, 0, ""},
		{"Checkpoint_2", GoalPhaseCheckpoint, 0, 2, 0, GoalPhaseCheckpointStuckAbortReason},
		{"Final_2", GoalPhaseFinal, 0, 0, 2, GoalPhaseFinalStuckAbortReason},
		{"Open_2", GoalPhaseOpen, 0, 0, 0, ""},
		{"EmptyPhase_2", GoalPhase(""), 0, 0, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computePhaseStuckAbortReasonForPhase(tt.phase, tt.set, tt.prog, tt.comp)
			if got != tt.wantReason {
				t.Fatalf("set=%d prog=%d comp=%d phase=%q → %q want=%q",
					tt.set, tt.prog, tt.comp, tt.phase, got, tt.wantReason)
			}
		})
	}
}

// TestRecordPhaseStuckToolFail_OriginalContractStillHolds is a regression
// test: the Phase 12.13 contract that recordPhaseStuckToolFail increments
// counters for lifecycle-tool validation failures (with ErrKind =
// ErrInvalidInput) MUST NOT regress after Fix B's recordPhaseStuckToolAllowedBlock
// addition. This test would have caught a careless edit that moved the
// switch case or accidentally renamed the method.
func TestRecordPhaseStuckToolFail_OriginalContractStillHolds(t *testing.T) {
	ts := &turnState{}
	ts.recordPhaseStuckToolFail("set_goal", "missing name")
	ts.recordPhaseStuckToolFail("goal_progress", "missing remaining_steps")
	ts.recordPhaseStuckToolFail("complete_goal", "missing summary")
	if ts.setGoalFailCount != 1 || ts.goalProgressFailCount != 1 || ts.completeGoalFailCount != 1 {
		t.Fatalf("recordPhaseStuckToolFail contract regressed: set=%d prog=%d comp=%d",
			ts.setGoalFailCount, ts.goalProgressFailCount, ts.completeGoalFailCount)
	}
}

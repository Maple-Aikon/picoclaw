// Package agent — Phase 12.53a tests.
//
// Item A (M1): persist the real stuck attempt count into the goal file
// (StuckAttemptCount) so a LATER turn renders the true "failed N
// attempt(s)" message instead of stuckCountAtLeastOne's fallback 1
// (12.52c M1: newTurnState zeroes in-memory counters every turn, so the
// 12.52c "(attempts: N)" stamp only ever lived same-turn).
//
// D-F2 (idempotency): a second archive call with a DIFFERENT count must
// still leave the on-disk value unchanged — status-guard no-op, not
// value-coincidence.
//
// D-F3 (legacy wrapper): finalizeGoalOnTurnEnd(reason) (count=0) must NOT
// write the field — the 6 legacy call sites stay byte-identical.
package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

func seedGoal(t *testing.T, st *goal.Store, sessionKey string, g *goal.Goal) {
	t.Helper()
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}
}

// T1 (A4): a NEW turn state (counters zero) reads the persisted count
// from the goal file → "failed 2 attempt(s)". RED pre-fix: the current
// code reads ts.goalProgressAttemptCount (=0) → stuckCountAtLeastOne → 1.
func TestPhaseStuckFallbackMessage_TurnAfterUsesPersistedCount(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-53a-t1-persisted"
	st := goal.NewStore(agent.Workspace)
	seedGoal(t, st, sessionKey, &goal.Goal{
		Name:              "stuck-checkpoint",
		Description:       goal.Description{Objective: "checkpoint stuck", SuccessCriteria: []string{"done"}},
		Status:            goal.StatusAborted,
		AbortReason:       GoalPhaseCheckpointStuckAbortReason,
		StuckAttemptCount: 2,
	})

	// Turn AFTER the stuck turn: fresh turnState → in-memory counters zero.
	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-53a-t1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	if ts.goalProgressAttemptCount != 0 {
		t.Fatalf("fixture: new turn state must start with zero counters, got %d", ts.goalProgressAttemptCount)
	}

	got := al.phaseStuckFallbackMessage(ts, mustReadGoal(t, st, sessionKey))
	if !strings.Contains(got, "failed 2 attempt(s)") {
		t.Fatalf("got %q, want message with persisted count 2 (RED pre-fix: atLeastOne(0)=1)", got)
	}
	if strings.Contains(got, "failed 1 attempt") {
		t.Errorf("got %q, must not fall back to count 1 when persisted count is 2", got)
	}
}

func mustReadGoal(t *testing.T, st *goal.Store, sessionKey string) *goal.Goal {
	t.Helper()
	g, err := st.Read(sessionKey)
	if err != nil {
		t.Fatalf("read goal: %v", err)
	}
	return g
}

// T2 (A5): exact persisted count renders for BOTH phase-stuck reasons
// (Checkpoint + Final), with the count coming from the goal file.
func TestPhaseStuckFallbackMessage_PersistedExactCount_CheckpointAndFinal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reason     string
		count      int
		wantSubstr string
	}{
		{"checkpoint", GoalPhaseCheckpointStuckAbortReason, 3, "failed 3 attempt(s)"},
		{"final", GoalPhaseFinalStuckAbortReason, 3, "failed 3 attempt(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
			defer cleanup()

			const sessionKeyPrefix = "test-53a-t2-"
			sessionKey := sessionKeyPrefix + tc.name
			st := goal.NewStore(agent.Workspace)
			seedGoal(t, st, sessionKey, &goal.Goal{
				Name:              "stuck-" + tc.name,
				Description:       goal.Description{Objective: "x", SuccessCriteria: []string{"done"}},
				Status:            goal.StatusAborted,
				AbortReason:       tc.reason,
				StuckAttemptCount: tc.count,
			})

			ts := &turnState{} // fresh — no in-memory counters
			got := al.phaseStuckFallbackMessage(ts, mustReadGoal(t, st, sessionKey))
			if !strings.Contains(got, tc.wantSubstr) {
				t.Fatalf("got %q, want %q from persisted count %d", got, tc.wantSubstr, tc.count)
			}
		})
	}
}

// T3 (A1/A7 + D-F3): round-trip persists the new field; legacy goal files
// (no field) parse to 0; the legacy finalizeGoalOnTurnEnd(reason) wrapper
// (count=0) must NOT write the field to disk.
func TestGoalStuckAttemptCount_RoundTripLegacyParseAndWrapper(t *testing.T) {
	// (1) round-trip: 2 → Serialize → Parse → 2
	rt := &goal.Goal{
		Name:              "rt",
		Description:       goal.Description{Objective: "o", SuccessCriteria: []string{"s"}},
		StuckAttemptCount: 2,
	}
	data, err := goal.Serialize(rt)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	parsed, err := goal.Parse("rt.md", data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.StuckAttemptCount != 2 {
		t.Errorf("round-trip: got %d, want 2", parsed.StuckAttemptCount)
	}

	// (2) legacy: serialize without the field → raw YAML has no
	// stuck_attempt_count key and parses to 0.
	legacy := &goal.Goal{
		Name:        "legacy",
		Description: goal.Description{Objective: "o", SuccessCriteria: []string{"s"}},
	}
	legacyData, err := goal.Serialize(legacy)
	if err != nil {
		t.Fatalf("serialize legacy: %v", err)
	}
	if strings.Contains(string(legacyData), "stuck_attempt_count") {
		t.Errorf("omitempty: legacy serialize must not emit stuck_attempt_count, got:\n%s", legacyData)
	}
	legacyParsed, err := goal.Parse("legacy.md", legacyData)
	if err != nil {
		t.Fatalf("parse legacy: %v", err)
	}
	if legacyParsed.StuckAttemptCount != 0 {
		t.Errorf("legacy parse: got %d, want 0", legacyParsed.StuckAttemptCount)
	}

	// (3) D-F3: finalizeGoalOnTurnEnd(reason) (count=0 wrapper) archives an
	// active goal WITHOUT adding the field — 6 legacy call sites unchanged.
	_, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	const sessionKey = "test-53a-t3-wrapper"
	st := goal.NewStore(agent.Workspace)
	seedGoal(t, st, sessionKey, &goal.Goal{
		Name:        "active",
		Description: goal.Description{Objective: "o", SuccessCriteria: []string{"s"}},
		Status:      goal.StatusActive,
	})
	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-53a-t3",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonBexhausted + ":test"); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	disk, err := st.Read(sessionKey)
	if err != nil {
		t.Fatalf("read archived goal: %v", err)
	}
	if disk.Status != goal.StatusAborted {
		t.Fatalf("wrapper: status = %q, want aborted", disk.Status)
	}
	if disk.StuckAttemptCount != 0 {
		t.Errorf("wrapper: stuck_attempt_count = %d, want 0 (legacy wrapper must not write the field)", disk.StuckAttemptCount)
	}
	raw, err := goal.Serialize(disk)
	if err != nil {
		t.Fatalf("serialize disk goal: %v", err)
	}
	if strings.Contains(string(raw), "stuck_attempt_count") {
		t.Errorf("wrapper: on-disk YAML must not contain stuck_attempt_count, got:\n%s", raw)
	}
}

// T4 (A6+A6b): same-turn render — real counter (2 fails) + "(attempts: 2)"
// stamp; the persisted StuckAttemptCount matches the in-memory counter at
// archive time (no silent drift between the two sources).
func TestFinalizePhaseStuckArchive_PersistsCountMatchingCounter(t *testing.T) {
	_, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-53a-t4"
	st := goal.NewStore(agent.Workspace)
	seedGoal(t, st, sessionKey, &goal.Goal{
		Name:        "active",
		Description: goal.Description{Objective: "o", SuccessCriteria: []string{"s"}},
		Status:      goal.StatusActive,
	})

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-53a-t4",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey

	// 2 real fails → counter = 2 (no ratchet).
	ts.recordPhaseStuckToolFail("goal_progress", "missing required property remaining_steps")
	ts.recordPhaseStuckToolFail("goal_progress", "missing required property remaining_steps")
	if ts.goalProgressAttemptCount != 2 {
		t.Fatalf("fixture: attempt count = %d, want 2", ts.goalProgressAttemptCount)
	}

	finalizePhaseStuckArchive(ts, GoalPhaseCheckpoint, "BoundedRetry exhausted")

	// On-disk goal: aborted + phase-stuck reason + count == in-memory counter.
	disk := mustReadGoal(t, st, sessionKey)
	if disk.Status != goal.StatusAborted {
		t.Errorf("status = %q, want aborted", disk.Status)
	}
	if disk.AbortReason != GoalPhaseCheckpointStuckAbortReason {
		t.Errorf("abort_reason = %q, want %q", disk.AbortReason, GoalPhaseCheckpointStuckAbortReason)
	}
	if disk.StuckAttemptCount != ts.goalProgressAttemptCount {
		t.Errorf("A6b: persisted count %d != in-memory counter %d (sources drifted)", disk.StuckAttemptCount, ts.goalProgressAttemptCount)
	}
	// Same-turn stamp carries the count too.
	if want := "BoundedRetry exhausted (attempts: 2)"; ts.lastPhaseStuckError != want {
		t.Errorf("lastPhaseStuckError = %q, want %q", ts.lastPhaseStuckError, want)
	}
}

// T8 (A8 + D-F2): archive path called twice — second call with a DIFFERENT
// count must not overwrite the on-disk value (status-guard no-op proves
// idempotency is real, not value-coincidence).
func TestFinalizePhaseStuckArchive_IdempotentSecondCall(t *testing.T) {
	_, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-53a-t8"
	st := goal.NewStore(agent.Workspace)
	seedGoal(t, st, sessionKey, &goal.Goal{
		Name:        "active",
		Description: goal.Description{Objective: "o", SuccessCriteria: []string{"s"}},
		Status:      goal.StatusActive,
	})

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-53a-t8",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.recordPhaseStuckToolFail("goal_progress", "e1")
	ts.recordPhaseStuckToolFail("goal_progress", "e2") // counter = 2

	finalizePhaseStuckArchive(ts, GoalPhaseCheckpoint, "BoundedRetry exhausted")
	if got := mustReadGoal(t, st, sessionKey).StuckAttemptCount; got != 2 {
		t.Fatalf("after first archive: count = %d, want 2", got)
	}

	// Second archive call with a DIFFERENT count (5) — must be a no-op.
	ts.goalProgressAttemptCount = 5
	finalizePhaseStuckArchive(ts, GoalPhaseCheckpoint, "second call")
	disk := mustReadGoal(t, st, sessionKey)
	if disk.StuckAttemptCount != 2 {
		t.Errorf("idempotency: second archive with count 5 changed disk to %d, want 2 (status-guard no-op)", disk.StuckAttemptCount)
	}
}

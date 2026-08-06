// Package agent — Phase 12.52a tests.
//
// Item A (R10 F01 split): the dual-purpose counters
// setGoalFailCount / goalProgressFailCount / completeGoalFailCount
// become *AttemptCount (real per-failure count, display) +
// *ArchiveFlag (bool, set at archive event).
//
// Item B: applyFallbackForEmptyResponse reads the goal store ONCE
// (single ReadAny) instead of twice (goal.Summary + phaseStuckFallbackMessage).
package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// =============================================================================
// T1-1a / T1-2a / T1-2b — ArchiveFlag flips exactly once per phase
// =============================================================================

func TestSetGoalArchiveFlag_FlipsExactlyOnce(t *testing.T) {
	ts := &turnState{}

	// 1 fail → AttemptCount increments, flag stays false (not archived yet)
	ts.recordPhaseStuckToolFail("set_goal", "missing required property name")
	if ts.setGoalAttemptCount != 1 {
		t.Errorf("after 1 set_goal fail, attempt count should be 1, got %d", ts.setGoalAttemptCount)
	}
	if ts.setGoalArchiveFlag {
		t.Errorf("archive flag must be false before archive event")
	}

	// archive event → flag flips true; attempt count keeps the REAL count (1)
	ts.recordPhaseStuckArchive(GoalPhaseSet, "BoundedRetry exhausted")
	if !ts.setGoalArchiveFlag {
		t.Errorf("archive flag must be true after archive event")
	}
	if ts.setGoalAttemptCount != 1 {
		t.Errorf("attempt count must stay 1 (no ratchet to 2), got %d", ts.setGoalAttemptCount)
	}

	// subsequent fails → count grows, flag stays true (flips exactly once)
	ts.recordPhaseStuckToolFail("set_goal", "missing required property name")
	if ts.setGoalAttemptCount != 2 {
		t.Errorf("attempt count should be 2 after 2 fails, got %d", ts.setGoalAttemptCount)
	}
	if !ts.setGoalArchiveFlag {
		t.Errorf("archive flag must stay true once set")
	}
}

func TestGoalProgressArchiveFlag_FlipsExactlyOnce(t *testing.T) {
	ts := &turnState{}
	ts.recordPhaseStuckToolFail("goal_progress", "missing remaining_steps")
	if ts.goalProgressAttemptCount != 1 {
		t.Errorf("after 1 goal_progress fail, attempt count should be 1, got %d", ts.goalProgressAttemptCount)
	}
	if ts.goalProgressArchiveFlag {
		t.Errorf("archive flag must be false before archive event")
	}
	ts.recordPhaseStuckArchive(GoalPhaseCheckpoint, "BoundedRetry exhausted")
	if !ts.goalProgressArchiveFlag {
		t.Errorf("archive flag must be true after archive event")
	}
	if ts.goalProgressAttemptCount != 1 {
		t.Errorf("attempt count must stay 1 (no ratchet to 2), got %d", ts.goalProgressAttemptCount)
	}
	ts.recordPhaseStuckToolFail("goal_progress", "missing remaining_steps")
	if ts.goalProgressAttemptCount != 2 {
		t.Errorf("attempt count should be 2, got %d", ts.goalProgressAttemptCount)
	}
	if !ts.goalProgressArchiveFlag {
		t.Errorf("archive flag must stay true once set")
	}
}

func TestCompleteGoalArchiveFlag_FlipsExactlyOnce(t *testing.T) {
	ts := &turnState{}
	ts.recordPhaseStuckToolFail("complete_goal", "summary too short")
	if ts.completeGoalAttemptCount != 1 {
		t.Errorf("after 1 complete_goal fail, attempt count should be 1, got %d", ts.completeGoalAttemptCount)
	}
	if ts.completeGoalArchiveFlag {
		t.Errorf("archive flag must be false before archive event")
	}
	ts.recordPhaseStuckArchive(GoalPhaseFinal, "BoundedRetry exhausted")
	if !ts.completeGoalArchiveFlag {
		t.Errorf("archive flag must be true after archive event")
	}
	if ts.completeGoalAttemptCount != 1 {
		t.Errorf("attempt count must stay 1 (no ratchet to 2), got %d", ts.completeGoalAttemptCount)
	}
}

// =============================================================================
// T1-1b / T1-1c / T1-1d — user-facing messages read the split fields
// (sites turn_coord.go:830/832/834 via phaseStuckFallbackMessage)
// =============================================================================

func TestSetGoalArchiveFlag_UserMessageReadsFlag_Site830(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-session-52a-site830"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "stuck-set",
		Description: goal.Description{
			Objective:       "Set up",
			SuccessCriteria: []string{"done"},
		},
		Status:      goal.StatusAborted,
		AbortReason: GoalPhaseSetStuckAbortReason,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-52a-site830",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.setGoalAttemptCount = 1
	ts.setGoalArchiveFlag = true
	ts.lastPhaseStuckError = "missing required property name"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal setup could not complete") {
		t.Fatalf("expected fallback to contain 'Goal setup could not complete', got %q", got)
	}
	if !strings.Contains(got, "failed 1 attempt(s)") {
		t.Errorf("message must show the REAL attempt count (1), got %q", got)
	}
	if strings.Contains(got, "failed 2 attempt(s)") {
		t.Errorf("message must NOT show ratcheted count '2 times', got %q", got)
	}
	assertNoPhaseClaim(t, got)
}

func TestGoalProgressArchiveFlag_UserMessageReadsFlag_Site832(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-session-52a-site832"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "stuck-checkpoint",
		Description: goal.Description{
			Objective:       "Continue",
			SuccessCriteria: []string{"done"},
		},
		Status:      goal.StatusAborted,
		AbortReason: GoalPhaseCheckpointStuckAbortReason,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-52a-site832",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalProgressAttemptCount = 1
	ts.goalProgressArchiveFlag = true
	ts.lastPhaseStuckError = "missing remaining_steps"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal continuation could not complete") {
		t.Fatalf("expected fallback to contain 'Goal continuation could not complete', got %q", got)
	}
	if !strings.Contains(got, "failed 1 attempt(s)") {
		t.Errorf("message must show the REAL attempt count (1), got %q", got)
	}
	if strings.Contains(got, "failed 2 attempt(s)") {
		t.Errorf("message must NOT show ratcheted count '2 times', got %q", got)
	}
	assertNoPhaseClaim(t, got)
}

func TestCompleteGoalArchiveFlag_UserMessageReadsFlag_Site834(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-session-52a-site834"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "stuck-final",
		Description: goal.Description{
			Objective:       "Finalize",
			SuccessCriteria: []string{"done"},
		},
		Status:      goal.StatusAborted,
		AbortReason: GoalPhaseFinalStuckAbortReason,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-52a-site834",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.completeGoalAttemptCount = 1
	ts.completeGoalArchiveFlag = true
	ts.lastPhaseStuckError = "summary too short"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal finalization could not complete") {
		t.Fatalf("expected fallback to contain 'Goal finalization could not complete', got %q", got)
	}
	if !strings.Contains(got, "failed 1 attempt(s)") {
		t.Errorf("message must show the REAL attempt count (1), got %q", got)
	}
	if strings.Contains(got, "failed 2 attempt(s)") {
		t.Errorf("message must NOT show ratcheted count '2 times', got %q", got)
	}
	assertNoPhaseClaim(t, got)
}

// =============================================================================
// T1-3 — F08 ordering invariant + call-site coverage.
// After the archive path fires (routeTextOnlyThroughRecovery RecoveryArchiveGoal
// / retryExecuteToolChain OnExhausted), AttemptCount and ArchiveFlag must be
// consistent AT THE SAME INSTANT: count >= 1 AND flag == true.
// =============================================================================
func TestArchiveFlag_SetAfterAttemptIncrement(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	_, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Same-instant check after the archive path.
	if ts.goalProgressAttemptCount < 1 {
		t.Errorf("attempt count must be >= 1 at archive instant, got %d", ts.goalProgressAttemptCount)
	}
	if !ts.goalProgressArchiveFlag {
		t.Errorf("archive flag must be true at archive instant")
	}
}

// =============================================================================
// T1-4 — user-visible bug: message shows REAL count, not the ratcheted 2.
// 1 real fail + archive → "1 failed attempt", never "2 times in a row".
// =============================================================================
func TestStuckMessage_ShowsRealAttemptCount_NotRatcheted(t *testing.T) {
	ts := &turnState{}
	ts.recordPhaseStuckToolFail("complete_goal", "summary too short")
	ts.recordPhaseStuckArchive(GoalPhaseFinal, "BoundedRetry exhausted")

	if ts.completeGoalAttemptCount != 1 {
		t.Fatalf("fixture: attempt count must be 1 (real fails), got %d", ts.completeGoalAttemptCount)
	}
	if !ts.completeGoalArchiveFlag {
		t.Fatalf("fixture: archive flag must be true")
	}
	// The user-facing message format must read the real count — mirror the
	// exact format call at turn_coord.go:834 (phaseStuckFallbackMessage).
	msg := fmt.Sprintf(GoalPhaseFinalStuckMessage, ts.completeGoalAttemptCount, "summary too short")
	if !strings.Contains(msg, "failed 1 attempt(s)") {
		t.Errorf("message must say 'failed 1 attempt(s)', got %q", msg)
	}
	if strings.Contains(msg, "failed 2 attempt(s)") {
		t.Errorf("message must NOT say 'failed 2 attempt(s)' (ratcheted count), got %q", msg)
	}
}

// =============================================================================
// T2-1..T2-4 — Item B (DOUBT-F1+F2, SIMP-F1): cache *goal.Goal in
// applyFallbackForEmptyResponse — exactly ONE store read per call.
// =============================================================================

// countingGoalStore wraps *goal.Store and counts ReadAny calls so the
// single-read invariant is observable.
type countingGoalStore struct {
	*goal.Store
	reads int
}

func (c *countingGoalStore) ReadAny(sessionKey string) (*goal.Goal, error) {
	c.reads++
	return c.Store.ReadAny(sessionKey)
}

func TestApplyFallbackForEmptyResponse_GoalStoreReadOnce(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-52a-t2-1-readonce"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name:        "readonce",
		Description: goal.Description{Objective: "read once", SuccessCriteria: []string{"done"}},
		Status:      goal.StatusActive,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-52a-t2-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	ts.iteration = ts.iterationCap // branch 1.5 fires at Checkpoint

	counting := &countingGoalStore{Store: st}
	al.SetGoalStoreOverrideForTest(counting)
	defer al.SetGoalStoreOverrideForTest(nil)

	got := al.applyFallbackForEmptyResponse(ts)
	if got != ToolLimitCheckpointRetryMessage {
		t.Fatalf("got = %q, want %q", got, ToolLimitCheckpointRetryMessage)
	}
	if counting.reads != 1 {
		t.Errorf("goal store read count = %d, want 1 (Phase 12.52a Item B: single ReadAny)", counting.reads)
	}
}

func TestPhaseStuckFallbackMessage_AcceptsGoalPointer(t *testing.T) {
	al, _, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	ts := &turnState{
		lastPhaseStuckError:      "BoundedRetry exhausted",
		goalProgressAttemptCount: 1,
		goalProgressArchiveFlag:  true,
	}

	// nil goal → "" (fail-safe)
	if msg := al.phaseStuckFallbackMessage(ts, nil); msg != "" {
		t.Fatalf("nil goal: got %q, want empty", msg)
	}

	// archived goal with phase-stuck abort reason → message with REAL count
	g := &goal.Goal{Status: goal.StatusAborted, AbortReason: GoalPhaseCheckpointStuckAbortReason}
	msg := al.phaseStuckFallbackMessage(ts, g)
	if msg == "" {
		t.Fatal("archived goal with abort reason: want non-empty message")
	}
	if !strings.Contains(msg, "1 attempt") {
		t.Errorf("message must show real attempt count 1, got %q", msg)
	}
	if !strings.Contains(msg, "BoundedRetry exhausted") {
		t.Errorf("message must include last error, got %q", msg)
	}

	// active goal → ""
	if msg := al.phaseStuckFallbackMessage(ts, &goal.Goal{Status: goal.StatusActive}); msg != "" {
		t.Fatalf("active goal: got %q, want empty", msg)
	}
}

func TestApplyFallbackForEmptyResponse_Branch15_CachesGoalStore(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-52a-t2-3-cache"
	st := goal.NewStore(agent.Workspace)
	base := &goal.Goal{
		Name:        "cachetest",
		Description: goal.Description{Objective: "branch coverage", SuccessCriteria: []string{"done"}},
	}

	newTS := func(finalized bool) *turnState {
		ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
			turnID:  "turn-52a-t2-3",
			context: newTurnContext(nil, nil, nil),
		})
		ts.sessionKey = sessionKey
		ts.goalFinalized = finalized
		ts.postCompleteGoalReportSent = false
		ts.iteration = ts.iterationCap
		return ts
	}

	// Branch A: goal.Summary (finalized + empty text + archived goal with summary)
	ga := *base
	ga.Status = goal.StatusAborted
	ga.Summary = "archived with summary"
	if err := st.Write(sessionKey, &ga); err != nil {
		t.Fatalf("seed archived summary goal: %v", err)
	}
	tsA := newTS(true)
	countA := &countingGoalStore{Store: st}
	al.SetGoalStoreOverrideForTest(countA)
	if got := al.applyFallbackForEmptyResponse(tsA); got != ga.Summary {
		t.Fatalf("summary branch: got %q, want %q", got, ga.Summary)
	}
	if countA.reads != 1 {
		t.Errorf("summary branch reads = %d, want 1", countA.reads)
	}

	// Branch B: phase-stuck (finalized + empty text + archived w/ abort reason, no summary)
	gb := *base
	gb.Status = goal.StatusAborted
	gb.AbortReason = GoalPhaseCheckpointStuckAbortReason
	gb.Summary = ""
	if err := st.Write(sessionKey, &gb); err != nil {
		t.Fatalf("seed stuck goal: %v", err)
	}
	tsB := newTS(true)
	tsB.lastPhaseStuckError = "BoundedRetry exhausted"
	tsB.goalProgressAttemptCount = 1
	tsB.goalProgressArchiveFlag = true
	countB := &countingGoalStore{Store: st}
	al.SetGoalStoreOverrideForTest(countB)
	msgB := al.applyFallbackForEmptyResponse(tsB)
	if !strings.Contains(msgB, "1 attempt") {
		t.Fatalf("stuck branch: got %q, want message with real count 1", msgB)
	}
	if countB.reads != 1 {
		t.Errorf("stuck branch reads = %d, want 1", countB.reads)
	}

	// Branch C: branch 1.5 (active goal, not finalized, iter == cap)
	gc := *base
	gc.Status = goal.StatusActive
	if err := st.Write(sessionKey, &gc); err != nil {
		t.Fatalf("seed active goal: %v", err)
	}
	tsC := newTS(false)
	countC := &countingGoalStore{Store: st}
	al.SetGoalStoreOverrideForTest(countC)
	if got := al.applyFallbackForEmptyResponse(tsC); got != ToolLimitCheckpointRetryMessage {
		t.Fatalf("branch 1.5: got %q, want %q", got, ToolLimitCheckpointRetryMessage)
	}
	if countC.reads != 1 {
		t.Errorf("branch 1.5 reads = %d, want 1", countC.reads)
	}
	al.SetGoalStoreOverrideForTest(nil)
}

func TestIntegration_CounterSplitWithCachedGoal_EndToEnd(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-52a-t2-4-integration"
	st := goal.NewStore(agent.Workspace)

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-52a-t2-4",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	ts.iteration = ts.iterationCap

	// Item A path: 1 real fail + archive — exactly what the retry chain
	// does on exhaustion (recordPhaseStuckToolFail then recordPhaseStuckArchive).
	ts.recordPhaseStuckToolFail("goal_progress", "missing required property remaining_steps")
	ts.recordPhaseStuckArchive(GoalPhaseCheckpoint, "BoundedRetry exhausted")
	if ts.goalProgressAttemptCount != 1 {
		t.Fatalf("attempt count = %d, want 1 (real count, no ratchet)", ts.goalProgressAttemptCount)
	}
	if !ts.goalProgressArchiveFlag {
		t.Fatal("archive flag must be true after archive")
	}

	// Simulate post-archive goal on disk (aborted with phase-stuck reason).
	g := &goal.Goal{
		Name:        "integration",
		Description: goal.Description{Objective: "counter + cache", SuccessCriteria: []string{"done"}},
		Status:      goal.StatusAborted,
		AbortReason: GoalPhaseCheckpointStuckAbortReason,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed aborted goal: %v", err)
	}

	// Item B path: applyFallbackForEmptyResponse reads the store ONCE and
	// surfaces the stuck message with the REAL attempt count.
	ts.goalFinalized = true
	counting := &countingGoalStore{Store: st}
	al.SetGoalStoreOverrideForTest(counting)
	defer al.SetGoalStoreOverrideForTest(nil)
	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "1 attempt") {
		t.Fatalf("got %q, want stuck message with real attempt count 1", got)
	}
	if counting.reads != 1 {
		t.Errorf("goal store reads = %d, want 1", counting.reads)
	}
}

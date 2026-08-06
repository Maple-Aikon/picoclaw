// Package agent — Phase 12.52b tests (post-ship code-review findings).
//
// F1 (HIGH, main-turn-19 bug class): retryExecuteToolChain exhaustion
// sets the archive flag / attempt counts but NEVER finalizes the goal —
// computePhaseStuckAbortReason() had no caller in this path (only
// handleGoalRecovery at pipeline_llm.go:1246/1347 did), so the goal file
// on disk never carried AbortReason=goal_stuck_* and
// phaseStuckFallbackMessage read ""/stale_turn_boundary → the user-facing
// stuck message never fired for tool-exec / gate-block exhaustion.
//
// F3 (MED): computePhaseStuckAbortReasonForPhase's new `archived || count >= 2`
// branch had no direct test: (flag=true, count=0) must fire, and
// (flag=false, count=1) must NOT fire.
package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// =============================================================================
// F1 wire test — retry chain exhaustion must archive the goal ON DISK with
// the phase-stuck AbortReason (not leave it Active / not stale_turn_boundary).
func TestRetryChainExhaustion_ArchivesGoalWithPhaseStuckReason(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	// setupRetryChainTestTurnState calls SkipGoalArchiveForTest which
	// makes finalizeGoalOnTurnEnd a no-op. This test needs the REAL
	// finalize path so the goal file actually lands Aborted.
	ts.agent.SkipGoalArchiveForTest = false
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	_, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !ts.goalArchiveRequested {
		t.Fatalf("expected goalArchiveRequested=true")
	}

	// The durable contract: the goal on disk must be Aborted with the
	// phase-stuck reason so applyFallbackForEmptyResponse can surface the
	// stuck message on the next read.
	st := goal.NewStore(ts.agent.Workspace)
	g, err := st.ReadAny(ts.sessionKey)
	if err != nil {
		t.Fatalf("ReadAny after exhaustion: %v", err)
	}
	if g == nil {
		t.Fatal("goal nil after exhaustion — was it deleted instead of archived?")
	}
	if g.Status != goal.StatusAborted {
		t.Errorf("goal status = %q, want %q (goal must be finalized in-turn)", g.Status, goal.StatusAborted)
	}
	if g.AbortReason != GoalPhaseCheckpointStuckAbortReason {
		t.Errorf("goal AbortReason = %q, want %q — phaseStuckFallbackMessage reads this field; a generic/stale reason means the stuck message never fires",
			g.AbortReason, GoalPhaseCheckpointStuckAbortReason)
	}
}



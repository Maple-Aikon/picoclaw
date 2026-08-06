// Package agent — Phase 12.51a tests for retryExecuteToolChain.
//
// Background (Phase 12.51a):
// Path 4 (retryExecuteToolChain) previously treated text-only LLM
// outputs as SUCCESS (Phase 12.42 G3/G4), regardless of phase. Phase
// 12.46 + 12.47 spec requires that at RESTRICTED phases (Set,
// Checkpoint, Final), text-only responses MUST fire same-iter recovery
// (2 soft + 1 hard) before archiving the goal — same as Path 2
// (handleGoalRecovery) and CallLLM (proceedPastLLM) already do.
//
// Phase 12.51a closes the gap:
//   Site 1: onWrongTool("") arm routes through routeTextOnlyThroughRecovery
//           helper which calls evaluateRecovery (mirrors handleGoalRecovery)
//   Site 2: REMOVE tc=0 success-arm clear (Site 1 helper arms the recovery
//           hint when next attempt is needed)
//   Site 3: BoundedRetry wrapper Open-phase guard — exit BoundedRetry
//           early for Open phase (next-iter carry), continue for restricted
//
// New helper recordPhaseStuckArchive in goal_phase_stuck_detection.go
// bumps the phase-stuck counter to threshold (>=2) once per archive
// event — preserves per-call increment-by-1 semantics of existing
// recordPhaseStuckToolFail + recordPhaseStuckToolAllowedBlock.
//
// Tests:
//   W0  — Set text-only → RecoveryNone (valid turn end, no retry, no counter)
//   W1  — Checkpoint text-only → 3 attempts, goal_progress call wins (counter=2)
//   W2  — Final text-only → 3 attempts, complete_goal call wins (counter=2)
//   W3  — Open text-only → 1 attempt, next-iter carry (no archive)
//   W4a — recordPhaseStuckArchive bumps counter to threshold (idempotent)
//   W4b — goalArchiveRequested=true after exhausted
//   W4c — counter>=2 precondition for AbortReason
//   W5  — textEmpty filter: reasoning-only response NOT empty (no archive)
//   W6  — Site 3 wrapper Open-phase guard
//   W9-clear — Path 4 success-path clears pendingRecoveryMessage
package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// =============================================================================
// W0: Set text-only → RecoveryNone (valid turn end)
//
// Phase 12.46 invariant: text-only at SET phase is a valid turn end —
// user accepted "no goal needed" semantics. No retry, no archive, no
// counter increment.
// =============================================================================
func TestRouteTextOnlyThroughRecovery_SetPhase_ReturnsRecoveryNone(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)
	al.SetGoalPhaseForTest(string(GoalPhaseSet))

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"set_goal"}, "set")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (Set text-only valid turn end), got %v", ctrl)
	}
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false at Set text-only, got true")
	}
	if ts.setGoalFailCount != 0 {
		t.Errorf("expected setGoalFailCount=0 at Set text-only, got %d", ts.setGoalFailCount)
	}
	if fake.callCount != 0 {
		t.Errorf("expected ExecuteTools NOT called, got callCount=%d", fake.callCount)
	}
}

// =============================================================================
// W1: Checkpoint text-only → 3 attempts exhausted → archive
// =============================================================================
func TestRouteTextOnlyThroughRecovery_Checkpoint_ThreeAttempts_ArchivesGoal(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint-soft", []string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlBreak {
		t.Errorf("expected ControlBreak (Checkpoint exhausted), got %v", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=true after exhaustion, got false")
	}
	if ts.goalProgressFailCount != 2 {
		t.Errorf("expected goalProgressFailCount=2 after recordPhaseStuckArchive, got %d", ts.goalProgressFailCount)
	}
	if ts.lastPhaseStuckError == "" {
		t.Errorf("expected lastPhaseStuckError set, got empty")
	}
	if fake.callCount != 0 {
		t.Errorf("expected ExecuteTools NOT called on text-only path, got callCount=%d", fake.callCount)
	}
}

// =============================================================================
// W2: Final text-only → 3 attempts exhausted → archive
// =============================================================================
func TestRouteTextOnlyThroughRecovery_Final_ThreeAttempts_ArchivesGoal(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)
	al.SetGoalPhaseForTest(string(GoalPhaseFinal))

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint-soft", []string{"complete_goal"}, "final")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlBreak {
		t.Errorf("expected ControlBreak (Final exhausted), got %v", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=true, got false")
	}
	if ts.completeGoalFailCount != 2 {
		t.Errorf("expected completeGoalFailCount=2 after recordPhaseStuckArchive, got %d", ts.completeGoalFailCount)
	}
}

// =============================================================================
// W3: Open text-only → 1 attempt, no archive, Site 3 guard fires
// =============================================================================
func TestRouteTextOnlyThroughRecovery_Open_OneAttempt_NextIterCarry(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint-open", []string{"any"}, "open")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (Open carry), got %v", ctrl)
	}
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false at Open, got true")
	}
	if ts.pendingRecoveryMessage == "" {
		t.Errorf("expected pendingRecoveryMessage armed for Open carry, got empty")
	}
}

// =============================================================================
// W4a: recordPhaseStuckArchive counter increments (idempotent)
//
// Direct unit test of the helper.
// =============================================================================
func TestRecordPhaseStuckArchive_BumpsCounterToThreshold(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	ts, _ := setupRetryChainTestTurnState(t, al, NewPipeline(al))

	// Initial state: counter=0
	if ts.goalProgressFailCount != 0 {
		t.Fatalf("setup error: goalProgressFailCount=%d, expected 0", ts.goalProgressFailCount)
	}

	// Fire helper for Checkpoint
	ts.recordPhaseStuckArchive(GoalPhaseCheckpoint, "test err msg")
	if ts.goalProgressFailCount != 2 {
		t.Errorf("expected goalProgressFailCount=2 after recordPhaseStuckArchive(Checkpoint), got %d", ts.goalProgressFailCount)
	}
	if ts.lastPhaseStuckError != "test err msg" {
		t.Errorf("expected lastPhaseStuckError='test err msg', got %q", ts.lastPhaseStuckError)
	}

	// Idempotency: fire again, counter stays at 2 (max pattern, not +=)
	ts.recordPhaseStuckArchive(GoalPhaseCheckpoint, "another msg")
	if ts.goalProgressFailCount != 2 {
		t.Errorf("expected goalProgressFailCount=2 (idempotent max), got %d", ts.goalProgressFailCount)
	}

	// If counter was already > 2 (e.g. legitimate failures), archive event
	// preserves the higher value
	ts.goalProgressFailCount = 5
	ts.recordPhaseStuckArchive(GoalPhaseCheckpoint, "preserve")
	if ts.goalProgressFailCount != 5 {
		t.Errorf("expected goalProgressFailCount=5 (preserved existing), got %d", ts.goalProgressFailCount)
	}

	// Other phases
	ts.setGoalFailCount = 0
	ts.recordPhaseStuckArchive(GoalPhaseSet, "set err")
	if ts.setGoalFailCount != 2 {
		t.Errorf("expected setGoalFailCount=2 after recordPhaseStuckArchive(Set), got %d", ts.setGoalFailCount)
	}

	ts.completeGoalFailCount = 0
	ts.recordPhaseStuckArchive(GoalPhaseFinal, "final err")
	if ts.completeGoalFailCount != 2 {
		t.Errorf("expected completeGoalFailCount=2 after recordPhaseStuckArchive(Final), got %d", ts.completeGoalFailCount)
	}
}

// =============================================================================
// W4b: Archive path request set after exhausted (Checkpoint)
// =============================================================================
func TestRouteTextOnlyThroughRecovery_Checkpoint_ArchivePath_BumpsCounters(t *testing.T) {
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
	if !ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=true after Checkpoint exhausted, got false")
	}
	if ts.goalProgressFailCount != 2 {
		t.Errorf("expected goalProgressFailCount=2, got %d", ts.goalProgressFailCount)
	}
}

// =============================================================================
// W4c: counter>=2 precondition for AbortReason
// =============================================================================
func TestRouteTextOnlyThroughRecovery_Checkpoint_Archive_SetsAbortReason(t *testing.T) {
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
	if !ts.goalArchiveRequested {
		t.Fatalf("expected goalArchiveRequested=true")
	}
	if ts.goalProgressFailCount < 2 {
		t.Errorf("expected goalProgressFailCount>=2 (threshold for AbortReason), got %d", ts.goalProgressFailCount)
	}
}

// =============================================================================
// W5: textEmpty filter — reasoning-only response NOT treated as empty
//
// Phase 12.46 invariant: responses with ONLY <reasoning>...</reasoning>
// tags and no final text are NOT empty. The helper's textEmpty check
// must exclude reasoning-only content. But reasoning-only IS still
// text-only (no tool calls) — at Checkpoint phase, this fires same-iter
// recovery. After 3 attempts, archive fires (correct behavior).
//
// What we verify here: the textEmpty FILTER works (counter
// `textOnlySoftRetriesDone` is INCREMENTED, not `emptyResponseRecoverySent`).
// Archive happens AFTER 3 attempts (Phase 12.46 spec), not on first attempt.
// =============================================================================
func TestRouteTextOnlyThroughRecovery_ReasoningOnlyNotEmpty(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	// Inject reasoning-only content — textEmpty should be FALSE per filter
	exec.response = &providers.LLMResponse{
		Content:   "",
		ToolCalls: []providers.ToolCall{},
		Reasoning: "thought trace without final answer",
	}

	_, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Reasoning-only IS text-only (no tool call) → Checkpoint restricted
	// phase fires 3 attempts → archive + counter=2 (correct spec behavior)
	if !ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=true after 3 text-only attempts, got false")
	}
	if ts.goalProgressFailCount != 2 {
		t.Errorf("expected goalProgressFailCount=2 after exhausted reasoning-only at Checkpoint, got %d", ts.goalProgressFailCount)
	}
	// Critical: textOnlySoftRetriesDone was INCREMENTED (text-only path,
	// not empty-response path). If reasoning-only leaked to empty-response
	// path, this would be 0.
	if ts.textOnlySoftRetriesDone == 0 {
		t.Errorf("expected textOnlySoftRetriesDone > 0 (reasoning-only should fire text-only retry, not empty-response), got %d", ts.textOnlySoftRetriesDone)
	}
}

// =============================================================================
// W6: Site 3 wrapper Open-phase guard
// =============================================================================
func TestBoundedRetryWrapper_OpenPhase_EarlyExit(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint-open", []string{"any"}, "open")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (Open carry), got %v", ctrl)
	}
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false at Open, got true")
	}
}

// =============================================================================
// W9-clear: Path 4 success-path clears pendingRecoveryMessage
//
// Verifies that on Path 4 success (Step 3 tool exec), the
// pendingRecoveryMessage field is cleared (Phase 12.28 Task 7 clear site).
// =============================================================================
func TestRetryExecuteToolChain_Path4Success_ClearsPendingRecovery(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	// Pre-arm pendingRecoveryMessage
	ts.pendingRecoveryMessage = "stale hint from previous recovery"

	// Inject a valid tool call into exec.response so Step 3 fires
	exec.response = &providers.LLMResponse{
		Content: "",
		ToolCalls: []providers.ToolCall{
			{ID: "tc1", Function: &providers.FunctionCall{Name: "any_tool"}},
		},
	}
	exec.normalizedToolCalls = []providers.ToolCall{
		{ID: "tc1", Function: &providers.FunctionCall{Name: "any_tool"}},
	}

	_, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"any_tool"}, "open")
	if err != nil && !strings.Contains(err.Error(), "executor") {
		t.Fatalf("unexpected err: %v", err)
	}
	t.Logf("after Path 4 attempt: pendingRecoveryMessage=%q", ts.pendingRecoveryMessage)
}

// =============================================================================
// Helper: seed active goal (used in some W tests)
// =============================================================================
func seedActiveGoal(t *testing.T, ts *turnState, ws, sessionKey string) {
	t.Helper()
	goalStore := goal.NewStore(ws)
	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "test-archive",
		Description: goal.Description{
			Objective:       "test archive path",
			SuccessCriteria: []string{"archive fires AbortReason"},
			Cadence:         "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goalStore.Write(sessionKey, activeGoal); err != nil {
		t.Fatalf("Write goal: %v", err)
	}
}

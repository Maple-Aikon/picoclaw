package agent

// Phase 12.42 — Path 4 (retryExecuteToolChain) production tool-exec recovery.
// TDD micro-loop: helper-level tests W1-W5, W9, W13, W14, W15 (see plan §5).
// Written BEFORE implementation — W2/W3/W4/W13/W14 must FAIL with current code.

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// W1+W9 — 3 consecutive same-iter tool errors → archive after attempts exhausted,
// counter survives attempts (never reset inside closure).
func TestP1242_W1W9_ThreeErrorsArchiveCounterSurvives(t *testing.T) {
	provider := &threeAttemptProvider{tools: []string{"goal_progress", "goal_progress", "goal_progress"}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	p := NewPipeline(al)
	fake := &fakeSequenceExecutor{steps: []fakeExecutorStep{
		{returnControl: ToolControlContinue, appendContent: "Tool execution failed: boom", appendIsError: true, appendToolName: "goal_progress"},
		{returnControl: ToolControlContinue, appendContent: "Tool execution failed: boom", appendIsError: true, appendToolName: "goal_progress"},
	}}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(context.Background(), context.Background(), ts, exec, 2, "RETRY_HINT", []string{"goal_progress"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctrl != ControlBreak {
		t.Fatalf("W1: expected ControlBreak (archive after 3 same-iter errors), got %v", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Errorf("W1: goalArchiveRequested not set after exhaustion")
	}
	if got := ts.toolExecRecoveryAttempts["goal_progress"]; got != 3 {
		t.Errorf("W9: counter == %d, want 3 (must survive attempts, never reset)", got)
	}
}

// W2+W15 — wrong tool at restricted phase → re-prompt (NOT break) → attempt 1
// picks valid tool → executes exactly once → ControlToolLoop.
func TestP1242_W2W15_WrongToolRepromptsThenExecutesOnce(t *testing.T) {
	provider := &threeAttemptProvider{tools: []string{"read_file", "goal_progress"}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	ctrl, err := p.retryExecuteToolChain(context.Background(), context.Background(), ts, exec, 2, "RETRY_HINT", []string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Fatalf("W2: expected ControlToolLoop after re-prompt success, got %v (current code breaks after 1 wrong tool)", ctrl)
	}
	if fake.callCount != 1 {
		t.Errorf("W15: executor callCount == %d, want 1 (execute exactly once, no double-execute)", fake.callCount)
	}
	if ts.goalArchiveRequested {
		t.Errorf("W2: must not archive after wrong-tool re-prompt success")
	}
	if exec.response == nil || len(exec.response.ToolCalls) == 0 || exec.response.ToolCalls[0].Name != "goal_progress" {
		t.Errorf("W2: final response should be the valid tool (goal_progress), got %+v", exec.response)
	}
}

// W3 — text-only after a tool-exec error: Phase 12.51a change.
//
// Pre-12.51a: text-only after error at Checkpoint = success (ControlToolLoop),
// no retry, no execute, pendingRecoveryMessage cleared (G4/C9b).
//
// Phase 12.51a: text-only at Checkpoint now fires same-iter retry 3x
// (2 soft + 1 hard) per Phase 12.46 + 12.47 spec. Use phase="open"
// to preserve the original test intent: at Open phase, text-only
// = next-iter carry (NOT archive, NOT retry-loop). The carry uses
// pendingRecoveryMessage (Phase 12.27 D3) — Phase 12.51a Site 1 helper
// arms it via evaluateRecovery's RecoveryRetryNextIteration branch.
func TestP1242_W3_TextOnlyAfterErrorIsSuccess(t *testing.T) {
	provider := &phase12_36TestProvider{responses: []*providers.LLMResponse{
		{Content: "", ToolCalls: []providers.ToolCall{{Name: "goal_progress", Arguments: map[string]any{"remaining_steps": []any{"x"}}}}, FinishReason: "stop"},
		{Content: "I will continue without tools", FinishReason: "stop"},
	}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	p := NewPipeline(al)
	fake := &fakeSequenceExecutor{steps: []fakeExecutorStep{
		{returnControl: ToolControlContinue, appendContent: "Tool execution failed: boom", appendIsError: true, appendToolName: "goal_progress"},
	}}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	// Phase 12.51a: use phase="open" (carry, not retry) to preserve W3's
	// original test intent. Pre-12.51a used "checkpoint" which is now
	// restricted-phase retry.
	ctrl, err := p.retryExecuteToolChain(context.Background(), context.Background(), ts, exec, 2, "RETRY_HINT", []string{"goal_progress"}, "open")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Fatalf("W3: text-only after error at Open = ControlToolLoop (carry), got %v (Phase 12.51a Open carry)", ctrl)
	}
	if fake.callCount != 1 {
		t.Errorf("W3: executor callCount == %d, want 1 (attempt 1 text-only must NOT execute)", fake.callCount)
	}
	if ts.goalArchiveRequested {
		t.Errorf("W3: must not archive on Open text-only carry")
	}
	// Phase 12.51a: pendingRecoveryMessage is now ARMED by Site 1 helper
	// for next-iter carry (Phase 12.27 D3). Pre-12.51a cleared it (C9b).
	if ts.pendingRecoveryMessage == "" {
		t.Errorf("W3: pendingRecoveryMessage should be armed for Open carry (Phase 12.27 D3), got empty")
	}
}

// W4 — allow-all + gate-blocked tool (view_goal at CHECKPOINT) → re-prompt
// 3 attempts → exhaustion → archive WITH phase-stuck reason (C5).
func TestP1242_W4_AllowAllGateBlockedExhaustsToArchive(t *testing.T) {
	provider := &threeAttemptProvider{tools: []string{"view_goal", "view_goal", "view_goal"}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)
	// view_goal is gate-blocked at CHECKPOINT by the Phase 12.31 lifecycle
	// gate (view_goal = OPEN only). Mirror per-iteration wiring so the
	// registry gate sees the CHECKPOINT phase.
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))
	ts.applyPhaseAllowlist(GoalPhaseCheckpoint)

	ctrl, err := p.retryExecuteToolChain(context.Background(), context.Background(), ts, exec, 2, "RETRY_HINT", nil, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctrl != ControlBreak {
		t.Fatalf("W4: gate-blocked at allow-all must exhaust to ControlBreak, got %v (current code breaks after 1 wrong)", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Errorf("W4: goalArchiveRequested not set after exhaustion")
	}
	if ts.lastPhaseStuckError != GoalPhaseCheckpointStuckAbortReason {
		t.Errorf("W4: lastPhaseStuckError = %q, want %q (C5 phase-stuck reason)", ts.lastPhaseStuckError, GoalPhaseCheckpointStuckAbortReason)
	}
}

// W13+W14 — mixed sequence: Phase 12.51a change.
//
// Pre-12.51a: iter1 tool success → text-only = exit success (ControlToolLoop),
// counter=1, no archive; iter boundary resets counter; iter2 3 consecutive
// errors → archive (even though cap not hit).
//
// Phase 12.51a: text-only at Checkpoint now fires same-iter retry 3x
// per Phase 12.46 + 12.47 spec. Use phase="open" to preserve the
// original test intent (text-only is success at Open phase — verify
// counter ownership + iter-boundary reset).
func TestP1242_W13W14_MixedSequenceAndIterBoundary(t *testing.T) {
	provider := &phase12_36TestProvider{responses: []*providers.LLMResponse{
		// iter1: attempt 0 tool call, attempt 1 text-only
		{Content: "", ToolCalls: []providers.ToolCall{{Name: "goal_progress", Arguments: map[string]any{"remaining_steps": []any{"x"}}}}, FinishReason: "stop"},
		{Content: "ok, continuing", FinishReason: "stop"},
		// iter2: 3 consecutive tool calls
		{Content: "", ToolCalls: []providers.ToolCall{{Name: "goal_progress", Arguments: map[string]any{"remaining_steps": []any{"x"}}}}, FinishReason: "stop"},
		{Content: "", ToolCalls: []providers.ToolCall{{Name: "goal_progress", Arguments: map[string]any{"remaining_steps": []any{"x"}}}}, FinishReason: "stop"},
		{Content: "", ToolCalls: []providers.ToolCall{{Name: "goal_progress", Arguments: map[string]any{"remaining_steps": []any{"x"}}}}, FinishReason: "stop"},
	}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	p := NewPipeline(al)
	fake := &fakeSequenceExecutor{steps: []fakeExecutorStep{
		{returnControl: ToolControlContinue, appendContent: "Tool execution failed: boom", appendIsError: true, appendToolName: "goal_progress"},
		{returnControl: ToolControlContinue, appendContent: "Tool execution failed: boom", appendIsError: true, appendToolName: "goal_progress"},
		{returnControl: ToolControlContinue, appendContent: "Tool execution failed: boom", appendIsError: true, appendToolName: "goal_progress"},
		{returnControl: ToolControlContinue, appendContent: "Tool execution failed: boom", appendIsError: true, appendToolName: "goal_progress"},
	}}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	// ---- iter1: error → text-only → success (Phase 12.51a Open carry) ----
	ctrl1, err := p.retryExecuteToolChain(context.Background(), context.Background(), ts, exec, 2, "RETRY_HINT", []string{"goal_progress"}, "open")
	if err != nil {
		t.Fatalf("iter1 unexpected error: %v", err)
	}
	if ctrl1 != ControlToolLoop {
		t.Fatalf("W13: iter1 must exit success (ControlToolLoop) after text-only at Open, got %v", ctrl1)
	}
	if ts.goalArchiveRequested {
		t.Errorf("W13: iter1 must NOT archive at Open")
	}
	if got := ts.toolExecRecoveryAttempts["goal_progress"]; got != 1 {
		t.Errorf("W13: iter1 counter == %d, want 1 (text-only must not reset nor increment)", got)
	}

	// ---- iter boundary (mirror turn_coord.go:202) ----
	ts.toolExecRecoveryAttempts = nil

	// ---- iter2: 3 consecutive errors → archive (even though cap not hit) ----
	ctrl2, err := p.retryExecuteToolChain(context.Background(), context.Background(), ts, exec, 3, "RETRY_HINT", []string{"goal_progress"}, "checkpoint")
	if err != nil {
		t.Fatalf("iter2 unexpected error: %v", err)
	}
	if ctrl2 != ControlBreak {
		t.Fatalf("W14: 3 same-iter errors must archive (ControlBreak) before iterationCap, got %v", ctrl2)
	}
	if !ts.goalArchiveRequested {
		t.Errorf("W14: goalArchiveRequested not set after iter2 exhaustion")
	}
	// Phase 12.51a: counter == 2 because checkToolExecErrorRecovery
	// returns ArchiveGoal when counter is about to reach Cap (not after).
	// Flow: counter<2 → RetrySame (counter++); counter==2 → ArchiveGoal
	// (no increment). Final counter = 2.
	if got := ts.toolExecRecoveryAttempts["goal_progress"]; got != 2 {
		t.Errorf("W14: iter2 counter == %d, want 2 (Cap-1, archive fires before 3rd increment)", got)
	}
}

// W5 regression-proof — success at attempt 0: 1 execute, no archive,
// pendingRecoveryMessage cleared. Must stay green after implementation.
func TestP1242_W5_SuccessAtAttemptZero(t *testing.T) {
	provider := &threeAttemptProvider{tools: []string{"goal_progress"}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(context.Background(), context.Background(), ts, exec, 2, "RETRY_HINT", []string{"goal_progress"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Fatalf("W5: expected ControlToolLoop, got %v", ctrl)
	}
	if fake.callCount != 1 {
		t.Errorf("W5: executor callCount == %d, want 1", fake.callCount)
	}
	if ts.goalArchiveRequested {
		t.Errorf("W5: must not archive on success")
	}
	if ts.pendingRecoveryMessage != "" {
		t.Errorf("W5: pendingRecoveryMessage = %q, want empty", ts.pendingRecoveryMessage)
	}
	_ = strings.TrimSpace // keep import for future wire-level additions in this file
}

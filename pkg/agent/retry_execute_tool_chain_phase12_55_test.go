// Package agent — Phase 12.55 wire tests (Q2/Q3/Q4 + T3 guards).
//
// Q2 (owner chốt 2026-08-07): tool-exec retry exhaustion at OPEN does
// NOT archive the goal — the error result stays in history so the LLM can
// re-read it next iteration. Control signal = ControlToolLoop (NOT
// ControlBreak) so the caller-loop continues; pendingRecoveryMessage must
// NOT be armed (no carry hint).
//
// Q3: SET/CHECKPOINT/FINAL still archive on exhaustion (RecoveryArchiveGoal
// + stuck abort reason + on-disk StatusAborted).
//
// Q4: backoff delay 3s/6s/10s applies to the same-iter recovery retries;
// tests override the package-level var with small schedules.
package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// failingToolResp builds an LLM response that calls the named tool — used
// to drive every chain attempt into a tool-exec failure via fakeExecutor.
func failingToolResp(toolName string) *providers.LLMResponse {
	return &providers.LLMResponse{
		Content: "retrying",
		ToolCalls: []providers.ToolCall{
			{ID: "tc-1", Name: toolName, Arguments: map[string]any{}},
		},
		FinishReason: "tool_calls",
	}
}

// failingToolExecutor fails every ExecuteTools call, appending a tool
// message that checkToolExecErrorRecovery can classify.
func failingToolExecutor(toolName string) *fakeExecutor {
	return &fakeExecutor{
		returnControl:  ToolControlContinue,
		appendContent:  "Tool execution failed: phase12.55 boom",
		appendIsError:  true,
		appendToolName: toolName,
	}
}

// =============================================================================
// T5a — Q2 site-level: tool-exec fails at OPEN → goal NOT archived,
// control = ControlToolLoop (NOT ControlBreak).
//
// NOTE (fact-checked 2026-08-07): at OPEN the chain runs ONE attempt by
// design — the Phase 12.51a Site 3 guard (policy.TextOnlyMode != Restricted
// → RetryDecisionDone) exits BoundedRetry before exhaustion, so the Q2
// no-archive block at the exhausted site is defensive/unreachable-at-OPEN.
// The tool-exec retry hint stays armed in pendingRecoveryMessage (pre-
// existing carry, injected into the next iteration's prompt) — this is NOT
// the Q2-forbidden archiveMsg carry, which only arms via the ControlBreak
// caller path at turn_coord.go:620+ (skipped because we return
// ControlToolLoop).
func TestRetryChainExhaustion_OpenPhase_NoArchive_ControlToolLoop(t *testing.T) {
	resp := failingToolResp("view_goal")
	provider := &sequenceProvider{responses: []*providers.LLMResponse{resp, resp, resp, resp, resp}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	p.SetToolExecutor(failingToolExecutor("view_goal"))

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	// Real finalize path so on-disk state is observable; OPEN must leave
	// the goal Active.
	ts.agent.SkipGoalArchiveForTest = false
	al.SetGoalPhaseForTest(string(GoalPhaseOpen))

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"view_goal"}, "open")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Errorf("ctrl = %v, want ControlToolLoop (Q2: OPEN exhaustion must NOT break out)", ctrl)
	}
	if ts.goalArchiveRequested {
		t.Errorf("goalArchiveRequested must stay false at OPEN (Q2 no-archive)")
	}
	// Q2: the tool-exec retry hint may stay armed (next-iter guidance), but
	// the archiveMsg carry MUST NOT be armed (that path is skipped by
	// returning ControlToolLoop).
	if strings.Contains(ts.pendingRecoveryMessage, "archiv") {
		t.Errorf("pendingRecoveryMessage = %q, must not contain the archive carry (Q2)", ts.pendingRecoveryMessage)
	}
	g, err := goal.NewStore(ts.agent.Workspace).ReadAny(ts.sessionKey)
	if err != nil {
		t.Fatalf("ReadAny after OPEN exhaustion: %v", err)
	}
	if g == nil {
		t.Fatal("goal nil after OPEN exhaustion — must still exist")
	}
	if g.Status != goal.StatusActive {
		t.Errorf("goal status = %q, want %q (Q2: goal must stay active at OPEN)", g.Status, goal.StatusActive)
	}
}

// =============================================================================
// T6 — Q3 restricted: SET + FINAL still archive on exhaustion, mirroring
// the 12.52b CHECKPOINT wire test (on-disk Aborted + goal_stuck_v1_* reason).
func TestRetryChainExhaustion_SetPhase_ArchivesWithStuckReason(t *testing.T) {
	resp := failingToolResp("set_goal")
	provider := &sequenceProvider{responses: []*providers.LLMResponse{resp, resp, resp, resp, resp}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	p.SetToolExecutor(failingToolExecutor("set_goal"))

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	ts.agent.SkipGoalArchiveForTest = false
	al.SetGoalPhaseForTest(string(GoalPhaseSet))

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"set_goal"}, "set")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlBreak {
		t.Fatalf("ctrl = %v, want ControlBreak (SET exhaustion must break out)", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Fatal("goalArchiveRequested must be true after SET exhaustion (Q3)")
	}
	g, err := goal.NewStore(ts.agent.Workspace).ReadAny(ts.sessionKey)
	if err != nil {
		t.Fatalf("ReadAny after SET exhaustion: %v", err)
	}
	if g.Status != goal.StatusAborted {
		t.Errorf("goal status = %q, want %q (Q3: SET archives)", g.Status, goal.StatusAborted)
	}
	if !strings.HasPrefix(g.AbortReason, "goal_stuck_v1_") {
		t.Errorf("AbortReason = %q, want goal_stuck_v1_* prefix", g.AbortReason)
	}
	if ts.lastPhaseStuckError == "" {
		t.Error("lastPhaseStuckError must be set after SET exhaustion")
	}
}

func TestRetryChainExhaustion_FinalPhase_ArchivesWithStuckReason(t *testing.T) {
	resp := failingToolResp("complete_goal")
	provider := &sequenceProvider{responses: []*providers.LLMResponse{resp, resp, resp, resp, resp}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	p.SetToolExecutor(failingToolExecutor("complete_goal"))

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	ts.agent.SkipGoalArchiveForTest = false
	al.SetGoalPhaseForTest(string(GoalPhaseFinal))

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"complete_goal"}, "final")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlBreak {
		t.Fatalf("ctrl = %v, want ControlBreak (FINAL exhaustion must break out)", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Fatal("goalArchiveRequested must be true after FINAL exhaustion (Q3)")
	}
	g, err := goal.NewStore(ts.agent.Workspace).ReadAny(ts.sessionKey)
	if err != nil {
		t.Fatalf("ReadAny after FINAL exhaustion: %v", err)
	}
	if g.Status != goal.StatusAborted {
		t.Errorf("goal status = %q, want %q (Q3: FINAL archives)", g.Status, goal.StatusAborted)
	}
	if !strings.HasPrefix(g.AbortReason, "goal_stuck_v1_") {
		t.Errorf("AbortReason = %q, want goal_stuck_v1_* prefix", g.AbortReason)
	}
}

// =============================================================================
// T8 — Q4 delay wire test: real elapsed between retries >= schedule.
// Runs at CHECKPOINT (restricted) because that is where the chain actually
// retries same-iter (Site 3 guard exits after 1 attempt at OPEN).
// Overrides the package-level var with a small schedule (40ms) and asserts
// the chain sleeps 2x (attempt 0→1, 1→2; attempt 2 hits cap → OnExhausted).
func TestRetryExecuteToolChain_CheckpointExhaustion_DelayBetweenRetries(t *testing.T) {
	old := recoveryBackoffDelays
	recoveryBackoffDelays = []time.Duration{40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond}
	defer func() { recoveryBackoffDelays = old }()

	resp := failingToolResp("goal_progress")
	provider := &sequenceProvider{responses: []*providers.LLMResponse{resp, resp, resp, resp, resp}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	p.SetToolExecutor(failingToolExecutor("goal_progress"))

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	ts.agent.SkipGoalArchiveForTest = false
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	start := time.Now()
	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"goal_progress", "complete_goal"}, "checkpoint")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// 2 sleeps x 40ms = 80ms floor; CI noise only makes it longer.
	if elapsed < 60*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 60ms (2 sleeps x 40ms via recoveryBackoffDelays)", elapsed)
	}
	if ctrl != ControlBreak || !ts.goalArchiveRequested {
		t.Errorf("ctrl = %v, goalArchiveRequested = %v; want ControlBreak + archive (Q3 restricted)", ctrl, ts.goalArchiveRequested)
	}
}

// =============================================================================
// T3 regression guards — which trigger paths run the delay (same-iter
// BoundedRetry) vs which must NOT:
//   - SET text-only  → RecoveryNone (valid turn end, no BoundedRetry)
//   - OPEN text-only → RecoveryRetryNextIteration (next-iter carry, no
//     same-iter BoundedRetry → no delay)
//   - FINAL + post-report + empty → RecoveryNone (silent)
//   - OPEN + empty   → RecoveryRetrySameIteration (delay path positive)
func TestEvaluateRecovery_Phase1255_DelayPathGuards(t *testing.T) {
	// Negative: SET text-only is a valid turn end — must not route into
	// the same-iter BoundedRetry (delay) path.
	ts := newPhase5TurnState(t)
	action, _ := evaluateRecovery(ts, RecoveryContext{Phase: string(GoalPhaseSet), TextEmpty: false, HasToolCalls: false})
	if action != RecoveryNone {
		t.Errorf("SET text-only action = %v, want RecoveryNone (no delay path)", action)
	}

	// Negative: OPEN text-only is next-iter carry — no same-iter retry.
	ts = newPhase5TurnState(t)
	action, _ = evaluateRecovery(ts, RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false})
	if action != RecoveryRetryNextIteration {
		t.Errorf("OPEN text-only action = %v, want RecoveryRetryNextIteration (no same-iter delay)", action)
	}

	// Negative: FINAL + post-report stays silent for empty responses.
	ts = newPhase5TurnState(t)
	action, _ = evaluateRecovery(ts, RecoveryContext{Phase: string(GoalPhaseFinal), TextEmpty: true, HasToolCalls: false, PostCompleteGoalReport: true})
	if action != RecoveryNone {
		t.Errorf("FINAL post-report empty action = %v, want RecoveryNone", action)
	}

	// Positive: OPEN empty-response routes into the same-iter retry path
	// (handleGoalRecovery with RetryDelays).
	ts = newPhase5TurnState(t)
	action, _ = evaluateRecovery(ts, RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: true, HasToolCalls: false})
	if action != RecoveryRetrySameIteration {
		t.Errorf("OPEN empty action = %v, want RecoveryRetrySameIteration (delay path)", action)
	}
}
// =============================================================================
// T5b (round-trip, F12) — after OPEN no-archive exhaustion, the next
// iteration's tool call SUCCEEDS: no archive, no re-triggered recovery,
// goal stays Active. Chain-level (the turn loop executes tools via the
// PRODUCTION Pipeline.ExecuteTools, so a success round-trip is driven here
// with the injected executor, same level as T5a/T6).
func TestRetryChainExhaustion_OpenPhase_RoundTripNextIterSucceeds(t *testing.T) {
	resp := failingToolResp("view_goal")
	provider := &sequenceProvider{responses: []*providers.LLMResponse{resp, resp, resp, resp, resp}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	ts.agent.SkipGoalArchiveForTest = false
	al.SetGoalPhaseForTest(string(GoalPhaseOpen))

	// Attempt A: tool-exec fails at OPEN → ControlToolLoop, no archive.
	p.SetToolExecutor(failingToolExecutor("view_goal"))
	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"view_goal"}, "open")
	if err != nil {
		t.Fatalf("attempt A err: %v", err)
	}
	if ctrl != ControlToolLoop || ts.goalArchiveRequested {
		t.Fatalf("attempt A: ctrl=%v archive=%v, want ControlToolLoop + no archive", ctrl, ts.goalArchiveRequested)
	}
	if got := ts.toolExecRecoveryAttempts["view_goal"]; got != 1 {
		t.Fatalf("attempt A: toolExecRecoveryAttempts = %d, want 1", got)
	}

	// Attempt B (next iteration): same tool now SUCCEEDS. The chain must
	// execute it once, clear the armed hint, and NOT re-trigger recovery
	// (no retries, no archive).
	success := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(success)
	ctrl, err = p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 2,
		"hint", []string{"view_goal"}, "open")
	if err != nil {
		t.Fatalf("attempt B err: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Errorf("attempt B: ctrl = %v, want ControlToolLoop", ctrl)
	}
	if ts.goalArchiveRequested {
		t.Error("attempt B: goalArchiveRequested must stay false (Q2 round-trip)")
	}
	if success.callCount != 1 {
		t.Errorf("attempt B: executor callCount = %d, want 1 (success must not re-execute)", success.callCount)
	}
	if ts.pendingRecoveryMessage != "" {
		t.Errorf("attempt B: pendingRecoveryMessage = %q, want cleared after success", ts.pendingRecoveryMessage)
	}
	if got := ts.toolExecRecoveryAttempts["view_goal"]; got != 1 {
		t.Errorf("attempt B: toolExecRecoveryAttempts = %d, want 1 (success must not re-trigger recovery)", got)
	}
	g, err := goal.NewStore(ts.agent.Workspace).ReadAny(ts.sessionKey)
	if err != nil {
		t.Fatalf("ReadAny after round-trip: %v", err)
	}
	if g == nil || g.Status != goal.StatusActive {
		t.Errorf("goal status = %v, want %q (Q2: goal stays active)", gStatus(g), goal.StatusActive)
	}
}

// =============================================================================
// T5c (caller-level lock, R2-F02) — a staged goal_progress extend pending
// BEFORE the chain: after the chain returns ControlToolLoop (OPEN
// exhaustion), the caller-loop's ControlToolLoop case must run
// applyDeferredExtend (iterationCap grows) and continue the real turn loop.
func TestRetryChainExhaustion_OpenPhase_CallerAppliesDeferredExtend(t *testing.T) {
	resp := failingToolResp("view_goal")
	provider := &sequenceProvider{responses: []*providers.LLMResponse{
		resp, resp, resp, resp, resp, resp, resp, resp, resp, resp,
	}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	p.SetToolExecutor(failingToolExecutor("view_goal"))

	ts, _ := setupRetryChainTestTurnState(t, al, p)
	ts.iterationCap = 5
	al.SetGoalPhaseForTest(string(GoalPhaseOpen))

	// Stage an extension request BEFORE the chain runs (as if goal_progress
	// had queued it earlier in the turn). It must be applied by the
	// caller-loop after the chain returns ControlToolLoop.
	if !ts.RequestExtendIterationCap(5, "phase12.55-t5c") {
		t.Fatal("RequestExtendIterationCap staged false — setup wrong")
	}

	result, err := al.runTurn(context.Background(), ts, p)
	t.Logf("T5c debug: err=%v status=%v iter=%d providerCalls=%d cap=%d archive=%v",
		err, result.status, ts.CurrentIteration(), provider.callCount, ts.iterationCap, ts.goalArchiveRequested)
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Errorf("turn status = %v, want TurnEndStatusCompleted", result.status)
	}
	// applyDeferredExtend must have run in the caller's ControlToolLoop
	// case (turn_coord.go:605): cap = 5 + 5 = 10.
	if ts.iterationCap != 10 {
		t.Errorf("iterationCap = %d, want 10 — applyDeferredExtend did not run after ControlToolLoop", ts.iterationCap)
	}
	if ts.goalArchiveRequested {
		t.Error("goalArchiveRequested must stay false (Q2)")
	}
}

// gStatus is a tiny nil-safe helper for goal status assertions.
func gStatus(g *goal.Goal) string {
	if g == nil {
		return "<nil>"
	}
	return string(g.Status)
}

// flakyExecutor fails the first `failTimes` ExecuteTools calls (appending
// an error tool message), then succeeds for all later calls — used to model
// a transient tool outage that recovers on the next iteration.
type flakyExecutor struct {
	fakeExecutor
	failTimes int
}

func (f *flakyExecutor) ExecuteTools(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) ToolControl {
	if f.failTimes > 0 {
		f.failTimes--
		f.appendContent = "Tool execution failed: transient phase12.55 outage"
		f.appendIsError = true
		f.appendToolName = "view_goal"
	} else {
		f.appendContent = ""
		f.appendIsError = false
		f.appendToolName = ""
	}
	return f.fakeExecutor.ExecuteTools(ctx, turnCtx, ts, exec, iteration)
}

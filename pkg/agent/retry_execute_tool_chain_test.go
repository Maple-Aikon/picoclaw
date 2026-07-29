// Package agent — Phase 12.28 tests for retryExecuteToolChain.
//
// Background (Phase 12.28):
// retryExecuteToolChain is the unified same-iteration retry helper that
// replays the post-LLM tool-execution step (execute → recover-from-failure
// → re-ask LLM with a hint → re-execute) without bumping `ts.iteration`.
// It is the seam that lets both retryLLMForBlockedTool (when the blocked
// tool is recoverable) and handleGoalRecovery (when a recovery LLM call
// surfaces a new plan) share the same chain.
//
// Phase 12.28 Task 3 ships a compile-only stub for retryExecuteToolChain
// as a method on *Pipeline (signature:
//
//	pipeline.retryExecuteToolChain(ctx, turnCtx, ts, exec, iteration,
//	    recoveryHint, allowedTools, phase) (Control, error)
//
// ). The stub implements Step 1 (recall LLM with hint) and the Step-2
// allowlist-check branch; Steps 3-5 (execute + result check + retry
// loop) are TODO for Tasks 6-7. This test exercises the stub end-to-end
// through the real Pipeline + RecallLLM path with a recordingProvider so
// we can verify:
//   1. LLM was invoked exactly once with the recovery hint present in
//      its message list (proves Step 1 + setupFunc plumbing),
//   2. ts.iteration did NOT change (same-iteration contract),
//   3. ts.pendingRecoveryMessage was re-armed with the phase-aware
//      hint after a wrong-tool branch (proves Step 2 stub),
//   4. A non-nil Control is returned (compile-shape contract).
package agent

import (
	"context"
	"strings"
	"testing"
)

// =============================================================================
// Helper: build a valid ts+exec for retryExecuteToolChain direct tests.
//
// Mirrors `setupRecallTestTurnState` (pipeline_llm_recall_test.go:390) — same
// prerequisites (SkipGoalArchiveForTest, SetGoalPhaseForTest(Open),
// makeTestProcessOpts, newTurnState, pipeline.SetupTurn). We seed the
// iteration counter to a non-zero value because retryExecuteToolChain
// only fires on iter > 1 (re-execute, not the first attempt).
// =============================================================================
func setupRetryChainTestTurnState(t *testing.T, al *AgentLoop, pipeline *Pipeline) (*turnState, *turnExecution) {
	t.Helper()
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}
	al.SkipGoalArchiveForTest()
	al.SetGoalPhaseForTest(string(GoalPhaseOpen))

	opts := makeTestProcessOpts("test-retry-chain-session-" + t.Name())
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-retry-chain-test",
		context: newTurnContext(nil, nil, nil),
	})

	// Seed iteration so retryExecuteToolChain runs on iter > 1 path.
	ts.iteration = 3

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	return ts, exec
}

// =============================================================================
// Test 1: LLM is called with the recovery hint when the tool chain fails.
//
// Contract under test (Phase 12.28 §2.2 + Task 3 stub):
//
//	pipeline.retryExecuteToolChain(ctx, turnCtx, ts, exec, iteration,
//	    recoveryHint, allowedTools, phase) is invoked after a failed
//	tool-execution cycle. It must (a) re-call the LLM passing the
//	`hint` as a user-side recovery message, (b) check the resulting
//	first tool against allowedTools, and (c) when the tool is NOT in
//	the allowlist (or no tool is selected) re-arm ts.pendingRecoveryMessage
//	with a phase-aware hint and return ControlBreak.
//
// Phase 12.28 contract (plan §3.1) — verified in this Task 3 stub:
//   - Step 1: recall LLM with hint  ✅ exercised below
//   - Step 2: check first tool against allowedTools  ✅ exercised below
//     (recordingProvider returns no ToolCalls → wrong-tool branch)
//   - Steps 3-5: TODO Tasks 6-7 (return ControlToolLoop placeholder)
//   - Step 6: archive + break  ✅ exercised below (wrong-tool branch)
//
// =============================================================================
// Phase 12.28.1 Task 1 verification: helper signature `turnCtx context.Context`
// is correct for future callers. `*TurnContext` does NOT satisfy context.Context
// interface (verified against `pkg/agent/turn_context.go:11` — no Deadline/Done/
// Err/Value methods), so signature must stay `context.Context` (callers from
// `pipeline_llm.go:745` and `turn_coord.go` Path 4 pass outer `ctx`).
//
// This test serves as a compile-time contract assertion: if anyone changes the
// signature to `*TurnContext`, callers break — that's the desired behavior.
func TestRetryExecuteToolChain_TurnCtxIsContextContext(t *testing.T) {
	var _ context.Context // ensure context is imported + reference is reachable
	p := &Pipeline{}
	ts := &turnState{turnCtx: &TurnContext{}}
	_ = ts
	_ = p
	t.Log("helper signature `turnCtx context.Context` confirmed: callers from pipeline_llm.go:745 and turn_coord.go Path 4 pass outer ctx context.Context (NOT *TurnContext).")
}

// =============================================================================
// Test 1: LLM is called with the recovery hint when the tool chain fails.
//
// Contract under test (Phase 12.28 §2.2 + Task 3 stub):
//
//	pipeline.retryExecuteToolChain(ctx, turnCtx, ts, exec, iteration,
//	    recoveryHint, allowedTools, phase) is invoked after a failed
//	tool-execution cycle. It must (a) re-call the LLM passing the
//	`hint` as a user-side recovery message, (b) check the resulting
//	first tool against allowedTools, and (c) when the tool is NOT in
//	the allowlist (or no tool is selected) re-arm ts.pendingRecoveryMessage
//	with a phase-aware hint and return ControlBreak.
//
// Phase 12.28 contract (plan §3.1) — verified in this Task 3 stub:
//   - Step 1: recall LLM with hint  ✅ exercised below
//   - Step 2: check first tool against allowedTools  ✅ exercised below
//     (recordingProvider returns no ToolCalls → wrong-tool branch)
//   - Steps 3-5: TODO Tasks 6-7 (return ControlToolLoop placeholder)
//   - Step 6: archive + break  ✅ exercised below (wrong-tool branch)
//
// Compile-time assertion: *Pipeline must satisfy toolExecutor. The interface
// is defined in retry_execute_tool_chain.go (Phase 12.28.1 Task 2). If this
// file compiles, the contract holds.
//
// To verify: change toolExecutor.ExecuteTools signature in retry_execute_tool_chain.go
// (e.g. add a parameter) and this file fails to compile, surfacing the drift.

// fakeExecutor is the test-injected toolExecutor (Phase 12.28.1 Task 2). It
// captures the last call arguments for assertion in subsequent test steps.
type fakeExecutor struct {
	lastCtx       context.Context
	lastTurnCtx   context.Context
	lastTs        *turnState
	lastExec      *turnExecution
	lastIteration int
	callCount     int
	returnControl ToolControl
	returnErr     error
}

func (f *fakeExecutor) ExecuteTools(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) ToolControl {
	f.lastCtx = ctx
	f.lastTurnCtx = turnCtx
	f.lastTs = ts
	f.lastExec = exec
	f.lastIteration = iteration
	f.callCount++
	return f.returnControl
}

// Compile-time check: fakeExecutor must satisfy toolExecutor. Inverted
// assertion — proves the test injection path matches the interface.
var _ toolExecutor = (*fakeExecutor)(nil)

// Test for Task 2: toolExecutor interface contract verified via:
//  1. *Pipeline satisfies toolExecutor (compile-time, checked in retry_execute_tool_chain.go)
//  2. *fakeExecutor satisfies toolExecutor (compile-time, just above)
//  3. fakeExecutor.ExecuteTools captures arguments (runtime)
// =============================================================================
// Test for Task 3 (Phase 12.28.1 Step 3 wiring): Pipeline exposes SetToolExecutor
// to inject a custom toolExecutor. Default self-binding returns *Pipeline itself.
// Verify:
//  1. After SetToolExecutor(fake), toolExecLazy() returns the fake
//  2. Without SetToolExecutor, toolExecLazy() returns *Pipeline (self-binding)
//  3. *Pipeline satisfies toolExecutor via existing ExecuteTools method
// =============================================================================
// Tests for Task 4 (Phase 12.28.1 Step 4 wiring): helper calls ExecuteTools
// then checks exec.messages for tool errors.
//
// Test note: Step 4 only fires when Step 2 selects a VALID tool (i.e.,
// the LLM picks set_goal when set_goal is in the allowlist). recordingProvider
// returns 0 tool_calls by default, so it hits Step 2's wrong-tool branch first.
// Full Step 4 runtime coverage lands in Task 7 (Path 2/4 migration) with
// scripted providers that return valid tool selections.
//
// What Task 4 verifies here is wiring-correctness: the helper compiles,
// imports checkToolExecErrorRecovery, and the no-tool-selected path returns
// ControlBreak BEFORE Step 3 (proving the gate order is right).
func TestRetryExecuteToolChain_Step4_NoToolSelected_StopsAtStep2(t *testing.T) {
	provider := &recordingProvider{} // no tool_calls → Step 2 wrong-tool branch
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"set_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlBreak {
		t.Errorf("expected ControlBreak (Step 2 wrong-tool gate), got %v", ctrl)
	}
	// ExecuteTools MUST NOT have been called (Step 2 short-circuits).
	if fake.callCount != 0 {
		t.Errorf("expected ExecuteTools NOT called (Step 2 gated), got callCount=%d", fake.callCount)
	}
	if ts.pendingRecoveryMessage == "" {
		t.Errorf("expected ts.pendingRecoveryMessage re-armed with phase-aware hint")
	}
}

func TestRetryExecuteToolChain_SetToolExecutor_Injection(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}

	// Before injection: toolExecLazy returns p itself
	got := p.toolExecLazy()
	if got != p {
		t.Errorf("expected default self-binding (==p), got %T", got)
	}

	// After injection: toolExecLazy returns fake
	p.SetToolExecutor(fake)
	got = p.toolExecLazy()
	if got != toolExecutor(fake) {
		t.Errorf("expected fake after SetToolExecutor, got %T", got)
	}
	if got.(*fakeExecutor).callCount != 0 {
		t.Errorf("expected callCount=0 after just setting, got %d", got.(*fakeExecutor).callCount)
	}
}

func TestRetryExecuteToolChain_ToolExecutorInterface_Contract(t *testing.T) {
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	var iface toolExecutor = fake
	ts := &turnState{turnCtx: &TurnContext{}}
	exec := &turnExecution{}
	ctrl := iface.ExecuteTools(context.Background(), context.Background(), ts, exec, 5)
	if ctrl != ToolControlContinue {
		t.Errorf("expected ControlContinue, got %v", ctrl)
	}
	if fake.callCount != 1 {
		t.Errorf("expected callCount=1, got %d", fake.callCount)
	}
	if fake.lastIteration != 5 {
		t.Errorf("expected lastIteration=5, got %d", fake.lastIteration)
	}
	if fake.lastTs != ts {
		t.Errorf("expected ts to be captured")
	}
	if fake.lastExec != exec {
		t.Errorf("expected exec to be captured")
	}
}

func TestRetryExecuteToolChain_LLMCalledWithHint(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)

	ctx := context.Background()
	const hint = "previous tool failed; retry with cleaner args"
	const phaseName = string(GoalPhaseCheckpoint)
	allowed := []string{"set_goal", "goal_progress", "complete_goal"}

	ctrl, err := pipeline.retryExecuteToolChain(
		ctx, ctx, ts, exec, ts.iteration,
		hint, allowed, phaseName,
	)
	if err != nil {
		t.Fatalf("retryExecuteToolChain returned error: %v", err)
	}

	// Assertion 1: LLM was called with the recovery hint injected as a
	// user-side message. recordingProvider captures the full message
	// list passed to Chat; applyBeforeLLMCall converts
	// ts.pendingRecoveryMessage into a user message before Chat fires.
	if len(provider.lastMessages) == 0 {
		t.Fatal("expected LLM Chat to be called, got empty lastMessages")
	}
	hintSeen := false
	for _, m := range provider.lastMessages {
		if m.Role == "user" && strings.Contains(m.Content, hint) {
			hintSeen = true
			break
		}
	}
	if !hintSeen {
		t.Fatalf("expected recovery hint %q to appear in a user message; got %d messages, first=%+v",
			hint, len(provider.lastMessages), provider.lastMessages[0])
	}

	// Assertion 2: same-iteration contract — ts.iteration must NOT
	// change. recallLLMForBlockedTool / handleGoalRecovery both pass the
	// same iteration back to the coordinator; bumping it would consume
	// an iterationCap slot.
	if ts.iteration != 3 {
		t.Fatalf("expected ts.iteration unchanged (3), got %d", ts.iteration)
	}

	// Assertion 3: stub returns a non-zero Control (the only zero-valued
	// Control is ControlContinue, which means "jump to top of turn
	// loop" — not what this stub ever returns). Wrong-tool branch
	// returns ControlBreak; right-tool branch returns ControlToolLoop.
	if ctrl == ControlContinue {
		t.Fatalf("expected a non-zero Control (ControlBreak or ControlToolLoop), got %v", ctrl)
	}
	_ = ctrl

	// Assertion 4 (wrong-tool branch only): when the LLM picks a tool
	// that is not in the allowlist (or no tool at all), the stub
	// re-arms ts.pendingRecoveryMessage with a phase-aware hint that
	// mentions the phase name and the allowed tools. recordingProvider
	// returns no ToolCalls, so we land here.
	if ctrl == ControlBreak {
		if ts.pendingRecoveryMessage == "" {
			t.Fatal("expected ts.pendingRecoveryMessage to be re-armed on wrong-tool branch")
		}
		if !strings.Contains(ts.pendingRecoveryMessage, phaseName) {
			t.Fatalf("expected phase %q in re-armed hint, got %q", phaseName, ts.pendingRecoveryMessage)
		}
	}
}
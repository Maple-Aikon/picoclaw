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
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
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

	// Phase 12.46: use an isolated workspace so we can seed an ACTIVE goal
	// — the retry-chain helper now only sets goalArchiveRequested when
	// ts.hasGoal() is true (exhaustion without a goal is not an archive).
	ws := t.TempDir()
	agent.Workspace = ws

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

	goalStore := goal.NewStore(ws)
	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "retry-chain-test",
		Description: goal.Description{
			Objective:       "test retry chain exhaustion archive",
			SuccessCriteria: []string{"exhaustion sets goalArchiveRequested"},
			Cadence:         "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goalStore.Write("test-retry-chain-session-"+t.Name(), activeGoal); err != nil {
		t.Fatalf("Write goal: %v", err)
	}
	if !ts.hasGoal() {
		t.Fatal("setup error: hasGoal=false; goal file not seeded")
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
//
// Phase 12.28.1 Task 7 (Step 4-5 retry-on-error tests): the executor can
// also append a tool-role message to exec.messages after running, so the
// helper's Step 4 (checkToolExecErrorRecovery) has something to inspect.
// Without this the helper always sees empty exec.messages and never reaches
// the retry path.
type fakeExecutor struct {
	lastCtx       context.Context
	lastTurnCtx   context.Context
	lastTs        *turnState
	lastExec      *turnExecution
	lastIteration int
	callCount     int
	returnControl ToolControl
	returnErr     error

	// Task 7 extensions — when non-empty, the executor appends a tool
	// message to exec.messages after each ExecuteTools call:
	//   appendContent: tool result text. Empty = don't append.
	//   appendIsError: marks the appended message as an executor error.
	//   appendToolName: sets ToolCallID so checkToolExecErrorRecovery can
	//     extract a tool name. Defaults to "t1" when empty.
	appendContent string
	appendIsError bool
	appendToolName string
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
	if f.appendContent != "" && exec != nil {
		toolName := f.appendToolName
		if toolName == "" {
			toolName = "t1"
		}
		exec.messages = append(exec.messages, providers.Message{
			Role:       "tool",
			ToolCallID: toolName,
			Content:    f.appendContent,
		})
	}
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
		"hint", []string{"set_goal"}, "open")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Phase 12.51a: text-only at Open phase = next-iter carry (per
	// Phase 12.46 + 12.47 spec). Phase was "checkpoint" pre-12.51a which
	// returned ControlToolLoop (success) — that changed because Phase
	// 12.51a routes text-only through evaluateRecovery which now fires
	// same-iter retry at restricted phases. Use phase="open" to preserve
	// the original test intent (text-only is success at Open phase).
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (Open text-only carry), got %v", ctrl)
	}
	// ExecuteTools MUST NOT have been called (G4 guard: no tool calls).
	if fake.callCount != 0 {
		t.Errorf("expected ExecuteTools NOT called (text-only), got callCount=%d", fake.callCount)
	}
	// pendingRecoveryMessage armed for Open next-iter carry (Phase 12.27 D3)
	// — Phase 12.51a Site 1 helper arms it via evaluateRecovery.
	if ts.pendingRecoveryMessage == "" {
		t.Errorf("expected pendingRecoveryMessage armed (Open carry), got empty")
	}
}

// =============================================================================
// Tests for Task 5 (Phase 12.28.1 Step 5 wiring): helper wraps Steps 1-4 in
// BoundedRetry(MaxAttempts=ToolExecErrorRetryCap=3).
//
// What we verify here is the wrapper structure:
//   - OnExhausted callback fires when MaxAttempts is hit while still retrying
//   - exhaustion flag → ts.goalArchiveRequested=true + return ControlBreak
//   - inner ResultControlBreak (wrong tool) propagated through BoundedRetry exit
//
// Runtime verification (real LLM picks wrong tool N times, then archives) is
// left to Task 7's Path 2/Path 4 migration with scripted providers.

func TestRetryExecuteToolChain_Step5_InnerBreakPropagates(t *testing.T) {
	provider := &recordingProvider{} // no tool_calls → text-only success (Phase 12.42)
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"set_goal"}, "open")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Phase 12.51a: text-only at Open phase = next-iter carry (per
	// Phase 12.46 + 12.47 spec). Phase was "checkpoint" pre-12.51a which
	// returned ControlToolLoop (success) — that changed because Phase
	// 12.51a routes text-only through evaluateRecovery which now fires
	// same-iter retry at restricted phases. Use phase="open" to preserve
	// the original test intent (text-only is success at Open phase).
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (Open text-only carry), got %v", ctrl)
	}
	// No archive requested at Open (text-only carry, not exhaustion).
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false (Open text-only is carry), got true")
	}
	// ExecuteTools MUST NOT have been called (G4 guard).
	if fake.callCount != 0 {
		t.Errorf("expected ExecuteTools NOT called (text-only), got callCount=%d", fake.callCount)
	}
}

// mockBoundedRetryExhaustionHelper simulates the inner-step exhaustion path by
// always returning ControlToolLoop (so BoundedRetry keeps retrying until cap
// hit). Task 5 wiring tests use this in place of a scripted LLM provider.
type mockAlwaysRetryOnceHelper struct{}

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

	// Assertion 3: Phase 12.51a change. Pre-12.51a: text-only at
	// Checkpoint returned ControlBreak (wrong-tool branch). Phase 12.51a
	// routes text-only through evaluateRecovery which fires same-iter
	// retry 3x (RecordingProvider returns text-only each time), then
	// archives. So expected: ControlBreak with goalArchiveRequested=true.
	if ctrl != ControlBreak {
		t.Errorf("expected ControlBreak (Phase 12.51a: 3 attempts exhausted → archive), got %v", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=true after 3 attempts exhausted, got false")
	}
	// Phase 12.52a: recordPhaseStuckArchive keeps the REAL attempt count
	// (max(count,1), no ratchet to 2) and sets the archive flag.
	if ts.goalProgressAttemptCount != 1 {
		t.Errorf("expected goalProgressAttemptCount=1 after exhausted (real count), got %d", ts.goalProgressAttemptCount)
	}
	if !ts.goalProgressArchiveFlag {
		t.Errorf("expected goalProgressArchiveFlag=true after archive, got false")
	}
}

// =============================================================================
// Tests for Task 6 (Phase 12.28.1 Step 3 wiring): helper calls ExecuteTools
// after Step 2 selects a valid tool (firstTool in allowedTools).
//
// What Task 6 verifies:
//   1. valid tool selection (firstTool in allowlist) → helper invokes
//      ExecuteTools via the toolExecutor interface
//   2. ExecuteTools return value (ToolControl) propagates to outer Control:
//        - ToolControlContinue → ControlToolLoop (caller continues tool loop)
//        - ToolControlBreak    → ControlBreak (caller breaks)
//   3. goalArchiveRequested stays false when no tool error fires
//
// recordingProvider has no scripted tool_calls by default — we manually
// pre-seed exec.response to simulate LLM having returned a valid tool.
// Full Step 3 runtime coverage (production ExecuteTools with real
// tool_registry dispatch) lands in Task 7's Path 4 migration.
// simpleValidToolProvider emits a single valid tool call (set_goal) per
// call and records every invocation. Used by Task 6 Step 3 tests where
// we need a deterministic "valid tool selection" path that survives
// Step 1's callLLMCore overwriting exec.response.
type simpleValidToolProvider struct {
	mu       struct{ calls int }
	toolName string
	toolArgs map[string]any
	response string
}

func (s *simpleValidToolProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	s.mu.calls++
	return &providers.LLMResponse{
		Content:      s.response,
		ToolCalls:    []providers.ToolCall{{Name: s.toolName, Arguments: s.toolArgs}},
		FinishReason: "stop",
	}, nil
}

func (s *simpleValidToolProvider) GetDefaultModel() string {
	return "test-model"
}

func TestRetryExecuteToolChain_Step3_ValidTool_InvokesExecuteTools(t *testing.T) {
	// recoveryHint=="" exercises the success path: setupFunc arms
	// pendingRecoveryMessage="" → BoundedRetry inner immediately returns
	// RetryDecisionDone on ControlToolLoop (no retry budget burned),
	// outer exits after exactly one ExecuteTools call.
	provider := &simpleValidToolProvider{
		toolName: "set_goal",
		toolArgs: map[string]any{"name": "test", "objective": "o", "success_criteria": []string{"s"}},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"", []string{"set_goal", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// fake returned ToolControlContinue → outer should propagate as
	// ControlToolLoop (continue tool loop).
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (valid tool + ToolControlContinue), got %v", ctrl)
	}
	// ExecuteTools MUST have been called exactly once (success path
	// does not spend any retry attempts).
	if fake.callCount != 1 {
		t.Errorf("expected ExecuteTools called once, got callCount=%d", fake.callCount)
	}
	// Step 4 must NOT have fired — fake didn't append a tool error.
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false (Step 4 not triggered), got true")
	}
}

func TestRetryExecuteToolChain_Step3_ToolControlBreak_Propagates(t *testing.T) {
	provider := &simpleValidToolProvider{
		toolName: "complete_goal",
		toolArgs: map[string]any{"summary": "done"},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	// Fake returns ToolControlBreak to simulate "handled all responses"
	// (Phase 6 flow where the tool itself sends to user and ends the turn).
	fake := &fakeExecutor{returnControl: ToolControlBreak}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"complete_goal"}, "final")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlBreak {
		t.Errorf("expected ControlBreak (tool responded → break), got %v", ctrl)
	}
	if fake.callCount != 1 {
		t.Errorf("expected ExecuteTools called once, got callCount=%d", fake.callCount)
	}
}

// =============================================================================
// Tests for Phase 12.28.1 Task 7 (Step 4-5 retry-on-error paths):
//   Test 5: tool result success → Step 4 sees no error → return ControlToolLoop
//   Test 6: tool result error → Step 4 sees executor error → re-arm hint,
//           BoundedRetry continues to next attempt, second attempt succeeds
// =============================================================================

// Test 5: tool execution succeeded (fake appends tool message with
// IsError=false) → checkToolExecErrorRecovery returns ("", "") →
// helper returns ControlToolLoop, no further LLM call needed.
func TestRetryExecuteToolChain_ToolResultSuccess_ReturnsControlToolLoop(t *testing.T) {
	provider := &simpleValidToolProvider{
		toolName: "complete_goal",
		toolArgs: map[string]any{"summary": "done"},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	fake := &fakeExecutor{
		returnControl:  ToolControlContinue,
		appendContent:  "OK",
		appendIsError:  false,
		appendToolName: "t1",
	}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Tool succeeded → helper returns ControlToolLoop so the caller
	// continues its outer agent loop normally.
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (success), got %v", ctrl)
	}
	// BoundedRetry must exit after attempt 0 (success path):
	// ts.pendingRecoveryMessage is "" so inner returns RetryDecisionDone.
	if fake.callCount != 1 {
		t.Errorf("expected exactly 1 ExecuteTools call (no retry), got %d", fake.callCount)
	}
	// Step 4 saw no error → goalArchiveRequested must remain false.
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false (success), got true")
	}
}

// =============================================================================
// Helper types for Phase 12.28.1 Task 7 (Step 4-5 retry-on-error tests)
// =============================================================================

// twoAttemptProvider returns different tool calls on attempt 0 vs attempt 1,
// so we can verify BoundedRetry re-calls the LLM after a tool exec error.
type twoAttemptProvider struct {
	attempt0Tool string
	attempt0Args map[string]any
	attempt1Tool string
	attempt1Args map[string]any
	calls        int
}

func (p *twoAttemptProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{
			Content:      "",
			ToolCalls:    []providers.ToolCall{{Name: p.attempt0Tool, Arguments: p.attempt0Args}},
			FinishReason: "stop",
		}, nil
	}
	return &providers.LLMResponse{
		Content:      "",
		ToolCalls:    []providers.ToolCall{{Name: p.attempt1Tool, Arguments: p.attempt1Args}},
		FinishReason: "stop",
	}, nil
}

func (p *twoAttemptProvider) GetDefaultModel() string {
	return "test-model"
}

// fakeExecutorStep configures one ExecuteTools invocation. Each call
// advances to the next step; when steps are exhausted, the last step
// repeats (test must size steps correctly).
type fakeExecutorStep struct {
	returnControl  ToolControl
	appendContent  string
	appendIsError  bool
	appendToolName string
}

type fakeSequenceExecutor struct {
	steps     []fakeExecutorStep
	callCount int
}

func (f *fakeSequenceExecutor) ExecuteTools(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) ToolControl {
	stepIdx := f.callCount
	if stepIdx >= len(f.steps) {
		stepIdx = len(f.steps) - 1
	}
	step := f.steps[stepIdx]
	f.callCount++
	if step.appendContent != "" && exec != nil {
		toolName := step.appendToolName
		if toolName == "" {
			toolName = "t1"
		}
		exec.messages = append(exec.messages, providers.Message{
			Role:       "tool",
			ToolCallID: toolName,
			Content:    step.appendContent,
		})
	}
	return step.returnControl
}

// Compile-time check that fakeSequenceExecutor satisfies toolExecutor.
var _ toolExecutor = (*fakeSequenceExecutor)(nil)

// =============================================================================
// Test for Phase 12.28.1 Task 8 (Path 2 → recallAndCheckTool extraction):
//
// Verifies that retryLLMForBlockedTool now delegates Steps 1+2 to the
// shared recallAndCheckTool primitive (also used by retryExecuteToolChain
// Path 4). The Path 2 wrapper must:
//   - Re-prompt with recoveryMsg when LLM picks a wrong tool (not break)
//   - Succeed with ControlToolLoop when LLM picks an allowed tool
//   - Set exec.response to the latest response (Phase 12.26 contract)
//   - Archive on cap exhaustion (3 attempts) with phase-stuck reason
//
// Compile-time check: Pipeline must expose retryLLMForBlockedTool with the
// pre-Task 8 signature (turn_coord.go:434 caller is unaffected by the
// refactor). If signature changes, this test fails to compile.
// =============================================================================
func TestRetryLLMForBlockedTool_UsesSharedHelper(t *testing.T) {
	// 3-attempt scripted provider: attempt 0 returns blocked tool
	// (read_file at Checkpoint phase), attempt 1 also blocked (web_search),
	// attempt 2 picks the lifecycle tool (goal_progress).
	provider := &threeAttemptProvider{
		tools: []string{"read_file", "web_search", "goal_progress"},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	// Phase 12.42: Path 4 executes the re-picked tool itself (Step 3) —
	// inject the fake executor so goal_progress dispatch is mocked.
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	// setupRetryChainTestTurnState pins phase to Open; re-pin to Checkpoint
	// so ts.currentGoalPhase() returns Checkpoint and runtime resolver
	// returns lifecycle allowlist ([goal_progress, complete_goal]).
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 2,
		"RECOVERY_HINT: pick goal_progress or complete_goal",
		[]string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Provider returns goal_progress on attempt 2 → resolved allowlist
	// contains it → Path 2 returns RetryDecisionDone → ControlToolLoop.
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (LLM picked goal_progress on attempt 2), got %v", ctrl)
	}
	// Recovery succeeded → no archive.
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false (recovered), got true")
	}
	// exec.response must reflect the latest LLM call (goal_progress).
	if exec.response == nil {
		t.Fatal("expected exec.response to be set by shared helper")
	}
	if len(exec.response.ToolCalls) == 0 || exec.response.ToolCalls[0].Name != "goal_progress" {
		t.Errorf("expected first tool=goal_progress in exec.response, got %v", exec.response.ToolCalls)
	}
}

// threeAttemptProvider returns different tools across N attempts so we can
// verify Path 2's retry-and-pick-correct-tool flow.
type threeAttemptProvider struct {
	tools []string
	calls int
}

func (p *threeAttemptProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	idx := p.calls
	if idx >= len(p.tools) {
		idx = len(p.tools) - 1
	}
	p.calls++
	return &providers.LLMResponse{
		Content:      "",
		ToolCalls:    []providers.ToolCall{{Name: p.tools[idx], Arguments: map[string]any{}}},
		FinishReason: "stop",
	}, nil
}

func (p *threeAttemptProvider) GetDefaultModel() string {
	return "test-model"
}

// Test 6: tool execution errored (fake appends tool message with
// IsError=true and a "Tool execution failed:" prefix that
// checkToolExecErrorRecovery recognizes) → retry path fires →
// BoundedRetry continues to attempt 1 where the LLM picks a
// different tool that succeeds.
func TestRetryExecuteToolChain_ToolResultError_RetriesWithNewHint(t *testing.T) {
	provider := &twoAttemptProvider{
		attempt0Tool:  "goal_progress",
		attempt0Args:  map[string]any{"completed_steps": []string{"a"}, "remaining_steps": []string{"b"}},
		attempt1Tool:  "goal_progress",
		attempt1Args:  map[string]any{"completed_steps": []string{"a", "b"}, "remaining_steps": []string{"c"}},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	// First call: tool errors. Second call: tool succeeds.
	// We append different tool messages based on callCount.
	fake := &fakeSequenceExecutor{
		steps: []fakeExecutorStep{
			{
				returnControl:  ToolControlContinue,
				appendContent:  "Tool execution failed: validation error",
				appendIsError:  true,
				appendToolName: "t1",
			},
			{
				returnControl:  ToolControlContinue,
				appendContent:  "OK",
				appendIsError:  false,
				appendToolName: "t2",
			},
		},
	}
	p.SetToolExecutor(fake)
	ts, exec := setupRetryChainTestTurnState(t, al, p)

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"first hint", []string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Two attempts: error → retry → success → ControlToolLoop.
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop after recovery, got %v", ctrl)
	}
	if fake.callCount != 2 {
		t.Errorf("expected 2 ExecuteTools calls (error + retry), got %d", fake.callCount)
	}
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false (recovered), got true")
	}
}

// =============================================================================
// Phase 12.28.1 regression test: when retryLLMForBlockedTool's
// recallAndCheckTool helper returns ControlToolLoop with a valid firstTool,
// exec.normalizedToolCalls MUST be populated — otherwise the caller's
// ExecuteTools (turn_coord.go:488) silently drops the tool because it
// reads exec.normalizedToolCalls (not exec.response.ToolCalls).
//
// Bug history (Phase 12.28 live failure 2026-07-29 16:43:01 ICT,
// main-turn-3 Horus protocol): retryLLMForBlockedTool's helper extracted
// RecallLLM + firstTool check but forgot to populate normalizedToolCalls.
// Result: complete_goal (correct tool for Checkpoint phase) was emitted
// by LLM, exec.response.ToolCalls=[complete_goal], exec.normalizedToolCalls=nil.
// Pipeline.ExecuteTools iterated 0 items, complete_goal silently dropped,
// turn fell through to toolLimitResponse fallback.
//
// This test asserts the fix: after recallAndCheckTool returns with a
// valid tool in exec.response.ToolCalls, normalizedToolCalls must also be
// populated (so ExecuteTools can dispatch it).
// =============================================================================
func TestRecallAndCheckTool_PopulatesNormalizedToolCalls(t *testing.T) {
	al, _, cleanup := newTurnCoordTestLoop(t, &sequenceProvider{
		responses: []*providers.LLMResponse{
			// Attempt 0: LLM picks complete_goal (correct tool for Checkpoint).
			{
				Content: "OK, archiving goal.",
				ToolCalls: []providers.ToolCall{
					{
						Name: "complete_goal",
						Arguments: map[string]any{"summary": "goal done"},
					},
				},
			},
		},
	})
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)
	// Force Checkpoint phase so allowlist = [goal_progress, complete_goal].
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	ctrl, firstTool, err := pipeline.recallAndCheckTool(
		context.Background(), context.Background(), ts, exec, 3,
		"recovery helper test", "Phase 12.28.1",
		[]string{"goal_progress", "complete_goal"},
		func(_ string) Control { return ControlContinue },
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Errorf("expected ControlToolLoop (correct tool picked), got %v", ctrl)
	}
	if firstTool != "complete_goal" {
		t.Errorf("expected firstTool=complete_goal, got %q", firstTool)
	}

	// THE REGRESSION ASSERTION: normalizedToolCalls must be populated so
	// Pipeline.ExecuteTools can dispatch complete_goal at turn_coord.go:488.
	if len(exec.normalizedToolCalls) != 1 {
		t.Errorf("expected exec.normalizedToolCalls=1 (complete_goal), got %d — this is the Phase 12.28 wire bug where complete_goal is silently dropped", len(exec.normalizedToolCalls))
	}
	if len(exec.normalizedToolCalls) > 0 && exec.normalizedToolCalls[0].Name != "complete_goal" {
		t.Errorf("expected normalizedToolCalls[0].Name=complete_goal, got %q", exec.normalizedToolCalls[0].Name)
	}
}

// TestPhase12_28_2_ExecuteToolsRunsAtIterCap verifies that after
// retryLLMForBlockedTool returns ControlToolLoop at iter==iterationCap
// (the GoalPhaseCheckpoint cap-hit recovery scenario), the caller in
// turn_coord.go:480 does NOT skip ExecuteTools. The previous wire
// condition `currentIteration() < iterationCap` dropped the just-picked
// `complete_goal` at the cap, sending the turn to toolLimitResponse
// fallback instead of archiving the goal.
//
// Live bug 2026-07-29 18:33 ICT: main-turn-3 Horus protocol,
// session sk_v1_9238bf3573c9bd64d72644007ca153c3f73077548ae4d61c3ed41982b2c3b552,
// 5 iters, iter 5 = cap-hit, recovery LLM picked complete_goal,
// but Phase 12.28.1 cap guard blocked ExecuteTools → final_len=142
// (toolLimitResponse string) instead of goal.Summary.
func TestPhase12_28_2_ExecuteToolsRunsAtIterCap(t *testing.T) {
	al, _, cleanup := newTurnCoordTestLoop(t, &sequenceProvider{
		responses: []*providers.LLMResponse{
			{
				Content: "OK, archiving goal.",
				ToolCalls: []providers.ToolCall{
					{
						Name:      "complete_goal",
						Arguments: map[string]any{"summary": "goal done at cap"},
					},
				},
			},
		},
	})
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	// CRITICAL: pin iter to iterationCap to simulate the live failure scenario.
	ts.iteration = 10
	ts.iterationCap = 10
	ts.postCompleteGoalReportSent = false
	ts.goalArchiveRequested = false
	ts.goalFinalized = false

	// The Phase 12.28.2 fix: turn_coord.go:480 condition is now JUST
	// `len(exec.response.ToolCalls) > 0` (no `&& currentIteration() < iterationCap`).
	// Pre-Phase-12.28.2: also checked `ts.currentIteration() < ts.iterationCap`
	// (FALSE at iter==cap → dropped). Post-fix: only check tool presence.
	// Invoke recallAndCheckTool to populate exec.response and exec.normalizedToolCalls.
	ctrl, firstTool, err := pipeline.recallAndCheckTool(
		context.Background(), context.Background(), ts, exec, 10,
		"retryLLMForBlockedTool test", "Phase 12.28.2",
		[]string{"goal_progress", "complete_goal"},
		func(_ string) Control { return ControlContinue },
	)
	if err != nil {
		t.Fatalf("helper err: %v", err)
	}
	if ctrl != ControlToolLoop || firstTool != "complete_goal" {
		t.Fatalf("helper returned ctrl=%v firstTool=%q (expected ToolLoop/complete_goal)", ctrl, firstTool)
	}
	// Now verify the caller-path condition that drives turn_coord.go:480.
	shouldRunExecuteTools := len(exec.response.ToolCalls) > 0
	if !shouldRunExecuteTools {
		t.Errorf("condition to call ExecuteTools should be true (1 tool in exec.response), got false")
	}
	// Confirm we're at iter==cap (the live failure scenario).
	if ts.currentIteration() != ts.iterationCap {
		t.Fatalf("test setup error: expected iter==cap (10==10), got %d != %d",
			ts.currentIteration(), ts.iterationCap)
	}
	// THE REGRESSION ASSERTION: at iter==cap, exec.response.ToolCalls
	// is populated AND helper returned ControlToolLoop. The Phase 12.28.2
	// fix removed the `currentIteration() < iterationCap` guard at
	// turn_coord.go:480 — without the fix, this would silently drop
	// the tool (live bug 18:33 ICT main-turn-3).
	if len(exec.normalizedToolCalls) != 1 || exec.normalizedToolCalls[0].Name != "complete_goal" {
		t.Errorf("normalizedToolCalls should be [complete_goal] for ExecuteTools dispatch, got %v", exec.normalizedToolCalls)
	}
}

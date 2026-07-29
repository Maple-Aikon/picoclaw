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
// This file is the test fixture (Task 2 of Phase 12.28). Task 3 implements
// the function; the test below is deliberately a stub that compiles only
// when retryExecuteToolChain is defined. Until Task 3 lands, `go test
// ./pkg/agent/ -run TestRetryExecuteToolChain_LLMCalledWithHint -v` will
// fail with: `undefined: retryExecuteToolChain`.
//
// Deviations from the plan §2.2 (with justification):
//
//   - Plan suggested `newHookTestLoop` to wire the fixture. That helper
//     returns (*AgentLoop, *AgentInstance, cleanup) and never constructs a
//     turnState. Phase 12.27's proven pattern (`setupRecallTestTurnState`
//     in pipeline_llm_recall_test.go) builds ts+exec directly via
//     `newTurnCoordTestLoop` + `NewPipeline(al)` + `newTurnState` +
//     `pipeline.SetupTurn`. We follow that pattern because it is what
//     already drives the RecallLLM direct-test suite — same shape, same
//     invariants, no extra helpers needed.
//
//   - Plan referenced `ts.agent.Pipeline` (Pipeline field on AgentInstance).
//     AgentInstance has no Pipeline field. The Pipeline is constructed
//     separately via `NewPipeline(al)` and passed explicitly — same as
//     in `pipeline_llm_recall_test.go` and `pipeline_llm_recovery_test.go`.
//
//   - Plan referenced `exec.AttachMockPipeline`. No such helper exists.
//     The Pipeline that retryExecuteToolChain will eventually be a method
//     of (per Phase 12.28 spec) is the same `pipeline` we build here via
//     `NewPipeline(al)`. For the stub test we don't need to attach it;
//     the test only exercises the function signature, not the body.
//
//   - `retryExecuteToolChain` is declared in the plan §2.2 as a free
//     function (not a Pipeline method) taking (ts, exec, hint). Task 3
//     will resolve the final receiver; the stub below only references it
//     by that name so the compile error is unambiguous.
package agent

import (
	"context"
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
// Contract under test (Phase 12.28 §2.2):
//
//   retryExecuteToolChain(ts, exec, hint) is invoked after a failed
//   tool-execution cycle. It must (a) re-call the LLM passing the
//   `hint` as a system-side message, (b) re-execute the resulting tool
//   call(s) within the same iteration, and (c) return a ControlBreak
//   so the coordinator loop re-enters proceedPastLLM with a fresh
//   tool result.
//
// This is the FIRST test for the new function. Task 3 implements the
// body; until then this test fails to COMPILE with:
//
//   ./retry_execute_tool_chain_test.go: undefined: retryExecuteToolChain
//
// which is the desired "first failing test" for TDD red phase.
// =============================================================================
func TestRetryExecuteToolChain_LLMCalledWithHint(t *testing.T) {
	// Minimal provider — the stub below does not exercise Chat yet
	// (the function under test doesn't exist), but we wire a real
	// provider so Task 3's implementation can use it without changing
	// the fixture.
	provider := &simpleConvProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)

	// The failing call. Task 3 will define this function. The exact
	// signature is captured in plan §2.2: retryExecuteToolChain takes
	// (ts, exec, hint) and returns a ControlBreak. Provider and
	// provider-ctx are intentionally omitted from the stub call —
	// Task 3 may fold them into ts/exec or pass them explicitly; the
	// test will be tightened then.
	//
	// Until the function exists, the line below triggers the compile
	// error that proves the test is genuinely red.
	_ = retryExecuteToolChain(ts, exec, "previous tool failed; retry with cleaner args")

	// Assertion stub. Task 3 will fill in:
	//   - verify provider.Chat received a system message containing
	//     the hint string,
	//   - verify ts.iteration did NOT bump (same-iteration contract),
	//   - verify the returned ControlBreak re-enters the coordinator.
	//
	// We don't assert anything here yet — the function doesn't exist.
	// The compile error alone is the failing-test signal.
	_ = ts
	_ = exec
	_ = pipeline
}
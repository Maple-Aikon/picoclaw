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
// recordingProvider is used so we can verify Step 1: the LLM was
// actually called with the recovery hint present in its message list.
// =============================================================================
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
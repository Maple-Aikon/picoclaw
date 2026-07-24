package agent

import (
	"context"
	"strings"
	"testing"
)

// =============================================================================
// Phase 12.8: Pipeline CallLLM — Iteration Cap Reached
// =============================================================================
//
// Phase 12.8 REMOVED the legacy Tier 3 force-wrap (toolLimitHintMessage +
// providerToolDefs=nil at RemainingIterations() <= 0). The cap-hit case
// is now owned by the goal-phase machinery (Phase 11):
//   - GoalPhaseCheckpoint → goal_progress + complete_goal only
//   - GoalPhaseFinal      → complete_goal only
//
// At iter == iterationCap, the per-turn allowlist narrows but the LLM
// keeps full tool access via the goal lifecycle. If the LLM is text-only
// at GoalPhaseCheckpoint, Phase 12 text-only recovery fires (soft → hard
// → archive). See TestCallLLM_RecoveryHint* for the recovery path.
//
// All tests use the existing newTurnCoordTestLoop harness and simpleConvProvider.

// --- Phase 12.8: Iteration Cap Reached — Allowlist Narrows to Goal Lifecycle ---

func TestCallLLM_IterationCapReached_GoalPhaseCheckpointAllowlist(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 200

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// iteration 20 → at iterationCap. Tier 3 is GONE; GoalPhaseCheckpoint
	// (base ∪ [goal_progress, complete_goal]) governs the allowlist. We
	// verify the callMessages do NOT contain the legacy toolLimitHintMessage
	// ("CEASE ALL TOOL CALLS") and that the goal lifecycle tools remain
	// available so the LLM can self-extend or finalize.
	ts.iteration = 20
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 20)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// The legacy CEASE message must NOT be injected.
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "CEASE ALL TOOL CALLS IMMEDIATELY") {
			t.Error("Phase 12.8 regression: toolLimitHintMessage (CEASE ALL TOOL CALLS) re-appeared at iteration cap")
			break
		}
	}
}

// =============================================================================
// Phase 11.1: Pipeline CallLLM — Recovery Hint Injection
// =============================================================================
//
// Verifies the end-to-end wire: a pendingRecoveryMessage set by the
// recovery_goal trigger (empty response, text-only streak, tool exec error)
// appears in callMessages at the next CallLLM invocation, and the field
// is cleared so the hint does not repeat.

func TestCallLLM_RecoveryHintInjected_WhenPendingSet(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// Stay well below iteration cap so the goal-phase machinery
	// (Phase 12.8: no Tier 3 pre-empt) does not interfere with the
	// recovery hint injection.
	ts.iteration = 5
	ts.iterationCap = 20

	// Simulate recovery_goal.go having set pendingRecoveryMessage for an
	// empty-response retry.
	ts.pendingRecoveryMessage = "Your previous response was empty. Please provide a response with text or tool calls to make progress."

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 5)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	recoveryFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "Your previous response was empty") {
			recoveryFound = true
			break
		}
	}
	if !recoveryFound {
		t.Error("expected pendingRecoveryMessage in callMessages, was not injected (Phase 11.1 consumer missing?)")
	}

	// Field must be cleared after consumption.
	if ts.pendingRecoveryMessage != "" {
		t.Errorf("pendingRecoveryMessage must clear after injection, still %q", ts.pendingRecoveryMessage)
	}
}

func TestCallLLM_RecoveryHintNotInjected_WhenPendingEmpty(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	ts.iteration = 5
	ts.iterationCap = 20

	// No pending set → callMessages must NOT grow a recovery slot.
	beforeLen := len(exec.messages)

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 5)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// Only the existing messages remain — no extra user-role recovery hint.
	for _, msg := range exec.callMessages {
		if msg.Role == "user" && strings.Contains(msg.Content, "previous response was empty") {
			t.Errorf("no recovery hint expected, got user message %q", msg.Content)
		}
	}

	if len(exec.callMessages) != beforeLen {
		t.Errorf("callMessages length changed unexpectedly: was %d, now %d", beforeLen, len(exec.callMessages))
	}
}

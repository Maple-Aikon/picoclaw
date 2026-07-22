package agent

import (
	"context"
	"strings"
	"testing"
)

// =============================================================================
// Phase 10: Pipeline CallLLM — Tier 3 (Iteration Cap) Behavior Tests
// =============================================================================
//
// Phase 10 removed extend_turn_iteration tool and the three-tier windowed
// hint logic. The only remaining iteration-cap signal is Tier 3: when
// iteration >= iterationCap, the LLM is forced to wrap up — we inject
// toolLimitHintMessage and strip all tool definitions.
//
// All tests use the existing newTurnCoordTestLoop harness and simpleConvProvider.

// --- Tier 3: Iteration Cap Reached ---

func TestCallLLM_IterationCapReached_AllToolsStripped(t *testing.T) {
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

	// iteration 20 → remaining = 0 → Tier 3 fires
	ts.iteration = 20
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 20)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// Verify tool-limit message
	limitFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "CEASE ALL TOOL CALLS IMMEDIATELY") {
			limitFound = true
			break
		}
	}
	if !limitFound {
		t.Error("expected toolLimitHintMessage (CEASE ALL TOOL CALLS) in callMessages at iteration cap")
	}

	// Verify NO tools are available
	if len(exec.providerToolDefs) != 0 {
		t.Errorf("expected providerToolDefs to be empty at Tier 3, got %d defs", len(exec.providerToolDefs))
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

	// Stay well below iteration cap so Tier 3 doesn't fire and pre-empt
	// the recovery hint with its own CEASE directive.
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

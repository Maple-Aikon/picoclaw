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

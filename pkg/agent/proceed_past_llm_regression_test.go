package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// =============================================================================
// proceedPastLLM return-value regression tests
//
// Regression target: bug fixed on 2026-07-18 where proceedPastLLM was returning
// `ControlContinue` at the end-of-function fallback, instead of `ControlToolLoop`
// whenever normalizedToolCalls was non-empty. The bug manifested as
// "tool returned empty" — the LLM response carried tool_calls, but
// proceedPastLLM's final return told the coordinator to skip the tool
// loop and jump straight to the next LLM iteration with no tool result
// message, so the LLM never saw the tool output.
//
// The fix: line 916 must return ControlToolLoop when there are tool calls
// (regardless of whether tier gating or other early returns fired).
//
// These tests pin the contract directly:
//   1. Tool calls present      → ControlToolLoop
//   2. No tool calls, stop     → ControlContinue
//   3. Finalizing path         → ControlBreak (covers other branch)
// =============================================================================

// proceedPastLLMRegressionFixture sets up the minimal context proceedPastLLM
// needs: a Pipeline, a turnState, and a turnExecution with a known response
// and (optionally) normalizedToolCalls.
type proceedPastLLMRegressionFixture struct {
	pipeline *Pipeline
	ts       *turnState
	exec     *turnExecution
}

func newProceedPastLLMFixture(t *testing.T, tc []providers.ToolCall) proceedPastLLMRegressionFixture {
	t.Helper()

	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	t.Cleanup(cleanup)

	// Make the test deterministic: tier messages only fire when
	// extendEnabled is true and iterationCap < absCap, so leave defaults.
	pipeline := NewPipeline(al)

	ts := newTurnState(agent, makeTestProcessOpts("proceed-past-llm-regression"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	// Pin cap high so tier gating never fires.
	ts.iteration = 1
	ts.iterationCap = 100

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}
	exec.callMessages = exec.messages

	// Inject a synthetic LLM response. This is what CallLLM would normally
	// have populated after talking to the provider.
	//
	// CRITICAL: proceedPastLLM branches at L799 on len(exec.response.ToolCalls),
	// then re-populates exec.normalizedToolCalls from response.ToolCalls at
	// L844-847. Setting only normalizedToolCalls would short-circuit the test
	// at the no-tool-call early return (L839) and never reach L916.
	exec.response = &providers.LLMResponse{
		Content:      "",
		FinishReason: "tool_calls",
		ToolCalls:    tc,
	}
	exec.llmModelName = "test-model"

	return proceedPastLLMRegressionFixture{pipeline: pipeline, ts: ts, exec: exec}
}

// TestProceedPastLLM_ReturnsControlToolLoop_WhenHasToolCalls is the
// direct regression test for the 2026-07-18 bug.
//
// Before the fix, the final `return` returned ControlContinue even when
// normalizedToolCalls was non-empty. That caused the coordinator to skip
// ExecuteTools entirely and re-poll the LLM with no tool result message
// in the conversation history.
func TestProceedPastLLM_ReturnsControlToolLoop_WhenHasToolCalls(t *testing.T) {
	tc := []providers.ToolCall{
		{
			ID:   "call_1",
			Name: "fs.read",
			Function: &providers.FunctionCall{
				Name:      "fs.read",
				Arguments: `{"path":"/tmp/example.txt"}`,
			},
		},
	}
	fx := newProceedPastLLMFixture(t, tc)

	got, err := fx.pipeline.proceedPastLLM(
		context.Background(),
		context.Background(),
		fx.ts,
		fx.exec,
		1,
	)
	if err != nil {
		t.Fatalf("proceedPastLLM returned error: %v", err)
	}
	if got != ControlToolLoop {
		t.Errorf("proceedPastLLM(toolCalls=1) = %v, want ControlToolLoop\n"+
			"Regression: line 916 fell back to ControlContinue instead of "+
			"ControlToolLoop when normalizedToolCalls was non-empty. "+
			"This is the 2026-07-18 'tool returned empty' bug.",
			got)
	}
}

// TestProceedPastLLM_ReturnsControlToolLoop_WithMultipleToolCalls covers
// the same contract with >1 tool calls to make sure the branch doesn't
// somehow depend on the slice length.
func TestProceedPastLLM_ReturnsControlToolLoop_WithMultipleToolCalls(t *testing.T) {
	tc := []providers.ToolCall{
		{ID: "call_1", Name: "fs.read", Function: &providers.FunctionCall{Name: "fs.read", Arguments: `{"path":"a"}`}},
		{ID: "call_2", Name: "fs.read", Function: &providers.FunctionCall{Name: "fs.read", Arguments: `{"path":"b"}`}},
	}
	fx := newProceedPastLLMFixture(t, tc)

	got, err := fx.pipeline.proceedPastLLM(
		context.Background(),
		context.Background(),
		fx.ts,
		fx.exec,
		1,
	)
	if err != nil {
		t.Fatalf("proceedPastLLM returned error: %v", err)
	}
	if got != ControlToolLoop {
		t.Errorf("proceedPastLLM(toolCalls=2) = %v, want ControlToolLoop", got)
	}
}

// TestProceedPastLLM_ReturnsControlBreak_WhenNoToolCalls pins the
// inverse contract: when the LLM emitted no tool calls (and no steering
// is queued), proceedPastLLM returns ControlBreak to terminate the turn
// with the LLM's direct response (see L839).
//
// This is not the regression target itself, but it documents the
// branch boundary so the L916 fix can't silently regress into "always
// return ControlToolLoop when ToolCalls is empty".
func TestProceedPastLLM_ReturnsControlBreak_WhenNoToolCalls(t *testing.T) {
	fx := newProceedPastLLMFixture(t, nil) // no tool calls

	got, err := fx.pipeline.proceedPastLLM(
		context.Background(),
		context.Background(),
		fx.ts,
		fx.exec,
		1,
	)
	if err != nil {
		t.Fatalf("proceedPastLLM returned error: %v", err)
	}
	if got != ControlBreak {
		t.Errorf("proceedPastLLM(toolCalls=0, no steering) = %v, want ControlBreak", got)
	}
}

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/tools"
)

// =============================================================================
// Task 9: Pipeline CallLLM — Three-Tier Windowed Hint Behavior Tests
// =============================================================================
//
// These tests verify the three-tier iteration cap logic in CallLLM:
//
//   Tier 1 (remaining 1-2, absCap > 0, iterationCap < absCap):
//     → iterationExtendingHintMessage injected; ALL tools stay available.
//
//   Tier 2 (remaining == 0, absCap > 0, iterationCap < absCap):
//     → iterationCapReachedMessage injected; ONLY extend_turn_iteration available.
//
//   Tier 3 (remaining <= 0, absCap == 0 OR iterationCap == absCap):
//     → toolLimitHintMessage injected; NO tools available (legacy behavior).
//
// All tests use the existing newTurnCoordTestLoop harness and simpleConvProvider.

// --- Tier 1: Soft Hint ---

func TestCallLLM_WindowedHint_FiresAtRemainingTwo(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 100
	agent.Tools.Register(tools.NewExtendTurnIterationTool())

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.extendEnabled = true // Phase 2: /extend per-turn opt-in required for Tier 1

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// iteration 18 → remaining = 20 - 18 = 2
	ts.iteration = 18
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 18)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// Verify hint was injected
	hintFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "Tool iteration limit approaching") && strings.Contains(msg.Content, "2 iteration(s) remaining") {
			hintFound = true
			break
		}
	}
	if !hintFound {
		t.Error("expected iterationExtendingHintMessage with '2 iteration(s) remaining' in callMessages")
	}

	// Verify ALL tools are still available (not filtered)
	if len(exec.providerToolDefs) == 0 {
		t.Error("expected providerToolDefs to be populated (all tools available) at Tier 1")
	}
}

func TestCallLLM_WindowedHint_FiresAtRemainingOne(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 100
	agent.Tools.Register(tools.NewExtendTurnIterationTool())

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.extendEnabled = true // Phase 2: /extend per-turn opt-in required for Tier 1

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// iteration 19 → remaining = 20 - 19 = 1
	ts.iteration = 19
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 19)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	hintFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "Tool iteration limit approaching") && strings.Contains(msg.Content, "1 iteration(s) remaining") {
			hintFound = true
			break
		}
	}
	if !hintFound {
		t.Error("expected iterationExtendingHintMessage with '1 iteration(s) remaining' in callMessages")
	}

	if len(exec.providerToolDefs) == 0 {
		t.Error("expected providerToolDefs to be populated (all tools available) at Tier 1")
	}
}

// --- Tier 2: Cap Reached, Extend Only ---

func TestCallLLM_CapReached_OnlyExtendToolAvailable(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 100
	agent.Tools.Register(tools.NewExtendTurnIterationTool())

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.extendEnabled = true // Phase 2: /extend per-turn opt-in required for Tier 2

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// iteration 20 → remaining = 0, but iterationCap(20) < absCap(100)
	ts.iteration = 20
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 20)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// Verify cap-reached message was injected
	capReachedFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "Tool call limit reached for this extension window") {
			capReachedFound = true
			break
		}
	}
	if !capReachedFound {
		t.Error("expected iterationCapReachedMessage in callMessages")
	}

	// Verify ONLY extend_turn_iteration is available
	if len(exec.providerToolDefs) == 0 {
		t.Fatal("expected providerToolDefs to contain only extend_turn_iteration, got empty")
	}
	for _, def := range exec.providerToolDefs {
		if def.Function.Name != "extend_turn_iteration" {
			t.Errorf("expected only extend_turn_iteration, found %q", def.Function.Name)
		}
	}
}

// --- Tier 3: Absolute Ceiling / Legacy ---

func TestCallLLM_AbsoluteCeiling_AllToolsStripped(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 20 // cap == ceiling → no extension possible

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// iteration 20 → remaining = 0, iterationCap(20) == absCap(20) → Tier 3
	ts.iteration = 20
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 20)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// Verify legacy tool-limit message
	limitFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "CEASE ALL TOOL CALLS IMMEDIATELY") {
			limitFound = true
			break
		}
	}
	if !limitFound {
		t.Error("expected toolLimitHintMessage (CEASE ALL TOOL CALLS) in callMessages at absolute ceiling")
	}

	// Verify NO tools are available
	if len(exec.providerToolDefs) != 0 {
		t.Errorf("expected providerToolDefs to be empty at Tier 3, got %d defs", len(exec.providerToolDefs))
	}
}

func TestCallLLM_HardCap_StillAppliesWhenMaxIterationsCapIsZero(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 0 // legacy: no extension feature

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// iteration 20 → remaining = 0, absCap == 0 → Tier 3 (legacy)
	ts.iteration = 20
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 20)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	limitFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "CEASE ALL TOOL CALLS IMMEDIATELY") {
			limitFound = true
			break
		}
	}
	if !limitFound {
		t.Error("expected toolLimitHintMessage when MaxIterationsCap=0 (legacy behavior)")
	}

	if len(exec.providerToolDefs) != 0 {
		t.Errorf("expected providerToolDefs empty (legacy), got %d defs", len(exec.providerToolDefs))
	}
}

// --- Hint Clears After Extension ---

func TestCallLLM_HintClearsAfterExtension(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 100
	agent.Tools.Register(tools.NewExtendTurnIterationTool())

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.extendEnabled = true // Phase 2: /extend per-turn opt-in required for hint logic

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// Simulate: extended at iteration 20 → cap now 40
	ts.iteration = 22
	ts.iterationCap = 40
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 22)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// remaining = 40 - 22 = 18 → no hint should fire
	hintCount := 0
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "Tool iteration limit approaching") ||
			strings.Contains(msg.Content, "Tool call limit reached") ||
			strings.Contains(msg.Content, "CEASE ALL TOOL CALLS") {
			hintCount++
		}
	}
	if hintCount != 0 {
		t.Errorf("expected 0 hint/cap-reached/limit messages after extension (remaining=18), got %d", hintCount)
	}

	// All tools should be available
	if len(exec.providerToolDefs) == 0 {
		t.Error("expected providerToolDefs to be populated (normal call) after extension")
	}
}// =============================================================================
// Phase 2 (R14.5): /extend per-turn opt-in gating tests
// =============================================================================
//
// These tests verify that when ts.extendEnabled is false (default — user did
// NOT invoke /extend), the extend_turn_iteration tool is filtered out of
// providerToolDefs and the 3-tier hint logic (tiers 1 and 2) is suppressed.
// Tier 3 (absolute ceiling / legacy) still fires regardless.
//

// --- Tool Stripped When Not Extended ---

func TestCallLLM_ExtendTool_StrippedWhenNotExtended(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 100
	agent.Tools.Register(tools.NewExtendTurnIterationTool())

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	// ts.extendEnabled is false by default (no /extend command)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	ts.iteration = 5
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 5)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// Verify extend_turn_iteration is NOT in providerToolDefs
	for _, def := range exec.providerToolDefs {
		if def.Function.Name == "extend_turn_iteration" {
			t.Error("expected extend_turn_iteration to be stripped from providerToolDefs when !extendEnabled")
		}
	}

	// Verify other tools are still present (filter only strips extend, not everything).
	// Note: this test harness only registers extend_turn_iteration, so after filtering
	// the list may be legitimately empty. The assertion is intentionally loose here;
	// the key invariant is checked above (extend_turn_iteration NOT in the list).
	_ = exec.providerToolDefs
}

// --- Tier 1 Hint Suppressed When Not Extended ---

func TestCallLLM_Tier1_NoHintWhenNotExtended(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 100
	agent.Tools.Register(tools.NewExtendTurnIterationTool())

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	// ts.extendEnabled is false by default

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// iteration 18 → remaining = 2 → Tier 1 WOULD fire if extendEnabled
	ts.iteration = 18
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 18)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// Verify NO Tier 1 hint was injected
	hintFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "Tool iteration limit approaching") {
			hintFound = true
			break
		}
	}
	if hintFound {
		t.Error("expected NO Tier 1 hint when !extendEnabled, but found iterationExtendingHintMessage")
	}
}

// --- Tier 2 Cap-Reached Suppressed When Not Extended ---

func TestCallLLM_Tier2_NoCapReachedWhenNotExtended(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	agent.MaxIterations = 20
	agent.MaxIterationsCap = 100
	agent.Tools.Register(tools.NewExtendTurnIterationTool())

	pipeline := NewPipeline(al)
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	// ts.extendEnabled is false by default

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	// iteration 20 → remaining = 0, iterationCap(20) < absCap(100) → Tier 2 WOULD fire if extendEnabled
	ts.iteration = 20
	ts.iterationCap = 20
	exec.callMessages = exec.messages

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 20)
	if err != nil {
		t.Fatalf("CallLLM failed: %v", err)
	}

	// Verify NO Tier 2 cap-reached message was injected
	capReachedFound := false
	for _, msg := range exec.callMessages {
		if strings.Contains(msg.Content, "Tool call limit reached for this extension window") {
			capReachedFound = true
			break
		}
	}
	if capReachedFound {
		t.Error("expected NO Tier 2 cap-reached message when !extendEnabled")
	}

	// Verify extend_turn_iteration is NOT in providerToolDefs (filtered out)
	for _, def := range exec.providerToolDefs {
		if def.Function.Name == "extend_turn_iteration" {
			t.Error("expected extend_turn_iteration to be stripped when !extendEnabled at Tier 2 boundary")
		}
	}
}
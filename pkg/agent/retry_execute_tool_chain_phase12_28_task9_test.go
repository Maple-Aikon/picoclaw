// Package agent — Phase 12.28 Task 9 integration tests.
//
// Background: Phase 12.28 unified the 4 same-iter retry paths
// (handleGoalRecovery, retryLLMForBlockedTool, handleHookReplay, error
// recovery at turn_coord.go:373-553). Tasks 1-8 shipped the wiring; this
// file adds the integration-level regression-proofs that tie the wires
// together through the turn coordination layer.
//
// Two tests:
//   T9-1 (RetryLLMForBlockedTool_ExecutesToolBeforeCapExit)
//        — Regression-proof for the dropped-tool bug fixed in Phase 12.28
//        B1 (turn_coord.go:488). When retryLLMForBlockedTool returns
//        ControlToolLoop at iter==cap, the freshly-picked tool MUST be
//        carried through exec.response so the caller's ExecuteTools call
//        dispatches it. Without Task 12.28's fix, the tool would be
//        silently discarded (the old turn_coord only re-read exec.messages
//        without calling ExecuteTools).
//   T9-2 (RestrictedPhase_ExhaustionArchivesWithPhaseStuckReason)
//        — Exhausted BoundedRetry at GoalPhaseCheckpoint sets
//        goalArchiveRequested and stamps the phase-stuck abort reason
//        so finalizeGoalOnTurnEnd can produce a phase-aware user-facing
//        message instead of the generic toolLimitResponse.
//
// Note: Open-phase next-iter strategy + handleGoalRecovery is covered by
// pipeline_llm_recovery_test.go (Phase 12.11/12.27). This file focuses
// on retryLLMForBlockedTool because that's the path Phase 12.28 Task 8
// refactored to use the shared recallAndCheckTool primitive.
//
// See plan file ~/.picoclaw/workspace/memory/plan/picoclaw-phase12.28-
// unify-same-iter-retry-chains-20260729.md §Task 9.
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// dualScriptedToolProvider emits a script of tool-call responses for the
// initial LLM call, and a different script for retry attempts (heuristic:
// contains "RECOVERY_HINT" in last user message).
type dualScriptedToolProvider struct {
	initialLLMToolCalls []providers.ToolCall
	retryLLMToolCalls   []providers.ToolCall
	mu                  struct {
		idx        int
		retryCount int
	}
}

func (p *dualScriptedToolProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolsDef []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	// Detect retry attempt by scanning for RECOVERY_HINT marker in any
	// trailing user message — the helper's setupFunc injects the hint on
	// every wrong-tool retry path.
	isRetry := false
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.Contains(messages[i].Content, "RECOVERY_HINT") {
			isRetry = true
			break
		}
	}
	if isRetry {
		idx := p.mu.retryCount
		p.mu.retryCount++
		if idx < len(p.retryLLMToolCalls) {
			return &providers.LLMResponse{
				Content:   "",
				ToolCalls: []providers.ToolCall{p.retryLLMToolCalls[idx]},
			}, nil
		}
		return &providers.LLMResponse{Content: "retry fail"}, nil
	}
	idx := p.mu.idx
	p.mu.idx++
	if idx < len(p.initialLLMToolCalls) {
		return &providers.LLMResponse{
			Content:   "",
			ToolCalls: []providers.ToolCall{p.initialLLMToolCalls[idx]},
		}, nil
	}
	return &providers.LLMResponse{Content: "empty"}, nil
}

func (p *dualScriptedToolProvider) GetDefaultModel() string { return "test-model" }

// dualExhaustThenFixProvider emits blocked tools across all attempts of
// the BoundedRetry so Path 2 OnExhausted fires with phase-stuck archive.
type dualExhaustThenFixProvider struct {
	mu struct {
		callIdx int
	}
}

func (p *dualExhaustThenFixProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolsDef []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.callIdx++
	return &providers.LLMResponse{
		Content:   "",
		ToolCalls: []providers.ToolCall{{Name: "read_file", Arguments: map[string]any{"path": "/tmp/x"}}},
	}, nil
}

func (p *dualExhaustThenFixProvider) GetDefaultModel() string { return "test-model" }

// T9-1: Regression-proof for Phase 12.28 B1 (dropped-tool fix).
//
// At iter==cap, when retryLLMForBlockedTool returns ControlToolLoop
// (LLM picked valid tool after a previous blocked tool), the freshly-
// picked tool MUST be carried through exec.response so the caller's
// ExecuteTools call (turn_coord.go:480-488) dispatches it.
//
// Without Task 12.28's fix, the tool would be silently discarded (the
// old turn_coord only re-read exec.messages without calling ExecuteTools).
func TestRetryExecuteToolChain_T9_1_RetryLLMForBlockedTool_ExecutesToolBeforeCapExit(t *testing.T) {
	// Provider emits blocked tool first, then on retry emits valid tool.
	provider := &dualScriptedToolProvider{
		initialLLMToolCalls: []providers.ToolCall{
			{Name: "read_file", Arguments: map[string]any{"path": "/tmp/x"}},
		},
		retryLLMToolCalls: []providers.ToolCall{
			{Name: "goal_progress", Arguments: map[string]any{}},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	// Pin to Checkpoint so resolveAgentToolAllowlistWithPhase returns
	// lifecycle allowlist ([goal_progress, complete_goal]).
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))
	if ts.goalArchiveRequested {
		t.Fatalf("setup: goalArchiveRequested should be false")
	}

	// Drive retryExecuteToolChain (Phase 12.42 Path 4 wiring — Path 2
	// retryLLMForBlockedTool was merged and deleted). Path 4 expects:
	//   attempt 0: blocked (read_file, not in allowlist) → re-prompt
	//   attempt 1: goal_progress → in allowlist → Step 3 executes via fake
	fake := &fakeExecutor{returnControl: ToolControlContinue}
	p.SetToolExecutor(fake)
	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 2,
		"RECOVERY_HINT: pick goal_progress or complete_goal",
		[]string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("retryExecuteToolChain: %v", err)
	}
	if ctrl != ControlToolLoop {
		t.Fatalf("expected ControlToolLoop (LLM picked goal_progress on retry), got %v", ctrl)
	}
	// Path 4 returns ControlToolLoop → the helper already executed the
	// tool (Step 3). Verify exec.response is set + carries goal_progress
	// so the retry succeeded with the right tool.
	if exec.response == nil {
		t.Fatal("exec.response must be set (Phase 12.26 contract)")
	}
	if len(exec.response.ToolCalls) == 0 ||
		exec.response.ToolCalls[0].Name != "goal_progress" {
		t.Fatalf("expected exec.response.ToolCalls[0].Name=goal_progress, got %+v",
			exec.response.ToolCalls)
	}
	// ts.goalArchiveRequested must remain false (recovery succeeded).
	if ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=false after successful retry, got true")
	}
}

// T9-2: Exhausted BoundedRetry at GoalPhaseCheckpoint sets
// goalArchiveRequested + stamps phase-stuck abort reason so the
// finalizeGoalOnTurnEnd hook produces a phase-aware user-facing message
// rather than the generic "I've reached max_tool_iterations" fallback.
//
// Verifies the wire between retryLLMForBlockedTool OnExhausted callback
// and the Phase 12.21 phase-stuck abort-reason stamp.
func TestRetryExecuteToolChain_T9_2_RestrictedPhase_ExhaustionArchivesWithPhaseStuckReason(t *testing.T) {
	// Provider always emits blocked tool → all 3 attempts fail →
	// OnExhausted fires.
	provider := &dualExhaustThenFixProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))
	if ts.goalArchiveRequested {
		t.Fatalf("setup: goalArchiveRequested should be false")
	}
	if ts.lastPhaseStuckError != "" {
		t.Fatalf("setup: lastPhaseStuckError should be empty, got %q",
			ts.lastPhaseStuckError)
	}

	// Drive retryExecuteToolChain (Phase 12.42 Path 4 wiring) to exhaustion.
	// read_file is not in the Checkpoint allowlist, so all 3 attempts fail
	// → OnExhausted fires (C5 stamps phase-stuck reason).
	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 2,
		"RECOVERY_HINT: pick goal_progress or complete_goal",
		[]string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("retryExecuteToolChain: %v", err)
	}
	// Exhausted path returns ControlBreak.
	if ctrl != ControlBreak {
		t.Errorf("expected ControlBreak (exhausted), got %v", ctrl)
	}
	// Archive must be requested.
	if !ts.goalArchiveRequested {
		t.Errorf("expected goalArchiveRequested=true after exhaustion, got false")
	}
	// Phase-stuck reason must be stamped for finalizeGoalOnTurnEnd.
	wantReason := GoalPhaseCheckpointStuckAbortReason
	if ts.lastPhaseStuckError != wantReason {
		t.Errorf("expected lastPhaseStuckError=%q, got %q",
			wantReason, ts.lastPhaseStuckError)
	}
	// Sanity check: re-verify the wire constants used by finalize helpers.
	if GoalPhaseCheckpointStuckAbortReason == "" {
		t.Fatal("GoalPhaseCheckpointStuckAbortReason not defined")
	}
}

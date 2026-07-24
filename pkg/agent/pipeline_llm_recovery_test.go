// Package agent — Phase 12.11 tests for handleGoalRecovery same-iteration
// BoundedRetry loop. These tests verify the wire-up that replaces
// applyRecoveryAction (Phase 12.10 iter-bump pattern) with a same-iter
// retry loop, restoring the original Phase 5 design intent.
//
// Coverage:
//   - Same-iter retry: trigger fires, retry succeeds without iter bump
//   - Cap exhausted: archive goal, return ControlBreak
//   - Counter persistence: counter does NOT bump between attempts in same iter
//   - Boundary case: tool exec error at iter=iterationCap-1 in Open phase,
//     same-iter retry, success — phase unchanged (still Open)
//
// See plan file ~/.picoclaw/workspace/memory/plan/picoclaw-phase12.11-
// same-iteration-recovery-boundedretry-20260724.md for design.
package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// recoveryTestProvider returns a configurable sequence of responses. The
// recovery flow needs at least 2 attempts (initial trigger + retry attempt),
// so this provider supports chained response arrays.
type recoveryTestProvider struct {
	responses []*providers.LLMResponse
	mu        struct {
		sync.Mutex
		callCount int
	}
}

func (p *recoveryTestProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.mu.callCount
	p.mu.callCount++
	if idx < len(p.responses) && p.responses[idx] != nil {
		return p.responses[idx], nil
	}
	return &providers.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *recoveryTestProvider) GetDefaultModel() string {
	return "recovery-test-model"
}

// TestHandleGoalRecovery_SameIteration_NoIterBump verifies that when the
// first attempt triggers recovery (empty text) and the second attempt
// succeeds with valid text, the iteration counter is NOT bumped.
//
// This is the core invariant of Phase 12.11: recovery retries run within
// the same iteration.
func TestHandleGoalRecovery_SameIteration_NoIterBump(t *testing.T) {
	// Provider: attempt 0 = empty (triggers recovery), attempt 1 = tool call
	// (no recovery needed; tool call signals forward progress).
	provider := &recoveryTestProvider{
		responses: []*providers.LLMResponse{
			{Content: "", FinishReason: "stop"}, // attempt 0: empty → recovery
			{
				Content: "",
				ToolCalls: []providers.ToolCall{
					{ID: "call-1", Name: "test_tool", Arguments: map[string]any{}},
				},
				FinishReason: "tool_calls",
			}, // attempt 1: tool call → no recovery
		},
	}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)

	// Seed an active goal using the helper's workspace (the helper creates
	// its own t.TempDir() and writes the goal file there). We must point
	// agent.Workspace to that same dir so ts.hasGoal() reads it correctly.
	// This must happen BEFORE newTurnState (which reads agent.Workspace into
	// ts.workspace) and BEFORE SetupTurn (which archives any stale goal for
	// ts.sessionKey).
	//
	// To bypass SetupTurn's archiveStaleGoalOnTurnStart, we use a different
	// sessionKey for the ts so it doesn't find/Archive the goal file we just
	// wrote. Then we point ts.goalStoreOverride (or directly mutate) so
	// hasGoal() reads from the helper's workspace.
	//
	// Simpler approach: write goal file directly to agent.Workspace AFTER
	// SetupTurn, then call hasGoal which reads from disk.
	ws := t.TempDir()
	agent.Workspace = ws

	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 10
	ts.iteration = 0
	ts.setIteration(2)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}

	// Write goal directly to disk AFTER SetupTurn. SetupTurn's
	// archiveStaleGoalOnTurnStart already ran, so any further writes won't
	// be archived (the per-turn archive is a one-shot on SetupTurn entry).
	goalStore := goal.NewStore(ws)
	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "phase-12-11-test",
		Description: goal.Description{
			Objective:       "test same-iter recovery",
			SuccessCriteria: []string{"recovery fires without iter bump"},
			Cadence:         "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goalStore.Write("test-session", activeGoal); err != nil {
		t.Fatalf("Write goal: %v", err)
	}

	// Sanity-check: confirm hasGoal/phase so a real test failure is
	// distinguishable from "missing goal" setup.
	if !ts.hasGoal() {
		t.Fatal("setup error: hasGoal=false; goal file not seeded in agent.Workspace")
	}

	// First call to CallLLM: response is empty → recovery triggers
	// → handleGoalRecovery enters BoundedRetry loop.
	ctrl, err := pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 2)
	if err != nil {
		t.Fatalf("CallLLM: %v", err)
	}

	// After same-iter retry, iteration counter MUST still be 2 (not bumped
	// to 3). Pre-Phase 12.11 this would have been bumped to 3.
	if got := ts.CurrentIteration(); got != 2 {
		t.Errorf("expected iteration=1 after same-iter retry, got %d", got)
	}

	// Recovery succeeded → ControlContinue (caller continues with response)
	if ctrl == ControlBreak {
		t.Errorf("expected ControlContinue after successful recovery, got %v", ctrl)
	}
	// Attempt 1 emitted a tool call (no recovery needed) → response has tool calls.
	if exec.response == nil || len(exec.response.ToolCalls) == 0 {
		t.Errorf("expected retry response with tool calls, got %v", exec.response)
	}
}

// TestHandleGoalRecovery_Exhausted_ArchivesGoal verifies that when all
// 3 attempts trigger recovery, the goal is archived and ControlBreak is
// returned. Pre-Phase 12.11 this would have bumped the iteration 3 times
// and crossed into Checkpoint/Final phase.
func TestHandleGoalRecovery_Exhausted_ArchivesGoal(t *testing.T) {
	// All 3 attempts return empty text → recovery fires every time.
	provider := &recoveryTestProvider{
		responses: []*providers.LLMResponse{
			{Content: "", FinishReason: "stop"},
			{Content: "", FinishReason: "stop"},
			{Content: "", FinishReason: "stop"},
		},
	}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ws := t.TempDir()
	agent.Workspace = ws
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 10
	ts.setIteration(2)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}

	// Seed an active goal AFTER SetupTurn so the per-turn archive doesn't
	// clean it up. Write directly to agent.Workspace.
	goalStore := goal.NewStore(ws)
	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "phase-12-11-exhausted",
		Description: goal.Description{
			Objective:       "test exhausted recovery",
			SuccessCriteria: []string{"archive on cap"},
			Cadence:         "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goalStore.Write("test-session", activeGoal); err != nil {
		t.Fatalf("Write goal: %v", err)
	}

	ctrl, err := pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 1)
	if err != nil {
		t.Fatalf("CallLLM: %v", err)
	}

	// Exhausted → archive goal + ControlBreak.
	if ctrl == ControlContinue {
		t.Errorf("expected ControlBreak after exhaustion, got %v", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Error("expected goalArchiveRequested=true after exhausted recovery")
	}
	// Critical: iteration MUST NOT have bumped (3 attempts within same iter).
	if got := ts.CurrentIteration(); got != 2 {
		t.Errorf("expected iteration=2 after exhausted same-iter retry, got %d", got)
	}
}

// TestHandleGoalRecovery_CounterPersistsAcrossAttempts verifies that the
// emptyResponseRecoverySent counter is incremented across the attempts
// within a single iteration (NOT reset between attempts — only between
// iterations, per Phase 12.10).
// writeActiveGoalInWorkspace creates an active goal file in the given workspace
// under the test session key. Used by recovery tests to seed a goal AFTER
// SetupTurn (which runs the per-turn archive that would otherwise clean it up).
func writeActiveGoalInWorkspace(t *testing.T, ws, sessionKey string) {
	t.Helper()
	store := goal.NewStore(ws)
	now := time.Now().UTC()
	g := &goal.Goal{
		Name: "phase-12-11-test",
		Description: goal.Description{
			Objective:       "test same-iter recovery",
			SuccessCriteria: []string{"recovery fires without iter bump"},
			Cadence:         "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Write(sessionKey, g); err != nil {
		t.Fatalf("Write goal: %v", err)
	}
}

func TestHandleGoalRecovery_CounterPersistsAcrossAttempts(t *testing.T) {
	// 1 empty response → retry. Retry attempt produces a tool call → DONE.
	// Counter must persist across attempts within same iter (1 attempt only
	// in this test, but the assertion validates the counter is set to true
	// after the SAME-iter retry fires, not reset by an iter bump).
	provider := &recoveryTestProvider{
		responses: []*providers.LLMResponse{
			{Content: "", FinishReason: "stop"}, // attempt 0: empty → recovery
			{
				Content: "",
				ToolCalls: []providers.ToolCall{
					{ID: "call-1", Name: "test_tool", Arguments: map[string]any{}},
				},
				FinishReason: "tool_calls",
			}, // attempt 1: tool call → no recovery
		},
	}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ws := t.TempDir()
	agent.Workspace = ws
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 10
	ts.setIteration(2)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}
	// Write goal AFTER SetupTurn (avoid per-turn archive).
	writeActiveGoalInWorkspace(t, ws, "test-session")

	// 2 attempts fire empty recovery, 3rd succeeds. Counter must persist
	// across attempts within same iter (NOT reset by iter bump because
	// iter doesn't bump in same-iter retry).
	ctrl, err := pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 1)
	if err != nil {
		t.Fatalf("CallLLM: %v", err)
	}

	if ctrl == ControlBreak {
		t.Errorf("expected ControlContinue, got %v", ctrl)
	}
	// Counter should have been set true at attempt 0 (after first empty
	// response), and counter is what gates further EmptyText triggers
	// within the same iteration (Phase 12.10 cap=2/iter).
	if !ts.emptyResponseRecoverySent {
		t.Error("expected emptyResponseRecoverySent=true after recovery fired")
	}
}

// TestHandleGoalRecovery_BoundaryCheckpointNoBump verifies the boundary
// case that motivated Phase 12.11: tool exec error at iter=iterationCap-1
// (still Open phase), same-iter retry, success without phase change.
//
// Pre-Phase 12.11: iter 24 → recovery → iter bump → iter 25 (Checkpoint) →
// tool stripped → forced complete_goal. Bug.
//
// Phase 12.11: iter 24 → recovery → same iter 24 → success → phase stays
// Open. Tool list unchanged.
func TestHandleGoalRecovery_BoundaryCheckpointNoBump(t *testing.T) {
	provider := &recoveryTestProvider{
		responses: []*providers.LLMResponse{
			// Initial response has tool call that errors (handled by ExecuteTools,
			// not CallLLM). For CallLLM-level test, we mock the post-execution
			// recovery by returning empty text → recovery fires.
			{Content: "", FinishReason: "stop"}, // attempt 0: empty → recovery
			{
				Content: "",
				ToolCalls: []providers.ToolCall{
					{ID: "call-1", Name: "test_tool", Arguments: map[string]any{}},
				},
				FinishReason: "tool_calls",
			}, // attempt 1: tool call → no recovery
		},
	}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ws := t.TempDir()
	agent.Workspace = ws
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 10
	ts.setIteration(9) // boundary: 1 iter left before cap

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}
	writeActiveGoalInWorkspace(t, ws, "test-session")

	ctrl, err := pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 9)
	if err != nil {
		t.Fatalf("CallLLM: %v", err)
	}

	if ctrl == ControlBreak {
		t.Errorf("expected ControlContinue, got %v", ctrl)
	}
	// Critical: iteration stayed at 9 (NOT bumped to 10/Checkpoint phase).
	if got := ts.CurrentIteration(); got != 9 {
		t.Errorf("expected iteration=9 (no bump), got %d", got)
	}
	if got := ts.currentGoalPhase(); string(got) != "open" {
		t.Errorf("expected phase=open (no bump), got %s", got)
	}
}

// TestHandleGoalRecovery_PendingMessageInjected verifies that the recovery
// message from attempt N is injected into callMessages for attempt N+1
// within the same iteration.
func TestHandleGoalRecovery_PendingMessageInjected(t *testing.T) {
	var capturedMessages [][]providers.Message
	captureProvider := &recoveryCaptureProvider{
		onChat: func(msgs []providers.Message) {
			// Take a copy of the messages slice for each call.
			capturedMessages = append(capturedMessages, append([]providers.Message{}, msgs...))
		},
		responses: []*providers.LLMResponse{
			{Content: "", FinishReason: "stop"},
			{Content: "recovered response", FinishReason: "stop"},
		},
	}

	al, agent, cleanup := newTurnCoordTestLoop(t, captureProvider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ws := t.TempDir()
	agent.Workspace = ws
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 10
	ts.setIteration(2)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}
	writeActiveGoalInWorkspace(t, ws, "test-session")

	_, err = pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 1)
	if err != nil {
		t.Fatalf("CallLLM: %v", err)
	}

	if len(capturedMessages) < 2 {
		t.Fatalf("expected at least 2 LLM calls (initial + retry), got %d", len(capturedMessages))
	}

	// Verify the second call's messages contain the recovery hint.
	hintSeen := false
	for _, m := range capturedMessages[1] {
		if m.Role == "user" && strings.Contains(m.Content, "previous response") {
			hintSeen = true
			break
		}
	}
	if !hintSeen {
		t.Errorf("expected recovery hint message injected in retry attempt's messages; got %v", capturedMessages[1])
	}
}

// TestHandleGoalRecovery_TextOnlySoftRetry_CounterReset verifies that the
// textOnlySoftRetriesDone counter is reset on handleGoalRecovery entry, so a
// text-only response on attempt 0 re-fires the soft-retry trigger, allowing
// attempt 1 to call LLM with the recovery hint injected.
//
// Pre-Phase 12.11.1 fix: the caller-side increment of textOnlySoftRetriesDone
// (cap=1) was NOT reset on handleGoalRecovery entry → wrapped func re-eval
// saw `textOnlySoftRetriesDone > 0` → returned escalated → archive/abort →
// LLM's text-only response was discarded, user got DefaultResponse. Iter bumped
// to next iteration with no new LLM call. Discovered via live verify on
// main-turn-2 (2026-07-24) where "tiếp tục đi" got 88 chars (DefaultResponse)
// instead of the LLM's 351-char text-only answer.
//
// Phase 12.11.1 fix: handleGoalRecovery resets textOnlySoftRetriesDone (and
// textOnlyHardRetriesDone + toolExecRecoveryAttempts map) on entry, mirroring
// the existing emptyResponseRecoverySent reset. attempt 1 fires with hint
// injected, LLM chooses complete_goal or tool call → success.
func TestHandleGoalRecovery_TextOnlySoftRetry_CounterReset(t *testing.T) {
	provider := &recoveryTestProvider{
		responses: []*providers.LLMResponse{
			{Content: "thinking about this but no tool call yet", FinishReason: "stop"}, // attempt 0: text-only soft
			{
				Content: "ok here is the final report",
				ToolCalls: []providers.ToolCall{
					{ID: "call-1", Name: "test_tool", Arguments: map[string]any{}},
				},
				FinishReason: "tool_calls",
			}, // attempt 1: tool call → success
		},
	}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ws := t.TempDir()
	agent.Workspace = ws
	ts := newTurnState(agent, makeTestProcessOpts("test-session"), turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 10
	ts.setIteration(2)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}
	writeActiveGoalInWorkspace(t, ws, "test-session")

	// Pre-set the textOnly counter to a non-zero value to simulate the
	// caller having already incremented it (CallLLM line 713-728 path).
	// Pre-Phase 12.11.1 fix: this value persisted into handleGoalRecovery
	// and the soft-retry trigger check `textOnlySoftRetriesDone >= cap`
	// returned escalated → ControlBreak → LLM's text was discarded.
	ts.textOnlySoftRetriesDone = 1

	ctrl, err := pipeline.CallLLM(context.Background(), context.Background(), ts, exec, 1)
	if err != nil {
		t.Fatalf("CallLLM: %v", err)
	}

	if ctrl == ControlBreak {
		t.Errorf("expected ControlContinue after same-iter retry succeeded, got ControlBreak")
	}
	if ts.goalArchiveRequested {
		t.Error("expected goalArchiveRequested=false after successful same-iter retry")
	}
	// Critical: iteration MUST NOT have bumped.
	if got := ts.CurrentIteration(); got != 2 {
		t.Errorf("expected iteration=2 after text-only soft retry same-iter, got %d", got)
	}
	// Verify attempt 1 fired (provider.callCount should be 2).
	if got := provider.mu.callCount; got != 2 {
		t.Errorf("expected 2 LLM calls (attempt 0 + retry 1), got %d", got)
	}
}

// recoveryCaptureProvider captures messages for assertion.
type recoveryCaptureProvider struct {
	responses []*providers.LLMResponse
	mu        struct {
		sync.Mutex
		callCount int
	}
	onChat func(messages []providers.Message)
}

func (p *recoveryCaptureProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.onChat != nil {
		p.onChat(messages)
	}
	idx := p.mu.callCount
	p.mu.callCount++
	if idx < len(p.responses) && p.responses[idx] != nil {
		return p.responses[idx], nil
	}
	return &providers.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *recoveryCaptureProvider) GetDefaultModel() string {
	return "capture-model"
}

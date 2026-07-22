// Phase 5 unit tests for goal-lifecycle recovery triggers. Verifies the 5
// triggers from plan §5.2 + §8.3 wire correctly across phases.

package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func newPhase5TurnState() *turnState {
	return &turnState{
		toolExecRecoveryAttempts: make(map[string]int),
	}
}

func TestEvaluateRecovery_EmptyText_PhaseOpen_InjectsOnce(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{
		Phase:        "Open",
		Iteration:    1,
		TextEmpty:    true,
		HasToolCalls: false,
	}
	action, msg := evaluateRecovery(ts, ctx)
	if action != RecoveryRetrySameIteration {
		t.Fatalf("expected RecoveryRetrySameIteration, got %v", action)
	}
	if msg != EmptyResponseRecoveryMessage {
		t.Fatalf("expected EMPTY_FINAL message, got %q", msg)
	}
	if !ts.emptyResponseRecoverySent {
		t.Fatalf("expected emptyResponseRecoverySent=true after first fire")
	}
}

func TestEvaluateRecovery_EmptyText_AlreadySent_NoRetry(t *testing.T) {
	ts := newPhase5TurnState()
	ts.emptyResponseRecoverySent = true // simulate second empty response in same iteration
	ctx := RecoveryContext{Phase: "Open", TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("expected RecoveryNone after one-shot injection, got %v", action)
	}
}

func TestEvaluateRecovery_EmptyText_PhaseLock_NoTrigger(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "Lock", TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("expected RecoveryNone in Lock phase, got %v", action)
	}
}

func TestEvaluateRecovery_TextOnly2x_PhaseOpen_ForceComplete(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "Open", TextEmpty: false, HasToolCalls: false}
	// First text-only: streak becomes 1, soft prompt fires (Phase 12)
	action1, msg1 := evaluateRecovery(ts, ctx)
	if action1 != RecoveryRetrySameIteration {
		t.Fatalf("first text-only should soft retry, got %v", action1)
	}
	if msg1 != TextOnlySoftRetryMessage {
		t.Fatalf("expected soft prompt message, got %q", msg1)
	}
	if ts.textOnlyStreak != 1 {
		t.Fatalf("expected streak=1 after first text-only, got %d", ts.textOnlyStreak)
	}
	if ts.textOnlySoftRetriesDone != 1 {
		t.Fatalf("expected soft_retries=1, got %d", ts.textOnlySoftRetriesDone)
	}
	// Second text-only (same iter, immediately after): hard prompt fires
	action2, msg2 := evaluateRecovery(ts, ctx)
	if action2 != RecoveryRetrySameIteration {
		t.Fatalf("second text-only should hard retry, got %v", action2)
	}
	if msg2 != TextOnlyHardRetryMessage {
		t.Fatalf("expected hard prompt message, got %q", msg2)
	}
	if ts.textOnlyStreak != 2 {
		t.Fatalf("expected streak=2, got %d", ts.textOnlyStreak)
	}
	if ts.textOnlyHardRetriesDone != 1 {
		t.Fatalf("expected hard_retries=1, got %d", ts.textOnlyHardRetriesDone)
	}
}

func TestEvaluateRecovery_TextOnly3x_ArchiveGoal(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "Open", TextEmpty: false, HasToolCalls: false}
	// Phase 12: soft + hard both fire within iteration; 3rd text-only
	// exceeds the (1 soft + 1 hard = 2 retry) cap, archive fires.
	_, _ = evaluateRecovery(ts, ctx) // soft
	_, _ = evaluateRecovery(ts, ctx) // hard
	action3, _ := evaluateRecovery(ts, ctx)
	if action3 != RecoveryArchiveGoal {
		t.Fatalf("3rd text-only should archive goal, got %v", action3)
	}
}

func TestEvaluateRecovery_TextOnly_ToolCallResetsStreak(t *testing.T) {
	ts := newPhase5TurnState()
	ctxText := RecoveryContext{Phase: "Open", HasToolCalls: false}
	ctxTool := RecoveryContext{Phase: "Open", HasToolCalls: true}
	evaluateRecovery(ts, ctxText)
	if ts.textOnlyStreak != 1 {
		t.Fatalf("streak should be 1, got %d", ts.textOnlyStreak)
	}
	evaluateRecovery(ts, ctxTool)
	if ts.textOnlyStreak != 0 {
		t.Fatalf("streak should reset to 0 after tool call, got %d", ts.textOnlyStreak)
	}
}

func TestEvaluateRecovery_ToolExecError_RetrySameIteration(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "Open", ToolName: "view_goal"}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryRetrySameIteration {
		t.Fatalf("expected RecoveryRetrySameIteration, got %v", action)
	}
	if ts.toolExecRecoveryAttempts["view_goal"] != 1 {
		t.Fatalf("expected view_goal attempt=1, got %d", ts.toolExecRecoveryAttempts["view_goal"])
	}
}

func TestEvaluateRecovery_ToolExecError_ExhaustCap_Archive(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "Open", ToolName: "view_goal"}
	for i := 0; i < ToolExecErrorRetryCap; i++ {
		evaluateRecovery(ts, ctx)
	}
	// 4th call exceeds cap
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryArchiveGoal {
		t.Fatalf("expected RecoveryArchiveGoal after %d retries, got %v", ToolExecErrorRetryCap, action)
	}
}

func TestEvaluateRecovery_ToolExecError_LockPhase_NoRetry(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "Lock", ToolName: "view_goal"}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("Lock phase should not retry tool errors, got %v", action)
	}
}

func TestEvaluateRecovery_ProviderTransient_AlwaysArchive(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "Open", ProviderError: true}
	action, msg := evaluateRecovery(ts, ctx)
	if action != RecoveryArchiveGoal {
		t.Fatalf("expected RecoveryArchiveGoal for provider transient, got %v", action)
	}
	if msg == "" {
		t.Fatalf("expected archive message")
	}
}

func TestEvaluateRecovery_NoGoalPhase_NoTrigger(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "", TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("no goal phase should not trigger recovery, got %v", action)
	}
}

func TestEvaluateRecovery_FinalPhase_NoTrigger(t *testing.T) {
	ts := newPhase5TurnState()
	ctx := RecoveryContext{Phase: "Final", TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("Final phase should not trigger recovery, got %v", action)
	}
}

// TestCheckToolExecErrorRecovery_NoError verifies the helper is silent
// when the most recent message is not an executor error.
func TestCheckToolExecErrorRecovery_NoError(t *testing.T) {
	ts := newPhase5TurnState()
	exec := &turnExecution{
		messages: []providers.Message{{Role: "tool", Content: "ok: result"}},
	}
	tool, _ := checkToolExecErrorRecovery(ts, exec)
	if tool != "" {
		t.Fatalf("expected no trigger on non-error message, got tool=%q", tool)
	}
}

// TestCheckToolExecErrorRecovery_ExecutorError_Retries verifies trigger
// fires when the executor reports an error and per-tool cap is not yet hit.
func TestCheckToolExecErrorRecovery_ExecutorError_Retries(t *testing.T) {
	ts := newPhase5TurnState()
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "view_goal", Content: "Tool execution failed: connection refused"},
		},
	}
	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool != "" {
		t.Fatalf("expected retry (no archive) on first error, got tool=%q msg=%q", tool, msg)
	}
	if ts.toolExecRecoveryAttempts["view_goal"] != 1 {
		t.Fatalf("expected attempt count=1, got %d", ts.toolExecRecoveryAttempts["view_goal"])
	}
}

// TestCheckToolExecErrorRecovery_CapExhausted_Archives verifies the helper
// returns the tool name when the per-tool retry cap has been hit.
func TestCheckToolExecErrorRecovery_CapExhausted_Archives(t *testing.T) {
	ts := newPhase5TurnState()
	for i := 0; i < ToolExecErrorRetryCap; i++ {
		evaluateRecovery(ts, RecoveryContext{Phase: "Open", ToolName: "view_goal"})
	}
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "view_goal", Content: "Tool execution failed: timeout"},
		},
	}
	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool != "view_goal" {
		t.Fatalf("expected archive trigger for view_goal, got tool=%q msg=%q", tool, msg)
	}
	if msg == "" {
		t.Fatalf("expected non-empty archive message")
	}
}

// TestCheckToolExecErrorRecovery_EmptyMessages verifies the helper is safe
// against nil/empty exec.messages.
func TestCheckToolExecErrorRecovery_EmptyMessages(t *testing.T) {
	ts := newPhase5TurnState()
	if tool, _ := checkToolExecErrorRecovery(ts, nil); tool != "" {
		t.Fatalf("expected no trigger on nil exec, got %q", tool)
	}
	exec := &turnExecution{messages: nil}
	if tool, _ := checkToolExecErrorRecovery(ts, exec); tool != "" {
		t.Fatalf("expected no trigger on empty messages, got %q", tool)
	}
}

// TestCheckToolExecErrorRecovery_NonToolRole verifies only tool-role
// messages are inspected.
func TestCheckToolExecErrorRecovery_NonToolRole(t *testing.T) {
	ts := newPhase5TurnState()
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "assistant", Content: "Tool execution failed: ignore me"},
		},
	}
	if tool, _ := checkToolExecErrorRecovery(ts, exec); tool != "" {
		t.Fatalf("expected no trigger on assistant message, got %q", tool)
	}
}

func TestBuildToolExecErrorRetryMessage_NoRegistry_BaseOnly(t *testing.T) {
	got := buildToolExecErrorRetryMessage("web_search", "connection refused", nil)
	want := fmt.Sprintf(ToolExecErrorRetryMessage, "connection refused")
	if got != want {
		t.Fatalf("nil registry should return base message\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildToolExecErrorRetryMessage_RegistryNoKnowledge_BaseOnly(t *testing.T) {
	r := tools.NewToolRegistry()
	// No lesson recorded for "web_search" — LoadForEscalation returns "".
	got := buildToolExecErrorRetryMessage("web_search", "connection refused", r)
	want := fmt.Sprintf(ToolExecErrorRetryMessage, "connection refused")
	if got != want {
		t.Fatalf("registry without knowledge should return base message\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildToolExecErrorRetryMessage_WithKnowledge_Appends(t *testing.T) {
	ws := t.TempDir()
	store, err := tools.NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}
	body := "Always include retry_count argument; default 0 makes call infinite-loop."
	if _, _, err := store.Save("web_search", body); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Stand up a minimal registry that exposes this store. The ToolRegistry
	// constructor wires an empty store by default; we replace it for the
	// test scope.
	r := tools.NewToolRegistry()
	r.SetToolKnowledgeStore(store)

	got := buildToolExecErrorRetryMessage("web_search", "connection refused", r)
	want := fmt.Sprintf(ToolExecErrorRetryMessage, "connection refused")
	if got == want {
		t.Fatalf("registry with knowledge should append, got base message only")
	}
	if !strings.Contains(got, body) {
		t.Fatalf("expected knowledge body in message, got %q", got)
	}
}

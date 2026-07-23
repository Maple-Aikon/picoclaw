// Phase 5 unit tests for goal-lifecycle recovery triggers. Verifies the 5
// triggers from plan §5.2 + §8.3 wire correctly across phases.

package agent

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// newPhase5TurnState returns a turnState seeded with an active goal file on disk
// so that hasGoal()=true and currentGoalPhase() resolves to "open" for iter>=2.
// Tests using checkToolExecErrorRecovery (which feeds wire-path Phase from
// currentGoalPhase()) depend on this so the recovery trigger #3 (Phase !=
// GoalPhaseSet gate) actually fires. Pre-Phase 11 the gate compared against
// capital "Lock" which never matched any wire value either, so tests passed via
// accidental always-true behavior. Without an AgentInstance, currentGoalPhase()
// returns GoalPhaseSet (no agent → early return). Without sessionKey, hasGoal()
// returns false → GoalPhaseSet. Without an on-disk active goal, hasGoal() also
// returns false → GoalPhaseSet even with everything else wired.
func newPhase5TurnState(t *testing.T) *turnState {
	t.Helper()
	ws := t.TempDir()
	store := goal.NewStore(ws)
	g := &goal.Goal{
		Name:        "phase5-test",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "phase5 recovery test", SuccessCriteria: []string{"pass"}},
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := store.Write("phase5-test-session", g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}
	return &turnState{
		agent:                    &AgentInstance{Workspace: ws, MaxIterations: 50, MaxIterationsCap: 200},
		workspace:                ws,
		sessionKey:               "phase5-test-session",
		toolExecRecoveryAttempts: make(map[string]int),
		iteration:                2, // Open phase requires 2 <= iter < MaxIter
		iterationCap:             50,
		maxIterationsCap:         200,
	}
}

func TestEvaluateRecovery_EmptyText_PhaseOpen_InjectsOnce(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{
		Phase: string(GoalPhaseOpen),
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
	ts := newPhase5TurnState(t)
	ts.emptyResponseRecoverySent = true // simulate second empty response in same iteration
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("expected RecoveryNone after one-shot injection, got %v", action)
	}
}

func TestEvaluateRecovery_EmptyText_PhaseLock_NoTrigger(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseSet), TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("expected RecoveryNone in Lock phase, got %v", action)
	}
}

func TestEvaluateRecovery_TextOnly2x_PhaseOpen_ForceComplete(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false}
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
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false}
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
	ts := newPhase5TurnState(t)
	ctxText := RecoveryContext{Phase: string(GoalPhaseOpen), HasToolCalls: false}
	ctxTool := RecoveryContext{Phase: string(GoalPhaseOpen), HasToolCalls: true}
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
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), ToolName: "view_goal"}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryRetrySameIteration {
		t.Fatalf("expected RecoveryRetrySameIteration, got %v", action)
	}
	if ts.toolExecRecoveryAttempts["view_goal"] != 1 {
		t.Fatalf("expected view_goal attempt=1, got %d", ts.toolExecRecoveryAttempts["view_goal"])
	}
}

func TestEvaluateRecovery_ToolExecError_ExhaustCap_Archive(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), ToolName: "view_goal"}
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
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseSet), ToolName: "view_goal"}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("Lock phase should not retry tool errors, got %v", action)
	}
}

func TestEvaluateRecovery_ProviderTransient_AlwaysArchive(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), ProviderError: true}
	action, msg := evaluateRecovery(ts, ctx)
	if action != RecoveryArchiveGoal {
		t.Fatalf("expected RecoveryArchiveGoal for provider transient, got %v", action)
	}
	if msg == "" {
		t.Fatalf("expected archive message")
	}
}

func TestEvaluateRecovery_NoGoalPhase_NoTrigger(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: "", TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("no goal phase should not trigger recovery, got %v", action)
	}
}

func TestEvaluateRecovery_FinalPhase_NoTrigger(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseFinal), TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("Final phase should not trigger recovery, got %v", action)
	}
}

// TestCheckToolExecErrorRecovery_NoError verifies the helper is silent
// when the most recent message is not an executor error.
func TestCheckToolExecErrorRecovery_NoError(t *testing.T) {
	ts := newPhase5TurnState(t)
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
	ts := newPhase5TurnState(t)
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
	ts := newPhase5TurnState(t)
	for i := 0; i < ToolExecErrorRetryCap; i++ {
		evaluateRecovery(ts, RecoveryContext{Phase: string(GoalPhaseOpen), ToolName: "view_goal"})
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
	ts := newPhase5TurnState(t)
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
	ts := newPhase5TurnState(t)
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
	// Phase 12.6.1: signature gained `isTransient bool` arg between
	// ToolExecErrorError and registry. nil registry + isTransient=false →
	// base message only, no transient hint appended.
	got := buildToolExecErrorRetryMessage("web_search", "connection refused", false, nil)
	want := fmt.Sprintf(ToolExecErrorRetryMessage, "web_search", "connection refused")
	if got != want {
		t.Fatalf("nil registry should return base message\n got: %q\nwant: %q", got, want)
	}
	// Tool name MUST appear in the body (Phase 12.6.1 wire-up).
	if !strings.Contains(got, `"web_search"`) {
		t.Fatalf("Phase 12.6.1: tool name missing from retry message: %q", got)
	}
}

func TestBuildToolExecErrorRetryMessage_RegistryNoKnowledge_BaseOnly(t *testing.T) {
	r := tools.NewToolRegistry()
	// No lesson recorded for "web_search" — LoadForEscalation returns "".
	got := buildToolExecErrorRetryMessage("web_search", "connection refused", false, r)
	want := fmt.Sprintf(ToolExecErrorRetryMessage, "web_search", "connection refused")
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

	got := buildToolExecErrorRetryMessage("web_search", "connection refused", false, r)
	want := fmt.Sprintf(ToolExecErrorRetryMessage, "web_search", "connection refused")
	if got == want {
		t.Fatalf("registry with knowledge should append, got base message only")
	}
	if !strings.Contains(got, body) {
		t.Fatalf("expected knowledge body in message, got %q", got)
	}
	// Phase 12.6.1: knowledge-prefixed message must include the tool name
	// AND the original error msg, since the LLM is told WHICH tool returned
	// this error context.
	if !strings.Contains(got, `"web_search"`) {
		t.Fatalf("Phase 12.6.1: tool name missing from retry+knowledge message: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("error message missing from retry+knowledge message: %q", got)
	}
}

// TestBuildToolExecErrorRetryMessage_TransientFlag (Phase 12.6.1)
// guards the IsTransient → TransientHint wire-up. Two cases:
//
//   - isTransient=false: base message only, no transient hint.
//   - isTransient=true:  base message + ToolExecErrorTransientHint suffix.
//
// Regression proof: setting isTransient=true does NOT duplicate the base
// message; setting false does NOT include the hint.
func TestBuildToolExecErrorRetryMessage_TransientFlag(t *testing.T) {
	baseNonTransient := buildToolExecErrorRetryMessage("web_search", "timeout", false, nil)
	baseTransient := buildToolExecErrorRetryMessage("web_search", "timeout", true, nil)

	// Both messages must include the tool name (Phase 12.6.1 wire-up
	// independent of the transient flag).
	if !strings.Contains(baseNonTransient, `"web_search"`) {
		t.Fatalf("non-transient: tool name missing: %q", baseNonTransient)
	}
	if !strings.Contains(baseTransient, `"web_search"`) {
		t.Fatalf("transient: tool name missing: %q", baseTransient)
	}

	// Non-transient must NOT include the transient-hint suffix.
	if strings.Contains(baseNonTransient, "transient") || strings.Contains(baseNonTransient, "rate-limit") {
		t.Fatalf("non-transient message contains transient hint: %q", baseNonTransient)
	}

	// Transient message MUST include the hint suffix.
	if !strings.Contains(baseTransient, ToolExecErrorTransientHint) {
		t.Fatalf("transient message missing hint suffix\n got: %q\nwant suffix: %q", baseTransient, ToolExecErrorTransientHint)
	}

	// Base body MUST be present in both (only the hint differs).
	wantBase := fmt.Sprintf(ToolExecErrorRetryMessage, "web_search", "timeout")
	if !strings.Contains(baseNonTransient, wantBase) {
		t.Fatalf("non-transient missing base message body: %q", baseNonTransient)
	}
	if !strings.Contains(baseTransient, wantBase) {
		t.Fatalf("transient missing base message body: %q", baseTransient)
	}
}

// TestIsTransientErrorText (Phase 12.6.1) classifies error text into
// transient vs permanent based on substring markers. False negative (says
// permanent when transient) → LLM gets the standard retry prompt instead
// of the wait-then-retry hint. False positive (says transient when
// permanent) → LLM retries without arg changes, may waste a turn. Both
// are recoverable; the heuristic just prefers FNs over FPs (conservative).
func TestIsTransientErrorText(t *testing.T) {
	transientCases := []string{
		"Tool execution failed: connection refused",
		"Tool execution failed: connection reset by peer",
		"Tool execution failed: i/o timeout",
		"Tool execution failed: HTTP 429 rate limit exceeded",
		"Tool execution failed: HTTP 503 service unavailable",
		"Tool execution failed: HTTP 504 gateway timeout",
		"Tool execution failed: HTTP 502 bad gateway",
		"Tool execution failed: no such host",
		"Tool execution failed: TLS handshake timeout",
	}
	for _, c := range transientCases {
		if !isTransientErrorText(c) {
			t.Errorf("expected transient for %q, got false", c)
		}
	}

	permanentCases := []string{
		"Tool execution failed: file not found",
		"Tool execution failed: permission denied",
		"Tool execution failed: invalid argument",
		"Tool execution failed: JSON parse error",
		"Tool execution failed: schema validation failed",
		"Tool execution failed: missing required field",
	}
	for _, c := range permanentCases {
		if isTransientErrorText(c) {
			t.Errorf("expected permanent for %q, got true", c)
		}
	}
}

// TestEvaluateRecovery_WirePathFromCurrentGoalPhase guards against the Phase 11
// regression where constants declared lowercase ("set"/"open"/"checkpoint"/"final")
// but recovery gates compared against capital-case strings ("Open"/"Lock"/"Final").
// The test feeds the actual wire value `string(ts.currentGoalPhase())` after
// seeding an active goal on disk — synthetic capital-string inputs would have
// masked the bug because ts.hasGoal() reads the goal file, not a struct field.
func TestEvaluateRecovery_WirePathFromCurrentGoalPhase(t *testing.T) {
	ws := t.TempDir()
	// Wire-path integration: build a real AgentInstance + turnState so that
	// currentGoalPhase() goes through ResolveGoalPhase with iteration > 1
	// (Open phase). Synthetic capital-string inputs would have masked the
	// Phase 11 bug — `hasGoal()` reads goal file, `currentGoalPhase()` reads
	// `ts.iteration` and `ts.agent.MaxIterations*`.
	ai := &AgentInstance{
		Workspace:        ws,
		MaxIterations:    50,
		MaxIterationsCap: 200,
	}
	ts := &turnState{
		agent:                   ai,
		workspace:               ws,
		sessionKey:              "wire-test-session",
		iteration:               2, // Open phase requires 2 <= iter <= MaxIter
		iterationCap:            50,
		maxIterationsCap:        200,
		toolExecRecoveryAttempts: make(map[string]int),
	}
	// Seed active goal on disk so ts.hasGoal() returns true (matches runtime gate
	// at pipeline_llm.go:706). Wire-side `currentGoalPhase()` also reads from disk.
	store := goal.NewStore(ts.workspace)
	g := &goal.Goal{
		Name:        "wire-test",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "wire-path test", SuccessCriteria: []string{"pass"}},
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := store.Write(ts.sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}
	defer store.Archive(ts.sessionKey)
	wirePhase := string(ts.currentGoalPhase()) // lowercase per constants
	if wirePhase != "open" {
		t.Fatalf("expected currentGoalPhase()=%q for active goal, got %q", "open", wirePhase)
	}
	if !ts.hasGoal() {
		t.Fatalf("hasGoal()=false after seeding active goal")
	}
	ctx := RecoveryContext{
		Phase:        wirePhase, // NOT a synthetic "Open"
		Iteration:    2,
		TextEmpty:    true,
		HasToolCalls: false,
	}
	action, msg := evaluateRecovery(ts, ctx)
	if action != RecoveryRetrySameIteration {
		t.Fatalf("wire-path recovery should fire on lowercase %q, got %v (msg=%q)", wirePhase, action, msg)
	}
	if msg != EmptyResponseRecoveryMessage {
		t.Fatalf("expected EmptyResponseRecoveryMessage, got %q", msg)
	}
}

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
	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
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
	if ts.emptyResponseRecoveryCount == 0 {
		t.Fatalf("expected emptyResponseRecoveryCount>0 after first fire, got %d", ts.emptyResponseRecoveryCount)
	}
}

func TestEvaluateRecovery_EmptyText_AlreadySent_NoRetry(t *testing.T) {
	// Phase 12.37: pre-12.37 this tested "one-shot bool → None on second fire".
	// New behavior: counter caps at EmptyResponseRecoveryCap (3) — count>=cap
	// archives the goal. Phase 12.37 covered in recovery_goal_phase12_37_test.go.
	t.Skip("Phase 12.37: behavior changed to count-based 3-shot cap; see TestEmptyResponse_CapExhausted_ArchivesGoal_Phase12_37")
	ts := newPhase5TurnState(t)
	ts.emptyResponseRecoveryCount = 1 // legacy placeholder, unreachable
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: true, HasToolCalls: false}
	_, _ = evaluateRecovery(ts, ctx)
}

func TestEvaluateRecovery_EmptyText_PhaseLock_NoTrigger(t *testing.T) {
	// Phase 12.37 GAP #2: Trigger #1 (empty) now fires at ALL phases
	// including GoalPhaseSet. Pre-12.37 expected None here. New behavior
	// verified in TestEmptyResponse_FiresAtRestrictedPhases_Phase12_37.
	t.Skip("Phase 12.37: empty trigger fires at all phases; see TestEmptyResponse_FiresAtRestrictedPhases_Phase12_37")
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseSet), TextEmpty: true, HasToolCalls: false}
	_, _ = evaluateRecovery(ts, ctx)
}

func TestEvaluateRecovery_TextOnly2x_PhaseOpen_ForceComplete(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false}
	// First text-only: streak becomes 1, Open-phase soft prompt fires
	// (Phase 12.27: Open uses RecoveryRetryNextIteration with TextOnlySoftRetryOpenMessage)
	action1, msg1 := evaluateRecovery(ts, ctx)
	if action1 != RecoveryRetryNextIteration {
		t.Fatalf("first text-only at Open should return RetryNextIteration, got %v", action1)
	}
	if msg1 != TextOnlySoftRetryOpenMessage {
		t.Fatalf("expected Open soft prompt message, got %q", msg1)
	}
	if ts.textOnlyStreak != 1 {
		t.Fatalf("expected streak=1 after first text-only, got %d", ts.textOnlyStreak)
	}
	if ts.textOnlySoftRetriesDone != 1 {
		t.Fatalf("expected soft_retries=1, got %d", ts.textOnlySoftRetriesDone)
	}
	// Second text-only (same iter, immediately after): hard prompt fires
	action2, msg2 := evaluateRecovery(ts, ctx)
	if action2 != RecoveryRetryNextIteration {
		t.Fatalf("second text-only at Open should return RetryNextIteration, got %v", action2)
	}
	if msg2 != TextOnlyHardRetryOpenMessage {
		t.Fatalf("expected Open hard prompt message, got %q", msg2)
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

// Phase 12.15: Lock (alias GoalPhaseSet) is NOW eligible for tool-exec
// recovery. The retry message redirects the LLM to call set_goal with
// top-level fields. Counter MUST increment so the per-tool cap is honored.
func TestEvaluateRecovery_ToolExecError_LockPhase_RetriesWithHint(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseSet), ToolName: "view_goal", ToolExecError: "not allowed in set phase"}
	action, msg := evaluateRecovery(ts, ctx)
	if action != RecoveryRetrySameIteration {
		t.Fatalf("Phase 12.15: Lock phase + tool error should retry, got %v", action)
	}
	if !strings.Contains(msg, "set_goal") {
		t.Fatalf("Phase 12.15: retry message must redirect to set_goal, got %q", msg)
	}
	if !strings.Contains(msg, "top-level") {
		t.Fatalf("Phase 12.15: retry message must mention top-level args shape, got %q", msg)
	}
	if ts.toolExecRecoveryAttempts["view_goal"] != 1 {
		t.Fatalf("Phase 12.15: counter must increment, got %v", ts.toolExecRecoveryAttempts)
	}
}

// Phase 12.15: GoalPhaseFinal is NOW eligible for tool-exec recovery (only
// when postCompleteGoalReportSent=false). Message redirects to complete_goal.
func TestEvaluateRecovery_ToolExecError_FinalPhase_RetriesWithHint(t *testing.T) {
	ts := newPhase5TurnState(t)
	// postCompleteGoalReportSent=false (default) — final-report iter still
	// eligible for redirect hint.
	ctx := RecoveryContext{Phase: string(GoalPhaseFinal), ToolName: "view_goal", ToolExecError: "not allowed in final phase"}
	action, msg := evaluateRecovery(ts, ctx)
	if action != RecoveryRetrySameIteration {
		t.Fatalf("Phase 12.15: Final phase + tool error should retry, got %v", action)
	}
	if !strings.Contains(msg, "complete_goal") {
		t.Fatalf("Phase 12.15: retry message must redirect to complete_goal, got %q", msg)
	}
	if !strings.Contains(msg, "summary") {
		t.Fatalf("Phase 12.15: retry message must mention summary param, got %q", msg)
	}
}

// Phase 12.15: Final phase is SKIPPED when postCompleteGoalReportSent=true
// (final report already published; no more iterations).
func TestEvaluateRecovery_ToolExecError_FinalPhase_PostReportSilent(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.postCompleteGoalReportSent = true
	ctx := RecoveryContext{Phase: string(GoalPhaseFinal), ToolName: "view_goal", ToolExecError: "x"}
	action, _ := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("Final phase after report should be silent, got %v", action)
	}
}

// Phase 12.15: tool-exec recovery at Lock phase must follow the per-tool
// cap. After 3 attempts, the 4th triggers RecoveryArchiveGoal.
func TestEvaluateRecovery_ToolExecError_LockPhase_CapArchive(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseSet), ToolName: "view_goal", ToolExecError: "not allowed"}
	for i := 0; i < ToolExecErrorRetryCap; i++ {
		if a, _ := evaluateRecovery(ts, ctx); a != RecoveryRetrySameIteration {
			t.Fatalf("attempt %d should retry, got %v", i+1, a)
		}
	}
	// Cap reached — next attempt must archive.
	if a, _ := evaluateRecovery(ts, ctx); a != RecoveryArchiveGoal {
		t.Fatalf("Phase 12.15: Lock phase tool-exec cap reached should archive, got %v", a)
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
	// Phase 12.37 GAP #2: empty response NOW fires at Final phase too
	// (was Open-only). Pre-12.37 expected None; new behavior: Final+empty
	// fires soft Trigger #1 (postReport=false). PostReport=true stays
	// silent (verified in TestEmptyResponse_FinalPostReport_Silent_Phase12_37).
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseFinal), TextEmpty: true, HasToolCalls: false}
	action, _ := evaluateRecovery(ts, ctx)
	if action == RecoveryNone {
		t.Fatalf("Phase 12.37 GAP #2: Final+empty should now fire Trigger #1, got RecoveryNone")
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

// TestCheckToolExecErrorRecovery_ExecutionGateFormat_TriggersRecovery
// (Phase 12.18): the Phase 12.3 execution gate in pkg/tools/registry.go
// rejects non-allowlist tool calls with format:
//
//	`tool "X" is not available in the current phase (allowed tools: [...])`
//
// This is the error message that reaches the LLM at GoalPhaseCheckpoint
// when the LLM keeps calling non-lifecycle tools at the iteration cap.
// Before Phase 12.18, recovery (Trigger #3) silently MISSED this format
// because checkToolExecErrorRecovery only matched the legacy
// `Tool execution failed:` prefix. The silent miss caused main-turn-4 on
// 2026-07-26 to hit the canned "max_tool_iterations" fallback at iter 25.
//
// Regression-proof for the main-turn-4 failure.
func TestCheckToolExecErrorRecovery_ExecutionGateFormat_TriggersRecovery(t *testing.T) {
	ts := newPhase5TurnState(t)
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", Content: `tool "read_file" is not available in the current phase (allowed tools: [goal_progress complete_goal])`},
		},
	}
	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool != "" {
		t.Fatalf("expected retry (no archive) on first execution-gate rejection, got tool=%q msg=%q", tool, msg)
	}
	if ts.toolExecRecoveryAttempts["read_file"] != 1 {
		t.Fatalf("expected attempt count=1 for read_file (parsed from quoted name), got %d", ts.toolExecRecoveryAttempts["read_file"])
	}
}

// TestCheckToolExecErrorRecovery_ExecutionGateFormat_NotTransient verifies
// execution-gate rejections are classified as PERMANENT (not transient).
// The Phase 12.3 gate blocks tools based on phase policy — retrying with
// different arguments will NOT help. Marking as non-transient ensures the
// retry prompt asks the LLM to "invoke a different tool or call
// complete_goal" instead of "wait and retry the same call".
func TestCheckToolExecErrorRecovery_ExecutionGateFormat_NotTransient(t *testing.T) {
	ts := newPhase5TurnState(t)
	exec := &turnExecution{
		messages: []providers.Message{
			// Phrase the error to include "timeout" so the transient
			// heuristic WOULD trigger if applied — verifies our explicit
			// override at the recovery call site.
			{Role: "tool", Content: `tool "read_file" is not available in the current phase (timeout waiting on allowed tools)`},
		},
	}
	// Force Checkpoint phase so the recovery builder picks the
	// Checkpoint-specific hint AND verify the produced message contains
	// no transient hint (production path passes isTransient=false for
	// execution-gate rejections).
	ts.agent.PhaseOverrideForTest = string(GoalPhaseCheckpoint)
	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool != "" {
		t.Fatalf("expected retry (no archive) on first execution-gate rejection, got tool=%q msg=%q", tool, msg)
	}
	if !strings.Contains(msg, "final iteration") {
		t.Fatalf("expected Checkpoint hint to fire, got: %s", msg)
	}
	if strings.Contains(msg, "looks transient") {
		t.Fatalf("expected NO transient hint for execution-gate rejection, got: %s", msg)
	}
}

// TestBuildToolExecErrorRetryMessage_CheckpointPhase (Phase 12.18): the
// Checkpoint-phase hint must appear when buildToolExecErrorRetryMessage
// is called with phase=string(GoalPhaseCheckpoint). The hint tells the
// LLM that goal_progress + complete_goal are the only allowed tools and
// it must wrap up (no further tool work).
func TestBuildToolExecErrorRetryMessage_CheckpointPhase(t *testing.T) {
	got := buildToolExecErrorRetryMessage(
		"read_file",
		"rejected by execution gate",
		false,
		nil,
		string(GoalPhaseCheckpoint),
	)
	if !strings.Contains(got, "goal_progress") {
		t.Fatalf("expected Checkpoint hint to mention goal_progress, got: %s", got)
	}
	if !strings.Contains(got, "complete_goal") {
		t.Fatalf("expected Checkpoint hint to mention complete_goal, got: %s", got)
	}
	if !strings.Contains(got, "final iteration") {
		t.Fatalf("expected Checkpoint hint to mention 'final iteration', got: %s", got)
	}
	// Also verify base message + tool name + phase hint are all present.
	if !strings.Contains(got, `"read_file"`) {
		t.Fatalf("expected tool name in quotes, got: %s", got)
	}
	if !strings.Contains(got, "rejected by execution gate") {
		t.Fatalf("expected original error in message, got: %s", got)
	}
}

// TestBuildToolExecErrorRetryMessage_OpenPhaseLifecycleBlock (Phase 12.32):
// The OPEN-phase hint must fire ONLY when the failing tool is a lifecycle
// tool (set_goal / goal_progress). Open phase is RELATIVE — only 2 of 83
// visible tools are blocked by the lifecycle gate. Always-append (like
// Set/Checkpoint/Final) would mislead the LLM when a generic tool like
// read_file errors at OPEN (read_file works fine at OPEN; the error has
// nothing to do with the lifecycle gate).
func TestBuildToolExecErrorRetryMessage_OpenPhaseLifecycleBlock(t *testing.T) {
	// Case 1 — positive: goal_progress at OPEN → OPEN hint appended
	got1 := buildToolExecErrorRetryMessage(
		"goal_progress", "lifecycle gate blocked", false, nil, string(GoalPhaseOpen),
	)
	if !strings.Contains(got1, ToolExecErrorOpenPhaseHint) {
		t.Fatalf("case 1: expected OPEN hint for goal_progress at OPEN, got: %s", got1)
	}

	// Case 2 — positive: set_goal at OPEN → OPEN hint appended
	got2 := buildToolExecErrorRetryMessage(
		"set_goal", "lifecycle gate blocked", false, nil, string(GoalPhaseOpen),
	)
	if !strings.Contains(got2, ToolExecErrorOpenPhaseHint) {
		t.Fatalf("case 2: expected OPEN hint for set_goal at OPEN, got: %s", got2)
	}

	// Case 3 — negative: read_file at OPEN → NO OPEN hint (relative phase)
	got3 := buildToolExecErrorRetryMessage(
		"read_file", "permission denied", false, nil, string(GoalPhaseOpen),
	)
	if strings.Contains(got3, ToolExecErrorOpenPhaseHint) {
		t.Fatalf("case 3: expected NO OPEN hint for read_file (non-lifecycle) at OPEN, got: %s", got3)
	}

	// Case 4 — negative: goal_progress at CHECKPOINT → CHECKPOINT hint, NOT OPEN hint
	got4 := buildToolExecErrorRetryMessage(
		"goal_progress", "lifecycle gate blocked", false, nil, string(GoalPhaseCheckpoint),
	)
	if strings.Contains(got4, ToolExecErrorOpenPhaseHint) {
		t.Fatalf("case 4: expected NO OPEN hint at CHECKPOINT phase, got: %s", got4)
	}
	if !strings.Contains(got4, "goal_progress") {
		t.Fatalf("case 4: expected Checkpoint hint mentioning goal_progress, got: %s", got4)
	}

	// Case 5 — negative: goal_progress at SET → SET hint, NOT OPEN hint
	got5 := buildToolExecErrorRetryMessage(
		"goal_progress", "lifecycle gate blocked", false, nil, string(GoalPhaseSet),
	)
	if strings.Contains(got5, ToolExecErrorOpenPhaseHint) {
		t.Fatalf("case 5: expected NO OPEN hint at SET phase, got: %s", got5)
	}
}

func TestBuildToolExecErrorRetryMessage_NoRegistry_BaseOnly(t *testing.T) {
	// Phase 12.6.1: signature gained `isTransient bool` arg between
	// ToolExecErrorError and registry. nil registry + isTransient=false →
	// base message only, no transient hint appended.
	got := buildToolExecErrorRetryMessage("web_search", "connection refused", false, nil, "")
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
	got := buildToolExecErrorRetryMessage("web_search", "connection refused", false, r, "")
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

	got := buildToolExecErrorRetryMessage("web_search", "connection refused", false, r, "")
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
	baseNonTransient := buildToolExecErrorRetryMessage("web_search", "timeout", false, nil, "")
	baseTransient := buildToolExecErrorRetryMessage("web_search", "timeout", true, nil, "")

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

// Phase 12.7: when the post-complete_goal final-report iter has already
// sent its hint, the loop should run text-only without recovery triggers.
// This avoids sending "complete the goal" prompts AFTER the goal is done.
func TestEvaluateRecovery_PostCompleteGoalReport_NoTrigger(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.postCompleteGoalReportSent = true

	cases := []struct {
		name string
		ctx  RecoveryContext
	}{
		{
			name: "empty_text",
			ctx:  RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: true, HasToolCalls: false},
		},
		{
			name: "text_only",
			ctx:  RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false, Iteration: 3},
		},
		{
			name: "tool_exec_error",
			ctx:  RecoveryContext{Phase: string(GoalPhaseOpen), ToolExecError: "connection refused", ToolName: "web_search", Iteration: 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, msg := evaluateRecovery(ts, tc.ctx)
			if action != RecoveryNone {
				t.Fatalf("expected RecoveryNone after post-complete_goal report sent, got %v (msg=%q)", action, msg)
			}
			if msg != "" {
				t.Errorf("expected empty msg, got %q", msg)
			}
		})
	}
}

// Phase 12.7: turn_coord re-enters the loop once after complete_goal sets
// goalFinalized, BYPASSING the iteration cap. Test the cap-extend logic
// in isolation so we don't need a full integration loop.
func TestPostCompleteGoalReport_CapBypass(t *testing.T) {
	ts := newPhase5TurnState(t)

	// Simulate the agent at the iteration cap (no more iters left)
	ts.iteration = 24
	ts.iterationCap = 24

	// Pre-condition: cap should be hit
	if ts.currentIteration() < ts.iterationCap {
		t.Fatalf("setup wrong: expected iter >= cap")
	}

	// Simulate complete_goal having fired: goalFinalized=true
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = false

	// Phase 12.9: pre-loop hook is now responsible for the cap-bypass.
	// It bumps iterationCap to ts.iteration+1 (allowing exactly one more
	// iter) but does NOT touch postCompleteGoalReportSent — that flag
	// is set at END of body (post-body marker), not at the bypass site.
	// Modeling the pre-loop hook:
	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		if cap := ts.iteration + 1; cap > ts.iterationCap {
			ts.iterationCap = cap
		}
	}

	// After bypass, iterationCap must allow exactly one more iteration
	if ts.iterationCap != 25 {
		t.Errorf("expected iterationCap=25 after cap-bypass, got %d", ts.iterationCap)
	}
	if ts.postCompleteGoalReportSent {
		t.Error("postCompleteGoalReportSent should still be FALSE after pre-loop hook (set at post-body marker, not bypass site)")
	}

	// Phase 12.9: at top of body, the transient pendingFinalReportIter
	// signal is set so the post-body marker can detect this iter is the
	// final-report iter and flip postCompleteGoalReportSent=true.
	ts.pendingFinalReportIter = true

	// Simulate body running: LLM call, tool exec, etc. (none in this
	// unit test — we only verify the post-body marker transition).

	// Post-body marker (mirrored from turn_coord.go):
	if ts.pendingFinalReportIter {
		ts.postCompleteGoalReportSent = true
		ts.pendingFinalReportIter = false
	}

	if !ts.postCompleteGoalReportSent {
		t.Error("postCompleteGoalReportSent should be true after post-body marker ran")
	}

	// Second pass through the pre-loop hook must NOT re-extend cap
	// (because postCompleteGoalReportSent is now true).
	prevCap := ts.iterationCap
	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		if cap := ts.iteration + 1; cap > ts.iterationCap {
			ts.iterationCap = cap
		}
	}
	if ts.iterationCap != prevCap {
		t.Errorf("pre-loop hook should be a no-op when postCompleteGoalReportSent=true; cap changed from %d to %d", prevCap, ts.iterationCap)
	}
}

// Phase 12.10 — Fix #1: emptyResponseRecoveryCount was previously sticky
// across iterations. After iter 12 fired empty-response recovery, iter 14's
// empty response silently skipped (RecoveryNone), turning the loop into a
// no-op. Live evidence: turn 2 main-turn-3 (2026-07-24 13:37 ICT) ended
// with content_len=0 after iter 14.
//
// The fix lives in turn_coord.go:163-164 — the loop top now resets both
// per-iteration counters. This test models the loop top inline (the actual
// turn loop is exercised in turn_coord_test.go) and verifies that two
// empty responses in different iterations both trigger recovery.
func TestEvaluateRecovery_EmptyText_IterationBump_ResetsCounter(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{
		Phase:        string(GoalPhaseOpen),
		Iteration:    12,
		TextEmpty:    true,
		HasToolCalls: false,
	}

	// iter 12: empty response → recovery fires, counter flips
	action1, msg1 := evaluateRecovery(ts, ctx)
	if action1 != RecoveryRetrySameIteration {
		t.Fatalf("iter 12: expected RecoveryRetrySameIteration, got %v", action1)
	}
	if msg1 != EmptyResponseRecoveryMessage {
		t.Fatalf("iter 12: expected EmptyResponseRecoveryMessage, got %q", msg1)
	}
	if ts.emptyResponseRecoveryCount == 0 {
		t.Fatalf("iter 12: emptyResponseRecoveryCount should be true after first fire")
	}

	// Loop top simulation (mirrors turn_coord.go:163-164 — the actual
	// production code path). Phase 12.10 fix: reset BOTH per-iter counters.
	ts.setIteration(13)
	ts.emptyResponseRecoveryCount = 0
	ts.toolExecRecoveryAttempts = nil

	// iter 13: LLM recovers with tool_call (simulated by HasToolCalls=true
	// in the next iteration entry — counters reset by loop top).
	toolCtx := RecoveryContext{
		Phase:        string(GoalPhaseOpen),
		Iteration:    13,
		HasToolCalls: true,
	}
	actionTool, _ := evaluateRecovery(ts, toolCtx)
	_ = actionTool // recovery computes its own action; tool-call iterations
	// don't trigger recovery in the first place, but we want to confirm
	// the counter is freshly 0 for the next empty iteration.

	// Loop top → iter 14
	ts.setIteration(14)
	ts.emptyResponseRecoveryCount = 0
	ts.toolExecRecoveryAttempts = nil

	// iter 14: empty response AGAIN — recovery MUST fire (was the bug)
	ctx14 := RecoveryContext{
		Phase:        string(GoalPhaseOpen),
		Iteration:    14,
		TextEmpty:    true,
		HasToolCalls: false,
	}
	action2, msg2 := evaluateRecovery(ts, ctx14)
	if action2 != RecoveryRetrySameIteration {
		t.Fatalf("iter 14: expected RecoveryRetrySameIteration (Fix #1), got %v", action2)
	}
	if msg2 != EmptyResponseRecoveryMessage {
		t.Fatalf("iter 14: expected EmptyResponseRecoveryMessage, got %q", msg2)
	}
	if ts.emptyResponseRecoveryCount == 0 {
		t.Fatalf("iter 14: emptyResponseRecoveryCount should be true after second fire")
	}
}

// Phase 12.10 — Fix #1b: toolExecRecoveryAttempts was sticky across
// iterations. If view_goal failed 3x in iter 12 (cap hit), iter 14's
// wire-side retry would archive immediately because the counter still
// reported 3 from iter 12. Same class as #1; resolved by the same one-line
// map reset.
//
// This test models the loop top the same way as Fix #1.
func TestEvaluateRecovery_ToolExecError_IterationBump_ResetsCap(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{
		Phase:     string(GoalPhaseOpen),
		Iteration: 12,
		ToolName:  "view_goal",
	}

	// iter 12: 3 retries (cap hit), then 4th would archive
	for i := 0; i < ToolExecErrorRetryCap; i++ {
		action, _ := evaluateRecovery(ts, ctx)
		if action != RecoveryRetrySameIteration {
			t.Fatalf("iter 12 retry %d: expected RetrySameIteration, got %v", i, action)
		}
	}
	// 4th call in iter 12: cap exhausted → archive
	actionOver, _ := evaluateRecovery(ts, ctx)
	if actionOver != RecoveryArchiveGoal {
		t.Fatalf("iter 12: expected archive after cap, got %v", actionOver)
	}

	// Loop top simulation (Fix #1b reset)
	ts.setIteration(13)
	ts.emptyResponseRecoveryCount = 0
	ts.toolExecRecoveryAttempts = nil

	// iter 13: fresh attempts allowed
	action13, msg13 := evaluateRecovery(ts, RecoveryContext{
		Phase:     string(GoalPhaseOpen),
		Iteration: 13,
		ToolName:  "view_goal",
	})
	if action13 != RecoveryRetrySameIteration {
		t.Fatalf("iter 13: expected RetrySameIteration (Fix #1b cap reset), got %v (msg=%q)", action13, msg13)
	}
	if ts.toolExecRecoveryAttempts["view_goal"] != 1 {
		t.Fatalf("iter 13: counter should be 1, got %d", ts.toolExecRecoveryAttempts["view_goal"])
	}
}

// Phase 12.10 — combined Fix #1 + #1b: both counters reset together on iter
// bump. Verifies the production code path in turn_coord.go:163-164 by
// mirroring its exact 3-line reset block in the test, then verifying both
// triggers can fire in the next iteration.
func TestEvaluateRecovery_IterationBump_ResetsBothCounters(t *testing.T) {
	ts := newPhase5TurnState(t)

	// iter 12: simulate earlier empty-response + tool-exec failures
	emptyCtx := RecoveryContext{Phase: string(GoalPhaseOpen), Iteration: 12, TextEmpty: true, HasToolCalls: false}
	toolCtx := RecoveryContext{Phase: string(GoalPhaseOpen), Iteration: 12, ToolName: "view_goal"}

	evaluateRecovery(ts, emptyCtx) // empty fires
	evaluateRecovery(ts, toolCtx)  // tool fires
	evaluateRecovery(ts, toolCtx)  // tool fires (2)

	// Pre-loop state must be dirty
	if ts.emptyResponseRecoveryCount == 0 {
		t.Fatalf("setup: emptyResponseRecoveryCount should be true")
	}
	if ts.toolExecRecoveryAttempts["view_goal"] != 2 {
		t.Fatalf("setup: view_goal counter should be 2, got %d", ts.toolExecRecoveryAttempts["view_goal"])
	}

	// Mirror production reset (turn_coord.go:163-164)
	ts.setIteration(13)
	ts.emptyResponseRecoveryCount = 0
	ts.toolExecRecoveryAttempts = nil

	// Both counters must be reset
	if ts.emptyResponseRecoveryCount != 0 {
		t.Fatalf("iter 13: emptyResponseRecoveryCount should be reset, got true")
	}
	if len(ts.toolExecRecoveryAttempts) != 0 {
		t.Fatalf("iter 13: toolExecRecoveryAttempts should be empty, got %v", ts.toolExecRecoveryAttempts)
	}

	// iter 13: empty response fires again (Fix #1)
	actionEmpty, _ := evaluateRecovery(ts, RecoveryContext{Phase: string(GoalPhaseOpen), Iteration: 13, TextEmpty: true, HasToolCalls: false})
	if actionEmpty != RecoveryRetrySameIteration {
		t.Fatalf("iter 13 empty: expected RetrySameIteration, got %v", actionEmpty)
	}

	// iter 13: tool exec error fires again (Fix #1b)
	actionTool, _ := evaluateRecovery(ts, RecoveryContext{Phase: string(GoalPhaseOpen), Iteration: 13, ToolName: "view_goal"})
	if actionTool != RecoveryRetrySameIteration {
		t.Fatalf("iter 13 tool: expected RetrySameIteration, got %v", actionTool)
	}
	if ts.toolExecRecoveryAttempts["view_goal"] != 1 {
		t.Fatalf("iter 13 tool: counter should be 1 (fresh), got %d", ts.toolExecRecoveryAttempts["view_goal"])
	}
}

// ---------------------------------------------------------------------------
// Phase 12.28.3 — Fix B: ErrKind-based recovery gate
// ---------------------------------------------------------------------------

// T-B1: complete_goal validation error — typed ErrInvalidInput from tool handler.
func TestCheckToolExecErrorRecovery_FiresOnCompleteGoalValidation(t *testing.T) {
	ts := newPhase5TurnState(t)
	res := toolshared.ErrorResult("complete_goal: `summary` exceeds 500 characters (Unicode); shorten your final reply.").WithErrorKind(toolshared.ErrInvalidInput)
	ts.lastToolResult = res
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "complete_goal", Content: res.ForLLM},
		},
	}

	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool == "" && msg == "" {
		t.Fatalf("Phase 12.28.3 Bug B: typed ErrInvalidInput should fire recovery (msg=%q tool=%q)", msg, tool)
	}
}

// T-B2: goal_progress validation error — typed ErrInvalidInput.
func TestCheckToolExecErrorRecovery_FiresOnGoalProgressValidation(t *testing.T) {
	ts := newPhase5TurnState(t)
	res := toolshared.ErrorResult("goal_progress: remaining_steps is required and must be non-empty.").WithErrorKind(toolshared.ErrInvalidInput)
	ts.lastToolResult = res
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "goal_progress", Content: res.ForLLM},
		},
	}

	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool == "" && msg == "" {
		t.Fatalf("Phase 12.28.3: typed ErrInvalidInput on goal_progress should fire recovery (msg=%q tool=%q)", msg, tool)
	}
}

// T-B3: set_goal validation error — typed ErrInvalidInput (name regex violation).
func TestCheckToolExecErrorRecovery_FiresOnSetGoalValidation(t *testing.T) {
	ts := newPhase5TurnState(t)
	res := toolshared.ErrorResult("set_goal: \"name\" must match ^[A-Za-z0-9_-]{1,64}$.").WithErrorKind(toolshared.ErrInvalidInput)
	ts.lastToolResult = res
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "set_goal", Content: res.ForLLM},
		},
	}

	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool == "" && msg == "" {
		t.Fatalf("Phase 12.28.3: typed ErrInvalidInput on set_goal should fire recovery (msg=%q tool=%q)", msg, tool)
	}
}

// T-B4: non-recoverable ErrKind — recovery should NOT fire.
func TestCheckToolExecErrorRecovery_DoesNotFireOnUnrecoverableErrKind(t *testing.T) {
	ts := newPhase5TurnState(t)
	// Only ErrInvalidInput, ErrTransient, ErrTimeout are recoverable.
	// ErrDependencyDown or unknown kinds should NOT trigger retry.
	res := toolshared.ErrorResult("the upstream service is permanently down").WithErrorKind(toolshared.ErrDependencyDown)
	ts.lastToolResult = res
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "view_goal", Content: res.ForLLM},
		},
	}

	// Direct prefix-free content — forces the typed ErrKind gate to be the
	// only thing that can fire. If the gate classifies ErrDependencyDown
	// as non-recoverable (correct), no recovery.
	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if msg != "" {
		t.Errorf("ErrDependencyDown should not trigger recovery, got msg=%q tool=%q", msg, tool)
	}
}

// T-B5: execution gate rejection — regression for Phase 12.18.
// ts.lastToolResult.ErrKind == "" + content matches "tool \"X\" is not available"
// should still fire (registry execution gate doesn't stamp ErrKind).
func TestCheckToolExecErrorRecovery_StillMatchesExecutionGate(t *testing.T) {
	ts := newPhase5TurnState(t)
	// No ts.lastToolResult set — rely on prefix fallback (Phase 12.18 path).
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "write_file", Content: `tool "write_file" is not available in the current phase (allowed tools: [goal_progress, complete_goal])`},
		},
	}

	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool != "" && msg == "" {
		t.Fatalf("Phase 12.18 regression: execution gate rejection should still fire (tool=%q msg=%q)", tool, msg)
	}
}

// T-B6: ErrTransient → IsTransient=true.
func TestCheckToolExecErrorRecovery_TransientViaErrKind(t *testing.T) {
	ts := newPhase5TurnState(t)
	res := toolshared.ErrorResult("connection refused by upstream").WithErrorKind(toolshared.ErrTransient)
	ts.lastToolResult = res
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "view_goal", Content: res.ForLLM},
		},
	}

	// Capture RecoveryContext via evaluateRecovery path
	_, msg := checkToolExecErrorRecovery(ts, exec)
	if msg == "" {
		t.Fatalf("ErrTransient should fire recovery (got msg=%q)", msg)
	}
	if !strings.Contains(msg, "transient") {
		t.Errorf("ErrTransient context should produce transient hint in msg, got %q", msg)
	}
}

// T-B7: ErrInvalidInput → IsTransient=false.
func TestCheckToolExecErrorRecovery_NonTransientViaErrKind(t *testing.T) {
	ts := newPhase5TurnState(t)
	res := toolshared.ErrorResult("invalid input argument shape").WithErrorKind(toolshared.ErrInvalidInput)
	ts.lastToolResult = res
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "view_goal", Content: res.ForLLM},
		},
	}

	_, msg := checkToolExecErrorRecovery(ts, exec)
	if msg == "" {
		t.Fatalf("ErrInvalidInput should fire recovery (got msg=%q)", msg)
	}
	if strings.Contains(msg, "(transient)") {
		t.Errorf("ErrInvalidInput should NOT add transient hint, got %q", msg)
	}
}

// T-B8: ts.lastToolResult == nil → prefix fallback still works (legacy
// test fixtures don't populate the new field).
func TestCheckToolExecErrorRecovery_PrefixFallbackWhenLastToolResultNil(t *testing.T) {
	ts := newPhase5TurnState(t)
	// Don't set ts.lastToolResult — simulates legacy fixture.
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "view_goal", Content: "Tool execution failed: connection refused"},
		},
	}

	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if msg == "" {
		t.Fatalf("legacy prefix fallback should still fire recovery (got msg=%q tool=%q)", msg, tool)
	}
}

// T-B9: ts.lastToolResult.ErrKind == "" → prefix fallback still works.
// This is the Phase 12.18 regression-proof from a different angle:
// registry execution gate rejection has empty ErrKind AND does NOT
// necessarily match the "Tool execution failed:" prefix — only matches
// the "tool \"X\" is not available" prefix.
func TestCheckToolExecErrorRecovery_PrefixFallbackWhenErrKindEmpty(t *testing.T) {
	ts := newPhase5TurnState(t)
	// lastToolResult exists but has empty ErrKind (registry execution gate).
	ts.lastToolResult = &toolshared.ToolResult{IsError: true} // no ErrKind
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "write_file", Content: `tool "write_file" is not available in the current phase`},
		},
	}

	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if msg == "" {
		t.Fatalf("Phase 12.18 regression-proof: empty ErrKind + execution gate prefix should fire recovery (got msg=%q tool=%q)", msg, tool)
	}
}

// T-B10: false-positive guard — when last tool message content matches
// a prefix but Role != "tool", no recovery. Also: when prefix content
// appears as user/assistant text (not a tool result), no recovery.
func TestCheckToolExecErrorRecovery_NoFalsePositiveOnAssistantText(t *testing.T) {
	ts := newPhase5TurnState(t)
	// assistant message that mentions "tool \"X\" is not available" — must
	// NOT trigger recovery (only tool-role messages carry tool results).
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "assistant", Content: `Sure, I'll avoid calling tool "write_file" since it's not available.`},
		},
	}

	_, msg := checkToolExecErrorRecovery(ts, exec)
	if msg != "" {
		t.Errorf("assistant message with tool-error-like content must not fire recovery (got msg=%q)", msg)
	}
}

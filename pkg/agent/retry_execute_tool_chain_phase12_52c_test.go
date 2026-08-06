// Package agent — Phase 12.52c tests (12.52b post-ship review follow-ups).
//
// F-A: path B — the `if exhausted` block in retryExecuteToolChain (tool-exec
// error ×3 → cap hit → finalize + ControlBreak) had no direct wire test;
// the 12.52b F1 test only exercised path A (RecoveryArchiveGoal inside the
// closure finalizes first, so the exhausted block skips via hasGoal=false).
//
// F-D: after archive, lastPhaseStuckError must carry the real attempt count
// (it is never reset — verified by grep — so it persists into the next turn),
// and phaseStuckFallbackMessage must never render "failed 0 attempt(s)" when
// the per-iteration counters were reset at the next turn boundary.
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// =============================================================================
// F-A (T1) — tool-exec error exhaustion → exhausted block finalizes the goal
// with the phase-stuck reason and returns ControlBreak.
func TestRetryChainExhaustion_ToolExecError_ArchivesInExhaustedBlock(t *testing.T) {
	tc := providers.ToolCall{
		ID:        "tc-1",
		Name:      "goal_progress",
		Arguments: map[string]any{"completed_steps": []string{"a"}, "remaining_steps": []string{"b"}},
	}
	resp := &providers.LLMResponse{
		Content:      "retrying",
		ToolCalls:    []providers.ToolCall{tc},
		FinishReason: "tool_calls",
	}
	// One tool-call response per RecallLLM: attempt 0 + retries + final
	// closure run. Extra responses are harmless (never reached).
	provider := &sequenceProvider{responses: []*providers.LLMResponse{resp, resp, resp, resp, resp}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	p := NewPipeline(al)
	// Every execution fails → each attempt ends in a tool-exec error.
	fake := &fakeExecutor{
		returnControl:  ToolControlContinue,
		appendContent:  "Tool execution failed: validation error",
		appendIsError:  true,
		appendToolName: "goal_progress",
	}
	p.SetToolExecutor(fake)

	ts, exec := setupRetryChainTestTurnState(t, al, p)
	// setupRetryChainTestTurnState calls SkipGoalArchiveForTest which makes
	// finalizeGoalOnTurnEnd a no-op; this test needs the REAL finalize path.
	ts.agent.SkipGoalArchiveForTest = false
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	ctrl, err := p.retryExecuteToolChain(
		context.Background(), context.Background(), ts, exec, 1,
		"hint", []string{"goal_progress", "complete_goal"}, "checkpoint")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ctrl != ControlBreak {
		t.Fatalf("ctrl = %v, want ControlBreak (exhausted block must break out)", ctrl)
	}
	if !ts.goalArchiveRequested {
		t.Fatal("goalArchiveRequested must be true after exhaustion")
	}

	g, err := goal.NewStore(ts.agent.Workspace).ReadAny(ts.sessionKey)
	if err != nil {
		t.Fatalf("ReadAny after exhaustion: %v", err)
	}
	if g.Status != goal.StatusAborted {
		t.Errorf("goal status = %q, want %q", g.Status, goal.StatusAborted)
	}
	if g.AbortReason != GoalPhaseCheckpointStuckAbortReason {
		t.Errorf("goal AbortReason = %q, want %q", g.AbortReason, GoalPhaseCheckpointStuckAbortReason)
	}
	if !strings.Contains(ts.lastPhaseStuckError, "attempts: ") {
		t.Errorf("lastPhaseStuckError = %q, want stamped attempt count (F-D)", ts.lastPhaseStuckError)
	}
}

// =============================================================================
// F-D (T2) — finalizePhaseStuckArchive stamps the real attempt count into
// lastPhaseStuckError (which is never reset, so it survives into the next
// turn where the per-iteration counters are zero).
func TestFinalizePhaseStuckArchive_StampsAttemptCount(t *testing.T) {
	al, _, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	ts := newTurnState(al.registry.GetDefaultAgent(), makeTestProcessOpts("t2-fd"), turnEventScope{
		turnID:  "t2-fd",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = "t2-fd"
	ws := t.TempDir()
	ts.workspace = ws                  // hasGoal() reads ts.workspace
	ts.agent.Workspace = ws            // finalizeGoalOnTurnEnd writes via agent.Workspace
	if err := goal.NewStore(ws).Write(ts.sessionKey, &goal.Goal{
		Name:        "fd",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "stamp count", SuccessCriteria: []string{"done"}},
	}); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	// Two real failures before the archive event.
	ts.recordPhaseStuckToolFail("set_goal", "missing required property")
	ts.recordPhaseStuckToolFail("set_goal", "missing required property")
	if ts.setGoalAttemptCount != 2 {
		t.Fatalf("setup: attempt count = %d, want 2", ts.setGoalAttemptCount)
	}

	finalizePhaseStuckArchive(ts, GoalPhaseSet, "BoundedRetry exhausted")
	if !strings.Contains(ts.lastPhaseStuckError, "attempts: 2") {
		t.Errorf("lastPhaseStuckError = %q, want stamped attempts: 2", ts.lastPhaseStuckError)
	}
	// The goal file must land Aborted with the phase-stuck reason (F1 contract).
	g, err := goal.NewStore(ws).ReadAny(ts.sessionKey)
	if err != nil {
		t.Fatalf("ReadAny: %v", err)
	}
	if g.Status != goal.StatusAborted || g.AbortReason != GoalPhaseSetStuckAbortReason {
		t.Errorf("goal = (%s, %q), want (aborted, %q)", g.Status, g.AbortReason, GoalPhaseSetStuckAbortReason)
	}
}

// =============================================================================
// F-D (T3) — the stuck message rendered in a LATER turn (counters reset to 0)
// must never say "failed 0 attempt(s)".
func TestStuckMessage_TurnAfter_CountFallbackAtLeastOne(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	st := goal.NewStore(agent.Workspace)
	if err := st.Write("turn-after-session", &goal.Goal{
		Name:        "turn-after",
		Status:      goal.StatusAborted,
		AbortReason: GoalPhaseCheckpointStuckAbortReason,
		Description: goal.Description{Objective: "archived last turn", SuccessCriteria: []string{"done"}},
	}); err != nil {
		t.Fatalf("seed aborted goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts("turn-after-session"), turnEventScope{
		turnID:  "turn-after",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = "turn-after-session"
	// Fresh turn: all per-iteration counters are 0 (reset at iter boundary).
	if ts.goalProgressAttemptCount != 0 {
		t.Fatalf("setup: want zeroed counter, got %d", ts.goalProgressAttemptCount)
	}

	got := al.applyFallbackForEmptyResponse(ts)
	if strings.Contains(got, "0 attempt") {
		t.Fatalf("stuck message across turns = %q — must not render 0 attempts", got)
	}
	if !strings.Contains(got, "attempt") {
		t.Fatalf("stuck message = %q — want attempt count text", got)
	}
}

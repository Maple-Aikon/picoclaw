package agent

// Phase 12.42 — wire-level tests W6 / W11 / W12 (plan §5).
// W11/W12 verify the G10 fix: ControlBreak from the retry helper with a
// clean terminal cause (hard abort / all responses handled) must NOT be
// misread as archive-after-exhaustion.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// W6 — static wire check: retry path must NOT call ExecuteTools anymore
// (G7 — the helper executes re-picked tools inside Step 3). Only the outer
// loop's ExecuteTools call site remains (turn_coord.go:423).
func TestP1242_W6_NoExecuteToolsInRetryPath(t *testing.T) {
	src, err := os.ReadFile("turn_coord.go")
	if err != nil {
		t.Fatalf("read turn_coord.go: %v", err)
	}
	const want = 1
	got := strings.Count(string(src), "pipeline.ExecuteTools(ctx, turnCtx, ts, exec, iteration)")
	if got != want {
		t.Errorf("W6: expected %d ExecuteTools call site in turn_coord.go (outer loop only, G7), got %d. "+
			"Phase 12.42 must remove the retry-path block (was turn_coord.go:539-556)", want, got)
	}
}

// phase1242AbortExecutor simulates the outer-loop executor (call 1: gate
// block error) then the Step-3 executor (call 2: ToolControlBreak with a
// caller-set terminal flag).
type phase1242AbortExecutor struct {
	calls   int
	log     []string
	setFlag func(exec *turnExecution)
}

func (f *phase1242AbortExecutor) ExecuteTools(
	ctx context.Context, turnCtx context.Context,
	ts *turnState, exec *turnExecution, iteration int,
) ToolControl {
	f.calls++
	// call1 fires inside the retry chain Step 3 (outer loop uses the
	// real Pipeline.ExecuteTools which is bypassed by SetToolExecutor).
	// Outer loop's read_file gets gate-blocked by the production gate
	// (real Executor) — checkToolExecErrorRecovery routes to
	// retryExecuteToolChain. Attempt 0 of the chain picks the valid
	// tool (e.g. complete_goal); Step 3 executes it. To exercise the
	// turn_coord ControlBreak flag precedence (G10/C7), Step 3 fires
	// setFlag (hard-abort / hook-abort / all-responses-handled) and
	// returns ToolControlBreak — retry chain returns ControlBreak,
	// outer switch reads the flag, abortTurn produces TurnEndStatusAborted.
	f.setFlag(exec)
	return ToolControlBreak
}

// phase1242WireSetup builds a full turn with a seeded goal at Checkpoint
// phase and a scripted LLM (attempt 0: blocked read_file; attempt 1:
// valid complete_goal).
func phase1242WireSetup(t *testing.T, setFlag func(exec *turnExecution)) (*AgentLoop, *turnState, *Pipeline, *restrictedPhaseToolBlockProvider) {
	t.Helper()
	provider := &restrictedPhaseToolBlockProvider{
		responses: []*providers.LLMResponse{
			{
				Content: "",
				ToolCalls: []providers.ToolCall{
					{ID: "call-blocked", Name: "read_file", Arguments: map[string]any{"path": "/tmp/foo"}},
				},
				FinishReason: "tool_calls",
			},
			{
				Content: "",
				ToolCalls: []providers.ToolCall{
					{ID: "call-good", Name: "complete_goal", Arguments: map[string]any{"summary": "done"}},
				},
				FinishReason: "tool_calls",
			},
		},
	}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	t.Cleanup(cleanup)

	al.SkipGoalArchiveForTest()
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	pipeline := NewPipeline(al)
	pipeline.SetToolExecutor(&phase1242AbortExecutor{setFlag: setFlag})

	ws := t.TempDir()
	agent.Workspace = ws

	ts := newTurnState(agent, makeTestProcessOpts("phase-12-42-session"), turnEventScope{
		turnID:  "turn-12-42",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 5
	ts.setIteration(2)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}
	_ = exec

	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "phase-12-42-test",
		Description: goal.Description{
			Objective:       "test G10 clean-terminal precedence in retry path",
			SuccessCriteria: []string{"clean abort is not misread as archive exhaustion"},
			Cadence:         "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goal.NewStore(ws).Write("phase-12-42-session", activeGoal); err != nil {
		t.Fatalf("Write goal: %v", err)
	}
	if !ts.hasGoal() {
		t.Fatal("setup error: hasGoal=false; goal file not seeded")
	}
	return al, ts, pipeline, provider
}

// W11 — G10: Step 3 executor returns ToolControlBreak + abortedByHardAbort
// → turn_coord case ControlBreak must delegate to abortTurn
// (TurnEndStatusAborted, no fake archive error).
func TestP1242_W11_HardAbortInRetryPathEndsAborted(t *testing.T) {
	al, ts, pipeline, _ := phase1242WireSetup(t, func(exec *turnExecution) {
		exec.abortedByHardAbort = true
	})
	fake := pipeline.toolExecLazy().(*phase1242AbortExecutor)

	result, err := al.runTurn(context.Background(), ts, pipeline)
	t.Logf("W11 debug: err=%v status=%v fakeCalls=%d archive=%v",
		err, result.status, fake.calls, ts.goalArchiveRequested)
	if err != nil {
		t.Fatalf("W11: runTurn returned error (should be clean abort), got %v", err)
	}
	if result.status != TurnEndStatusAborted {
		t.Errorf("W11: expected TurnEndStatusAborted, got %v (G10: hard abort misread as archive error)", result.status)
	}
}

// W12 — G10: Step 3 executor returns ToolControlBreak + allResponsesHandled
// → turn_coord case ControlBreak must finalize cleanly (no archive error).
func TestP1242_W12_AllResponsesHandledInRetryPathFinalizes(t *testing.T) {
	al, ts, pipeline, _ := phase1242WireSetup(t, func(exec *turnExecution) {
		exec.allResponsesHandled = true
	})

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("W12: runTurn returned error (should finalize cleanly), got %v", err)
	}
	if result.status == TurnEndStatusError {
		t.Errorf("W12: expected non-error finalize, got TurnEndStatusError (G10: allResponsesHandled misread as archive)")
	}
}
func (p *restrictedPhaseToolBlockProvider) idx() int {
	return p.mu.idx
}


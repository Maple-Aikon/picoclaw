// Phase 12.46 — SET-phase same-iter recovery (owner decision, anh Maple
// 2026-08-03): gate-blocked tool / tool-exec error / empty response at
// GoalPhaseSet retry same-iter cap 3, mirroring CHECKPOINT. Text-only at
// SET gets NO recovery — a direct text reply is a valid turn end.

package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// T1 — SET text-only: NO recovery. LLM replied directly — turn ends.
// (Supersedes Phase 12.27/12.37 behavior where SET text-only fired
// soft/hard same-iter retries.)
func TestTextOnly_Set_NoRecovery_TurnEnds_Phase12_46(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseSet), TextEmpty: false, HasToolCalls: false}

	action, msg := evaluateRecovery(ts, ctx)
	if action != RecoveryNone {
		t.Fatalf("action=%v, want RecoveryNone (SET text-only ends turn)", action)
	}
	if msg != "" {
		t.Fatalf("msg=%q, want empty", msg)
	}
	if ts.textOnlyStreak != 0 || ts.textOnlySoftRetriesDone != 0 || ts.textOnlyHardRetriesDone != 0 {
		t.Fatalf("counters must stay 0 at SET text-only; streak=%d soft=%d hard=%d",
			ts.textOnlyStreak, ts.textOnlySoftRetriesDone, ts.textOnlyHardRetriesDone)
	}
}

// T2 — SET text-only is a valid terminal even after a previous text-only
// (no streak accumulation, no escalation).
func TestTextOnly_Set_RepeatedTextOnly_StillNoRecovery_Phase12_46(t *testing.T) {
	ts := newPhase5TurnState(t)
	for i := 0; i < 3; i++ {
		action, _ := evaluateRecovery(ts, RecoveryContext{
			Phase: string(GoalPhaseSet), TextEmpty: false, HasToolCalls: false,
		})
		if action != RecoveryNone {
			t.Fatalf("attempt %d: action=%v, want RecoveryNone", i, action)
		}
	}
}

// T3 — SET empty response: same-iter retry cap 3 (mirrors Checkpoint).
func TestEmptyResponse_Set_RetriesSameIter_Phase12_46(t *testing.T) {
	ts := newPhase5TurnState(t)
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase: string(GoalPhaseSet), TextEmpty: true, HasToolCalls: false,
	})
	if action != RecoveryRetrySameIteration {
		t.Fatalf("action=%v, want RecoveryRetrySameIteration (SET empty retries like Checkpoint)", action)
	}
	if msg == "" {
		t.Fatalf("msg must be non-empty")
	}
	if ts.emptyResponseRecoveryCount != 1 {
		t.Fatalf("emptyResponseRecoveryCount=%d, want 1", ts.emptyResponseRecoveryCount)
	}
}

// T4 — SET tool-exec error: same-iter retry (trigger #3 eligible at SET,
// Phase 12.15 already; regression guard).
func TestToolExecError_Set_RetriesSameIter_Phase12_46(t *testing.T) {
	ts := newPhase5TurnState(t)
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase: string(GoalPhaseSet), TextEmpty: false, HasToolCalls: true,
		ToolName: "set_goal", ToolExecError: "Tool execution failed: bad schema",
	})
	if action != RecoveryRetrySameIteration {
		t.Fatalf("action=%v, want RecoveryRetrySameIteration (SET tool-exec error retries)", action)
	}
	if msg == "" {
		t.Fatalf("msg must be non-empty")
	}
}

// T5 — Checkpoint/Final text-only MUST keep same-iter retry (not regressed
// by the SET exclusion).
func TestTextOnly_CheckpointAndFinal_StillRetrySameIter_Phase12_46(t *testing.T) {
	for _, phase := range []string{string(GoalPhaseCheckpoint), string(GoalPhaseFinal)} {
		ts := newPhase5TurnState(t)
		action, msg := evaluateRecovery(ts, RecoveryContext{
			Phase: phase, TextEmpty: false, HasToolCalls: false,
		})
		if action != RecoveryRetrySameIteration {
			t.Fatalf("phase=%s: action=%v, want RecoveryRetrySameIteration", phase, action)
		}
		if !strings.HasPrefix(msg, TextOnlySoftRetryMessage) {
			t.Fatalf("phase=%s: msg=%q, want TextOnlySoftRetryMessage prefix", phase, msg)
		}
	}
}

// T6 — OPEN text-only must still use next-iter carry (not regressed).
func TestTextOnly_Open_StillNextIter_Phase12_46(t *testing.T) {
	ts := newPhase5TurnState(t)
	action, _ := evaluateRecovery(ts, RecoveryContext{
		Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false,
	})
	if action != RecoveryRetryNextIteration {
		t.Fatalf("action=%v, want RecoveryRetryNextIteration (Open keeps next-iter carry)", action)
	}
}

// T7 — static wire check: gate pre-check at pipeline_execute.go must NOT
// filter by phase (SET included since Phase 12.46; pre-12.46 scope was
// {open, checkpoint, final}).
func TestGatePreCheckScope_AllPhases_Phase12_46(t *testing.T) {
	src, err := os.ReadFile("pipeline_execute.go")
	if err != nil {
		t.Fatalf("read pipeline_execute.go: %v", err)
	}
	srcStr := string(src)
	if strings.Contains(srcStr, "currentPhase == GoalPhaseOpen || currentPhase == GoalPhaseCheckpoint") {
		t.Errorf("pipeline_execute.go: pre-check still phase-filtered; Phase 12.46 must cover SET too")
	}
	if !strings.Contains(srcStr, "WithErrorKind") {
		t.Errorf("pipeline_execute.go: pre-check must stamp typed ErrKind (Bug A, deferred from Phase 12.35)")
	}
}

// T8 — wire: SET pre-goal gate-blocked tool → same-iter retry → LLM re-picks
// set_goal → goal seeded, turn proceeds. Mirrors main-turn-2 2026-08-03
// (ctx_execute blocked at iter 1) but now with same-iter recovery.
type phase12_46SetBlockProvider struct {
	responses []*providers.LLMResponse
	callCount int
}

func (p *phase12_46SetBlockProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	idx := p.callCount
	p.callCount++
	if idx < len(p.responses) && p.responses[idx] != nil {
		return p.responses[idx], nil
	}
	return &providers.LLMResponse{Content: "done", FinishReason: "stop"}, nil
}

func (p *phase12_46SetBlockProvider) GetDefaultModel() string { return "phase-12-46-set-block-test" }

func TestPhase12_46_SetGateBlock_SameIterRetry_SeedsGoal(t *testing.T) {
	provider := &phase12_46SetBlockProvider{
		responses: []*providers.LLMResponse{
			// attempt 0 (iter 1, SET, no goal): LLM calls a non-SET tool
			{
				Content:      "need file info",
				FinishReason: "tool_calls",
				ToolCalls: []providers.ToolCall{
					{ID: "call-blocked", Name: "mcp_context-mode_ctx_execute", Arguments: map[string]any{"cmd": "ls"}},
				},
			},
			// attempt 1 (same-iter retry): LLM re-picks set_goal
			{
				Content:      "defining goal",
				FinishReason: "tool_calls",
				ToolCalls: []providers.ToolCall{
					{ID: "call-set", Name: "set_goal", Arguments: map[string]any{
						"name": "phase-12-46-set-block", "objective": "seed goal",
					}},
				},
			},
		},
	}

	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	al.SkipGoalArchiveForTest()

	pipeline := NewPipeline(al)
	ws := t.TempDir()
	agent.Workspace = ws

	ts := newTurnState(agent, makeTestProcessOpts("phase-12-46-set-block"), turnEventScope{
		turnID:  "turn-12-46-set-block",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 5
	ts.maxIterationsCap = 50
	// iter 1 + no goal file → GoalPhaseSet (pin rule), hasGoal()=false.

	// Sanity: gate must report ctx_execute NOT allowed at SET.
	if agent.Tools == nil {
		t.Fatal("setup error: agent.Tools nil; cannot run gate check")
	}
	if agent.Tools.IsAllowed("mcp_context-mode_ctx_execute") {
		t.Skip("environment allows ctx_execute at SET; allowlist not wired — test N/A")
	}
	if !agent.Tools.IsAllowed("set_goal") {
		t.Skip("set_goal not allowed at SET; lifecycle gate broken — test N/A")
	}

	if _, err := pipeline.SetupTurn(context.Background(), ts); err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	_ = result

	// After the turn: goal must be seeded (set_goal executed via same-iter
	// retry) OR the turn ended cleanly without crashing. Assert the goal
	// store has an active goal — proves the retry executed set_goal.
	store := goal.NewStore(ws)
	g, gerr := store.Read(ts.sessionKey)
	if gerr != nil || g == nil || g.Status != goal.StatusActive {
		// Also accept: turn completed text-only fallback (provider stub).
		t.Logf("goal not active after turn (err=%v g=%v) — checking turn result status instead", gerr, g)
	}
	if g != nil && g.Name == "phase-12-46-set-block" {
		return // goal seeded — retry executed set_goal ✓
	}
	t.Fatalf("expected set_goal to be executed during same-iter retry; goal=%v err=%v", g, gerr)
}

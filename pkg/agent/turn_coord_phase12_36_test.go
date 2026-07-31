package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// phase12_36TestProvider simulates an LLM at Checkpoint phase.
// Call 0 emits a blocked tool; call 1 emits goal_progress.
type phase12_36TestProvider struct {
	responses []*providers.LLMResponse
	callCount int
}

func (p *phase12_36TestProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	idx := p.callCount
	p.callCount++
	if idx < len(p.responses) && p.responses[idx] != nil {
		return p.responses[idx], nil
	}
	return &providers.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *phase12_36TestProvider) GetDefaultModel() string {
	return "phase-12-36-test-model"
}

func newPhase12_36AgentLoop(t *testing.T, provider *phase12_36TestProvider) (*AgentLoop, *AgentInstance, func()) {
	t.Helper()
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	agent.MaxIterations = 5
	agent.MaxIterationsCap = 50
	return al, agent, cleanup
}

// TestPhase12_36_AllContinuePathsGuardApplyDeferredExtend is a wire
// regression-proof for the Phase 12.36 fix. Verifies that every `continue`
// in turn_coord.go's main turn loop body is preceded by
// `_, _ = al.applyDeferredExtend(ts)` (either by falling through to outer
// block OR by per-site hook).
//
// Reproduction: 20:04 user turn main-turn-3 — iter 5 (CHECKPOINT) blocked
// read_file, retry emitted goal_progress with remaining_steps=[...], but
// loop exited because cap was not bumped. Bug: `continue` at line 551
// skipped applyDeferredExtend (which only fires at line 589 outer block).
//
// Wire expectation (post-Phase 12.36): 4 call sites total in turn_coord.go:
//   - line 398 (ControlContinue path)
//   - line 474 (graceful-interrupt path — NEW in 12.36)
//   - line 551 (retry path case ControlContinue — NEW in 12.36)
//   - line 589 (outer block)
//
// Pre-12.36 code has only 2 call sites (lines 398, 589). The test FAILS
// on pre-fix code (count = 2) and PASSES on post-fix code (count = 4).
func TestPhase12_36_AllContinuePathsGuardApplyDeferredExtend(t *testing.T) {
	src, err := os.ReadFile("turn_coord.go")
	if err != nil {
		t.Fatalf("read turn_coord.go: %v", err)
	}
	srcStr := string(src)

	// Count expected call sites: 4 (Phase 12.36 ships 2 new sites at 474 + 551).
	const wantCount = 4
	count := strings.Count(srcStr, "_, _ = al.applyDeferredExtend(ts)")
	if count != wantCount {
		t.Errorf("expected %d applyDeferredExtend call sites in turn_coord.go (Phase 12.36), got %d. "+
			"Phase 12.35 had 2 sites (lines 398, 589). Phase 12.36 adds 2 more "+
			"(lines 474 graceful-interrupt + 551 retry path) to close the bug where "+
			"`continue` bypassed the outer block's applyDeferredExtend.",
			wantCount, count)
	}
}

// TestPhase12_36_ApplyDeferredExtendFlushesStagedExtend verifies the
// helper-level behavior: when a RequestExtendIterationCap has been staged,
// applyDeferredExtend bumps iterationCap by MaxIterationsPerCheckpoint.
//
// This is the SAME call the fix puts before line 551's `continue`. So if
// the helper works AND the call site exists, the wire works.
func TestPhase12_36_ApplyDeferredExtendFlushesStagedExtend(t *testing.T) {
	al, agent, cleanup := newPhase12_36AgentLoop(t, &phase12_36TestProvider{})
	defer cleanup()

	al.SkipGoalArchiveForTest()
	al.SetGoalPhaseForTest(string(GoalPhaseCheckpoint))

	pipeline := NewPipeline(al)

	ws := t.TempDir()
	agent.Workspace = ws

	ts := newTurnState(agent, makeTestProcessOpts("phase-12-36-helper-flush"), turnEventScope{
		turnID:  "turn-12-36-helper",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 5
	ts.maxIterationsCap = 50
	ts.setIteration(5)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}

	goalStore := goal.NewStore(ws)
	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "phase-12-36-helper-flush",
		Description: goal.Description{
			Objective:       "verify applyDeferredExtend flushes staged extend",
			SuccessCriteria: []string{"iterationCap bumps 5→10 when staged extend flushes"},
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goalStore.Write("phase-12-36-helper-flush", activeGoal); err != nil {
		t.Fatalf("Write goal: %v", err)
	}

	// Stage a deferred-extend (simulates goal_progress tool Execute).
	// n must be > 0; FlushPendingExtend applies the default when amount=0
	// but RequestExtendIterationCap rejects n=0. Use the explicit default.
	extendAmount := ts.MaxIterationsPerCheckpoint()
	if !ts.RequestExtendIterationCap(extendAmount, "phase-12-36-test-extend") {
		t.Fatalf("RequestExtendIterationCap rejected (cap=%d, ceiling=%d)", ts.iterationCap, ts.maxIterationsCap)
	}

	if !ts.willExtendIterCap {
		t.Fatal("setup error: willExtendIterCap=false; staged request missing")
	}

	capBefore := ts.iterationCap

	// This is the SAME call the fix puts before line 551's `continue`.
	newCap, delta := al.applyDeferredExtend(ts)

	if delta == 0 {
		t.Errorf("expected delta > 0 after applyDeferredExtend, got delta=%d (cap=%d→%d)",
			delta, capBefore, newCap)
	}
	expectedNewCap := capBefore + ts.MaxIterationsPerCheckpoint()
	if newCap != expectedNewCap {
		t.Errorf("expected newCap=%d, got %d", expectedNewCap, newCap)
	}
	if ts.iterationCap != expectedNewCap {
		t.Errorf("expected ts.iterationCap=%d, got %d", expectedNewCap, ts.iterationCap)
	}

	_ = exec
}

// TestPhase12_36_RetryPathPreservesContinueForPendingRecovery verifies
// the regression-proof for the Option B bug caught in adversarial review:
// removing the retry path's `continue` would re-introduce the
// pendingRecoveryMessage = archiveMsg side effect (turn_coord.go:564-566).
//
// Wire expectation: the retry path's `case ControlContinue, ControlToolLoop:`
// case body MUST end with `continue` (or break/return) to skip the
// pendingRecoveryMessage setter. Fall-through to outer block would
// re-inject the blocked-tool recovery prompt next iter despite retry
// success.
//
// Static check: between "case ControlContinue, ControlToolLoop:" and the
// next `case` keyword, the substring "continue" must appear at least once.
func TestPhase12_36_RetryPathPreservesContinueForPendingRecovery(t *testing.T) {
	src, err := os.ReadFile("turn_coord.go")
	if err != nil {
		t.Fatalf("read turn_coord.go: %v", err)
	}
	srcStr := string(src)

	const caseHeader = "case ControlContinue, ControlToolLoop:"
	caseIdx := strings.Index(srcStr, caseHeader)
	if caseIdx < 0 {
		t.Fatalf("%s not found in turn_coord.go", caseHeader)
	}

	// Find the case body boundary (next "case " or "}"). Search from caseIdx.
	afterCase := srcStr[caseIdx+len(caseHeader):]
	// The retry path's case is inside `if isRestricted { ... }` so the
	// next `case ` is at the same indentation. Search up to 4000 chars.
	scope := afterCase
	if len(scope) > 4000 {
		scope = scope[:4000]
	}
	nextCase := strings.Index(scope, "\n\t\t\t\tcase ")
	if nextCase < 0 {
		nextCase = len(scope)
	}
	caseBody := scope[:nextCase]

	// Verify "continue" appears in case body.
	if !strings.Contains(caseBody, "continue") {
		t.Errorf("retry path's case ControlContinue, ControlToolLoop: MUST end with `continue` " +
			"to skip the pendingRecoveryMessage = archiveMsg setter at lines 564-566 " +
			"(Phase 12.36 adversarial review finding). Removing the continue would " +
			"re-inject blocked-tool recovery prompt next iter despite retry success.")
	}
}
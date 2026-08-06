// Phase 12.51b wire tests (W7-W13) — branch (1.5) escape hatch in
// applyFallbackForEmptyResponse. Defense-in-depth for Checkpoint/Final
// iter=cap scenarios where LLM text-only escapes Path 4 entirely.
//
// Phase 12.51b design (per plan v13 §3.4):
//   - Branch 1.5 in applyFallbackForEmptyResponse runs BETWEEN
//     phaseStuckFallbackMessage and toolLimitResponse.
//   - Triggers when: iter>=iterCap + hasGoal + !goalFinalized +
//     !postCompleteGoalReportSent + phase in {Checkpoint, Final}.
//   - Returns state-agnostic wording per Phase 12.40 spirit.
//   - Open/Set behavior unchanged (Open → toolLimitResponse,
//     Set → no retry prompt).
//
// Tests assert behavior at the helper entrypoint only; full turn loop
// behavior is covered by live verify (plan v13 §5.2 V1-V6).
package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// seedActiveGoalInWorkspace was removed in inline review (TDD-F4) — each test
// inlines the st.Write + newTurnState pattern from turn_coord_goal_summary_test.go.

// W7 — Checkpoint iter=cap with active goal NOT finalized must return
// ToolLimitCheckpointRetryMessage (NOT toolLimitResponse, NOT DefaultResponse).
func TestApplyFallbackForEmptyResponse_CheckpointIterCap_ReturnsCheckpointRetry(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-12-51b-w7-checkpoint"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name:        "escape-hatch-w7",
		Description: goal.Description{Objective: "branch 1.5 test", SuccessCriteria: []string{"phase-agnostic message returned"}},
		Status:      goal.StatusActive,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-12-51b-w7",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	// Force Checkpoint: iteration=cap with active goal.
	ts.iteration = ts.iterationCap

	// Precondition: phase must resolve to Checkpoint (NOT Set, NOT Open).
	phase := ts.currentGoalPhase()
	if phase != GoalPhaseCheckpoint {
		t.Fatalf("phase = %s, want %s (iter=%d iterCap=%d hasGoal=%v)",
			phase, GoalPhaseCheckpoint, ts.iteration, ts.iterationCap, ts.hasGoal())
	}

	got := al.applyFallbackForEmptyResponse(ts)
	want := ToolLimitCheckpointRetryMessage
	if got != want {
		t.Fatalf("got = %q, want %q", got, want)
	}
}

// W8 — Final iter=cap with active goal NOT finalized must return
// ToolLimitFinalRetryMessage.
func TestApplyFallbackForEmptyResponse_FinalIterCap_ReturnsFinalRetry(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-12-51b-w8-final"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name:        "final-escape-w8",
		Description: goal.Description{Objective: "branch 1.5 final test", SuccessCriteria: []string{"phase-agnostic final msg"}},
		Status:      goal.StatusActive,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-12-51b-w8",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	// Force Final phase: iteration >= maxIterationsCap.
	ts.iteration = ts.maxIterationsCap
	if ts.iterationCap < ts.iteration {
		ts.iterationCap = ts.iteration
	}

	phase := ts.currentGoalPhase()
	if phase != GoalPhaseFinal {
		t.Fatalf("phase = %s, want %s (iter=%d maxCap=%d)",
			phase, GoalPhaseFinal, ts.iteration, ts.maxIterationsCap)
	}

	got := al.applyFallbackForEmptyResponse(ts)
	want := ToolLimitFinalRetryMessage
	if got != want {
		t.Fatalf("got = %q, want %q", got, want)
	}
}

// W9 (regression) — Open iter=cap (no active goal) must fall through to
// toolLimitResponse (NOT the new Checkpoint/Final retry message).
func TestApplyFallbackForEmptyResponse_OpenIterCap_ReturnsToolLimitResponse(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-12-51b-w9-open"
	// No goal file for this session → hasGoal()=false.
	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-12-51b-w9",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.iteration = 5
	ts.iterationCap = 5
	ts.maxIterationsCap = 200

	if ts.hasGoal() {
		t.Fatalf("precondition: expected hasGoal()=false, got true")
	}

	got := al.applyFallbackForEmptyResponse(ts)
	if got != toolLimitResponse {
		t.Fatalf("got = %q, want toolLimitResponse (%q)", got, toolLimitResponse)
	}
}

// W10 (silent guard) — postCompleteGoalReportSent=true → silent (returns "").
// The postReport guard fires FIRST per Phase 12.44 — branch 1.5 must NOT
// override the silent skip.
func TestApplyFallbackForEmptyResponse_CheckpointPostFinal_ReturnsEmpty(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-12-51b-w10-postfinal"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name:        "post-final-guard-w10",
		Description: goal.Description{Objective: "branch 1.5 silent guard", SuccessCriteria: []string{"silent skip"}},
		Status:      goal.StatusActive,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-12-51b-w10",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = true
	ts.iteration = ts.iterationCap

	got := al.applyFallbackForEmptyResponse(ts)
	if got != "" {
		t.Fatalf("got = %q, want empty (postCompleteGoalReportSent silent skip)", got)
	}
}

// W11 (pre-goal) — hasGoal()=false at Checkpoint iter=cap → no retry prompt
// (no goal to escape from). Falls through to toolLimitResponse.
func TestApplyFallbackForEmptyResponse_PreGoalCheckpoint_ReturnsToolLimitResponse(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-12-51b-w11-pregoal"
	// No goal file → hasGoal()=false.
	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-12-51b-w11",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	ts.iteration = ts.iterationCap

	if ts.hasGoal() {
		t.Fatalf("precondition: expected hasGoal()=false")
	}

	got := al.applyFallbackForEmptyResponse(ts)
	if got != toolLimitResponse {
		t.Fatalf("got = %q, want toolLimitResponse (no goal → no escape)", got)
	}
}

// W12 — Mid-goal at Checkpoint iter=cap (NOT postReport, NOT pre-goal) →
// retry prompt.
func TestApplyFallbackForEmptyResponse_CheckpointNoGoalFinalized_ReturnsRetry(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-12-51b-w12-midgoal"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name:        "mid-goal-checkpoint-w12",
		Description: goal.Description{Objective: "branch 1.5 mid-goal", SuccessCriteria: []string{"retry prompt"}},
		Status:      goal.StatusActive,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-12-51b-w12",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	ts.iteration = ts.iterationCap

	if !ts.hasGoal() || ts.goalFinalized {
		t.Fatalf("precondition fail: hasGoal=%v goalFinalized=%v", ts.hasGoal(), ts.goalFinalized)
	}

	got := al.applyFallbackForEmptyResponse(ts)
	if got != ToolLimitCheckpointRetryMessage {
		t.Fatalf("got = %q, want ToolLimitCheckpointRetryMessage", got)
	}
}

// W13 (precedence) — goalSummary beats everything else, including phase-stuck
// and the new retry hint. Plan §3.5 documents the FULL preference order
// (lines 779-781 verified at HEAD):
//   1. goal.Summary (when goalFinalized + empty assistantText)
//   2. branch 1.5 retry hint (when iter=cap + active goal + phase ∈ Checkpoint/Final)
//   3. phaseStuckFallbackMessage (when StatusAborted + AbortReason)
//   4. toolLimitResponse (when iter>=cap)
//   5. opts.DefaultResponse
// This test verifies goal.Summary precedence by setting all 3 higher-priority
// conditions simultaneously and asserting goal.Summary wins.
func TestApplyFallbackForEmptyResponse_GoalSummaryPrecedenceOverRetryHint(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-12-51b-w13-summary-precedence"
	const wantSummary = "Goal accomplished: completed all success criteria."
	st := goal.NewStore(agent.Workspace)
	// Seed goal as StatusCompleted so hasGoal()=false (per turn_state_goal_phase.go:58)
	// but the archive-side ReadAny path in applyFallbackForEmptyResponse still
	// finds the Summary (line 770-776).
	g := &goal.Goal{
		Name:        "summary-precedence-w13",
		Description: goal.Description{Objective: "precedence test", SuccessCriteria: []string{"summary wins"}},
		Status:      goal.StatusCompleted,
		Summary:     wantSummary,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-12-51b-w13",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = false
	ts.assistantText = "" // empty prose — fallback chain applies
	ts.iteration = ts.iterationCap
	ts.lastPhaseStuckError = "BoundedRetry exhausted" // would trigger phase-stuck if reached

	got := al.applyFallbackForEmptyResponse(ts)
	if got != wantSummary {
		t.Fatalf("got = %q, want %q (goal.Summary must beat retry hint + phase-stuck)", got, wantSummary)
	}
}

// TestApplyFallbackForEmptyResponse_NoCapClaims — wording invariant per
// Phase 12.40 spirit: NO iteration-cap / cap-reached / cap-extended claims
// in LLM-visible text. Combines shared assertNoPhaseClaim (Phase 12.43)
// for phase claims + cap-specific banned list (Phase 12.40 spirit extends
// cap-claims class).
func TestApplyFallbackForEmptyResponse_NoCapClaims(t *testing.T) {
	capBanned := []string{
		"iteration cap", "iteration-cap",
		"cap reached", "cap-reached",
		"cap extended", "cap-extended",
		"max iter", "max-iter",
		"hit the ceiling", "ceiling",
	}
	for _, msg := range []string{ToolLimitCheckpointRetryMessage, ToolLimitFinalRetryMessage} {
		// Phase claims (Phase 12.43 helper, shared across LLM-visible text paths)
		assertNoPhaseClaim(t, msg)
		// Cap-claims (12.51b-specific — extends Phase 12.40 spirit)
		lower := strings.ToLower(msg)
		for _, b := range capBanned {
			if strings.Contains(lower, b) {
				t.Errorf("banned phrase %q found in message %q (Phase 12.40 invariant violation)",
					b, msg)
			}
		}
	}
}

// TestApplyFallbackForEmptyResponse_CheckpointRetryMentionsTools — wording
// invariants: Checkpoint retry message must enumerate BOTH available tools
// (`goal_progress`, `complete_goal`) for the LLM to act on Phase 12.43
// state-agnostic guidance.
func TestApplyFallbackForEmptyResponse_CheckpointRetryMentionsTools(t *testing.T) {
	msg := ToolLimitCheckpointRetryMessage
	for _, want := range []string{"`goal_progress`", "`complete_goal`"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected %q in ToolLimitCheckpointRetryMessage %q", want, msg)
		}
	}
	// Final retry message must enumerate `complete_goal` only.
	final := ToolLimitFinalRetryMessage
	if !strings.Contains(final, "`complete_goal`") {
		t.Errorf("expected `complete_goal` in ToolLimitFinalRetryMessage %q", final)
	}
	// Final message must NOT mention goal_progress (not available at Final).
	if strings.Contains(final, "goal_progress") {
		t.Errorf("Final retry message should NOT mention goal_progress (not available at Final): %q", final)
	}
}

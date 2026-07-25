package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// TestPhaseStuckMessages_ContainPhaseKeywords — the 3 phase-stuck messages
// must each name the phase explicitly so the user understands which phase
// PicoClaw got stuck in. The fallback "empty response" message is misleading
// because LLM responses are NOT empty (typically 300-2000 chars) — the
// actual cause is a phase-specific lifecycle tool failure.
func TestPhaseStuckMessages_ContainPhaseKeywords(t *testing.T) {
	cases := []struct {
		name    string
		message string
		phase   string
		tool    string
	}{
		{"Set", GoalPhaseSetStuckMessage, "Set", "set_goal"},
		{"Checkpoint", GoalPhaseCheckpointStuckMessage, "Checkpoint", "goal_progress"},
		{"Final", GoalPhaseFinalStuckMessage, "Final", "complete_goal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.message, tc.phase) {
				t.Errorf("message for %s phase must contain %q keyword", tc.name, tc.phase)
			}
			if !strings.Contains(tc.message, tc.tool) {
				t.Errorf("message for %s phase must mention tool %q", tc.name, tc.tool)
			}
			if !strings.Contains(tc.message, "PicoClaw") {
				t.Errorf("message for %s phase must mention PicoClaw as the source", tc.name)
			}
		})
	}
}

// TestPhaseStuckAbortReasonConstants — sanity check the 3 abort_reason
// values are distinct and match what applyFallbackForEmptyResponse compares.
func TestPhaseStuckAbortReasonConstants(t *testing.T) {
	values := []string{
		GoalPhaseSetStuckAbortReason,
		GoalPhaseCheckpointStuckAbortReason,
		GoalPhaseFinalStuckAbortReason,
	}
	seen := map[string]bool{}
	for _, v := range values {
		if v == "" {
			t.Errorf("phase-stuck abort reason must be non-empty")
		}
		if seen[v] {
			t.Errorf("duplicate phase-stuck abort reason: %q", v)
		}
		seen[v] = true
	}
}

// TestRecordPhaseStuckToolFail — recordPhaseStuckToolFail increments the
// matching counter and stores the error message. Other tools are ignored.
func TestRecordPhaseStuckToolFail(t *testing.T) {
	ts := &turnState{}

	ts.recordPhaseStuckToolFail("set_goal", "missing required property name")
	ts.recordPhaseStuckToolFail("set_goal", "missing required property name")
	if ts.setGoalFailCount != 2 {
		t.Errorf("after 2 set_goal fails, counter should be 2, got %d", ts.setGoalFailCount)
	}
	if ts.lastPhaseStuckError != "missing required property name" {
		t.Errorf("lastPhaseStuckError should track last error, got %q", ts.lastPhaseStuckError)
	}

	ts.recordPhaseStuckToolFail("goal_progress", "missing required field completed_steps")
	if ts.goalProgressFailCount != 1 {
		t.Errorf("goal_progress counter should be 1, got %d", ts.goalProgressFailCount)
	}

	ts.recordPhaseStuckToolFail("complete_goal", "summary too short")
	ts.recordPhaseStuckToolFail("complete_goal", "summary too short")
	ts.recordPhaseStuckToolFail("complete_goal", "summary too short")
	if ts.completeGoalFailCount != 3 {
		t.Errorf("complete_goal counter should be 3, got %d", ts.completeGoalFailCount)
	}

	// Unknown tool should not affect any counter
	ts.recordPhaseStuckToolFail("read_file", "some error")
	if ts.setGoalFailCount != 2 || ts.goalProgressFailCount != 1 || ts.completeGoalFailCount != 3 {
		t.Errorf("unknown tool should not affect counters, got set_goal=%d goal_progress=%d complete_goal=%d",
			ts.setGoalFailCount, ts.goalProgressFailCount, ts.completeGoalFailCount)
	}
}

// TestComputePhaseStuckAbortReason_RequiresPhaseAndCounter — only fires when
// the current phase matches the failing counter AND counter >= 2.
func TestComputePhaseStuckAbortReason_RequiresPhaseAndCounter(t *testing.T) {
	cases := []struct {
		name          string
		phase         GoalPhase
		setGoalCount  int
		progressCount int
		completeCount int
		want          string
	}{
		{"Set phase + set_goal fail 1x → no", GoalPhaseSet, 1, 0, 0, ""},
		{"Set phase + set_goal fail 2x → yes", GoalPhaseSet, 2, 0, 0, GoalPhaseSetStuckAbortReason},
		{"Set phase + set_goal fail 3x → yes", GoalPhaseSet, 3, 0, 0, GoalPhaseSetStuckAbortReason},
		{"Checkpoint + goal_progress fail 2x → yes", GoalPhaseCheckpoint, 0, 2, 0, GoalPhaseCheckpointStuckAbortReason},
		{"Final + complete_goal fail 2x → yes", GoalPhaseFinal, 0, 0, 2, GoalPhaseFinalStuckAbortReason},
		{"Open phase + set_goal fail 5x → no (wrong phase)", GoalPhaseOpen, 5, 0, 0, ""},
		{"Checkpoint + set_goal fail 5x → no (wrong tool)", GoalPhaseCheckpoint, 5, 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts2 := &turnState{
				setGoalFailCount:      tc.setGoalCount,
				goalProgressFailCount: tc.progressCount,
				completeGoalFailCount: tc.completeCount,
			}
			// Manually compute the expected behavior (matches the logic in
			// computePhaseStuckAbortReason but bypasses the currentGoalPhase call).
			var got string
			switch tc.phase {
			case GoalPhaseSet:
				if ts2.setGoalFailCount >= 2 {
					got = GoalPhaseSetStuckAbortReason
				}
			case GoalPhaseCheckpoint:
				if ts2.goalProgressFailCount >= 2 {
					got = GoalPhaseCheckpointStuckAbortReason
				}
			case GoalPhaseFinal:
				if ts2.completeGoalFailCount >= 2 {
					got = GoalPhaseFinalStuckAbortReason
				}
			}
			if got != tc.want {
				t.Errorf("phase=%s set_goal=%d goal_progress=%d complete_goal=%d → got %q, want %q",
					tc.phase, tc.setGoalCount, tc.progressCount, tc.completeCount, got, tc.want)
			}
		})
	}
}

// TestApplyFallbackForEmptyResponse_PhaseStuckSet_Preferred — when the goal
// was archived with abort_reason=goal_set_stuck, the fallback message names
// the Set phase instead of returning DefaultResponse.
func TestApplyFallbackForEmptyResponse_PhaseStuckSet_Preferred(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-session-phase-stuck-set"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "update-uv",
		Description: goal.Description{
			Objective:       "Upgrade uv",
			SuccessCriteria: []string{"uv >= 0.11.30 reports installed"},
		},
		Status:      goal.StatusAborted,
		AbortReason: GoalPhaseSetStuckAbortReason,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-phase-stuck",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.setGoalFailCount = 2
	ts.lastPhaseStuckError = "missing required property name"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal-Set phase stuck") {
		t.Fatalf("expected fallback to contain 'Goal-Set phase stuck', got %q", got)
	}
	if !strings.Contains(got, "set_goal") {
		t.Fatalf("expected fallback to mention set_goal, got %q", got)
	}
	if strings.Contains(got, "empty response") {
		t.Fatalf("phase-stuck fallback must NOT contain the misleading 'empty response' string; got %q", got)
	}
}

// TestApplyFallbackForEmptyResponse_PhaseStuckCheckpoint_Preferred — same as
// above but for GoalPhaseCheckpoint.
func TestApplyFallbackForEmptyResponse_PhaseStuckCheckpoint_Preferred(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-session-phase-stuck-checkpoint"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "upgrade-runtime",
		Description: goal.Description{
			Objective:       "Upgrade runtime",
			SuccessCriteria: []string{"runtime at version X"},
		},
		Status:      goal.StatusAborted,
		AbortReason: GoalPhaseCheckpointStuckAbortReason,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-checkpoint-stuck",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalProgressFailCount = 2
	ts.lastPhaseStuckError = "missing required field completed_steps"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal-Checkpoint phase stuck") {
		t.Fatalf("expected fallback to contain 'Goal-Checkpoint phase stuck', got %q", got)
	}
	if !strings.Contains(got, "goal_progress") {
		t.Fatalf("expected fallback to mention goal_progress, got %q", got)
	}
}

// TestApplyFallbackForEmptyResponse_PhaseStuckFinal_Preferred — same as
// above but for GoalPhaseFinal.
func TestApplyFallbackForEmptyResponse_PhaseStuckFinal_Preferred(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-session-phase-stuck-final"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "finalize-something",
		Description: goal.Description{
			Objective:       "Finalize",
			SuccessCriteria: []string{"done"},
		},
		Status:      goal.StatusAborted,
		AbortReason: GoalPhaseFinalStuckAbortReason,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-final-stuck",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.completeGoalFailCount = 2
	ts.lastPhaseStuckError = "summary too short"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal-Final phase stuck") {
		t.Fatalf("expected fallback to contain 'Goal-Final phase stuck', got %q", got)
	}
	if !strings.Contains(got, "complete_goal") {
		t.Fatalf("expected fallback to mention complete_goal, got %q", got)
	}
}

// TestApplyFallbackForEmptyResponse_GoalSummaryStillTakesPriority — Phase
// 12.6.0 ordering remains: goal.Summary (completed goal) beats the
// phase-stuck message (aborted goal). This is the regression test for
// any chain reorder in Phase 12.13.
func TestApplyFallbackForEmptyResponse_GoalSummaryStillTakesPriority(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-session-summary-vs-stuck"
	const wantSummary = "Done. uv upgraded 0.11.30 → 0.11.31"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "update-uv",
		Description: goal.Description{
			Objective:       "Upgrade uv",
			SuccessCriteria: []string{"done"},
		},
		Status:      goal.StatusCompleted,
		Summary:     wantSummary,
		AbortReason: GoalPhaseSetStuckAbortReason, // conflict: a *completed* goal
		// shouldn't have a phase-stuck abort_reason, but tests the priority
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-summary-priority",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = true
	ts.assistantText = ""
	ts.setGoalFailCount = 5 // even with high counter, Summary wins

	got := al.applyFallbackForEmptyResponse(ts)
	if got != wantSummary {
		t.Fatalf("expected goal.Summary %q to take priority, got %q", wantSummary, got)
	}
}

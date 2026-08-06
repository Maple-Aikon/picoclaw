package agent

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// Phase 12.43 — assertNoPhaseClaim helper.
// Per Phase 12.40 / 12.40.1 / 12.43 invariant: no phase claims in LLM-visible text.
// Q11-A: 10 phrases → 3 patterns (skip-message variants + 2 stuck-message patterns).
var phaseClaimRegex = regexp.MustCompile(`(?i)\b(at checkpoint|at final phase|open phase|current phase is set|Phase 12.35 gate pre-check at|Goal-(Set|Checkpoint|Final) phase stuck|the (Set|Checkpoint|Final) phase only allows)\b`)

func assertNoPhaseClaim(t *testing.T, msg string) {
	t.Helper()
	if match := phaseClaimRegex.FindString(strings.ToLower(msg)); match != "" {
		t.Errorf("LLM-visible text contains phase/state claim %q (must be consequence-based): %s", match, msg)
	}
}

// TestPhaseStuckMessages_ConsequenceBased — Phase 12.43: stuck messages must
// describe consequences (what failed, what to try), NOT phase state claims
// (which become stale after transitions). The 3 messages must each name the
// failing tool + action alternative, not "Goal-Set phase stuck" header.
func TestPhaseStuckMessages_ConsequenceBased(t *testing.T) {
	cases := []struct {
		name    string
		message string
		phase   string
		tool    string
	}{
		{"Set", GoalPhaseSetStuckMessage, "setup", "set_goal"},
		{"Checkpoint", GoalPhaseCheckpointStuckMessage, "continuation", "goal_progress"},
		{"Final", GoalPhaseFinalStuckMessage, "finalization", "complete_goal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.message, tc.tool) {
				t.Errorf("message for %s phase must mention tool %q", tc.name, tc.tool)
			}
			if !strings.Contains(tc.message, tc.phase) {
				t.Errorf("message for %s phase must mention consequence keyword %q", tc.name, tc.phase)
			}
			if !strings.Contains(tc.message, "PicoClaw") {
				t.Errorf("message for %s phase must mention PicoClaw as the source", tc.name)
			}
			// Phase 12.43: no phase enum claims in LLM-visible text.
			assertNoPhaseClaim(t, tc.message)
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
	if ts.setGoalAttemptCount != 2 {
		t.Errorf("after 2 set_goal fails, counter should be 2, got %d", ts.setGoalAttemptCount)
	}
	if ts.lastPhaseStuckError != "missing required property name" {
		t.Errorf("lastPhaseStuckError should track last error, got %q", ts.lastPhaseStuckError)
	}

	ts.recordPhaseStuckToolFail("goal_progress", "missing required field completed_steps")
	if ts.goalProgressAttemptCount != 1 {
		t.Errorf("goal_progress counter should be 1, got %d", ts.goalProgressAttemptCount)
	}

	ts.recordPhaseStuckToolFail("complete_goal", "summary too short")
	ts.recordPhaseStuckToolFail("complete_goal", "summary too short")
	ts.recordPhaseStuckToolFail("complete_goal", "summary too short")
	if ts.completeGoalAttemptCount != 3 {
		t.Errorf("complete_goal counter should be 3, got %d", ts.completeGoalAttemptCount)
	}

	// Unknown tool should not affect any counter
	ts.recordPhaseStuckToolFail("read_file", "some error")
	if ts.setGoalAttemptCount != 2 || ts.goalProgressAttemptCount != 1 || ts.completeGoalAttemptCount != 3 {
		t.Errorf("unknown tool should not affect counters, got set_goal=%d goal_progress=%d complete_goal=%d",
			ts.setGoalAttemptCount, ts.goalProgressAttemptCount, ts.completeGoalAttemptCount)
	}
}

// TestComputePhaseStuckAbortReason_RequiresPhaseAndCounter — only fires when
// the current phase matches its StuckBucket AND (archive flag set OR
// count >= 2). Phase 12.52a: the flag carries the archive signal (set by
// recordPhaseStuckArchive), the count stays a real per-failure count.
func TestComputePhaseStuckAbortReason_RequiresPhaseAndCounter(t *testing.T) {
	cases := []struct {
		name              string
		phase             GoalPhase
		setGoalCount      int
		setGoalArchived   bool
		progressCount     int
		progressArchived  bool
		completeCount     int
		completeArchived  bool
		want              string
	}{
		{"Set phase + set_goal fail 1x → no", GoalPhaseSet, 1, false, 0, false, 0, false, ""},
		{"Set phase + set_goal fail 2x → yes", GoalPhaseSet, 2, false, 0, false, 0, false, GoalPhaseSetStuckAbortReason},
		{"Set phase + set_goal fail 3x → yes", GoalPhaseSet, 3, false, 0, false, 0, false, GoalPhaseSetStuckAbortReason},
		{"Set phase + archived after 1 fail → yes (flag)", GoalPhaseSet, 1, true, 0, false, 0, false, GoalPhaseSetStuckAbortReason},
		{"Set phase + archived with 0 fails → yes (flag only)", GoalPhaseSet, 0, true, 0, false, 0, false, GoalPhaseSetStuckAbortReason},
		{"Checkpoint + goal_progress fail 2x → yes", GoalPhaseCheckpoint, 0, false, 2, false, 0, false, GoalPhaseCheckpointStuckAbortReason},
		{"Checkpoint + goal_progress archived after 1 fail → yes", GoalPhaseCheckpoint, 0, false, 1, true, 0, false, GoalPhaseCheckpointStuckAbortReason},
		{"Final + complete_goal fail 2x → yes", GoalPhaseFinal, 0, false, 0, false, 2, false, GoalPhaseFinalStuckAbortReason},
		{"Final + complete_goal archived after 1 fail → yes", GoalPhaseFinal, 0, false, 0, false, 1, true, GoalPhaseFinalStuckAbortReason},
		{"Open phase + set_goal archived → no (wrong phase)", GoalPhaseOpen, 0, true, 0, false, 0, false, ""},
		{"Checkpoint + set_goal archived → no (wrong tool)", GoalPhaseCheckpoint, 0, true, 0, false, 0, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computePhaseStuckAbortReasonForPhase(
				tc.phase,
				tc.setGoalCount, tc.setGoalArchived,
				tc.progressCount, tc.progressArchived,
				tc.completeCount, tc.completeArchived,
			)
			if got != tc.want {
				t.Errorf("phase=%s set=%d/%v progress=%d/%v complete=%d/%v → got %q, want %q",
					tc.phase,
					tc.setGoalCount, tc.setGoalArchived,
					tc.progressCount, tc.progressArchived,
					tc.completeCount, tc.completeArchived,
					got, tc.want)
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
	ts.setGoalAttemptCount = 2
	ts.lastPhaseStuckError = "missing required property name"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal setup could not complete") {
		t.Fatalf("expected fallback to contain 'Goal setup could not complete', got %q", got)
	}
	if !strings.Contains(got, "set_goal") {
		t.Fatalf("expected fallback to mention set_goal, got %q", got)
	}
	if strings.Contains(got, "empty response") {
		t.Fatalf("phase-stuck fallback must NOT contain the misleading 'empty response' string; got %q", got)
	}
	// Phase 12.43: no phase enum claims in LLM-visible text.
	assertNoPhaseClaim(t, got)
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
	ts.goalProgressAttemptCount = 2
	ts.lastPhaseStuckError = "missing required field completed_steps"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal continuation could not complete") {
		t.Fatalf("expected fallback to contain 'Goal continuation could not complete', got %q", got)
	}
	if !strings.Contains(got, "goal_progress") {
		t.Fatalf("expected fallback to mention goal_progress, got %q", got)
	}
	// Phase 12.43: no phase enum claims in LLM-visible text.
	assertNoPhaseClaim(t, got)
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
	ts.completeGoalAttemptCount = 2
	ts.lastPhaseStuckError = "summary too short"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "Goal finalization could not complete") {
		t.Fatalf("expected fallback to contain 'Goal finalization could not complete', got %q", got)
	}
	if !strings.Contains(got, "complete_goal") {
		t.Fatalf("expected fallback to mention complete_goal, got %q", got)
	}
	// Phase 12.43: no phase enum claims in LLM-visible text.
	assertNoPhaseClaim(t, got)
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
	ts.setGoalAttemptCount = 5 // even with high counter, Summary wins

	got := al.applyFallbackForEmptyResponse(ts)
	if got != wantSummary {
		t.Fatalf("expected goal.Summary %q to take priority, got %q", wantSummary, got)
	}
}

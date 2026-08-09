package agent

import "testing"

// TestGateSkipMessageForPhase_Set — Set phase: only set_goal is available.
// Must NOT recommend complete_goal (not in Set allowlist).
func TestGateSkipMessageForPhase_Set(t *testing.T) {
	got := gateSkipMessageForPhase("read_file", GoalPhaseSet)
	if !contains(got, "set_goal") {
		t.Errorf("Set variant must mention set_goal: %s", got)
	}
	if !contains(got, "is temporarily unavailable") {
		t.Errorf("must use new phrase: %s", got)
	}
	if contains(got, "complete_goal") {
		t.Errorf("Set variant must NOT mention complete_goal (not in Set allowlist): %s", got)
	}
	assertNoPhaseClaim(t, got)
}

// TestGateSkipMessageForPhase_Checkpoint — Checkpoint phase: goal_progress
// + complete_goal available. Must mention both. Phase 12.67: must state
// the TERMINAL consequence of complete_goal (permanent archive, no resume)
// so the LLM does not pick it as a harmless "send reply" mechanism
// (root cause of main-turn-2 2026-08-09: goal archived unintentionally).
func TestGateSkipMessageForPhase_Checkpoint(t *testing.T) {
	got := gateSkipMessageForPhase("read_file", GoalPhaseCheckpoint)
	if !contains(got, "goal_progress") {
		t.Errorf("Checkpoint variant must mention goal_progress: %s", got)
	}
	if !contains(got, "complete_goal") {
		t.Errorf("Checkpoint variant must mention complete_goal: %s", got)
	}
	if !contains(got, "ALIVE") {
		t.Errorf("Checkpoint variant must say goal_progress keeps the goal ALIVE: %s", got)
	}
	if !contains(got, "cannot be resumed") {
		t.Errorf("Checkpoint variant must state complete_goal cannot be resumed: %s", got)
	}
	if !contains(got, "PERMANENTLY ARCHIVES") {
		t.Errorf("Checkpoint variant must state complete_goal permanently archives: %s", got)
	}
	assertNoPhaseClaim(t, got)
}

// TestGateSkipMessageForPhase_Final — Final phase: only complete_goal
// available. Must NOT mention goal_progress or set_goal.
func TestGateSkipMessageForPhase_Final(t *testing.T) {
	got := gateSkipMessageForPhase("read_file", GoalPhaseFinal)
	if !contains(got, "complete_goal") {
		t.Errorf("Final variant must mention complete_goal: %s", got)
	}
	if contains(got, "goal_progress") {
		t.Errorf("Final variant must NOT mention goal_progress (not in Final allowlist): %s", got)
	}
	if contains(got, "set_goal") {
		t.Errorf("Final variant must NOT mention set_goal (not in Final allowlist): %s", got)
	}
	assertNoPhaseClaim(t, got)
}

// TestGateSkipMessageForPhase_Open — Open phase: generic guidance
// (full tool set available, no specific recommendation).
func TestGateSkipMessageForPhase_Open(t *testing.T) {
	got := gateSkipMessageForPhase("read_file", GoalPhaseOpen)
	if !contains(got, "Try a different tool") {
		t.Errorf("Open variant must keep generic guidance: %s", got)
	}
	assertNoPhaseClaim(t, got)
}

// TestGateSkipMessageForPhase_ToolNameInterpolated — tool name correctly
// inserted into message.
func TestGateSkipMessageForPhase_ToolNameInterpolated(t *testing.T) {
	got := gateSkipMessageForPhase("my_tool", GoalPhaseSet)
	if !contains(got, `"my_tool"`) {
		t.Errorf("must interpolate tool name: %s", got)
	}
}

// TestGateSkipMessageForPhase_AllPhasesNoPhaseClaim — DOUBT-1 verification.
func TestGateSkipMessageForPhase_AllPhasesNoPhaseClaim(t *testing.T) {
	for _, phase := range []GoalPhase{
		GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal,
	} {
		got := gateSkipMessageForPhase("any_tool", phase)
		assertNoPhaseClaim(t, got)
	}
}

// contains — local helper to avoid importing strings just for one call.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(s, substr string) int {
	n, m := len(s), len(substr)
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == substr {
			return i
		}
	}
	return -1
}

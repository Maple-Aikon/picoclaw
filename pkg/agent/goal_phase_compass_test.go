package agent

import (
	"strings"
	"testing"
)

// Phase 12.39 — formatIterCompass unit tests.
//
// This file precedes the production code in goal_phase_compass.go.
// Per TDD Iron Law: write tests first, verify they fail (RED), then
// implement helper to make them pass (GREEN).
//
// Helper signature: formatIterCompass(req PromptBuildRequest, phase GoalPhase, goalFinalized bool) string
//
// Contract (per plan §3.4):
//   - Pure function — no I/O, no mutation.
//   - Returns "" when req.MaxIterationsCap <= 0 (backward compat fallback).
//   - OPEN: defensive invariant — only render "Next CHECKPOINT at iter X" when
//     iter < iterCap AND iterCap < maxCap. Else fall through to
//     "FINAL phase will be at iter M".
//   - CHECKPOINT: header + "Only goal_progress/complete_goal available".
//   - FINAL: branches by goalFinalized.
//     - goalFinalized=true → "Goal is finalized. complete_goal is idempotent..."
//     - goalFinalized=false + iter >= maxCap → "This is the last iter, call complete_goal"
//   - default (GoalPhaseSet or unknown): renders base header line, caller decides.

// mustNotContain — companion to mustContain for paired assertions (Sonar F6).
func mustNotContain(t *testing.T, haystack, needle, rationale string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("expected haystack to NOT contain %q (%s), got:\n%s", needle, rationale, haystack)
	}
}

// Phase 12.72 Fix #2 — T4.1 by-block rewrite. formatIterCompass no
// longer renders the OPEN header (it migrated to user[0] via
// formatDynamicGoalPhaseBanner). OPEN case now returns "" — the helper
// is dead-code for OPEN, but kept as a sentinel so future refactors that
// re-introduce an OPEN call site will see a clear "" return and fail
// the by-block test instead of silently rendering into system. See
// plan §15 T1.7 + §4 Step 4 T4.1.
//
// T1 — OPEN at iter 2, cap 5/15: helper now returns "" (dead-code path).
func TestFormatIterCompass_Open_Iter2_NextCheckpointAtCap5(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        2,
		IterationCap:     5,
		MaxIterationsCap: 15,
	}, GoalPhaseOpen, false)
	if got != "" {
		t.Errorf("Phase 12.72 Fix #2: formatIterCompass(OPEN) must return \"\" (compass migrated to user[0]); got %q", got)
	}
}

// T2 — OPEN at iter 6, cap 10/15 (after goal_progress extend 5→10): helper now returns "".
func TestFormatIterCompass_Open_Iter6_AfterExtend_NextCheckpointAt10(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        6,
		IterationCap:     10,
		MaxIterationsCap: 15,
	}, GoalPhaseOpen, false)
	if got != "" {
		t.Errorf("Phase 12.72 Fix #2: formatIterCompass(OPEN) must return \"\"; got %q", got)
	}
}

// T3 — OPEN at iter 11, cap 15/15 (iterCap == maxCap): helper now returns "".
func TestFormatIterCompass_Open_Iter11_AtMaxCap_FinalMarker(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        11,
		IterationCap:     15,
		MaxIterationsCap: 15,
	}, GoalPhaseOpen, false)
	if got != "" {
		t.Errorf("Phase 12.72 Fix #2: formatIterCompass(OPEN) must return \"\"; got %q", got)
	}
}

// T4 — backward compat: MaxIterationsCap=0 → helper returns "" (unchanged).
func TestFormatIterCompass_Open_MaxCapZero_ReturnsEmpty(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        4,
		IterationCap:     5,
		MaxIterationsCap: 0,
	}, GoalPhaseOpen, false)
	if got != "" {
		t.Errorf("expected empty string for MaxIterationsCap=0, got: %q", got)
	}
}

// T11 — OPEN at iter 4, cap 5/15 (last iter before CHECKPOINT): helper now returns "".
func TestFormatIterCompass_Open_Iter4_LastBeforeCheckpoint(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        4,
		IterationCap:     5,
		MaxIterationsCap: 15,
	}, GoalPhaseOpen, false)
	if got != "" {
		t.Errorf("Phase 12.72 Fix #2: formatIterCompass(OPEN) must return \"\"; got %q", got)
	}
}

// T12 — Defensive invariant (Sonar F1): OPEN with malformed state iter=6, iterCap=5, maxCap=15
// is now moot post-12.72 (helper returns "" for OPEN unconditionally), but kept as a
// regression-proof that the OPEN case doesn't accidentally re-render FINAL fallback.
func TestFormatIterCompass_Open_MalformedIterGteIterCap_FallsThroughToFinal(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        6,
		IterationCap:     5,
		MaxIterationsCap: 15,
	}, GoalPhaseOpen, false)
	if got != "" {
		t.Errorf("Phase 12.72 Fix #2: formatIterCompass(OPEN) must return \"\"; got %q", got)
	}
}

// T14 — OPEN with iterCap=0, maxCap=15: helper now returns "".
func TestFormatIterCompass_Open_IterCapZero_FinalMarker(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        3,
		IterationCap:     0,
		MaxIterationsCap: 15,
	}, GoalPhaseOpen, false)
	if got != "" {
		t.Errorf("Phase 12.72 Fix #2: formatIterCompass(OPEN) must return \"\"; got %q", got)
	}
}

// T5 — CHECKPOINT at iter 5, maxCap=15 → "Only goal_progress/complete_goal available".
func TestFormatIterCompass_Checkpoint_Iter5_OnlyLifecycleTools(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        5,
		IterationCap:     5,
		MaxIterationsCap: 15,
	}, GoalPhaseCheckpoint, false)
	mustContain(t, got, "Goal phase: checkpoint (iter 5 / total 15 turn iters).", "CHECKPOINT base header")
	mustContain(t, got, "Only goal_progress/complete_goal available.", "CHECKPOINT affordance text")
}

// T6 — CHECKPOINT at iter 10, maxCap=15 → same shape.
func TestFormatIterCompass_Checkpoint_Iter10_OnlyLifecycleTools(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        10,
		IterationCap:     10,
		MaxIterationsCap: 15,
	}, GoalPhaseCheckpoint, false)
	mustContain(t, got, "Goal phase: checkpoint (iter 10 / total 15 turn iters).", "CHECKPOINT base header")
	mustContain(t, got, "Only goal_progress/complete_goal available.", "CHECKPOINT affordance text")
}

// T8 — FINAL at iter 15, maxCap=15, goalFinalized=false → "This is the last iter, call complete_goal".
func TestFormatIterCompass_Final_Iter15_CeilingReason_LastIterMessage(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        15,
		IterationCap:     15,
		MaxIterationsCap: 15,
	}, GoalPhaseFinal, false)
	mustContain(t, got, "Goal phase: final (iter 15 / total 15 turn iters).", "FINAL base header")
	mustContain(t, got, "This is the last iter, call complete_goal.", "FINAL ceiling-reason message")
	mustNotContain(t, got, "Goal is finalized", "ceiling-reason must NOT show finalized message")
}

// T9 — FINAL at iter 200, maxCap=200 (production default), goalFinalized=false → same shape.
func TestFormatIterCompass_Final_Iter200_ProductionDefault(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        200,
		IterationCap:     200,
		MaxIterationsCap: 200,
	}, GoalPhaseFinal, false)
	mustContain(t, got, "Goal phase: final (iter 200 / total 200 turn iters).", "FINAL base header")
	mustContain(t, got, "This is the last iter, call complete_goal.", "FINAL ceiling-reason message")
}

// T10 — backward compat: FINAL with maxCap=0 → helper returns "" (caller falls back to const legacy).
func TestFormatIterCompass_Final_MaxCapZero_ReturnsEmpty(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        15,
		IterationCap:     15,
		MaxIterationsCap: 0,
	}, GoalPhaseFinal, true)
	if got != "" {
		t.Errorf("expected empty string for MaxIterationsCap=0, got: %q", got)
	}
}

// T-F1 — FINAL with goalFinalized=true (post-complete_goal at low iter) → "Goal is finalized" message,
// NOT "This is the last iter" (would mislead since iter is low).
func TestFormatIterCompass_Final_GoalFinalized_FinalizedMessage(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        2,
		IterationCap:     5,
		MaxIterationsCap: 15,
	}, GoalPhaseFinal, true)
	mustContain(t, got, "Goal phase: FINAL. Goal is finalized.", "FINAL finalized-reason header")
	mustContain(t, got, "complete_goal is idempotent", "FINAL finalized-reason affordance")
	mustNotContain(t, got, "This is the last iter", "FINAL finalized must NOT show last-iter message")
}

// T13 — default branch: phase=GoalPhaseSet → helper returns base header line (caller decides whether to use).
// Locks default branch contract (Sonar F04 / F8).
func TestFormatIterCompass_Default_GoalPhaseSet_ReturnsBaseHeader(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        1,
		IterationCap:     5,
		MaxIterationsCap: 15,
	}, GoalPhaseSet, false)
	mustContain(t, got, "Goal phase: set (iter 1 / total 15 turn iters).", "default branch returns base header for unknown phase")
}

// T15 — default branch: unknown phase → same as T13 (any unknown phase renders base).
func TestFormatIterCompass_Default_UnknownPhase_ReturnsBaseHeader(t *testing.T) {
	got := formatIterCompass(PromptBuildRequest{
		Iteration:        4,
		IterationCap:     5,
		MaxIterationsCap: 15,
	}, GoalPhase("unknown"), false)
	mustContain(t, got, "Goal phase: unknown (iter 4 / total 15 turn iters).", "default branch returns base header for unknown phase")
}
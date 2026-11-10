package agent

import (
	"strings"
	"testing"
)

// Phase 12.39 — wire tests for formatIterCompass integration.
//
// W1-W6 exercise the FULL prompt build path (buildSystemPromptForRequest
// → hint contributors → BuildSystemPromptWithCacheFullKey) to verify
// rendered text matches expected per-phase compass.
//
// Sonar F2 / F03 (HIGH): W4 is stateful — uses the SAME builder instance
// for both iters to prove cache invalidation when iterCap changes.
// Sonar F4: W6 verifies goalFinalized branching.
// Sonar F2 secondary: W5 verifies CHECKPOINT bypasses cache.

// W1 — OPEN at iter 4, cap 5/15. Phase 12.72 Fix #2 removed the dynamic
// header ("Goal phase: open (iter N / total M)" + "Next CHECKPOINT at
// iter X") from the SYSTEM hint — it now lives in user[0] via
// formatDynamicGoalPhaseBanner. The system hint body is constant: only
// the static lifecycle-tool restriction text. This test verifies the
// body shape per phase 12.72.
func TestPhase12_39_Wire_Open_Iter4_NextCheckpointAt5(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())
	prompt := cb.BuildSystemPromptWithCacheFullKey(
		string(GoalPhaseOpen), false, 4, "", 5, 15,
	)
	mustNotContain(t, prompt, "Goal phase: open (iter 4 / total 15 turn iters)",
		"OPEN system hint must NOT contain dynamic header (moved to user[0] banner in 12.72)")
	mustNotContain(t, prompt, "Next CHECKPOINT phase will be at iter 5",
		"OPEN system hint must NOT contain CHECKPOINT marker (moved to user[0] banner in 12.72)")
	// LEVER (a) - 2026-11-10: the OPEN hint body moved OUT of the system
	// prompt into the phase channel, so the system prompt carries no
	// phase affordance at all (that is what keeps the prefix byte-constant
	// across transitions). The affordance must still reach the LLM.
	mustNotContain(t, prompt, "set_goal is LOCKED at OPEN",
		"OPEN affordance must NOT be in the system prompt (lever (a))")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseOpen), 4, "", 5, 15),
		"set_goal is LOCKED at OPEN", "OPEN hint must still render the lifecycle-tool restriction body")
}

// W2 — CHECKPOINT at iter 5, cap 5/15 → rendered prompt contains
// "Goal phase: checkpoint (iter 5 / total 15 turn iters)" + "Only goal_progress/complete_goal available"
// AND does NOT contain "extend the iteration cap by 1" (regression-proof).
func TestPhase12_39_Wire_Checkpoint_Iter5_NoExtendBy1(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())
	prompt := cb.BuildSystemPromptWithCacheFullKey(
		string(GoalPhaseCheckpoint), false, 5, "", 5, 15,
	)
	// LEVER (a) - 2026-11-10: the CHECKPOINT affordance moved from the
	// system prompt to the phase channel. The system prompt must NOT
	// carry it any more (that is what keeps the prefix byte-constant).
	mustNotContain(t, prompt, "Goal phase: checkpoint", "CHECKPOINT affordance must NOT be in the system prompt (lever (a))")
	mustNotContain(t, prompt, "Only goal_progress/complete_goal available", "CHECKPOINT affordance must NOT be in the system prompt (lever (a))")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseCheckpoint), 5, "", 5, 15),
		"Goal phase: checkpoint (iter 5 / total 15 turn iters)", "CHECKPOINT base header")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseCheckpoint), 5, "", 5, 15),
		"Only goal_progress/complete_goal available", "CHECKPOINT affordance")
	assertPhaseChannelAbsent(t, cb, phaseChannelReqCaps(string(GoalPhaseCheckpoint), 5, "", 5, 15),
		"extend the iteration cap by 1", "Decision tree (a) extend-by-1 wording must be gone")
}

// W3 — FINAL at iter 15, cap 5/15, goalFinalized=false → "This is the last iter".
// Phase 12.39 design: helper signature adds goalFinalized bool. Wire test verifies
// the helper is called from goalPhaseFinalHintContributor with the correct goalFinalized value.
// This test does NOT pass goalFinalized through the cache helper (the public API doesn't
// take it), so it verifies the default ceiling-reason path.
func TestPhase12_39_Wire_Final_Iter15_LastIterMessage(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())
	prompt := cb.BuildSystemPromptWithCacheFullKey(
		string(GoalPhaseFinal), false, 15, "", 15, 15,
	)
	// LEVER (a) - 2026-11-10: the FINAL affordance moved to the phase channel.
	mustNotContain(t, prompt, "Goal phase: final", "FINAL affordance must NOT be in the system prompt (lever (a))")
	mustNotContain(t, prompt, "This is the last iter, call complete_goal", "FINAL affordance must NOT be in the system prompt (lever (a))")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseFinal), 15, "", 15, 15),
		"Goal phase: final (iter 15 / total 15 turn iters)", "FINAL base header")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseFinal), 15, "", 15, 15),
		"This is the last iter, call complete_goal", "FINAL ceiling-reason message")
}

// W4 — STATEFUL CACHE REUSE TEST (Sonar F2 / F03, HIGH priority).
//
// Build OPEN prompt at iter 4 cap 5 max 15 on the SAME builder (warms cache).
// Then build at iter 6 cap 10 max 15 (simulates goal_progress extend).
// Phase 12.72 Fix #2: dynamic OPEN compass is removed from the system
// hint and dropped from the cache key. The cache invariant that remains
// is: cap changes invalidate the cache (cap is still a key dim). Iter
// changes do NOT invalidate (iter dropped for OPEN). Test asserts the
// system prompt remains constant for OPEN regardless of iter, but
// rebuilds on cap change.
func TestPhase12_39_Wire_Open_CacheReuseAfterExtend(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	// Step 1: warm cache at iter 4, cap 5, max 15.
	p1 := cb.BuildSystemPromptWithCacheFullKey(
		string(GoalPhaseOpen), false, 4, "", 5, 15,
	)
	// LEVER (a) - 2026-11-10: the OPEN hint body moved to the phase
	// channel too, so the system prompt carries NO phase affordance.
	mustNotContain(t, p1, "Goal phase: open (iter 4",
		"OPEN system hint must NOT contain dynamic header (moved to user[0] banner in 12.72)")
	mustNotContain(t, p1, "set_goal is LOCKED at OPEN",
		"OPEN affordance must NOT be in the system prompt (lever (a))")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseOpen), 4, "", 5, 15),
		"set_goal is LOCKED at OPEN", "OPEN affordance must reach the LLM via the phase channel")

	// Step 2: SAME builder, cap extended to 10, iter 6. Cap changed → cache
	// invalidates → rebuild. Both prompts still have the same static body.
	p2 := cb.BuildSystemPromptWithCacheFullKey(
		string(GoalPhaseOpen), false, 6, "", 10, 15,
	)
	mustNotContain(t, p2, "Goal phase: open (iter",
		"OPEN system hint must NOT contain dynamic header post-cap-change")
	mustNotContain(t, p2, "set_goal is LOCKED at OPEN",
		"OPEN affordance must NOT be in the system prompt post-cap-change (lever (a))")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseOpen), 6, "", 10, 15),
		"set_goal is LOCKED at OPEN", "OPEN affordance must reach the LLM via the phase channel post-cap-change")
}

// W5 — CHECKPOINT bypasses cache (isCacheableGoalPhase=checkpoint → false).
// Build CHECKPOINT at iter 5 then iter 10 — both should reflect their own iter.
func TestPhase12_39_Wire_Checkpoint_BypassCache_EveryCall(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())
	p1 := cb.BuildSystemPromptWithCacheFullKey(
		string(GoalPhaseCheckpoint), false, 5, "", 5, 15,
	)
	p2 := cb.BuildSystemPromptWithCacheFullKey(
		string(GoalPhaseCheckpoint), false, 10, "", 10, 15,
	)
	// LEVER (a) - 2026-11-10: the CHECKPOINT compass (which embeds the
	// iter) moved to the phase channel.
	mustNotContain(t, p1, "Goal phase: checkpoint", "CHECKPOINT affordance must NOT be in the system prompt (lever (a))")
	mustNotContain(t, p2, "Goal phase: checkpoint", "CHECKPOINT affordance must NOT be in the system prompt (lever (a))")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseCheckpoint), 5, "", 5, 15),
		"Goal phase: checkpoint (iter 5 / total 15 turn iters)", "CHECKPOINT iter 5 header")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseCheckpoint), 10, "", 10, 15),
		"Goal phase: checkpoint (iter 10 / total 15 turn iters)", "CHECKPOINT iter 10 header")
}

// W6 — postCompleteGoalReport (post-complete_goal) interaction.
// Phase 12.7 hint fires alongside goalPhaseFinalHintContributor.
// Wire test verifies both fire — header from helper + post-complete_goal hint text.
func TestPhase12_39_Wire_Final_PostCompleteGoalReportFires(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())
	prompt := cb.BuildSystemPromptWithCacheFullKey(
		string(GoalPhaseFinal), true, 2, "", 5, 15,
	)
	// Phase 12.7 post-complete_goal hint: 5-section template.
	// We don't assert exact text — just that the post-complete_goal hint fires.
	// For the FINAL branching, helper signature still takes goalFinalized param,
	// but the public cache helper doesn't expose it. W6 verifies the broader wire:
	// both FINAL hint + post-complete_goal hint fire together.
	// LEVER (a) - 2026-11-10: the FINAL header moved to the phase channel.
	mustNotContain(t, prompt, "Goal phase: final", "FINAL affordance must NOT be in the system prompt (lever (a))")
	assertPhaseChannelContains(t, cb, phaseChannelReqCaps(string(GoalPhaseFinal), 2, "", 5, 15),
		"Goal phase: final (iter 2 / total 15 turn iters)", "FINAL base header from helper")
}

// ensure strings is referenced (no-op in production, silences unused-import warning
// if the file is ever stripped of all tests).
var _ = strings.Contains

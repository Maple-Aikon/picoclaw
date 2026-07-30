package agent

import "fmt"

// Goal lifecycle tools at OPEN phase (Phase 12.32).
//
// Phase 12.31 added a defense-in-depth lifecycle gate at the execution
// layer (`isLifecycleToolAllowed` in pkg/tools/registry.go) that blocks
// `set_goal` and `goal_progress` calls outside their phase contract:
//   set_goal      → allowed only at GoalPhaseSet (iter 1)
//   goal_progress → allowed only at GoalPhaseCheckpoint (iter = max_tool_iterations)
//   view_goal     → allowed only at GoalPhaseOpen
//   complete_goal → allowed at any non-empty phase
//
// Phase 12.31 silently blocks but does not tell the LLM *why* the tool
// was rejected. The LLM's typical reaction is to retry the same blocked
// tool 2-4 times across iterations, wasting 30+ seconds on confused
// monologue (verified in HORUS Protocol trace 2026-07-30 main-turn-3:
// `goal_progress` blocked at iter 6, retried at iter 8, finally found
// `complete_goal` at iter 9 → 10 iters total when 5 would have sufficed).
//
// This hint fires in the system prompt at OPEN phase to proactively
// educate the LLM about the lifecycle tool restrictions so it doesn't
// waste iterations calling blocked tools. Reactive counterpart lives in
// `recovery_goal.go::buildToolExecErrorRetryMessage` — appends
// `ToolExecErrorOpenPhaseHint` only when the failing tool is a lifecycle
// tool (set_goal / goal_progress); other tool errors at OPEN get the
// generic retry message (Open is a RELATIVE phase — only 2 of 83
// visible tools are blocked, so always-appending would mislead).

// goalPhaseOpenHintText — the hint text injected into the system prompt
// at OPEN phase. Static (no per-iter binding).
const goalPhaseOpenHintText = `Goal lifecycle tools at OPEN phase:
- set_goal is LOCKED at OPEN — it only fires at the SET phase (iter 1, before any goal exists). If you want to define/redefine the goal, you must have called set_goal in a previous turn; otherwise the goal is already set and you should not call set_goal.
- goal_progress is CHECKPOINT-only — it only fires at iter = max_tool_iterations to extend the iteration cap. At any other iter, goal_progress is rejected by the lifecycle gate.
- view_goal works at OPEN — use it to recall the current goal details if needed.
- complete_goal works at OPEN — call it when the work is done. Required args: name (goal id), summary (1-500 char final report in the user's language).
If a lifecycle tool call is rejected, pivot to view_goal/complete_goal or the regular tools; do NOT retry the same blocked tool.`

// goalPhaseOpenHintContributor returns a Capability-layer / Tooling-slot
// PromptPart when the request is in GoalPhaseOpen phase. Returns nil for
// any other phase (Set, Checkpoint, Final) so the hint does not bleed
// across the rest of the turn's lifecycle.
//
// Phase 12.32: hint text is static (no iter-binding) because the
// lifecycle restriction semantics are constant across all OPEN-phase
// iters. This is safe to cache across turns.
func goalPhaseOpenHintContributor(req PromptBuildRequest) *PromptPart {
	if req.GoalPhase != string(GoalPhaseOpen) {
		return nil
	}
	return &PromptPart{
		ID:      "capability.goal_phase_open_hint",
		Layer:   PromptLayerCapability,
		Slot:    PromptSlotTooling,
		Source:  PromptSource{ID: PromptSourceGoalPhaseOpenHint, Name: "goal_phase_open_hint"},
		Title:   "Goal Phase Open Hint",
		Content: fmt.Sprintf("%s\n", goalPhaseOpenHintText),
		Stable:  true,
		Cache:   PromptCacheDefault,
	}
}
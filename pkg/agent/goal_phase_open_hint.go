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

// goalPhaseOpenHintText — dynamic header via formatIterCompass (Phase 12.39)
// + static body for lifecycle tool restriction semantics (Phase 12.32).
//
// Phase 12.39 replaced the Phase 12.38 v2 static "Iteration cap: M" + ceiling
// warning with an event-marker header ("Next CHECKPOINT at iter X" / "FINAL
// phase will be at iter M") per owner decision §1. The static body remains
// for lifecycle tool restriction semantics (constant across OPEN iters).
//
// Helper signature: formatIterCompass(req, phase, goalFinalized). For OPEN,
// we pass goalFinalized=false (OPEN never has goalFinalized=true by
// definition — that's a FINAL-state condition).
func goalPhaseOpenHintText(req PromptBuildRequest) string {
	header := formatIterCompass(req, GoalPhaseOpen, false)
	if header == "" {
		// Backward compat: no cap dims (MaxIterationsCap=0) → no header.
		return fmt.Sprintf("%s\n", goalPhaseOpenHintBodyText)
	}
	return fmt.Sprintf("%s\n%s\n", header, goalPhaseOpenHintBodyText)
}

// goalPhaseOpenHintBodyText — static body for the OPEN hint. Separated
// from the dynamic header so legacy callers (zero cap dims) get just
// the body, and new callers get header + body.
const goalPhaseOpenHintBodyText = `Goal lifecycle tools at OPEN phase:
- set_goal is LOCKED at OPEN — it only fires at the SET phase (iter 1, before any goal exists). If you want to define/redefine the goal, you must have called set_goal in a previous turn; otherwise the goal is already set and you should not call set_goal.
- goal_progress is CHECKPOINT-only — it only fires at a checkpoint to continue the goal lifecycle. At any other iter, goal_progress is rejected by the lifecycle gate.
- view_goal works at OPEN — use it to recall the current goal details if needed.
- complete_goal works at OPEN — call it when the work is done. Required args: name (goal id), summary (1-1000 char final report in the user's language).
If a lifecycle tool call is rejected, pivot to view_goal/complete_goal or the regular tools; do NOT retry the same blocked tool.`

// goalPhaseOpenHintContributor returns a Capability-layer / Tooling-slot
// PromptPart when the request is in GoalPhaseOpen phase. Returns nil for
// any other phase (Set, Checkpoint, Final) so the hint does not bleed
// across the rest of the turn's lifecycle.
//
// Phase 12.32: hint text was static (no iter-binding).
// Phase 12.38 §4: prepends a dynamic header when cap dims are threaded.
// Cache is invalidated via IterationCap/MaxIterationsCap dims on the
// systemPromptCacheKey (Phase 12.38 F50/F52/F58.2) so a cap extension
// at CHECKPOINT correctly invalidates the OPEN cache slot for the next
// iter.
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
		Content: goalPhaseOpenHintText(req),
		Stable:  true,
		Cache:   PromptCacheDefault,
	}
}

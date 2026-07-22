package agent

import "fmt"

// goalPhaseSetHintText is the static hint text injected at iter 1 of a turn
// when the per-turn goal lifecycle is in GoalPhaseSet phase. English per
// anh Maple preference (USER.md language note: EN preferred for LLM-bound
// prompt strings). The hint tells the LLM:
//
//  1. WHY it only sees set_goal (all other tools locked by 4-phase allowlist)
//  2. The two valid forward paths:
//     a. Call set_goal with the turn's objective → unlocks tools for iter 2+
//     b. Skip tool use entirely and reply directly to the user
//  3. Explicit guard: other tool calls will be blocked at execution (Phase 12.3)
//
// Phase 12.3 design: see plan file
// ~/.picoclaw/workspace/memory/plan/picoclaw-phase12.3-execution-gate-allowlist-prompt-20260723.md §3.2.
const goalPhaseSetHintText = `Goal phase: SET (iter 1).

In this phase, only set_goal is available. All other tools are temporarily locked until you set a goal for this turn.

Two valid paths forward:

1. If this turn requires tool use: call set_goal first with the turn's objective. After set_goal succeeds, the remaining tools will unlock for subsequent iterations (Open phase from iter 2 onward).

2. If this turn does not require any tool (e.g. answering a question, returning text only, or having a conversation): respond directly to the user without calling set_goal or any other tool. No set_goal call is required in this case.

Do not call other tools before set_goal — they will be blocked at execution.`

// goalPhaseSetHintContributor returns a Capability-layer / Tooling-slot
// PromptPart when the request is in GoalPhaseSet phase. Returns nil for any
// other phase (Open, Checkpoint, Final) so the hint does not bleed across
// the rest of the turn's lifecycle.
func goalPhaseSetHintContributor(req PromptBuildRequest) *PromptPart {
	if req.GoalPhase != string(GoalPhaseSet) {
		return nil
	}
	return &PromptPart{
		ID:      "capability.goal_phase_set_hint",
		Layer:   PromptLayerCapability,
		Slot:    PromptSlotTooling,
		Source:  PromptSource{ID: PromptSourceGoalPhaseSetHint, Name: "goal_phase_set_hint"},
		Title:   "Goal Phase Set Hint",
		Content: fmt.Sprintf("%s\n", goalPhaseSetHintText),
		Stable:  false,
		Cache:   PromptCacheNone,
	}
}
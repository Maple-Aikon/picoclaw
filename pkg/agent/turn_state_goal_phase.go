package agent

// Goal-phase-aware tool allowlist wiring (Delivery Phase 4 of the goal
// lifecycle plan; see plan §3.7 / §4 for the design rationale).
//
// Per-turn lifecycle tools (`set_goal`, `view_goal`, `goal_progress`,
// `complete_goal`) must be visible to the LLM at the right moments only.
// The 3 phases are:
//
//   GoalPhaseLock       — no active goal on the session. LLM must call
//                          set_goal before any other tool. Enforced by
//                          restrict the allowlist to {set_goal}.
//   GoalPhaseOpen       — active goal exists, iterationCap NOT reached.
//                          LLM can use view_goal + complete_goal + the
//                          normal base tools. set_goal is suppressed to
//                          prevent silent in-turn goal replacement.
//   GoalPhaseCheckpoint — active goal exists AND iterationCap reached
//                          but not yet at the absolute ceiling. LLM is
//                          forced toward goal_progress (extend) or
//                          complete_goal (finalize).
//
// These helpers turn that policy into a per-iteration `Tools.SetAllowlist`
// call. Phase 5+ (BoundedRetry) and Phase 6 (forceFinalGoalProgress)
// stack on top — Phase 4 keeps its scope narrow to the per-iteration
// allowlist recomputation.

import (
	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// hasGoal reports whether this turn's session has an active goal on disk.
//
// Semantics:
//   - missing goal file → false (callers should classify as GoalPhaseLock)
//   - completed/archived goal → false (the LLM must set a fresh goal)
//   - active goal → true
//
// Errors reading the store are treated as "no active goal" — fail-closed
// means the LLM is forced through set_goal, which is the safer side of
// the trade-off when persistence is broken.
func (ts *turnState) hasGoal() bool {
	if ts == nil {
		return false
	}
	if ts.sessionKey == "" || ts.workspace == "" {
		return false
	}
	store := goal.NewStore(ts.workspace)
	g, err := store.Read(ts.sessionKey)
	if err != nil || g == nil {
		return false
	}
	return g.Status == goal.StatusActive
}

// iterationCapReached mirrors the Tier-2 condition used in
// `pipeline_llm.go` (when `extendEnabled && remaining > 0 && remaining <= 2
// && iterationCap < absCap`). Centralising the predicate here keeps the
// phase classifier and the Tier-2 hint logic in lockstep.
//
// Returns false when iterationCap is unset (legacy behaviour), when the
// absolute ceiling has already been hit (that's Tier 3 — see
// iterationCapFinalized), or when extend is disabled.
func (ts *turnState) iterationCapReached() bool {
	if ts == nil || !ts.extendEnabled {
		return false
	}
	absCap := ts.agent.MaxIterationsCap
	if absCap <= 0 {
		return false
	}
	// Tier 3 (absolute ceiling) wins — don't downgrade to Tier 2.
	if ts.iterationCap >= absCap {
		return false
	}
	return ts.iteration >= ts.iterationCap
}

// iterationCapFinalized returns true when this turn has hit the absolute
// iteration ceiling (`iterationCap >= MaxIterationsCap`). It is the
// Tier-3 signal — once true, no further `extend_turn_iteration` calls
// will be honoured. Used by currentGoalPhase to decide whether to drop
// the LLM back to Lock (forcing a fresh goal) or stay in Checkpoint.
func (ts *turnState) iterationCapFinalized() bool {
	if ts == nil {
		return false
	}
	absCap := ts.agent.MaxIterationsCap
	if absCap <= 0 {
		return false
	}
	return ts.iterationCap >= absCap
}

// currentGoalPhase classifies the turn into one of the 3 goal phases.
// Mirrors the pseudocode from plan §4 and delegates to the package-level
// classifier (currentGoalPhase in tool_allowlist_phase.go) so the
// per-turn wiring and the test-only classifier stay in lockstep.
//
// Edge cases:
//   - no agent / no workspace / no sessionKey → GoalPhaseLock (fail-closed)
//   - hasGoal == false → GoalPhaseLock
//   - hasGoal == true && iterationCapFinalized → GoalPhaseLock
//     (treat the absolute ceiling as "start fresh" — the LLM can still
//     call set_goal to begin a new goal on the same session)
//   - hasGoal == true && iterationCapReached → GoalPhaseCheckpoint
//   - hasGoal == true && otherwise → GoalPhaseOpen
func (ts *turnState) currentGoalPhase() GoalPhase {
	if ts == nil || ts.agent == nil {
		return GoalPhaseLock
	}
	if !ts.hasGoal() {
		return GoalPhaseLock
	}
	if ts.iterationCapFinalized() {
		// Tier 3 hit: drop back to Lock so the LLM is forced to set a
		// fresh goal before the next iteration can proceed.
		return GoalPhaseLock
	}
	if ts.iterationCapReached() {
		return GoalPhaseCheckpoint
	}
	return GoalPhaseOpen
}

// applyPhaseAllowlist recomputes the tool allowlist for the given phase
// and pushes it onto the underlying ToolRegistry via SetAllowlist. The
// resolver is `resolveAgentToolAllowlistWithPhase` (Phase 3), which
// returns the fully-qualified names of every tool the LLM should see.
//
// Callers must invoke this once per iteration, BEFORE the LLM call,
// so that ToProviderDefs() reflects the phase-appropriate set. Calling
// it with phase == GoalPhase("") or unknown falls through to base-only
// (the safest degrade).
func (ts *turnState) applyPhaseAllowlist(phase GoalPhase) {
	if ts == nil || ts.agent == nil || ts.agent.Tools == nil {
		return
	}
	allowlist := resolveAgentToolAllowlistWithPhase(ts.agent.Definition, phase)
	ts.agent.Tools.SetAllowlist(allowlist)
}

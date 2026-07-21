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

// iterationCapFinalized returns true when this turn has hit the iteration
// cap. Phase 10 collapsed the previous Tier 2 / Tier 3 distinction — since
// extend_turn_iteration was removed, the iteration cap is effectively
// constant per turn (equal to agent.MaxIterations). Once iteration >=
// iterationCap, no more tools can be called and the goal phase classifier
// drops back to GoalPhaseLock so the LLM is forced to set a fresh goal
// before the next turn.
func (ts *turnState) iterationCapFinalized() bool {
	if ts == nil {
		return false
	}
	return ts.iteration >= ts.iterationCap
}
// currentGoalPhase classifies the turn into one of the 3 goal phases.
// Mirrors the pseudocode from plan §4 and delegates to the package-level
// classifier (currentGoalPhase in tool_allowlist_phase.go) so the
// per-turn wiring and the test-only classifier stay in lockstep.
//
// Edge cases (Phase 10 — extend_turn_iteration removed):
//   - no agent / no workspace / no sessionKey → GoalPhaseLock (fail-closed)
//   - hasGoal == false → GoalPhaseLock
//   - hasGoal == true && iterationCapFinalized → GoalPhaseLock
//     (the LLM is forced to set a fresh goal before the next iteration)
//   - hasGoal == true && otherwise → GoalPhaseOpen
//
// Note: Phase 10 collapsed the previous Tier-2 → Checkpoint path. With
// extend gone, the only way to reach GoalPhaseCheckpoint is via
// archiveGoalAfterCompletion / manual phase bumps. Production code paths
// no longer route through Checkpoint from currentGoalPhase.
func (ts *turnState) currentGoalPhase() GoalPhase {
	if ts == nil || ts.agent == nil {
		return GoalPhaseLock
	}
	if !ts.hasGoal() {
		return GoalPhaseLock
	}
	if ts.iterationCapFinalized() {
		// Hit the iteration cap — drop back to Lock so the LLM is
		// forced to set a fresh goal before the next iteration.
		return GoalPhaseLock
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

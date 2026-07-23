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
// currentGoalPhase classifies the turn into one of the 4 goal phases
// (Phase 11 redesign: set / open / checkpoint / final). Delegates to the
// package-level ResolveGoalPhase classifier so per-turn wiring and the
// test-only classifier stay in lockstep.
//
// Edge cases (Phase 11):
//   - no agent / no workspace / no sessionKey → GoalPhaseSet (fail-closed)
//   - hasGoal == false → GoalPhaseSet (LLM must seed per-turn goal)
//   - goalFinalized flag set → GoalPhaseFinal
//   - iterationCap >= MaxIterationsCap → GoalPhaseFinal
//   - iter >= iterationCap (but iterCap < ceiling) → GoalPhaseCheckpoint
//   - otherwise → GoalPhaseOpen
func (ts *turnState) currentGoalPhase() GoalPhase {
	if ts == nil || ts.agent == nil {
		return GoalPhaseSet
	}
	hasGoal := ts.hasGoal()
	return ResolveGoalPhase(
		hasGoal,
		ts.iteration,
		ts.iterationCap,
		ts.maxIterationsCap,
		ts.goalFinalized,
	)
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
	// Phase 12.5: tell the registry which goal phase is active so per-phase
	// rules inside toolAllowedLocked (e.g. suppress discovery-tool exemption
	// at GoalPhaseSet / GoalPhaseFinal) take effect. Without this, the
	// strict-single-tool allowlist at iter 1 would still show
	// tool_search_tool_bm25 because the unconditional discovery bypass rule
	// would override it. Threading phase here is the only call site that
	// knows the active GoalPhase at allowlist time.
	ts.agent.Tools.SetPhase(string(phase))
}

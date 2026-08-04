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
	"log"

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
		log.Printf("DEBUG[12.16] hasGoal session=%s workspace=%s result=NO (err=%v g=%v)", ts.sessionKey, ts.workspace, err, g)
		return false
	}
	isActive := g.Status == goal.StatusActive
	log.Printf("DEBUG[12.16] hasGoal session=%s name=%s status=%s active=%v", ts.sessionKey, g.Name, g.Status, isActive)
	return isActive
}

// iterationCapFinalized returns true when this turn has hit the iteration
// cap. Phase 10 collapsed the previous Tier 2 / Tier 3 distinction — since
// extend_turn_iteration was removed, the iteration cap is effectively
// constant per turn (equal to agent.MaxIterations) unless goal_progress
// self-extends (Phase 10.1). Once iteration >= iterationCap, the goal
// phase classifier narrows to GoalPhaseCheckpoint, which surfaces only
// the goal lifecycle tools (goal_progress + complete_goal). The LLM can
// either self-extend the cap via goal_progress or finalize the goal via
// complete_goal (which triggers Phase 12.7 final-report iter). Phase 12.8
// removed the legacy Tier 3 force-wrap (toolLimitHintMessage + tool
// stripping); if the LLM is text-only at GoalPhaseCheckpoint, Phase 12
// text-only recovery fires (soft → hard → archive).
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
//   - iter >= MaxIterationsCap → GoalPhaseFinal (Phase 12.30: was
//     `iterationCap >= MaxIterationsCap` — the cap variable is mutable
//     via goal_progress self-extend, comparing it makes FINAL fire too
//     early. Compare the iter index instead.)
//   - iter >= iterationCap (but iterCap < ceiling) → GoalPhaseCheckpoint
//   - otherwise → GoalPhaseOpen
func (ts *turnState) currentGoalPhase() GoalPhase {
	if ts == nil || ts.agent == nil {
		return GoalPhaseSet
	}
	// Test-only escape hatch: AgentInstance.PhaseOverrideForTest forces a
	// specific phase. Used by tests that need a specific phase (e.g.
	// GoalPhaseOpen) at iter 1 without driving a set_goal preamble. See
	// pkg/agent/instance.go for the field contract.
	if ts.agent.PhaseOverrideForTest != "" {
		log.Printf("DEBUG[12.16] currentGoalPhase session=%s phaseOverride=%s", ts.sessionKey, ts.agent.PhaseOverrideForTest)
		return GoalPhase(ts.agent.PhaseOverrideForTest)
	}
	// Phase 12.47 (E3): goal finalized → POST-FINAL, stable until end of
	// turn. No `!postCompleteGoalReportSent` guard — the phase must not
	// oscillate back to Final after the post-body marker flips sent=true
	// (turn-end telemetry/metrics read the phase consistently). Read the
	// field directly (no shouldEmitPostCompleteGoalReport — it locks ts.mu,
	// R4). Single-goroutine turn loop → no race (P4/E4).
	if ts.goalFinalized {
		return GoalPhasePostFinal
	}
	hasG := ts.hasGoal()
	resolved := ResolveGoalPhase(
		hasG,
		ts.iteration,
		ts.iterationCap,
		ts.maxIterationsCap,
		ts.goalFinalized,
	)
	log.Printf("DEBUG[12.16] currentGoalPhase session=%s hasGoal=%v iter=%d iterCap=%d maxCap=%d goalFinalized=%v -> %s", ts.sessionKey, hasG, ts.iteration, ts.iterationCap, ts.maxIterationsCap, ts.goalFinalized, resolved)
	return resolved
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

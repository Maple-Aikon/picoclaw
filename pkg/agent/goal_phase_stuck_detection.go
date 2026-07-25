package agent

// Phase 12.13 — phase-stuck detection helpers.
//
// When the goal is about to be archived (OnExhausted or RecoveryArchiveGoal),
// we need to decide whether the failure was a phase-specific lifecycle tool
// failure (Goal-Set, Goal-Checkpoint, Goal-Final) vs a generic recovery
// exhaustion. If the phase-stuck counter (setGoalFailCount,
// goalProgressFailCount, completeGoalFailCount) is >= 2, we stamp the
// abort_reason with the phase-specific value so applyFallbackForEmptyResponse
// can return a user-facing message that names the phase.

// computePhaseStuckAbortReason returns the phase-specific abort_reason
// if the current phase-stuck counter is >= 2 and current iteration is in
// the matching phase. Returns empty string if no phase-stuck condition is
// detected (caller should fall back to GoalAbortReasonBexhausted).
func (ts *turnState) computePhaseStuckAbortReason() string {
	phase := ts.currentGoalPhase()
	switch phase {
	case GoalPhaseSet:
		if ts.setGoalFailCount >= 2 {
			return GoalPhaseSetStuckAbortReason
		}
	case GoalPhaseCheckpoint:
		if ts.goalProgressFailCount >= 2 {
			return GoalPhaseCheckpointStuckAbortReason
		}
	case GoalPhaseFinal:
		if ts.completeGoalFailCount >= 2 {
			return GoalPhaseFinalStuckAbortReason
		}
	}
	return ""
}

// recordPhaseStuckToolFail (Phase 12.13) — called whenever a phase-specific
// lifecycle tool fails validation. Increments the corresponding counter and
// stores the last error message for use in phaseStuckFallbackMessage.
//
// Caller responsibility: pass the exact tool name (set_goal, goal_progress,
// complete_goal) and the error message from the failed tool result.
func (ts *turnState) recordPhaseStuckToolFail(toolName, errMsg string) {
	switch toolName {
	case "set_goal":
		ts.setGoalFailCount++
		ts.lastPhaseStuckError = errMsg
	case "goal_progress":
		ts.goalProgressFailCount++
		ts.lastPhaseStuckError = errMsg
	case "complete_goal":
		ts.completeGoalFailCount++
		ts.lastPhaseStuckError = errMsg
	}
}

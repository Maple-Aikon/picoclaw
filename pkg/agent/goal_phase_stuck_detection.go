package agent

import "fmt"

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
	return computePhaseStuckAbortReasonForPhase(
		ts.currentGoalPhase(),
		ts.setGoalFailCount,
		ts.goalProgressFailCount,
		ts.completeGoalFailCount,
	)
}

// computePhaseStuckAbortReasonForPhase is the static helper split out
// from computePhaseStuckAbortReason so tests can exercise the pure
// threshold logic without spinning up a full AgentLoop. Returns the
// matching GoalPhase*StuckAbortReason if the count >= 2 for that phase;
// empty string otherwise.
func computePhaseStuckAbortReasonForPhase(
	phase GoalPhase,
	setGoalFails,
	goalProgressFails,
	completeGoalFails int,
) string {
	// Phase 12.48b site 8: StuckBucket is the single source of truth for
	// which counter + abort-reason pair fires when this phase is stuck.
	// Counter mapping via StuckBucket.CounterField(); reason via
	// StuckBucket.AbortReason(); both data-driven.
	policy := PhasePolicyFor(phase)
	if policy == nil || policy.StuckBucket == StuckNone {
		return ""
	}
	counterField := policy.StuckBucket.CounterField()
	var count int
	switch counterField {
	case "setGoalFailCount":
		count = setGoalFails
	case "goalProgressFailCount":
		count = goalProgressFails
	case "completeGoalFailCount":
		count = completeGoalFails
	}
	if count >= 2 {
		return policy.StuckBucket.AbortReason()
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

// recordPhaseStuckToolAllowedBlock (Phase 12.21) — called whenever a tool
// call is rejected by the runtime allowlist (ExecuteWithContext→IsAllowed)
// while the agent is in a restricted-allowlist phase (Set/Checkpoint/Final).
// Maps the rejection to the matching phase-stuck counter so that 2+ blocked
// tool calls in the same restricted phase trigger GoalPhase*StuckMessage
// on archive (Phase 12.21 Fix B; see plan §2.2).
//
// WHY THIS IS DISTINCT FROM recordPhaseStuckToolFail: in a restricted
// phase, the LLM calling a NON-lifecycle tool (write_file, web_search,
// etc.) is itself a phase-stuck signal — the LLM should have called
// set_goal/goal_progress/complete_goal instead. We don't know which
// lifecycle tool the LLM "should have" called, so we bucket the failure
// by current phase only.
//
// Caller responsibility: pass the blocked tool name and the error message
// from the allowlist rejection. The caller is responsible for verifying
// the rejection came from the IsAllowed gate (text contains "is not
// available in the current phase") before calling.
func (ts *turnState) recordPhaseStuckToolAllowedBlock(toolName, errMsg string) {
	enrichedMsg := fmt.Sprintf(
		"called tool %q but only allowed tools are the phase-specific lifecycle tools",
		toolName,
	)
	if errMsg != "" {
		enrichedMsg = enrichedMsg + " — " + errMsg
	}
	recordPhaseStuckToolAllowedBlockInPhase(ts, ts.currentGoalPhase(), toolName, enrichedMsg)
}

// recordPhaseStuckToolAllowedBlockInPhase is the static helper used by
// recordPhaseStuckToolAllowedBlock; split out so tests can exercise the
// pure counter logic without spinning up a full AgentLoop. Same package,
// same function body otherwise.
func recordPhaseStuckToolAllowedBlockInPhase(ts *turnState, phase GoalPhase, toolName, enrichedMsg string) {
	switch phase {
	case GoalPhaseSet:
		ts.setGoalFailCount++
		ts.lastPhaseStuckError = enrichedMsg
	case GoalPhaseCheckpoint:
		ts.goalProgressFailCount++
		ts.lastPhaseStuckError = enrichedMsg
	case GoalPhaseFinal:
		ts.completeGoalFailCount++
		ts.lastPhaseStuckError = enrichedMsg
	}
	// GoalPhaseOpen: no phase-stuck semantics — full tool set is allowed,
	// so a runtime rejection is unexpected and not tracked here. Recovery
	// will still trigger via checkToolExecErrorRecovery (Phase 12.11).
}

// recordPhaseStuckArchive (Phase 12.51a)

// recordPhaseStuckArchive (Phase 12.51a) — called when an archive event
// fires (BoundedRetry exhausted at a restricted phase). Sets the
// phase-stuck counter to the threshold value (>= 2) in one shot via
// the `if count < 2 { count = 2 }` pattern so
// computePhaseStuckAbortReasonForPhase returns the matching StuckBucket
// abort reason.
//
// Distinct from recordPhaseStuckToolFail/recordPhaseStuckToolAllowedBlock:
// those track PER-FAILURE increments (each tool failure = +1), this
// helper tracks PER-ARCHIVE events (one archive event = threshold met).
// Per-failure and per-archive are different semantic units; mixing them
// would break the dual-purpose counter role (Phase 12.51 R10 F01 fix).
//
// Phase 12.51 R10 F01 design: counters are TEMPORARILY dual-purpose
// (failed-attempt count + archive-event marker). Phase 12.52 will split
// into `failedAttempts` + `archiveEvents` to remove the ambiguity.
//
// lastPhaseStuckError preservation: if per-fail increments already
// stamped a richer error message (e.g. "tool X not allowed"), the
// archive event does NOT overwrite it. The richer per-fail message is
// more informative than the generic archive event message.
func (ts *turnState) recordPhaseStuckArchive(phase GoalPhase, errMsg string) {
	switch phase {
	case GoalPhaseSet:
		if ts.setGoalFailCount < 2 {
			ts.setGoalFailCount = 2
		}
		if ts.lastPhaseStuckError == "" {
			ts.lastPhaseStuckError = errMsg
		}
	case GoalPhaseCheckpoint:
		if ts.goalProgressFailCount < 2 {
			ts.goalProgressFailCount = 2
		}
		if ts.lastPhaseStuckError == "" {
			ts.lastPhaseStuckError = errMsg
		}
	case GoalPhaseFinal:
		if ts.completeGoalFailCount < 2 {
			ts.completeGoalFailCount = 2
		}
		if ts.lastPhaseStuckError == "" {
			ts.lastPhaseStuckError = errMsg
		}
	}
	// GoalPhaseOpen / GoalPhasePostFinal: no phase-stuck semantics — archive
	// event in Open uses generic GoalAbortReasonBexhausted, archive event
	// in PostFinal is silent (computePhaseStuckAbortReason returns "").
}

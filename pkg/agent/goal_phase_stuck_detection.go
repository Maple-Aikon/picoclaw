package agent

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// Phase 12.13 — phase-stuck detection helpers.
//
// When the goal is about to be archived (OnExhausted or RecoveryArchiveGoal),
// we need to decide whether the failure was a phase-specific lifecycle tool
// failure (Goal-Set, Goal-Checkpoint, Goal-Final) vs a generic recovery
// exhaustion. If the phase-stuck archive flag (setGoalArchiveFlag,
// goalProgressArchiveFlag, completeGoalArchiveFlag) is set, or the
// per-failure attempt count (setGoalAttemptCount, goalProgressAttemptCount,
// completeGoalAttemptCount) is >= 2, we stamp the
// abort_reason with the phase-specific value so applyFallbackForEmptyResponse
// can return a user-facing message that names the phase.

// computePhaseStuckAbortReason returns the phase-specific abort_reason
// if the current phase-stuck counter is >= 2 and current iteration is in
// the matching phase. Returns empty string if no phase-stuck condition is
// detected (caller should fall back to GoalAbortReasonBexhausted).
func (ts *turnState) computePhaseStuckAbortReason() string {
	return computePhaseStuckAbortReasonForPhase(
		ts.currentGoalPhase(),
		ts.setGoalAttemptCount, ts.setGoalArchiveFlag,
		ts.goalProgressAttemptCount, ts.goalProgressArchiveFlag,
		ts.completeGoalAttemptCount, ts.completeGoalArchiveFlag,
	)
}

// computePhaseStuckAbortReasonForPhase is the static helper split out
// from computePhaseStuckAbortReason so tests can exercise the pure
// threshold logic without spinning up a full AgentLoop. Returns the
// matching GoalPhase*StuckAbortReason if the phase's archive flag is set
// (Phase 12.52a split) OR the count >= 2 (legacy gate preserved for the
// handleGoalRecovery path, which stamps the reason from per-failure
// counts without a recordPhaseStuckArchive call); empty string otherwise.
func computePhaseStuckAbortReasonForPhase(
	phase GoalPhase,
	setGoalFails int, setGoalArchived bool,
	goalProgressFails int, goalProgressArchived bool,
	completeGoalFails int, completeGoalArchived bool,
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
	var archived bool
	switch counterField {
	case "setGoalAttemptCount":
		count, archived = setGoalFails, setGoalArchived
	case "goalProgressAttemptCount":
		count, archived = goalProgressFails, goalProgressArchived
	case "completeGoalAttemptCount":
		count, archived = completeGoalFails, completeGoalArchived
	}
	if archived || count >= 2 {
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
		ts.setGoalAttemptCount++
		ts.lastPhaseStuckError = errMsg
	case "goal_progress":
		ts.goalProgressAttemptCount++
		ts.lastPhaseStuckError = errMsg
	case "complete_goal":
		ts.completeGoalAttemptCount++
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
	// Phase 12.53b Item B: data-driven counter via StuckBucket.counterTarget
	// (replaces the phase-keyed switch). Phase without a bucket → no-op,
	// matching the old default switch behavior.
	if policy := PhasePolicyFor(phase); policy != nil {
		if c := policy.StuckBucket.counterTarget(ts); c != nil {
			*c++
			ts.lastPhaseStuckError = enrichedMsg
		}
	}
	// GoalPhaseOpen: no phase-stuck semantics — full tool set is allowed,
	// so a runtime rejection is unexpected and not tracked here. Recovery
	// will still trigger via checkToolExecErrorRecovery (Phase 12.11).
}

// recordPhaseStuckArchive (Phase 12.52a) — called when an archive event
// fires (BoundedRetry exhausted at a restricted phase). Sets the
// phase's ArchiveFlag (bool) so computePhaseStuckAbortReasonForPhase
// returns the matching StuckBucket abort reason, and guarantees the
// AttemptCount is at least 1 (idempotent-max pattern: `if count < 1 { count = 1 }`)
// so the user-facing "failed N attempt(s)" message never reads 0.
//
// Distinct from recordPhaseStuckToolFail/recordPhaseStuckToolAllowedBlock:
// those track PER-FAILURE increments (each tool failure = +1), this
// helper tracks PER-ARCHIVE events (one archive event = flag set).
// Per-failure and per-archive are different semantic units; mixing them
// would break the dual-purpose counter role (Phase 12.51 R10 F01 fix).
//
// Phase 12.52a: counters SPLIT into *AttemptCount (per-failure increments)
// + *ArchiveFlag (bool, set at archive). The original R10 F01 design
// proposed `archiveEvents` (event count); a bool suffices because archive
// is terminal — at most one archive event per turn per phase (Q3=A chốt).
// Before the split, the count was ratcheted to 2 (dual-purpose) which
// inflated the user-facing "failed N times" display; the flag removes
// the ambiguity and the count stays honest.
//
// Phase 12.51a.1 F02 fix: OVERWRITE lastPhaseStuckError (do NOT preserve).
// Rationale: the bare StuckBucket constant (e.g. "goal_stuck_v1_continuation")
// that OnExhausted stamps first is too terse for the user-facing
// phaseStuckFallbackMessage. The archive event msg ("BoundedRetry
// exhausted at phase checkpoint") is richer and is what the user should
// see. recordPhaseStuckToolFail/recordPhaseStuckToolAllowedBlock still
// preserve (they fire BEFORE archive, when the error context is fresh);
// archive is the terminal event and should win.
// finalizePhaseStuckArchive (Phase 12.52b F1 — post-ship code-review
// finding) is the archive-event helper used by the tool-exec retry chain.
// It records the phase-stuck archive signal AND finalizes the goal on
// disk with the matching abort reason in the same turn.
//
// Why this exists: before 12.52b, retryExecuteToolChain only called
// recordPhaseStuckArchive (flag + counts) and set goalArchiveRequested —
// but computePhaseStuckAbortReason() had NO caller in that path (only
// handleGoalRecovery at pipeline_llm.go:1246/1347 did). phaseStuckFallbackMessage
// reads g.AbortReason from the goal FILE, so the goal was left StatusActive
// in-turn, got archived later as stale_turn_boundary, and the user-facing
// stuck message never fired (main-turn-19 bug class; plan-vs-code mismatch
// of the 12.51a F12 fix).
//
// Mirror of handleGoalRecovery OnExhausted (pipeline_llm.go:1240-1254).
func finalizePhaseStuckArchive(ts *turnState, phase GoalPhase, msg string) {
	if !ts.hasGoal() {
		return
	}
	ts.recordPhaseStuckArchive(phase, msg)
	// F-B (12.52c): resolve the stuck reason with the SAME phase the archive
	// event keyed on — not via computePhaseStuckAbortReason() which
	// re-resolves through currentGoalPhase() (extra store read + phase drift
	// if goalFinalized/iter moved between call and exhaustion).
	abortReason := GoalAbortReasonBexhausted + ":tool_exec"
	if phaseStuckReason := computePhaseStuckAbortReasonForPhase(phase,
		ts.setGoalAttemptCount, ts.setGoalArchiveFlag,
		ts.goalProgressAttemptCount, ts.goalProgressArchiveFlag,
		ts.completeGoalAttemptCount, ts.completeGoalArchiveFlag); phaseStuckReason != "" {
		abortReason = phaseStuckReason
	}
	// F-D (12.52c): stamp the real attempt count into lastPhaseStuckError.
	// This field has NO reset site anywhere in pkg/agent prod code (verified
	// by grep), so it survives into the next turn where the per-iteration
	// *AttemptCount counters are zeroed — phaseStuckFallbackMessage renders
	// both, and the count shown in a later turn must be the true one.
	// recordPhaseStuckArchive already bumped the counter to >= 1.
	// Phase 12.53b Item B: count via stuckCountForArchive (data-driven,
	// F4: phase without a bucket → 0 → omitempty leaves the field absent).
	count := stuckCountForArchive(ts, phase)
	ts.lastPhaseStuckError = fmt.Sprintf("%s (attempts: %d)", msg, count)
	if err := ts.finalizeGoalOnTurnEndWithCount(abortReason, count); err != nil {
		logger.WarnCF("agent", "finalizePhaseStuckArchive: finalizeGoalOnTurnEnd failed",
			map[string]any{"error": err.Error()})
	}
}

// stuckCountForArchive (Phase 12.53b Item B) returns the attempt counter
// for the phase's stuck bucket, or 0 for phases without a bucket
// (StuckNone — Open/PostFinal). 0 flows into finalizeGoalOnTurnEndWithCount
// as "not a phase-stuck archive" (omitempty → field absent on disk),
// replacing the old stamp default of 1 (F4 behavior change, T7c-tested).
func stuckCountForArchive(ts *turnState, phase GoalPhase) int {
	policy := PhasePolicyFor(phase)
	if policy == nil || policy.StuckBucket == StuckNone {
		return 0
	}
	if c := policy.StuckBucket.counterTarget(ts); c != nil {
		return *c
	}
	return 0
}

// stuckArchiveParams (Phase 12.53b D-F1 / F3) resolves the archive abort
// reason + stuck count from ONE phase snapshot so the persisted count and
// the reason cannot drift (Phase 12.52c F-B lesson). handleGoalRecovery's
// two archive sites (OnExhausted + RecoveryArchiveGoal) both call this;
// non-phase-stuck phases return defaultReason + 0 (no field stamped).
func (ts *turnState) stuckArchiveParams(defaultReason string) (string, int) {
	phase := ts.currentGoalPhase()
	abortReason := defaultReason
	if phaseStuckReason := computePhaseStuckAbortReasonForPhase(phase,
		ts.setGoalAttemptCount, ts.setGoalArchiveFlag,
		ts.goalProgressAttemptCount, ts.goalProgressArchiveFlag,
		ts.completeGoalAttemptCount, ts.completeGoalArchiveFlag); phaseStuckReason != "" {
		abortReason = phaseStuckReason
	}
	return abortReason, stuckCountForArchive(ts, phase)
}

func (ts *turnState) recordPhaseStuckArchive(phase GoalPhase, errMsg string) {
	switch phase {
	case GoalPhaseSet:
		if ts.setGoalAttemptCount < 1 {
			ts.setGoalAttemptCount = 1
		}
		ts.setGoalArchiveFlag = true
		if errMsg != "" {
			ts.lastPhaseStuckError = errMsg
		}
	case GoalPhaseCheckpoint:
		if ts.goalProgressAttemptCount < 1 {
			ts.goalProgressAttemptCount = 1
		}
		ts.goalProgressArchiveFlag = true
		if errMsg != "" {
			ts.lastPhaseStuckError = errMsg
		}
	case GoalPhaseFinal:
		if ts.completeGoalAttemptCount < 1 {
			ts.completeGoalAttemptCount = 1
		}
		ts.completeGoalArchiveFlag = true
		if errMsg != "" {
			ts.lastPhaseStuckError = errMsg
		}
	}
	// GoalPhaseOpen / GoalPhasePostFinal: no phase-stuck semantics — archive
	// event in Open uses generic GoalAbortReasonBexhausted, archive event
	// in PostFinal is silent (computePhaseStuckAbortReason returns "").
}

package agent

import (
	"log"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// GoalAbortReason values for ts.finalizeGoalOnTurnEnd. These appear in the
// persisted Goal.AbortReason field and in the Telegram alert so the user
// can later inspect why an in-flight goal was force-archived.
const (
	GoalAbortReasonRunTurnPanic = "runTurn_panic"
	GoalAbortReasonToolPanic    = "tool_panic"
	GoalAbortReasonBexhausted   = "bexhausted" // suffix appended with loop name, e.g. "bexhausted:hook_replay"
	GoalAbortReasonUserAbort    = "user_abort"
)

// finalizeGoalOnTurnEnd is the single source of truth (Phase 6 Hook 1 —
// plan §8.3) for force-archiving an in-flight goal when the agent loop
// cannot recover. It is invoked from 4 trigger points:
//
//	Hook 1: this method (called by Hooks 2/3/4 below)
//	Hook 2: defer recover() in runTurn — panic safety net
//	Hook 3: BoundedRetry.OnExhausted callback in handleHookReplay
//	Hook 4: handleToolPanic — recovery inside pipeline_execute tool loop
//
// finalizeGoalOnTurnEnd is a no-op (returns nil) when:
//   - ts.agent has no Workspace (tests / non-persistent mode)
//   - the current goal is not active (already completed/archived/aborted)
//   - the goal has been deleted from disk between read and write
//
// On success it writes the goal back to disk with StatusAborted +
// AbortedAt + AbortReason, and emits an InfoCF log line for observability.
// The Telegram alert is emitted separately by the caller (so each hook
// can attach its own contextual payload).
//
// This method is idempotent — calling it multiple times on the same goal
// does NOT bump AbortedAt past the first invocation; subsequent calls
// return nil silently because the goal is already in StatusAborted state.
// finalizeGoalOnTurnEnd is the legacy entry point (Phase 12.53a: kept for
// the 6 non-stuck call sites — panic, hook replay, goal recovery, tool
// panic). It archives WITHOUT persisting a stuck attempt count; phase-stuck
// archive paths (finalizePhaseStuckArchive) use
// finalizeGoalOnTurnEndWithCount so a later turn can render the true
// "failed N attempt(s)" count.
func (ts *turnState) finalizeGoalOnTurnEnd(reason string) error {
	return ts.finalizeGoalOnTurnEndWithCount(reason, 0)
}

// finalizeGoalOnTurnEndWithCount is the shared implementation. count is the
// real phase-stuck attempt count to persist into Goal.StuckAttemptCount;
// 0 means "not a phase-stuck archive" and leaves the field untouched
// (omitempty → key absent on disk).
func (ts *turnState) finalizeGoalOnTurnEndWithCount(reason string, count int) error {
	if ts == nil || ts.agent == nil {
		return nil
	}
	if ts.agent.Workspace == "" {
		return nil
	}

	// Test-only escape hatch: see AgentLoop.SkipGoalArchiveForTest. Tests
	// that pre-seed an active goal file before runAgentLoop need finalize
	// to be a no-op too, otherwise BoundedRetry goal-recovery's OnExhausted
	// (Phase 12.11) archives the seed mid-test. Production callers leave
	// this false; this branch is unreachable.
	if ts.agent.SkipGoalArchiveForTest {
		return nil
	}

	store := goal.NewStore(ts.agent.Workspace)
	sessionKey := ts.sessionKey
	if sessionKey == "" {
		return nil
	}

	g, err := store.Read(sessionKey)
	if err != nil {
		// Goal file missing or unreadable — nothing to finalize.
		logger.DebugCF("agent", "finalizeGoalOnTurnEnd: no active goal to archive",
			map[string]any{"session": sessionKey, "reason": reason, "error": err.Error()})
		return nil
	}
	if g == nil {
		return nil
	}
	if g.Status != goal.StatusActive {
		// Already finalized (completed/archived/aborted) — idempotent no-op.
		logger.DebugCF("agent", "finalizeGoalOnTurnEnd: goal already non-active, no archive needed",
			map[string]any{
				"session": sessionKey,
				"name":    g.Name,
				"status":  string(g.Status),
				"reason":  reason,
			})
		return nil
	}

	now := time.Now().UTC()
	g.Status = goal.StatusAborted
	g.AbortedAt = &now
	g.AbortReason = reason
	g.UpdatedAt = now
	if count > 0 {
		g.StuckAttemptCount = count
	}

	if err := store.Write(sessionKey, g); err != nil {
		logger.WarnCF("agent", "finalizeGoalOnTurnEnd: write failed",
			map[string]any{
				"session": sessionKey,
				"name":    g.Name,
				"reason":  reason,
				"error":   err.Error(),
			})
		return err
	}

	logger.InfoCF("agent", "Goal aborted by finalizeGoalOnTurnEnd",
		map[string]any{
			"agent_id":     ts.agent.ID,
			"session":      sessionKey,
			"name":         g.Name,
			"reason":       reason,
			"aborted_at":   now.Format(time.RFC3339),
		})
	return nil
}

// goalArchiveRequestedFromState inspects whether the current turn has
// requested goal archive via Phase 5 trigger flags. Used by callers
// (Hook 2/3/4) to decide whether to invoke Hook 1 (finalizeGoalOnTurnEnd)
// after the runTurn returns.
//
// Returns the GoalAbortReason* string (or "" if no archive was requested).
// Callers should also short-circuit on empty reason so this is safe to
// call unconditionally.
func (ts *turnState) goalArchiveRequestedFromState() string {
	if ts == nil {
		return ""
	}
	if !ts.goalArchiveRequested {
		return ""
	}
	// Phase 5 sets goalArchiveRequested but does not distinguish the
	// specific reason — pick a default that matches the most common
	// trigger (tool-exec error retry exhaustion).
	return GoalAbortReasonBexhausted + ":recovery_trigger"
}

// archiveStaleGoalOnTurnStart is the Phase 11 stale-recovery hook fired at
// the START of every turn (before SetUpTurn reads the goal store). It
// sweeps any StatusActive goal left on disk from a prior turn — the
// per-turn scope means a goal that did not transition to
// StatusCompleted/StatusArchived/StatusAborted before the previous turn
// ended is by definition stale and must not confuse the LLM on the new
// turn.
//
// Behavior:
//   - No workspace / no session key / no store → return nil (no-op).
//   - No active goal on disk → return nil (idempotent).
//   - Active goal on disk → mark StatusAborted + AbortReason="stale_turn_boundary"
//     + AbortedAt=now, write back, then move to archive/ dir.
//   - All errors are propagated (caller logs but does not fail SetupTurn
//     because the worst case is a stale file that the LLM will
//     surface via view_goal — recoverable on the next iteration).
//
// Wired from pkg/agent/pipeline_setup.go::SetupTurn as the very first
// step before any other state read.
func archiveStaleGoalOnTurnStart(al *AgentLoop, sessionKey string) error {
	if al == nil {
		log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart SKIP al=nil")
		return nil
	}
	// Test-only escape hatch: tests that pre-seed an active goal file
	// before runAgentLoop need the goal to survive into GoalPhaseOpen
	// (otherwise the execution gate added in Phase 12.3 blocks all
	// non-[set_goal] tools at iter 1). Production callers MUST leave
	// skipGoalArchiveOnTurnStart=false.
	if al.skipGoalArchiveOnTurnStart.Load() {
		log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart SKIP skipGoalArchiveOnTurnStart=true session=%s", sessionKey)
		return nil
	}
	if al.goalStore() == nil {
		log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart SKIP goalStore=nil session=%s", sessionKey)
		return nil
	}
	if sessionKey == "" {
		log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart SKIP sessionKey=empty")
		return nil
	}
	store := al.goalStore()
	g, err := store.Read(sessionKey)
	if err != nil {
		log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart Read err session=%s err=%v", sessionKey, err)
		// Missing/unreadable file → not stale.
		return nil
	}
	if g == nil || g.Status != goal.StatusActive {
		if g == nil {
			log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart no-op session=%s goal=nil", sessionKey)
		} else {
			log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart no-op session=%s status=%s (not active)", sessionKey, g.Status)
		}
		return nil
	}
	log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart ARCHIVING session=%s name=%s status=%s", sessionKey, g.Name, g.Status)
	now := time.Now().UTC()
	g.Status = goal.StatusAborted
	g.AbortedAt = &now
	g.AbortReason = "stale_turn_boundary"
	g.UpdatedAt = now
	if err := store.Write(sessionKey, g); err != nil {
		log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart Write FAILED session=%s err=%v", sessionKey, err)
		return err
	}
	logger.InfoCF("agent", "Stale active goal archived on turn start",
		map[string]any{
			"session": sessionKey,
			"name":    g.Name,
		})
	if err := store.Archive(sessionKey); err != nil {
		log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart Archive FAILED session=%s err=%v", sessionKey, err)
		return err
	}
	log.Printf("DEBUG[12.16] archiveStaleGoalOnTurnStart Archive OK session=%s name=%s", sessionKey, g.Name)
	return nil
}
// archiveAndResetPriorTurnGoal is the Phase 12.25 cross-turn scope hook.
// It is fired at the START of every new turn (after SetupTurn, BEFORE the
// Phase 12.9 cap+1 pre-loop bump) to enforce the per-turn goal scope:
//
//  1. Archive any unarchived prior-turn active goal on disk (delegates to
//     archiveStaleGoalOnTurnStart for the durable side-effect — file move
//     to archive/ dir).
//  2. Reset in-memory turn-state goal flags so the new turn starts with a
//     clean slate (regardless of whether the prior turn finished or was
//     interrupted). Reset fields:
//     - ts.goalFinalized = false
//     - ts.postCompleteGoalReportSent = false
//     - ts.goalArchiveRequested = false
//     - ts.pendingFinalReportIter = false
//
// Order rationale (Q6 in plan §14): archive FIRST (durable side-effect)
// then reset (in-memory). If the reset were first, an interrupted archive
// would leave in-memory state inconsistent with disk state. By archiving
// first, disk state is always committed before in-memory state is wiped.
//
// Errors from the archive sub-step are propagated (caller logs but does
// not fail the turn — same best-effort policy as archiveStaleGoalOnTurnStart).
//
// Wired from pkg/agent/turn_coord.go::runTurn AFTER SetupTurn and BEFORE
// the Phase 12.9 pre-loop cap+1 bump. This replaces the Phase 12.9
// cross-turn final-report-iter mechanism with explicit per-turn scope.
func archiveAndResetPriorTurnGoal(al *AgentLoop, ts *turnState) error {
	// Step 1: archive prior turn's unarchived active goal (durable).
	if err := archiveStaleGoalOnTurnStart(al, ts.sessionKey); err != nil {
		logger.WarnCF("agent", "phase12.25 archive-and-reset: archive failed",
			map[string]any{
				"session_key": ts.sessionKey,
				"error":       err.Error(),
			})
		// Continue with reset anyway — in-memory state must still be
		// wiped for the new turn to start clean.
	}
	// Step 2: reset in-memory goal flags (per-turn scope).
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	ts.goalArchiveRequested = false
	ts.pendingFinalReportIter = false
	logger.InfoCF("agent", "phase12.25 archive-and-reset: in-memory state cleared",
		map[string]any{
			"session_key": ts.sessionKey,
			"turn_id":     ts.turnID,
		})
	return nil
}

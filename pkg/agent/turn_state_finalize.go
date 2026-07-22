package agent

import (
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
func (ts *turnState) finalizeGoalOnTurnEnd(reason string) error {
	if ts == nil || ts.agent == nil {
		return nil
	}
	if ts.agent.Workspace == "" {
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
	if al == nil || al.goalStore() == nil {
		return nil
	}
	if sessionKey == "" {
		return nil
	}
	store := al.goalStore()
	g, err := store.Read(sessionKey)
	if err != nil {
		// Missing/unreadable file → not stale.
		return nil
	}
	if g == nil || g.Status != goal.StatusActive {
		return nil
	}
	now := time.Now().UTC()
	g.Status = goal.StatusAborted
	g.AbortedAt = &now
	g.AbortReason = "stale_turn_boundary"
	g.UpdatedAt = now
	if err := store.Write(sessionKey, g); err != nil {
		return err
	}
	logger.InfoCF("agent", "Stale active goal archived on turn start",
		map[string]any{
			"session": sessionKey,
			"name":    g.Name,
		})
	return store.Archive(sessionKey)
}
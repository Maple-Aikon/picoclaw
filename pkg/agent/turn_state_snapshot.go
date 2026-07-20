package agent

import (
	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

func (al *AgentLoop) goalWorkspace() string {
	if al.cfg == nil {
		return ""
	}
	return al.cfg.WorkspacePath()
}

// loadTaskSummary returns the 1-2 sentence task summary for a session.
// When useGoalProgress=true, reads from the active goal's StatusSnapshot field
// (Phase 7 plan §3.7 — single source of truth for cross-turn task context).
// When useGoalProgress=false, falls back to the legacy in-memory
// sessionTaskSummary sync.Map (Phases 1-5 default).
func (al *AgentLoop) loadTaskSummary(sessionKey string) string {
	if !al.useGoalProgress {
		if val, ok := al.legacyTaskSummary.Load(sessionKey); ok {
			if s, ok := val.(string); ok {
				return s
			}
		}
		return ""
	}
	ws := al.goalWorkspace()
	if ws == "" {
		return ""
	}
	store := goal.NewStore(ws)
	snapshot, err := store.LoadStatusSnapshot(sessionKey)
	if err != nil {
		return ""
	}
	return snapshot
}

// storeTaskSummary persists a 1-2 sentence task summary for a session.
// Routes to either the active goal's StatusSnapshot (when useGoalProgress=true)
// or the legacy sessionTaskSummary sync.Map. Errors from the goal store are
// non-fatal — we silently keep using the in-memory path if the goal write
// fails. The function is safe to call concurrently from goroutines.
func (al *AgentLoop) storeTaskSummary(sessionKey, summary string) {
	if summary == "" {
		return
	}
	if !al.useGoalProgress {
		al.legacyTaskSummary.Store(sessionKey, summary)
		return
	}
	ws := al.goalWorkspace()
	if ws == "" {
		return
	}
	store := goal.NewStore(ws)
	if err := store.UpdateStatusSnapshot(sessionKey, summary); err != nil {
		// ErrNoActiveGoal = graceful fallback to legacy map. Any other error
		// (transient I/O, permission) also falls back so cross-turn recovery
		// keeps working even if the disk write fails.
		al.legacyTaskSummary.Store(sessionKey, summary)
	}
}

// deleteTaskSummary clears any persisted task summary for a session, used by
// /clear and /new commands. Idempotent — safe to call when no summary exists.
func (al *AgentLoop) deleteTaskSummary(sessionKey string) {
	if !al.useGoalProgress {
		al.legacyTaskSummary.Delete(sessionKey)
		return
	}
	// For useGoalProgress=true we deliberately do NOT clear the goal file's
	// StatusSnapshot — the goal store is the system-of-record; /clear should
	// only affect legacy in-memory state. To fully reset a goal, callers use
	// the complete_goal tool (Phase 4 ship).
}

package agent

import (
	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// goalStore returns a fresh goal.Store rooted at the configured workspace, or
// nil if no workspace is configured. The store is cheap to construct (just a
// path wrapper) and stateless, so per-call construction is safe.
func (al *AgentLoop) goalStore() *goal.Store {
	ws := al.goalWorkspace()
	if ws == "" {
		return nil
	}
	return goal.NewStore(ws)
}

// goalWorkspace returns the on-disk workspace root for goal file storage.
// Returns "" when no config is wired (test stubs).
func (al *AgentLoop) goalWorkspace() string {
	if al.cfg == nil {
		return ""
	}
	return al.cfg.WorkspacePath()
}

// loadTaskSummary returns the cross-turn task context for the given session key.
// Phase 8.3: single-path implementation — reads from goal.StatusSnapshot only.
// Legacy in-memory sync.Map fallback was removed (was unreachable in production
// since Phase 7 flipped useGoalProgress=true; Phase 8.2 confirmed via live tests).
//
// Returns "" when no goal is active OR no snapshot has been written yet.
// Caller is responsible for raw-text fallback synthesis (see pipeline_setup.go).
func (al *AgentLoop) loadTaskSummary(sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	store := al.goalStore()
	if store == nil {
		return ""
	}
	snapshot, err := store.LoadStatusSnapshot(sessionKey)
	if err != nil {
		return ""
	}
	return snapshot
}

// storeTaskSummary persists the rendered cross-turn task context to the active
// goal's StatusSnapshot. Phase 8.3: direct write via goal.Store.UpdateStatusSnapshot.
// (Was a 3-call LLM extraction path in v1; Phase 8 collapsed it to a single
// RenderGoalSnapshot call whose output is committed here.)
//
// No-op when sessionKey/summary empty, no workspace, or no active goal.
func (al *AgentLoop) storeTaskSummary(sessionKey, summary string) {
	if sessionKey == "" || summary == "" {
		return
	}
	store := al.goalStore()
	if store == nil {
		return
	}
	_ = store.UpdateStatusSnapshot(sessionKey, summary)
}

// deleteTaskSummary clears any persisted task summary for a session, used by
// /clear and /new commands. Phase 8.3: no-op for goal-backed sessions because
// the goal store is the system-of-record and /clear should not nuke goal state
// (preserves the historical-record invariant from Phase 7 plan §3.7).
// Idempotent and safe to call when no summary exists.
func (al *AgentLoop) deleteTaskSummary(sessionKey string) {
	if sessionKey == "" {
		return
	}
	// Intentional no-op — see comment above. Retained as a stable API for
	// /clear and /new command handlers so future phases can decide whether
	// to also archive the active goal.
}
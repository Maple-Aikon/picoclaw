package agent

// Phase 11: this file is intentionally thin. The Phase 7-8.3 cross-turn
// task summary mechanism (StatusSnapshot field, loadTaskSummary /
// storeTaskSummary / deleteTaskSummary, injectedTaskSummary) has been
// removed because goal scope is now per-turn:
//
//	- A goal is seeded at turn start (set_goal in iter 1) and archived
//	  at turn end (Hook 1 in turn_state_finalize.go, or complete_goal).
//	- Cross-turn context does not need a separate mechanism — the next
//	  turn will see a fresh user message and the LLM will seed its own
//	  goal based on that.
//	- The StatusSnapshot / RenderGoalSnapshot / UpdateStatusSnapshot /
//	  LoadStatusSnapshot plumbing is gone (pkg/agent/goal/snapshot.go
//	  deleted, methods removed from pkg/agent/goal/store.go).
//
// What remains here is the goal workspace helpers (used by the turn
// setup path to wire the per-turn stale-goal recovery at turn start
// — see pkg/agent/turn_state_finalize.go::archiveStaleGoalOnTurnStart).

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

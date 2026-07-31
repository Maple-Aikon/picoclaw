package agent

import (
	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// loadGoalSnapshotForHint returns the rendered goal header (objective +
// success criteria + scopes) for the active goal of the given session, or
// empty string when no active goal exists.
//
// Phase 12.34: the CHECKPOINT-phase hint contributor reads this snapshot
// to prepend goal context to its text, so the LLM can choose between
// goal_progress (extend) and complete_goal (finalize) based on actual
// goal state rather than guessing from the bare "iter N" cue.
//
// Fail-closed semantics (matches `hasGoal` policy):
//   - empty workspace / sessionKey → "" (no goal context to inject)
//   - missing goal file             → "" (LLM sees the bare hint)
//   - completed/archived goal       → "" (terminal-state, no work pending)
//   - read error                    → "" (treat as missing; safer to omit
//     context than to inject stale data)
//   - active goal                   → goal.RenderHeader() output
//
// The function is intentionally NOT a method on `*turnState` (Finding #4
// in the Phase 12.34 plan review): keeping it pure makes it testable in
// isolation and avoids threading workspace/channel/chatID/agentID through
// a turnState method just for the hint path. Callers at
// promptBuildRequestForTurn already have all 4 fields in scope.
func loadGoalSnapshotForHint(workspace, sessionKey string) string {
	if workspace == "" || sessionKey == "" {
		return ""
	}
	store := goal.NewStore(workspace)
	g, err := store.Read(sessionKey)
	if err != nil || g == nil {
		return ""
	}
	if g.Status != goal.StatusActive {
		return ""
	}
	return g.RenderHeader()
}

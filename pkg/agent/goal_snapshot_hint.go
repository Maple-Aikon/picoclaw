package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/utils"
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

// loadActiveGoalObjectiveForFeedback returns the active goal's Objective text
// for the given session, or empty string when no active goal exists. The text
// is rune-truncated to 200 chars (via utils.Truncate) so it fits the
// tool-feedback card body without overflowing inline render.
//
// Phase 12.71 (proposed): when the LLM emits a tool call without explanation
// and the latest user message is empty (or the tool call is the user's first
// follow-up), the tool-feedback card would otherwise show a stale
// "Continuing the current task.: <last user message>" fallback or be empty.
// Reading the active goal's Objective gives the user immediate context of
// what task the agent is working on for every tool call.
//
// Fail-closed semantics (matches loadGoalSnapshotForHint policy):
//   - empty workspace / sessionKey → ""
//   - missing goal file             → ""
//   - completed/archived/aborted goal → "" (terminal-state, no work pending)
//   - read error                    → ""
//   - active goal with empty Objective (defensive — Validate rejects this,
//     but do not silently fabricate text if the on-disk file is corrupt) → ""
//   - active goal with objective    → utils.Truncate(trimmed, 200)
func loadActiveGoalObjectiveForFeedback(workspace, sessionKey string) string {
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
	objective := strings.TrimSpace(g.Description.Objective)
	if objective == "" {
		return ""
	}
	return utils.Truncate(objective, 200)
}

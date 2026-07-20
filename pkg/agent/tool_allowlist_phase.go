package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// GoalPhase classifies a turn along the goal-lifecycle axis so the tool
// allowlist can expand/contract based on what is appropriate to do at
// the current point in execution.
//
// Definitions (see plan §3.5):
//   - GoalPhaseLock:        no goal set yet — base allowlist is irrelevant,
//                           only set_goal is exposed so the LLM picks the
//                           goal before doing anything else.
//   - GoalPhaseOpen:        goal is active and under normal execution —
//                           view_goal + complete_goal are exposed alongside
//                           the base allowlist (set_goal is NOT exposed to
//                           prevent silent in-turn goal changes).
//   - GoalPhaseCheckpoint:  goal iter cap reached and awaiting conclusion —
//                           goal_progress + complete_goal are exposed so
//                           the LLM can either finalize or surface progress
//                           and request extension.
type GoalPhase string

const (
	GoalPhaseLock       GoalPhase = "lock"
	GoalPhaseOpen       GoalPhase = "open"
	GoalPhaseCheckpoint GoalPhase = "checkpoint"
)

// defaultGoalPhase is what we return when we cannot read goal state from
// disk (e.g. Phase 3 ships before the Phase 4 turn_state wiring). It biases
// toward Liveness: if goal layer is unreachable we still expose the
// base allowlist, which is the pre-Phase-3 behavior — no surprise deny.
const defaultGoalPhase = GoalPhaseLock

// GoalToolNamespace is the prefix that disambiguates lifecycle tools from
// any same-named agent-defined tool. Lifecycle tools are registered under
// this namespace and surfaced by allowlist unions.
const GoalToolNamespace = "lifecycle"

// GoalToolNames are the canonical names of the 4 lifecycle tools exported
// by pkg/agent/goal.
var GoalToolNames = []string{
	"set_goal",
	"view_goal",
	"goal_progress",
	"complete_goal",
}

// currentGoalPhase returns the phase appropriate for the current turn.
//
// Phase 3 implementation reads goal state from disk via the goal.Store.
// The full turn-state integration (capFinalized, forceFinalGoalProgress)
// is wired in Phase 4 — for now we drive phase purely off the persisted
// goal status + iteration count.
//
// Rules (see plan §3.6):
//   - No active goal file (or unreadable) → GoalPhaseLock
//   - Completed/archived goal persisted → GoalPhaseLock (force re-set)
//   - Active goal, iteration cap reached → GoalPhaseCheckpoint
//   - Active goal, iteration cap NOT reached → GoalPhaseOpen
//
// If workspace == "" or sessionKey == "" we return defaultGoalPhase (Lock)
// — fail-closed so the LLM is forced through set_goal first.
func currentGoalPhase(workspace, sessionKey string, iteration, iterationCap int) GoalPhase {
	if workspace == "" || sessionKey == "" {
		return defaultGoalPhase
	}
	store := goal.NewStore(workspace)
	g, err := store.Read(sessionKey)
	if err != nil || g == nil {
		return GoalPhaseLock
	}
	if g.Status != goal.StatusActive {
		return GoalPhaseLock
	}
	if iterationCap > 0 && iteration >= iterationCap {
		return GoalPhaseCheckpoint
	}
	return GoalPhaseOpen
}

// unionAllowlist returns a deduplicated union of two allowlist slices
// with stable ascending sort so callers can compare with
// reflect.DeepEqual in tests without caring about map iteration order.
func unionAllowlist(a, b []string) []string {
	seen := make(map[string]struct{})
	add := func(src []string) {
		for _, raw := range src {
			t := raw
			if t == "" {
				continue
			}
			seen[t] = struct{}{}
		}
	}
	add(a)
	add(b)

	if len(seen) == 0 {
		return nil
	}
	return sortedKeys(seen)
}

// resolveAgentToolAllowlistWithPhase returns the allowlist appropriate
// for the given phase. Phase semantics (plan §3.5):
//
//   - GoalPhaseLock:       just [set_goal] — bypass base allowlist
//   - GoalPhaseOpen:       base ∪ [view_goal, complete_goal]
//   - GoalPhaseCheckpoint: base ∪ [goal_progress, complete_goal]
//
// base := FRONTMATTER-declared tools, normalized via ToLower+TrimSpace
// (same normalization rule as pre-Phase-3 resolveAgentToolAllowlist).
//
// Fail-closed: frontmatterParseFailed → empty allowlist.
func resolveAgentToolAllowlistWithPhase(definition AgentContextDefinition, phase GoalPhase) []string {
	if frontmatterParseFailed(definition) {
		return []string{}
	}
	if definition.Agent == nil || !frontmatterDeclaresField(definition, "tools") {
		return nil
	}

	base := make(map[string]struct{}, len(definition.Agent.Frontmatter.Tools))
	for _, raw := range definition.Agent.Frontmatter.Tools {
		trimmed := strings.ToLower(strings.TrimSpace(raw))
		if trimmed == "" {
			continue
		}
		base[trimmed] = struct{}{}
	}

	switch phase {
	case GoalPhaseLock:
		// Lock phase overrides: do NOT expose base tools, only set_goal.
		// Returning exclusively ["set_goal"] forces the LLM to set a goal
		// before the runtime will surface anything else.
		return []string{"set_goal"}
	case GoalPhaseOpen:
		result := sortedKeys(base)
		return unionAllowlist(result, []string{"view_goal", "complete_goal"})
	case GoalPhaseCheckpoint:
		result := sortedKeys(base)
		return unionAllowlist(result, []string{"goal_progress", "complete_goal"})
	default:
		// Unknown phase → degrade to base only (safest default; no
		// lifecycle tool gets exposed if we cannot classify).
		return sortedKeys(base)
	}
}

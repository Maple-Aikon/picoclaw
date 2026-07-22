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
	// GoalPhaseSet = iter 1 (turn just started, no goal yet). The LLM may
	// only call set_goal to seed a per-turn goal before any other tool.
	// Phase 11 redesign: replaces the old GoalPhaseLock ("lock") semantics.
	GoalPhaseSet GoalPhase = "set"

	// GoalPhaseOpen = full tool set. LLM is free to use any enabled tool.
	// Reached after set_goal succeeds and we are inside the budget.
	GoalPhaseOpen GoalPhase = "open"

	// GoalPhaseCheckpoint = checkpoint phase. The LLM is restricted to
	// goal_progress + complete_goal so it self-evaluates before extending
	// the iteration cap. Reached when iter >= iterationCap while iterCap
	// is still below the absolute ceiling (MaxIterationsCap).
	GoalPhaseCheckpoint GoalPhase = "checkpoint"

	// GoalPhaseFinal = iterCap reached MaxIterationsCap ceiling OR iter
	// exceeded MaxIterationsCap. Only [complete_goal] is allowed. No
	// extension possible — the LLM must finalize the turn.
	// Phase 11: replaces the old "collapsed to allowlist" trick.
	GoalPhaseFinal GoalPhase = "final"

	// Phase 11 NOTE: GoalPhaseLock is kept as a synonym of GoalPhaseSet for
	// backward-compat with older tests/callers. New code should use
	// GoalPhaseSet directly.
	GoalPhaseLock GoalPhase = GoalPhaseSet
)

// defaultGoalPhase is what we return when we cannot read goal state from
// disk (e.g. Phase 3 ships before the Phase 4 turn_state wiring). It biases
// toward Lockdown: if goal layer is unreachable we fail-closed to
// GoalPhaseSet, which forces set_goal first — safer than open access.
const defaultGoalPhase = GoalPhaseSet

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
func currentGoalPhase(workspace, sessionKey string, iteration, iterationCap int, maxIterationsCap int) GoalPhase {
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
	if maxIterationsCap > 0 && iterationCap >= maxIterationsCap {
		return GoalPhaseFinal
	}
	if iterationCap > 0 && iteration >= iterationCap {
		return GoalPhaseCheckpoint
	}
	return GoalPhaseOpen
}

// Phase 11 redesign: per-turn scope. ResolveGoalPhase now operates on the
// new 4-phase scheme (set / open / checkpoint / final). The iter==0
// guard that mapped to GoalPhaseLock in earlier phases is GONE — fresh
// turns now start at iter==0 with no active goal, which resolves to
// GoalPhaseSet directly. The old "iterationCapFinalized → Lock" trick
// is replaced by the explicit GoalPhaseFinal constant.
//
// Iteration semantics:
//
//	GoalPhaseSet        — !hasActiveGoal OR iter <= 1 OR goalFinalized=false but goal already complete
//	GoalPhaseOpen       — iter in [2, iterationCap-1] AND goal active AND goalFinalized=false
//	GoalPhaseCheckpoint — iter >= iterationCap AND iterationCap < maxIterationsCap
//	                      AND goal active AND goalFinalized=false
//	GoalPhaseFinal      — iterationCap >= maxIterationsCap (>0)
//	                      OR iter > maxIterationsCap (>0)
//	                      OR goalFinalized=true
func ResolveGoalPhase(
	hasActiveGoal bool,
	iter int,
	iterationCap int,
	maxIterationsCap int,
	goalFinalized bool,
) GoalPhase {
	if goalFinalized {
		return GoalPhaseFinal
	}
	switch {
	case !hasActiveGoal || iter <= 1:
		return GoalPhaseSet
	case maxIterationsCap > 0 && iterationCap >= maxIterationsCap:
		return GoalPhaseFinal
	case maxIterationsCap > 0 && iter > maxIterationsCap:
		return GoalPhaseFinal
	case iter >= iterationCap:
		return GoalPhaseCheckpoint
	default:
		return GoalPhaseOpen
	}
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
// for the given phase. Phase semantics (Phase 11 redesign, plan §3.5):
//
//   - GoalPhaseSet:        just [set_goal] — bypass base allowlist
//   - GoalPhaseOpen:       base ∪ [view_goal, complete_goal]
//   - GoalPhaseCheckpoint: base ∪ [goal_progress, complete_goal]
//   - GoalPhaseFinal:      just [complete_goal] — no escape, must finalize
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
	case GoalPhaseSet:
		// Set phase overrides: do NOT expose base tools, only set_goal.
		// Returning exclusively ["set_goal"] forces the LLM to set a goal
		// before the runtime will surface anything else.
		return []string{"set_goal"}
	case GoalPhaseOpen:
		result := sortedKeys(base)
		return unionAllowlist(result, []string{"view_goal", "complete_goal"})
	case GoalPhaseCheckpoint:
		result := sortedKeys(base)
		return unionAllowlist(result, []string{"goal_progress", "complete_goal"})
	case GoalPhaseFinal:
		// Final phase: no escape. Only complete_goal allowed. The LLM
		// must finalize the turn — iterCap has hit MaxIterationsCap ceiling.
		return []string{"complete_goal"}
	default:
		// Unknown phase → degrade to base only (safest default; no
		// lifecycle tool gets exposed if we cannot classify).
		return sortedKeys(base)
	}
}

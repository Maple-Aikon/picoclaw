package agent

import (
	"log"
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
	// Phase 12.47: after the goal is finalized, currentGoalPhase() returns
	// GoalPhasePostFinal (below) instead — this constant now fires only
	// for the ceiling case (goalFinalized=false).
	GoalPhaseFinal GoalPhase = "final"

	// GoalPhasePostFinal = the post-complete_goal final-report iteration
	// (Phase 12.47). Reached as soon as ts.goalFinalized is set (wrapper in
	// currentGoalPhase), regardless of postCompleteGoalReportSent. One
	// iteration max: allowlist = [] (no tools), recovery is silent, the LLM
	// only emits the final user-facing report text.
	GoalPhasePostFinal GoalPhase = "post_final"

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
	if maxIterationsCap > 0 && iteration >= maxIterationsCap {
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
//	GoalPhaseFinal      — iter >= maxIterationsCap (>0)
//	                      OR goalFinalized=true (Phase 12.47: dead — wrapper
//	                      returns GoalPhasePostFinal first; kept for tests)
//	GoalPhasePostFinal  — goalFinalized=true (wrapper in currentGoalPhase,
//	                      turn_state_goal_phase.go; 1 iter, allowlist=[])
//
// Phase 12.30 bug fix: the FINAL-phase predicate used to compare
// `iterationCap >= maxIterationsCap` (the cap variable, which is
// mutable via goal_progress self-extend). That made FINAL fire too
// early when goal_progress extended iterationCap to the absolute
// ceiling, then the very next iter saw cap==ceiling and jumped to
// FINAL prematurely. Compare the iteration INDEX (`iter`) instead —
// that reflects the runtime position, not the user-config ceiling.
// See picoclaw-phase12.30 plan §6 for the live-verify that motivated
// the fix.
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
	case maxIterationsCap > 0 && iter >= maxIterationsCap:
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
		log.Printf("DEBUG[12.16] resolveAgentToolAllowlistWithPhase phase=%s frontmatterParseFailed=true -> []", phase)
		return []string{}
	}
	if definition.Agent != nil {
		log.Printf("DEBUG[12.16] resolveAgentToolAllowlistWithPhase phase=%s frontmatterTools=%d", phase, len(definition.Agent.Frontmatter.Tools))
	} else {
		log.Printf("DEBUG[12.16] resolveAgentToolAllowlistWithPhase phase=%s agentNil=true", phase)
	}

	// Phase override shortcuts that DO NOT depend on base allowlist.
	// GoalPhaseSet and GoalPhaseFinal are absolute — they pin tool
	// visibility to lifecycle tools regardless of what the agent's
	// frontmatter declares. This matters for agents whose frontmatter
	// omits the `tools:` field (which is the default for most agents
	// — tools are then sourced entirely from MCP/built-in registries).
	// Pre-fix, returning nil here meant SetAllowlist(nil) cleared the
	// registry's allowlist entirely, exposing ALL 84 registered tools
	// to the LLM at iter 1 (Phase 12.3 wire bug observed live).
	switch phase {
	case GoalPhaseSet:
		return []string{"set_goal"}
	case GoalPhaseFinal:
		return []string{"complete_goal"}
	case GoalPhasePostFinal:
		// Phase 12.47: POST-FINAL has NO tools at all — the LLM only emits
		// the final user-facing report text. Non-nil empty slice (R1): a nil
		// allowlist would mean "allow all" (SetAllowlist(nil) clears the
		// filter), leaking every registered tool into the report iter.
		return []string{}
	case GoalPhaseCheckpoint:
		// Checkpoint is also absolute: when the iter cap is hit, the LLM
		// must either extend (goal_progress) or finalize (complete_goal).
		// Base tools (read_file/exec/...) are intentionally NOT exposed —
		// there's no iteration budget left to spend on more tool work,
		// only on lifecycle decisions. Pre-fix (Phase 12.14), an agent
		// without a `tools:` frontmatter field fell through to the
		// "missing base = nil allowlist" branch, exposing all 85 tools
		// to the LLM at the cap-hit iteration. The LLM would emit
		// `exec` with broken args (MiniMax-M3 streaming quirk), the
		// executor would silently run the empty command, and the turn
		// ended on a toolLimitResponse fallback. See goal `crg-update-latest`
		// on session `sk_v1_9238bf3573...` from 2026-07-25 12:54 ICT.
		return []string{"goal_progress", "complete_goal"}
	}

	if definition.Agent == nil || !frontmatterDeclaresField(definition, "tools") {
		// Open/Checkpoint depend on base tools from frontmatter. If no
		// base is declared, the agent has implicitly opted-in to ALL
		// registered tools (nil allowlist = no filter on the registry).
		// Returning nil here preserves backward compat for agents whose
		// frontmatter omits `tools:` entirely — those rely on built-in
		// + MCP tool registries, not on frontmatter whitelists.
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
	case GoalPhaseOpen:
		// GoalPhaseOpen = "work" phase. LLM has full base tools plus
		// view_goal (peek at goal state) and complete_goal (early
		// completion without going through Checkpoint).
		// IMPORTANT: goal_progress is INTENTIONALLY NOT exposed here
		// (Phase 11 design decision, validated Phase 12.14/12.23/12.24d).
		// Self-extending iteration cap from Open would allow LLM to
		// bypass the cap-burst checkpoint semantics and risk infinite
		// loops. To extend cap, LLM must reach Checkpoint (iter ==
		// iterationCap) which exposes goal_progress as a deterministic
		// one-shot trigger.
		result := sortedKeys(base)
		return unionAllowlist(result, []string{"view_goal", "complete_goal"})
	case GoalPhaseCheckpoint:
		// GoalPhaseCheckpoint = "extend or complete" phase. Lifecycle-only
		// allowlist (goal_progress for self-extend, complete_goal for
		// finalization). ABSOLUTE — base tools not exposed regardless of
		// frontmatter config.
		result := sortedKeys(base)
		return unionAllowlist(result, []string{"goal_progress", "complete_goal"})
	default:
		// Unknown phase → degrade to base only (safest default; no
		// lifecycle tool gets exposed if we cannot classify).
		return sortedKeys(base)
	}
}

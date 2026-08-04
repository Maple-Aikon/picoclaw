// Package phases is the tokens-only source of truth for the 5 goal lifecycle
// phases (Phase 11/12.x). Both pkg/tools (string-keyed policy table) and
// pkg/agent (GoalPhase-keyed policy table) reference these constants — a typo
// in either package becomes a compile error.
//
// IMPORTANT (Phase 12.48, R3-F6 + Plan §3.1 + §4.20 L1):
//   - pkg/phases is TOKENS ONLY (5 untyped string consts).
//   - pkg/phases exposes NO synonyms (no "PhaseLock"). The GoalPhaseLock alias
//     lives at pkg/agent/tool_allowlist_phase.go (alias of GoalPhaseSet)
//     per Phase 11 backward compat.
//   - pkg/agent constants become TYPED ALIASES of these strings:
//     `GoalPhaseSet GoalPhase = phases.PhaseSet`, etc.
//
// Phase tokens (canonical, frozen post-Phase 12.47):
//
//	set         — iter 1, no active goal (allowlist = [set_goal] only)
//	open        — full tool set (allowlist = base ∪ [view_goal, complete_goal])
//	checkpoint  — iter == iterationCap, ABSOLUTE [goal_progress, complete_goal]
//	final       — iter >= MaxIterationsCap, ABSOLUTE [complete_goal] only
//	post_final  — goalFinalized=true (Phase 12.47), allowlist = []
package phases

// Phase token (untyped string) — canonical 5 phases.
const (
	PhaseSet        = "set"
	PhaseOpen       = "open"
	PhaseCheckpoint = "checkpoint"
	PhaseFinal      = "final"
	PhasePostFinal  = "post_final"
)

// allTokens is the canonical ordered list — used by generators and cross-table
// equality tests. Order matches the lifecycle (set → open → checkpoint →
// final → post_final).
var allTokens = []string{PhaseSet, PhaseOpen, PhaseCheckpoint, PhaseFinal, PhasePostFinal}

// knownSet is the O(1) lookup map for IsKnown.
var knownSet = map[string]bool{
	PhaseSet:       true,
	PhaseOpen:      true,
	PhaseCheckpoint: true,
	PhaseFinal:     true,
	PhasePostFinal: true,
}

// AllTokens returns the canonical ordered list of 5 phase tokens.
// Used by generators (pkg/tools/phase_policy.go + pkg/agent/phase_policy.go)
// and cross-table equality tests.
func AllTokens() []string {
	out := make([]string, len(allTokens))
	copy(out, allTokens)
	return out
}

// IsKnown reports whether the given string is one of the 5 canonical phase
// tokens. Returns false for empty string and unknown values — callers use
// this to distinguish "valid phase" from "no-phase-set" (default fallback).
func IsKnown(s string) bool {
	return knownSet[s]
}

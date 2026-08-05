//go:build strict_phases

package agent

import "github.com/sipeed/picoclaw/pkg/phases"

// PhasePolicyFor (STRICT BUILD) returns the canonical agent-side policy
// row for a phase. Panics via phases.MustBeKnown on any non-empty
// unknown phase (after Lock-alias resolution).
//
// Lookup is exact match (NO trim, NO lowercase) — strict semantics.
// Empty phase = "no phase set" = nil (no panic).
//
// This is the CI/staging fail-fast guard (Phase 12.49 §3.3, §4.1).
// Production gateway runs the default build (phase_policy.go), which
// preserves the historic fail-OPEN semantics for backward compat.
func PhasePolicyFor(phase GoalPhase) *AgentPhasePolicy {
	if string(phase) == "" {
		return nil
	}
	// Lock alias resolves BEFORE strict check (R8 verified 2026-08-05).
	if phase == GoalPhaseLock {
		phase = GoalPhaseSet
	}
	// phases.MustBeKnown accepts "set" string (Lock normalized to Set).
	// Strict compares token-string exact match against pkg/phases tokens.
	phases.MustBeKnown(string(phase))
	return agentPolicies[phase]
}

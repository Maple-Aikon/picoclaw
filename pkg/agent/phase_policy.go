// Phase 12.49 — PhasePolicyFor default-mode (fail-OPEN) lookup.
// Data table + type defs live in phase_policy_table.go.
// Strict-mode variant lives in phase_policy_strict.go.
//go:build !strict_phases

package agent

// PhasePolicyFor (DEFAULT BUILD) returns the canonical agent-side policy
// row for a phase, or nil if phase is unknown / empty.
//
// Maps GoalPhaseLock alias to GoalPhaseSet (Phase 11 backward compat).
// Lookup is case-insensitive on the lowercase string form (Phase 12.1
// regression guard).
//
// For strict-mode enforcement (panic on unknown), use the
// phase_policy_strict.go variant via `-tags strict_phases`.
//
// Phase 12.49 §3.4: when PICOCLAW_AGENT_STRICT_PHASES=1, ANY unknown
// non-empty phase is recorded as a miss for production canary telemetry
// (counter + warn log). Caller behavior is unchanged.
func PhasePolicyFor(phase GoalPhase) *AgentPhasePolicy {
	if string(phase) == "" {
		return nil
	}
	if phase == GoalPhaseLock {
		phase = GoalPhaseSet
	}
	trimmed := trimASCII(string(phase))
	key := GoalPhase(stringToLowerCopy(trimmed))
	p := agentPolicies[key]
	if p == nil && trimmed != "" {
		recordPhaseLookupMiss("phase_policy", trimmed)
	}
	return p
}

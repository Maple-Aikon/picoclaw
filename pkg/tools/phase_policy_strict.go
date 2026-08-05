//go:build strict_phases

package tools

import (
	"github.com/sipeed/picoclaw/pkg/phases"
)

// ToolPolicyForPhase (STRICT BUILD) returns the canonical policy row for
// a phase token. Panics with *phases.UnknownPhaseError when `phase` is
// non-empty and does NOT match one of the 5 canonical tokens (after
// strict equality — NO trim, NO lowercase).
//
// This is the CI/staging fail-fast guard (Phase 12.49 §3.3, §4.1).
// Production gateway runs the default build (phase_policy.go), which
// preserves the historic fail-OPEN semantics for backward compat. To
// enable strict enforcement in production opt-in telemetry mode, set
// PICOCLAW_AGENT_STRICT_PHASES=1 (covered separately via recordPhaseLookupMiss).
func ToolPolicyForPhase(phase string) *ToolVisibilityPolicy {
	phases.MustBeKnown(phase) // panics on non-empty + unknown
	return toolPolicies[phase]
}

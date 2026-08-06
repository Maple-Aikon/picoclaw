// Phase 12.49 — ToolPolicyForPhase: DEFAULT BUILD (fail-OPEN lookup).
// See phase_policy_strict.go for the strict-mode variant.
//go:build !strict_phases

package tools

import "strings"

// ToolPolicyForPhase (DEFAULT BUILD) returns the canonical policy row for
// a phase token, or nil if phase is unknown / empty. Case-insensitive
// (Phase 12.1 regression guard).
//
// NOTE: callers MUST handle nil. The 2 sensitive sites (IsLifecycleToolAllowed,
// toolAllowedLocked suppression path) treat nil as fail-CLOSED (R6-F1 L2).
// Non-sensitive sites (resolver, hint contributors) treat nil as fail-OPEN.
//
// For strict-mode enforcement (panic on unknown), use the
// `phase_policy_strict.go` variant compiled in via `-tags strict_phases`.
//
// Phase 12.49 §3.4: PICOCLAW_AGENT_STRICT_PHASES=1 records each unknown
// non-empty lookup miss for canary telemetry (counter + warn log).
func ToolPolicyForPhase(phase string) *ToolVisibilityPolicy {
	if phase == "" {
		return nil
	}
	// Case-insensitive lookup — phase tokens are canonical lowercase but
	// guard against accidental capital-O fallout (cf. Phase 12.1 regression).
	trimmed := strings.TrimSpace(phase)
	lower := strings.ToLower(trimmed)
	p := toolPolicies[lower]
	if p == nil && trimmed != "" {
		// Phase 12.50 §3.3: route through callback registry so pkg/agent
		// can publish into the same miss stream as PhasePolicyFor.
		// Pre-12.50: local no-op stub (counter only fired in pkg/agent).
		publishLookupMiss("tool_policy", trimmed)
		// Local counter (in-memory only, resets on restart) for direct
		// telemetry from pkg/tools path. Callback handler may also bump
		// the pkg/agent counter; that's fine — two counters with same
		// semantics, no enforcement.
		recordPhaseLookupMissLocal("tool_policy", trimmed)
	}
	return p
}

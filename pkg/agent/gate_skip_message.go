package agent

import "fmt"

// Phase 12.43 — gate-skip message helper (Q10-A phase-split).
//
// When a tool call is blocked at the runtime allowlist gate (Phase 12.35
// gate pre-check or registry.go denied path), the LLM-visible message
// must:
//   1. NOT contain phase/state claims (per Phase 12.40 / 12.40.1 invariant)
//   2. NOT recommend a tool blocked at the same gate (would mislead LLM)
//
// Each phase variant names ONLY the tools actually available at that
// phase. The user-facing message routes to the LLM via tool result and
// persists in session history (history-poison channel).
func gateSkipMessageForPhase(toolName string, phase GoalPhase) string {
	// Phase 12.48b site 9: policy-driven lookup. Each phase's GateSkipText
	// is the template; format with toolName at call site. Lock alias
	// resolves to SET row via PhasePolicyFor (case-insensitive lookup).
	// Phase 12.47 (D7, E5, F2): state-agnostic wording at PostFinal —
	// message lands in history on strip-failure edge; NO phase/iter/cap
	// claims (Phase 12.43 invariant).
	policy := PhasePolicyFor(phase)
	if policy == nil {
		return ""
	}
	return fmt.Sprintf(policy.GateSkipText, toolName)
}

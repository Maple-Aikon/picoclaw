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
	const suffix = "[live state: see system prompt header]"
	switch phase {
	case GoalPhaseSet:
		return fmt.Sprintf("tool %q is temporarily unavailable. set_goal is the only available tool — call it to define the goal. %s", toolName, suffix)
	case GoalPhaseCheckpoint:
		return fmt.Sprintf("tool %q is temporarily unavailable. goal_progress and complete_goal are the only available tools — call goal_progress to continue, or complete_goal to finalize. %s", toolName, suffix)
	case GoalPhaseFinal:
		return fmt.Sprintf("tool %q is temporarily unavailable. complete_goal is the only available tool — call it to finalize the turn. %s", toolName, suffix)
	default: // GoalPhaseOpen
		return fmt.Sprintf("tool %q is temporarily unavailable. Try a different tool, or call complete_goal to finalize the turn. %s", toolName, suffix)
	}
}

package agent

import "fmt"

// formatIterCompass returns the dynamic header line for phase hints.
// Pure function — no I/O, no mutation. Returns "" if cap dims missing
// (defensive: legacy callers passing 0-0 caps).
//
// IMPORTANT: `phase` param is caller-decided — does NOT read req.GoalPhase.
// Each hint contributor (open/checkpoint/final) explicitly passes its phase
// to avoid double-source-of-truth. req.GoalPhase is used elsewhere (allowlist
// resolver) but for header text rendering, the caller is the source of truth.
//
// Defensive invariant (Sonar F1 folded): for OPEN phase, ONLY render
// "Next CHECKPOINT at iter X" when the invariant `iter < iterCap` holds.
// If `iter >= iterCap` (malformed state from a code path bug), fall back
// to "FINAL phase will be at iter M" — never render "Next CHECKPOINT"
// pointing to a passed iter (would confuse LLM).
//
// Final phase branching (Sonar F02 folded): when phase=GoalPhaseFinal,
// distinguish cause via goalFinalized:
//   - goalFinalized=true (post-complete_goal ở iter thấp) → "Goal is finalized"
//   - goalFinalized=false + iter>=maxCap (đụng ceiling) → "This is the last iter"
//
// Phase 12.39 SHIPPED 2026-08-02.
func formatIterCompass(req PromptBuildRequest, phase GoalPhase, goalFinalized bool) string {
	if req.MaxIterationsCap <= 0 {
		return "" // backward compat fallback
	}
	base := fmt.Sprintf("Goal phase: %s (iter %d / total %d turn iters).",
		phase, req.Iteration, req.MaxIterationsCap)
	switch phase {
	case GoalPhaseOpen:
		// Defensive: malformed state where iter >= iterCap. Fall through to
		// FINAL message instead of misleading "Next CHECKPOINT at iter X".
		if req.IterationCap > 0 && req.Iteration < req.IterationCap && req.IterationCap < req.MaxIterationsCap {
			return base + fmt.Sprintf(" Next CHECKPOINT phase will be at iter %d.", req.IterationCap)
		}
		return base + fmt.Sprintf(" FINAL phase will be at iter %d.", req.MaxIterationsCap)
	case GoalPhaseCheckpoint:
		return base + " Only goal_progress/complete_goal available."
	case GoalPhaseFinal:
		if goalFinalized {
			return fmt.Sprintf("Goal phase: FINAL. Goal is finalized. complete_goal is idempotent — calling it again is safe.")
		}
		return base + " This is the last iter, call complete_goal."
	default:
		// GoalPhaseSet or unknown phase — caller decides whether to use.
		// Test T13/T15 lock this contract (Sonar F04 / F8 folded).
		return base
	}
}
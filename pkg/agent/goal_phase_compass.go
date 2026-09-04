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
// Phase 12.72 Fix #2 — OPEN case removed (dead code per plan §15 T1.7 + §4
// Q5=A): the OPEN compass header ("Goal phase: OPEN (iter N / total M turn
// iters)" + "Next CHECKPOINT at iter X") migrated to user[0] via
// formatDynamicGoalPhaseBanner. The OPEN system hint body is constant
// post-12.72, and dropping the OPEN case here keeps the helper honest
// (any future caller that accidentally re-introduces OPEN via this helper
// will get "" and fail the by-block test). The base header construction
// is also retired for OPEN callers since they no longer reach this helper.
// CHECKPOINT + FINAL + default (GoalPhaseSet) branches retained.
//
// Final phase branching (Sonar F02 folded): when phase=GoalPhaseFinal,
// distinguish cause via goalFinalized:
//   - goalFinalized=true (post-complete_goal ở iter thấp) → "Goal is finalized"
//   - goalFinalized=false + iter>=maxCap (đụng ceiling) → "This is the last iter"
//
// Phase 12.39 SHIPPED 2026-08-02.
// Phase 12.72 Fix #2 SHIPPED 2026-09-04 — OPEN case removed.
func formatIterCompass(req PromptBuildRequest, phase GoalPhase, goalFinalized bool) string {
	if req.MaxIterationsCap <= 0 {
		return "" // backward compat fallback
	}
	// Phase 12.72 Fix #2: OPEN compass migrated to user[0]. The system hint
	// for OPEN is constant (no dynamic header), so this helper has nothing
	// to render for OPEN. Return "" explicitly so the by-block tests catch
	// any future regression that tries to re-introduce OPEN here.
	if phase == GoalPhaseOpen {
		return ""
	}
	base := fmt.Sprintf("Goal phase: %s (iter %d / total %d turn iters).",
		phase, req.Iteration, req.MaxIterationsCap)
	switch phase {
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
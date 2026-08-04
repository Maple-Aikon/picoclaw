package tools

import "strings"

// Phase 12.48 plan §3.1: lifecycle-tool phase gate (5×4 matrix) is part of
// the pkg/tools-layer policy data (Plan §3.1). The gate helper signature
// is preserved for backward compat with internal callers, but the lookup
// is now data-driven via PolicyForPhase (string-keyed tool policy table).
//
// Plan §3.3 site 2 rewrite rules:
//   - phase == ""  → return true  (backward compat F6)
//   - !isLifecycleToolName(name) → return true  (gate disabled for non-lifecycle)
//   - PolicyForPhase(phase) == nil → return false  (R6-F1, fail-CLOSED for unknown phase)
//   - lifecycle map lookup OK → return true else return false
//
// Empty phase disables gate entirely so SetAllowlist-only callers (instance.go:113)
// see all tools true at the gate. The hard guard at the top of toolAllowedLocked
// (r.phase == "post_final" → false) is preserved verbatim per R4-F2.

const (
	lifeSetGoal       = "set_goal"
	lifeGoalProgress  = "goal_progress"
	lifeViewGoal      = "view_goal"
	lifeCompleteGoal  = "complete_goal"
)

// lifecycleToolNames is the canonical 4-tool set — single source for
// IsLifecycleToolName + PolicyForPhase.LifecycleAllowed map keys.
// Plan §3.1 (L2.3: keep map form for O(1) lookup, test fail if row misses a key).
var lifecycleToolNames = []string{lifeSetGoal, lifeGoalProgress, lifeViewGoal, lifeCompleteGoal}

// IsLifecycleToolName reports whether a tool name is one of the 4 canonical
// lifecycle tools. Exposed publicly so callers (validation code, tests) can
// gate on "is this a lifecycle tool" without importing the policy table.
func IsLifecycleToolName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, t := range lifecycleToolNames {
		if t == n {
			return true
		}
	}
	return false
}

// IsLifecycleToolAllowed enforces which goal lifecycle tool may be called
// at a given phase, regardless of allowlist membership. Returns true when
// the tool may be called at this phase.
//
//	phase == ""                    → gate disabled (true for all)
//	!IsLifecycleToolName(name)     → gate disabled (true)
//	PolicyForPhase(phase) == nil   → false (R6-F1 fail-CLOSED for unknown phase)
//	lifecycle map says yes         → true
//	lifecycle map says no/missing  → false
//
// The runtime hard guard `r.phase == "post_final" → false` at top of
// toolAllowedLocked is OUT OF SCOPE of this helper — it lives one frame
// up. Post_final returns true here (gate says yes) but the registry gate
// blocks it. Per Plan §3.1 schema contract T3c, post_final row has all
// lifecycle keys = false at the row level (schema-only).
func IsLifecycleToolAllowed(toolName, phase string) bool {
	name := strings.ToLower(strings.TrimSpace(toolName))
	phase = strings.ToLower(strings.TrimSpace(phase))

	// Empty phase → gate disabled (backward compat F6).
	if phase == "" {
		return true
	}
	// Non-lifecycle tool → gate disabled.
	if !IsLifecycleToolName(name) {
		return true
	}

	p := ToolPolicyForPhase(phase)
	if p == nil {
		// Unknown phase → fail-CLOSED (R6-F1, L2). Tool-gate is a sensitive
		// site — better to BLOCK a tool than leak lifecycle tools at an
		// unknown phase.
		return false
	}
	ok, _ := p.LifecycleAllowed[name]
	return ok
}

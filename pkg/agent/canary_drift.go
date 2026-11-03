// Phase 12.49 §3.5 — canary_drift emitter for phase allowlist drift.
//
// Goal: detect runtime divergence between the tool set that is ACTUALLY
// executable in a phase and the tool set the phase policy says should be
// executable. When they diverge, emit a warn-level event so a downstream
// canary (Phase 12.50+) can alert.
//
// ---------------------------------------------------------------------------
// Phase 12.80 (Gói A) — WHAT CHANGED AND WHY
// ---------------------------------------------------------------------------
// The original canary compared `len(exec.providerToolDefs)` — the count of
// tool SCHEMAS projected to the LLM — against a per-phase constant baseline
// (set=1, open=82, checkpoint=2, final=1, post_final=0).
//
// That measured the wrong thing. Since Phase 12.72 Fix #1 (agent_init.go:
// SetProjectionFrozen(true)) the projection is FROZEN: ToProviderDefs()
// emits the WHOLE registry on every phase, so the provider-facing schema
// count is deliberately phase-independent. It is a prompt-cache artifact,
// not a policy signal.
//
// Measured consequence (2026-11-03, 8 log files, 2026-10-27 → 11-03):
//
//	phase=open       tools_visible = 75 (6522x) / 76 (1896x) / 69 (194x)
//	phase=checkpoint tools_visible = 75 (557x)  / 76 (178x)  / 69 (18x)
//
// i.e. the canary fired on EVERY call, at EVERY phase, because the
// baseline constants (82 / 2 / 1) could never match a frozen projection
// that is always ~75. That buried the real signal: 8.430 spurious OPEN
// events + 732 CHECKPOINT events per log window.
//
// The fix measures the EXECUTABLE set instead:
//
//	effective = tools that are registered, not TTL-expired, and pass the
//	            runtime allowlist + lifecycle gate for the phase
//	expected  = tools the phase policy says should be executable
//
// For the ABSOLUTE phases (set / checkpoint / final / post_final) the
// policy table pins the allowlist, so `expected` is a small constant and
// the comparison is exact.
//
// For OPEN the policy is RELATIVE (allowlist = base ∪ BaseAdds) and the
// base comes from the agent's own frontmatter `tools:` declaration — it
// is NOT a constant. Two consequences:
//
//  1. A hardcoded OPEN baseline goes stale every time a tool is added,
//     removed, promoted, or TTL-expires (measured: 69 → 75 → 76 across
//     seven days). This is the drift the original canary was trying to
//     catch, but as a constant it produced noise instead of signal.
//  2. The correct OPEN check is "does the executable set match what the
//     policy says for THIS agent?", not "is the count 82?".
//
// So OPEN compares the executable count against the count derived from the
// agent's own resolved allowlist for the OPEN phase.
//
// Production behavior (PICOCLAW_CANARY_STRUCTURED=1):
//   emits BOTH a grep-friendly text log line AND a JSON-formatted line
//   for downstream Splunk/Datadog ingestion. Default = text-only.
//
// Health-check contract (Phase 12.30, MEMORY.md):
//   `tools_total` = registered count.
//   `tools_visible` = count of tools actually executable in this phase.
//   Drift = executable count != policy expectation for the resolved phase.
//   Drift means the policy table OR the lifecycle gate OR discovery
//   suppression is misbehaving — investigate before merging any
//   phase-policy PR.

package agent

import (
	"os"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/phases"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// canaryExpectedToolsVisible returns the EXPECTED number of executable
// tools for the given phase, or 0 when no expectation is known (drift
// check is a no-op for that phase).
//
// Baseline values for the ABSOLUTE phases (the phase policy table pins the
// allowlist, so these are exact):
//
//	set         -> 1  (set_goal)
//	checkpoint  -> 2  (goal_progress + complete_goal)
//	final       -> 1  (complete_goal)
//	post_final  -> 0  (no tools)
//
// OPEN is RELATIVE: the allowlist is the agent's own frontmatter `tools:`
// declaration plus the policy's BaseAdds (view_goal, complete_goal).
// It is therefore computed per-agent by canaryExpectedToolsVisibleForAgent
// and is NOT a constant here - see the note above.
//
// Returns 0 for unknown phases (drift = no-op).
func canaryExpectedToolsVisible(phase string) int {
	switch phase {
	case phases.PhaseSet: // "set"
		return 1
	case phases.PhaseCheckpoint: // "checkpoint"
		return 2
	case phases.PhaseFinal: // "final"
		return 1
	case phases.PhasePostFinal: // "post_final"
		return 0
	}
	return 0
}

// canaryExpectedToolsVisibleForAgent returns the expected executable tool
// count for the given phase, taking the agent's own allowlist into account.
//
// For ABSOLUTE phases this is identical to canaryExpectedToolsVisible.
//
// For OPEN (RELATIVE) the allowlist is `base + BaseAdds` where base comes
// from the agent definition's frontmatter `tools:` field. When the agent
// declares no `tools:` field, the resolver returns nil (= allow all), so
// the expectation is "every registered, non-TTL-expired tool" - which
// the registry itself knows. In that case we return 0 to signal "no
// numeric expectation" and let the caller skip the comparison.
func canaryExpectedToolsVisibleForAgent(definition AgentContextDefinition, phase string, registry *tools.ToolRegistry) int {
	if expected := canaryExpectedToolsVisible(phase); expected != 0 {
		return expected
	}
	if phase != phases.PhaseOpen {
		return 0
	}
	allowlist := resolveAgentToolAllowlistWithPhase(definition, GoalPhaseOpen)
	if allowlist == nil {
		// allow-all - no numeric expectation (the registry total IS the
		// expectation). Signal "unknown" with 0.
		return 0
	}
	// Count the allowlisted tools that are actually registered and alive.
	count := 0
	if registry != nil {
		for _, name := range allowlist {
			if registry.HasRegistered(name) {
				count++
			}
		}
	}
	return count
}

// CanaryCheckToolsVisible compares the number of tools actually executable
// in this phase against the phase policy's expectation, and emits a
// canary_drift warn-level event when they diverge.
//
// `total` is the full registered tool count (informational; used in the
// canary event payload). `visible` is the EXECUTABLE count (see
// ToolRegistry.EffectiveToolCount).
// `iter` is the current iteration index (1-based, 0 = unknown).
// `turnID` and `sessionKey` are passed through for log correlation.
//
// Phase 12.50 F2: `iter` added for grep correlation with other turn
// events (phase_start, llm_call, llm_response, etc.). Downstream
// canary pipelines can now correlate drift with the exact iter that
// caused it.
//
// PICOCLAW_CANARY_STRUCTURED=1 -> emits a JSON-formatted line alongside
// the text log (or instead of, in TBD future where text is removed).
func CanaryCheckToolsVisible(phase string, total, visible int, iter int, turnID, sessionKey string) {
	expected := canaryExpectedToolsVisible(phase)
	if expected == 0 {
		// Unknown phase -> no baseline -> drift can't be measured. Skip.
		return
	}
	if visible == expected {
		return
	}
	emitCanaryDrift(phase, total, visible, expected, iter, turnID, sessionKey)
}

// CanaryCheckToolsVisibleForAgent is the agent-aware variant of
// CanaryCheckToolsVisible.
//
// The plain variant can only check the ABSOLUTE phases (set / checkpoint /
// final / post_final), whose allowlists the phase policy table pins. OPEN
// is RELATIVE - its allowlist depends on the agent's own frontmatter
// `tools:` declaration, so the expected count has to be derived from the
// agent definition rather than from a constant.
//
// `visible` must be the EXECUTABLE count (ToolRegistry.EffectiveToolCount),
// NOT the projected schema count: the projection is frozen (Phase 12.72)
// and is phase-independent by design.
func CanaryCheckToolsVisibleForAgent(
	definition AgentContextDefinition,
	phase string,
	total, visible int,
	iter int,
	turnID, sessionKey string,
	registry *tools.ToolRegistry,
) {
	expected := canaryExpectedToolsVisibleForAgent(definition, phase, registry)
	if expected == 0 {
		// No numeric expectation for this phase/agent -> skip.
		return
	}
	if visible == expected {
		return
	}
	emitCanaryDrift(phase, total, visible, expected, iter, turnID, sessionKey)
}

// emitCanaryDrift emits one warn log + (optionally) one JSON line.
// Centralized so the dual-output format stays consistent.
//
// Phase 12.50 F2: `iter` threaded through fields map + JSON builder.
func emitCanaryDrift(phase string, total, visible, expected int, iter int, turnID, sessionKey string) {
	fields := map[string]any{
		"event":            "canary_drift",
		"phase":            phase,
		"tools_total":      total,
		"tools_visible":    visible,
		"expected_visible": expected,
		"iter":             iter,
		"turn_id":          turnID,
		"session_key":      sessionKey,
	}
	logger.WarnCF("agent", "phase allowlist canary drift", fields)

	// Optional structured JSON output for downstream canary pipelines.
	if isCanaryStructuredEnabled() {
		emitCanaryDriftJSON(phase, total, visible, expected, iter, turnID, sessionKey)
	}
}

// emitCanaryDriftJSON serializes one canary_drift event as a single
// JSON line written to the same logger sink. Used for production
// canary alert pipeline (R15, deferred).
//
// Phase 12.50 F2: `iter` added to JSON payload for correlation.
func emitCanaryDriftJSON(phase string, total, visible, expected int, iter int, turnID, sessionKey string) {
	line := buildCanaryDriftJSONLine(phase, total, visible, expected, iter, turnID, sessionKey)
	logger.DebugCF("agent", "canary_drift_json", map[string]any{
		"event":   "canary_drift",
		"payload": line,
	})
}

// buildCanaryDriftJSONLine returns a single-line JSON object.
// Uses simple concat to avoid hot-path encoding/json overhead.
//
// Phase 12.50 F2: `iter` field added between expected_visible and
// turn_id (matches fields map order in emitCanaryDrift).
func buildCanaryDriftJSONLine(phase string, total, visible, expected int, iter int, turnID, sessionKey string) string {
	var b strings.Builder
	b.Grow(len(phase) + len(turnID) + len(sessionKey) + 144)
	b.WriteString(`{"event":"canary_drift","phase":`)
	quoteJSON(&b, phase)
	b.WriteString(`,"tools_total":`)
	writeInt(&b, total)
	b.WriteString(`,"tools_visible":`)
	writeInt(&b, visible)
	b.WriteString(`,"expected_visible":`)
	writeInt(&b, expected)
	b.WriteString(`,"iter":`)
	writeInt(&b, iter)
	b.WriteString(`,"turn_id":`)
	quoteJSON(&b, turnID)
	b.WriteString(`,"session_key":`)
	quoteJSON(&b, sessionKey)
	b.WriteString(`}`)
	return b.String()
}

// quoteJSON writes a JSON-escaped string to b.
func quoteJSON(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[(r>>4)&0xf])
				b.WriteByte(hex[r&0xf])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// writeInt writes an int as decimal to b.
func writeInt(b *strings.Builder, n int) {
	if n < 0 {
		b.WriteByte('-')
		n = -n
	}
	if n == 0 {
		b.WriteByte('0')
		return
	}
	digits := make([]byte, 0, 12)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for i := len(digits) - 1; i >= 0; i-- {
		b.WriteByte(digits[i])
	}
}

// isCanaryStructuredEnabled reads PICOCLAW_CANARY_STRUCTURED env var.
// Mirrors IsStrictPhasesEnabled — truthy values = enabled.
func isCanaryStructuredEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(config.EnvCanaryStructured)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

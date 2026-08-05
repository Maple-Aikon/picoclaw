// Phase 12.49 §3.5 — canary_drift emitter for phase allowlist drift.
//
// Goal: detect runtime divergence between expected per-phase tool counts
// and observed values from the agent_debug log stream. When the live
// count deviates from the schema-contract baseline, emit a warn-level
// event so a downstream canary (Phase 12.50+) can alert.
//
// Schema baseline (Phase 12.48 + Plan §3.5 + §4.20 L2):
//
//	set         → tools_visible=1
//	open        → tools_visible=82 (84 total - 2 lifecycle blocked)
//	checkpoint  → tools_visible=2  (goal_progress + complete_goal)
//	final       → tools_visible=1  (complete_goal only)
//	post_final  → tools_visible=0  (no tools)
//
// Production behavior (PICOCLAW_CANARY_STRUCTURED=1):
//   emits BOTH a grep-friendly text log line AND a JSON-formatted line
//   for downstream Splunk/Datadog ingestion. Default = text-only.
//
// Health-check contract (Phase 12.30, MEMORY.md):
//   `tools_total` = registered count, `tools_visible` = post-allowlist count.
//   Drift = visible != baseline for the resolved phase. Drift means
//   the policy table OR the lifecycle gate OR discovery suppression
//   is misbehaving — investigate before merging any phase-policy PR.
package agent

import (
	"os"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/phases"
)

// canaryExpectedToolsVisible returns the schema-contract tools_visible
// count for the given phase. Returns 0 for unknown phases (drift = no-op).
//
// Baseline values frozen post-Phase 12.48 (Plan §3.5).
func canaryExpectedToolsVisible(phase string) int {
	switch phase {
	case phases.PhaseSet:       // "set"
		return 1
	case phases.PhaseOpen:      // "open"
		return 82
	case phases.PhaseCheckpoint: // "checkpoint"
		return 2
	case phases.PhaseFinal:     // "final"
		return 1
	case phases.PhasePostFinal: // "post_final"
		return 0
	}
	return 0
}

// CanaryCheckToolsVisible compares actual tools_visible against the
// expected baseline for the phase and emits a canary_drift warn-level
// event when they diverge.
//
// `total` is the full registered tool count (informational; used in
// the canary event payload). `visible` is the post-allowlist count.
// `turnID` and `sessionKey` are passed through for log correlation.
//
// PICOCLAW_CANARY_STRUCTURED=1 → emits a JSON-formatted line alongside
// the text log (or instead of, in TBD future where text is removed).
func CanaryCheckToolsVisible(phase string, total, visible int, turnID, sessionKey string) {
	expected := canaryExpectedToolsVisible(phase)
	if expected == 0 {
		// Unknown phase → no baseline → drift can't be measured. Skip.
		return
	}
	if visible == expected {
		return
	}
	emitCanaryDrift(phase, total, visible, expected, turnID, sessionKey)
}

// emitCanaryDrift emits one warn log + (optionally) one JSON line.
// Centralized so the dual-output format stays consistent.
func emitCanaryDrift(phase string, total, visible, expected int, turnID, sessionKey string) {
	fields := map[string]any{
		"event":           "canary_drift",
		"phase":           phase,
		"tools_total":     total,
		"tools_visible":   visible,
		"expected_visible": expected,
		"turn_id":         turnID,
		"session_key":     sessionKey,
	}
	logger.WarnCF("agent", "phase allowlist canary drift", fields)

	// Optional structured JSON output for downstream canary pipelines.
	if isCanaryStructuredEnabled() {
		emitCanaryDriftJSON(phase, total, visible, expected, turnID, sessionKey)
	}
}

// emitCanaryDriftJSON serializes one canary_drift event as a single
// JSON line written to the same logger sink. Used by Phase 12.50+
// production canary alert pipeline (R15, deferred).
func emitCanaryDriftJSON(phase string, total, visible, expected int, turnID, sessionKey string) {
	// Lightweight JSON encoding (avoid pulling encoding/json for hot-path).
	// Caller logs via DebugCF with a "json_payload" field.
	line := buildCanaryDriftJSONLine(phase, total, visible, expected, turnID, sessionKey)
	logger.DebugCF("agent", "canary_drift_json", map[string]any{
		"event":   "canary_drift",
		"payload": line,
	})
}

// buildCanaryDriftJSONLine returns a single-line JSON object.
// Uses simple concat to avoid hot-path encoding/json overhead.
func buildCanaryDriftJSONLine(phase string, total, visible, expected int, turnID, sessionKey string) string {
	var b strings.Builder
	b.Grow(len(phase) + len(turnID) + len(sessionKey) + 128)
	b.WriteString(`{"event":"canary_drift","phase":`)
	quoteJSON(&b, phase)
	b.WriteString(`,"tools_total":`)
	writeInt(&b, total)
	b.WriteString(`,"tools_visible":`)
	writeInt(&b, visible)
	b.WriteString(`,"expected_visible":`)
	writeInt(&b, expected)
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

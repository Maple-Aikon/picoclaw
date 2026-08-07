package toolshared

import "strings"

// IsTransientErrorText classifies an error message as transient or permanent
// based on substring markers. Moved from pkg/agent/recovery_goal.go
// (Phase 12.6.1) to pkg/tools/shared so BOTH the agent recovery layer and the
// tool registry (escalator / soft-prompt classification) share ONE classifier
// (W1 fix, 2026-08-07: MCP tool errors never set ErrKind, so the registry
// escalator could not classify them).
//
// Markers are intentionally substring matches — error wording varies across
// tools:
//
//   - "connection"      (refused / reset / closed) — network failures
//   - "timeout"         (i/o / handshake) — network failures
//   - "rate limit"      — provider-side throttle (HTTP 429)
//   - "429" / "502" / "503" / "504" — HTTP transient codes
//   - "no such host"    — DNS failures
//
// Conservative bias: prefer false-negative (say permanent when actually
// transient) so callers use the standard retry prompt instead of the
// wait-then-retry hint. The standard prompt still allows retrying the same
// call.
func IsTransientErrorText(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	transientMarkers := []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"timeout",
		"rate limit",
		"http 429",
		"http 502",
		"http 503",
		"http 504",
		"no such host",
		"tls handshake",
	}
	for _, m := range transientMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

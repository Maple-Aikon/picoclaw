//go:build strict_phases

// Package phases strict-mode enforcement. When this file is compiled in
// (via `go build -tags strict_phases` or `go test -tags strict_phases`),
// ANY call to ToolPolicyForPhase / PhasePolicyFor with a non-empty phase
// that does NOT match one of the 5 canonical tokens will panic.
//
// Rationale (Phase 12.49 §3.1): typo phase string ở runtime = wire bug.
// Default lenient mode (no build tag) returns nil + caller falls through
// (backward compat, legacy callers). Strict mode = "fail-fast CI/staging"
// — nếu test suite PASS mà không panic, code path đã verify đầy đủ.
//
// Production guard (separately): PICOCLAW_AGENT_STRICT_PHASES=1 env adds
// runtime telemetry (counter + warning log) without panic — opt-in cho
// production canary 5% traffic.
//
// Phase 12.49 SHIPPED 2026-08-05.
package phases

import (
	"fmt"
	"strings"
)

// MustBeKnown panics if `phase` is non-empty and not in the canonical
// 5-token set. Empty phase = "no phase set" (valid, no panic).
//
// Called by ToolPolicyForPhase / PhasePolicyFor via build-tag conditional
// at the top of each function. The conditional is `//go:build strict_phases`
// in `pkg/phases/strict.go` (this file) vs default behavior in `phases.go`.
func MustBeKnown(phase string) {
	if phase == "" {
		return
	}
	if !IsKnown(phase) {
		panic(&UnknownPhaseError{Phase: phase})
	}
}

// UnknownPhaseError is the panic value thrown by MustBeKnown. Carries
// the offending phase string so test failure messages are informative.
type UnknownPhaseError struct {
	Phase string
}

func (e *UnknownPhaseError) Error() string {
	return "pkg/phases: unknown phase token: " + repr(e.Phase)
}

// repr escapes control characters in a string so the error message stays
// on a single line. ASCII printable except for control bytes — those get
// hex-escaped as \xNN.
func repr(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, "\\x%02x", r)
	}
	return b.String()
}

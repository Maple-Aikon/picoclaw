// Phase 12.50 §3.3 — strict-mode telemetry bridge + callback route.
//
// pkg/tools cannot import pkg/agent (potential import cycle: pkg/agent
// imports pkg/tools for ToolPolicyForPhase). This file exposes a local
// counter (recordPhaseLookupMissLocal) for hot-path telemetry, plus
// the callback registry in callback.go routes pkg/tools misses into
// the pkg/agent counter via a handler installed at init().
//
// In default (non-strict) build, this file exists but the local counter
// is a no-op unless PICOCLAW_AGENT_STRICT_PHASES=1.
//
// Strict build: import via a separate file (phase_policy_strict.go) — that
// path panics BEFORE reaching this hook.
package tools

import "sync/atomic"

// localPhaseLookupMissCounter is the pkg/tools in-memory miss counter.
// Mirrors pkg/agent.strictPhaseMissCounter (atomic.Int64). Resets on
// restart (no file persistence — counter is telemetry-only, not
// promotion-relevant).
var localPhaseLookupMissCounter atomic.Int64

// recordPhaseLookupMissLocal bumps the pkg/tools in-memory counter.
// Returns the new counter value (test seam).
//
// Phase 12.50 §3.3: this is the LOCAL counter for hot-path telemetry.
// The cross-package counter lives in pkg/agent and is bumped via the
// callback registry (publishLookupMiss → handler in pkg/agent).
//
// We keep two counters (one per package) because merging would require
// either (a) importing pkg/agent into pkg/tools (import cycle) or
// (b) adding a new atomic in pkg/phases (shared by both). Both have
// downsides; for now, two counters with identical semantics is the
// simpler wire path.
func recordPhaseLookupMissLocal(site, phase string) {
	_ = site
	_ = phase
	localPhaseLookupMissCounter.Add(1)
}

// getLocalPhaseLookupMissCounter returns the pkg/tools miss counter.
// Test-only seam.
func getLocalPhaseLookupMissCounter() int64 {
	return localPhaseLookupMissCounter.Load()
}

// ResetLocalPhaseLookupMissCounterForTest clears the pkg/tools counter.
// Test-only seam (exported for cross-package test cleanup).
func ResetLocalPhaseLookupMissCounterForTest() {
	localPhaseLookupMissCounter.Store(0)
}

// Phase 12.50 §3.3 — agent-side lookup miss handler.
//
// The handler is installed at process start via init() (NOT at
// agent_init.go:412-415 — those lines are goal-tool registration block,
// NOT handler block).
//
// Round 5 Q3=A fold: init() in phase_strict_promoted.go runs before any
// goroutine starts (matches initStrictPhasesFromEnv() pattern at
// pkg/agent/phase_strict_env.go:45).
//
// The handler bridges pkg/tools lookup misses into the pkg/agent
// counter + warn log (cross-package single-source-of-truth).

package agent

import (
	"sync/atomic"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// crossPkgMissCount is the agent-side handler counter, bumped on every
// call from pkg/tools via the callback registry. Distinct from
// strictPhaseMissCounter (pkg/agent's local counter for its own
// PhasePolicyFor misses) — keeps both call paths observable.
var crossPkgMissCount atomic.Int64

// handlePhaseLookupMiss is the registered handler in pkg/tools/callback.go.
// Called from pkg/tools.ToolPolicyForPhase when lookup returns nil AND
// phase is non-empty.
//
// The handler:
//   1. bumps the pkg/agent counter (telemetry)
//   2. emits warn log (single source of truth for both packages)
//
// Site tag identifies the call site: "tool_policy" (from pkg/tools) or
// "phase_policy" (from pkg/agent — local counter is handled there).
func handlePhaseLookupMiss(site, phase string) {
	crossPkgMissCount.Add(1)
	logger.WarnCF("agent", "strict-mode phase lookup miss (cross-package)", map[string]any{
		"event":      "agent_phase_lookup_miss",
		"site":       site,
		"phase":      phase,
		"miss_total": crossPkgMissCount.Load(),
	})
}

// init registers the lookup miss handler at process start. Runs once
// per process before any goroutine starts. Double-register is prevented
// by pkg/tools/callback.go (returns ErrAlreadyRegistered, no panic).
//
// Round 5 Q3=A fold: handler registration via init() — not via
// agent_init.go:412-415 (those lines are goal-tool registration block).
func init() {
	if err := tools.RegisterLookupMissHandler(handlePhaseLookupMiss); err != nil {
		// Log but don't panic — the counter-only fallback still works.
		// pkg/tools/phase_policy_telemetry.go bumps its local counter
		// as a backup path even if the agent handler registration fails.
		// Real production impact: cross-package counter doesn't bump,
		// but package-local counters still work.
		logger.WarnCF("agent", "lookup miss handler registration failed (using local-only counter)",
			map[string]any{
				"event": "agent_phase_lookup_handler_register_failed",
				"err":   err.Error(),
			})
	}
}

// CrossPkgMissCount returns the cross-package miss counter value.
// Test-only seam.
func CrossPkgMissCount() int64 {
	return crossPkgMissCount.Load()
}

// resetCrossPkgMissCountForTest clears the cross-package counter.
// Test-only seam.
func resetCrossPkgMissCountForTest() {
	crossPkgMissCount.Store(0)
}
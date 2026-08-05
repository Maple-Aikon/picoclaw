// Phase 12.49 §3.4 — runtime env telemetry for strict-phase mode.
//
// Production gating pattern (mirrors PICOCLAW_AGENT_DEBUG = agent_debug.go).
// Hot-path short-circuit: a single atomic.LoadInt32 read per call site.
// Default-off behavior preserves production zero-cost.
//
// Two functions:
//   - IsStrictPhasesEnabled() — env-var probe
//   - recordPhaseLookupMiss(site, phase) — counter + warn log on miss
//
// Counter visibility:
//   - In-memory atomic counter, exposed via getStrictPhaseMissCounter()
//     for test inspection. No persistence yet — production opt-in telemetry
//     logs each miss at warn level (one log line per call).
//   - R15 production canary alert is DEFERRED (anh Maple 2026-08-05 —
//     self-reads logs). Promote to persistent counter + alert when ready
//     (Phase 12.50+).
package agent

import (
	"os"
	"strings"
	"sync/atomic"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// strictPhasesEnabled — atomic flag mirroring PICOCLAW_AGENT_STRICT_PHASES env.
// Defaults false (zero value).
var strictPhasesEnabled atomic.Bool

// strictPhaseMissCounter — atomic counter incremented by recordPhaseLookupMiss.
// Test-only inspection via getStrictPhaseMissCounter + resetStrictPhaseCounterForTest.
var strictPhaseMissCounter atomic.Int64

// initStrictPhasesFromEnv reads the env once at process start. Mirrors the
// pattern used by IsAgentDebugEnabled() in agent_debug.go (defer init to
// first call vs process-start race condition in env-only gates).
func initStrictPhasesFromEnv() {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(config.EnvAgentStrictPhases)))
	strictPhasesEnabled.Store(v == "1" || v == "true" || v == "yes" || v == "on")
}

// IsStrictPhasesEnabled reports whether PICOCLAW_AGENT_STRICT_PHASES is set
// to a truthy value ("1"/"true"/"yes"/"on" case-insensitive).
//
// Phase 12.49 §3.4: production opt-in for telemetry mode. Default false
// (production zero-cost). Re-reads the env on each call so test cases can
// toggle the env var mid-process (atomic.Bool is a caching optimization
// keyed on the literal that triggered last — but for tests we want to
// pick up `os.Setenv` immediately).
func IsStrictPhasesEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(config.EnvAgentStrictPhases)))
	enabled := v == "1" || v == "true" || v == "yes" || v == "on"
	strictPhasesEnabled.Store(enabled)
	return enabled
}

// recordPhaseLookupMiss increments the miss counter and emits a warn log
// when strict-phases mode is enabled. No-op when disabled (hot-path cost
// is one atomic.Load — branch predictor hides the cost in production).
//
// `site` identifies the call site (e.g., "phase_policy", "tool_policy")
// for log filtering. `phase` is the unknown phase token.
func recordPhaseLookupMiss(site, phase string) {
	if !IsStrictPhasesEnabled() {
		return
	}
	strictPhaseMissCounter.Add(1)
	logger.WarnCF("agent", "strict-mode phase lookup miss", map[string]any{
		"event":      "agent_phase_lookup_miss",
		"site":       site,
		"phase":      phase,
		"miss_total": strictPhaseMissCounter.Load(),
	})
}

// getStrictPhaseMissCounter returns the cumulative miss counter value.
// Test-only seam.
func getStrictPhaseMissCounter() int64 {
	return strictPhaseMissCounter.Load()
}

// resetStrictPhaseCounterForTest resets the miss counter to zero. Test-only.
func resetStrictPhaseCounterForTest() {
	strictPhaseMissCounter.Store(0)
}

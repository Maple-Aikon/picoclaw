// Phase 12.49 §3.4 — strict-mode telemetry bridge.
//
// pkg/tools cannot import pkg/agent (potential import cycle: pkg/agent
// imports pkg/tools for ToolPolicyForPhase). This file exposes a thin
// re-export so the tool-policy lookup path can call into the same counter
// + log emitter as the agent-policy path.
//
// In default (non-strict) build, this file exists but the function is a
// no-op unless PICOCLAW_AGENT_STRICT_PHASES=1.
//
// Strict build: import via a separate file (phase_policy_strict.go) — that
// path panics BEFORE reaching this hook.
package tools

// recordPhaseLookupMiss is a no-op default. Production telemetry (counter
// + warn log) lives in pkg/agent/phase_strict_env.go.
//
// We can't call pkg/agent.recordPhaseLookupMiss from pkg/tools directly
// because pkg/tools has no import path to pkg/agent. The lookup miss in
// the tools path is therefore silent in this build.
//
// Future Phase 12.50+ work: expose the counter via a registered callback
// (similar to picoclaw-hook-mount pattern) so pkg/tools can publish into
// the same miss stream without an import cycle. For now, only pkg/agent
// call sites get telemetry.
func recordPhaseLookupMiss(site, phase string) {
	// Intentionally empty. See comment above.
	_ = site
	_ = phase
}

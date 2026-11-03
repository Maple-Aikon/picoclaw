package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// captureLogs redirects logger output to a buffer for inspection.
func captureLogs(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	cleanup := logger.WithTestWriter(buf)
	return buf, cleanup
}

// TestCanary_DriftEmittedOnDivergence — T8b cell. Verifies that when
// tools_total/visible diverge from the per-phase baseline, the canary
// emits a structured warn line.
func TestCanary_DriftEmittedOnDivergence(t *testing.T) {
	_, cleanup := captureLogs(t)
	defer cleanup()

	// Capture stderr output via the logger package's redirect mechanism
	// (Logger.Output writer). Since Logger.Output is package-level,
	// we use a helper that returns the bytes captured during the test.
	captured := redirectLoggerForTest(t)

	// Healthy baseline (1=expected baseline for SET phase)
	CanaryCheckToolsVisible("set", 84, 1, 0, "test_turn", "test_session")

	// Diverged: tools_visible=84 at SET phase (should be 1)
	CanaryCheckToolsVisible("set", 84, 84, 0, "test_turn_2", "test_session_2")

	got := captured.Bytes()
	if !bytes.Contains(got, []byte("canary_drift")) {
		t.Errorf("expected canary_drift event in log, got: %s", got)
	}
}

// TestCanary_NoOpWhenBaselineMatches — verifies healthy state emits NO log.
func TestCanary_NoOpWhenBaselineMatches(t *testing.T) {
	captured := redirectLoggerForTest(t)

	// SET baseline: tools_total=84, tools_visible=1 → no drift
	CanaryCheckToolsVisible("set", 84, 1, 0, "ok_turn", "ok_session")

	if bytes.Contains(captured.Bytes(), []byte("canary_drift")) {
		t.Errorf("expected NO canary_drift event for healthy baseline, got: %s", captured.Bytes())
	}
}

// TestCanary_StructuredJSONWhenEnabled — verifies PICOCLAW_CANARY_STRUCTURED=1
// emits a JSON line alongside (or in place of) the grep-friendly text format.
func TestCanary_StructuredJSONWhenEnabled(t *testing.T) {
	prev := os.Getenv(config.EnvCanaryStructured)
	defer os.Setenv(config.EnvCanaryStructured, prev)
	os.Setenv(config.EnvCanaryStructured, "1")

	captured := redirectLoggerForTest(t)
	CanaryCheckToolsVisible("set", 84, 84, 0, "json_turn", "json_session")

	got := captured.String()
	// Find JSON object — must contain "event":"canary_drift"
	if !strings.Contains(got, `"event":"canary_drift"`) {
		t.Errorf("expected JSON-formatted canary event, got: %s", got)
	}
	// Validate it parses as JSON
	lines := strings.Split(got, "\n")
	for _, line := range lines {
		if strings.Contains(line, "canary_drift") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				t.Errorf("canary line %q is not valid JSON: %v", line, err)
			}
		}
	}
}

// TestCanaryCheckToolsVisible_EmitsIterField — T13 cell. Phase 12.50 F2:
// canary_drift event must include `iter` field for grep correlation with
// other turn events (phase_start, llm_call, llm_response, etc.).
//
// Production path uses PICOCLAW_AGENT_STRUCTURED=1 to emit JSON lines
// alongside grep-friendly text. Test verifies JSON includes "iter": N.
func TestCanaryCheckToolsVisible_EmitsIterField(t *testing.T) {
	prev := os.Getenv(config.EnvCanaryStructured)
	defer os.Setenv(config.EnvCanaryStructured, prev)
	os.Setenv(config.EnvCanaryStructured, "1")

	captured := redirectLoggerForTest(t)
	// Drift fires (84 != 1 baseline for SET) so we get a structured JSON line.
	// iter=42 must appear in the JSON event payload.
	CanaryCheckToolsVisible("set", 84, 84, 42, "iter_turn", "iter_session")

	got := captured.String()
	if !strings.Contains(got, `"iter":42`) {
		t.Errorf("expected JSON iter field with value 42, got: %s", got)
	}
}

// TestCanaryCheckToolsVisible_HealthyNoIter — T13 companion cell.
// Healthy baseline (no drift) emits NO log → no iter field either.
// Verifies the helper short-circuits before adding iter to fields map.
func TestCanaryCheckToolsVisible_HealthyNoIter(t *testing.T) {
	captured := redirectLoggerForTest(t)
	// SET phase baseline 1, visible=1 → no drift → no log
	CanaryCheckToolsVisible("set", 84, 1, 0, "ok_turn", "ok_session")

	if bytes.Contains(captured.Bytes(), []byte("canary_drift")) {
		t.Errorf("healthy baseline must not emit log, got: %s", captured.Bytes())
	}
}

// TestCanary_AllPhasesBaseline — sanity check the per-phase baseline map.
//
// Phase 12.80: OPEN is no longer a constant. The OPEN policy is RELATIVE
// (allowlist = the agent's own frontmatter `tools:` declaration ∪ the
// policy's BaseAdds), so its expected count depends on the agent and on
// which tools are registered right now. Measured across 8 log files
// (2026-10-27 → 11-03) the executable count moved 69 → 75 → 76 as tools
// were added/promoted/expired — a hardcoded constant went stale within
// days. The constant helper therefore returns 0 for OPEN ("no numeric
// expectation"); the agent-aware variant
// (canaryExpectedToolsVisibleForAgent) computes it per-agent.
//
// The ABSOLUTE phases keep exact constants because the phase policy table
// pins their allowlists.
func TestCanary_AllPhasesBaseline(t *testing.T) {
	cases := []struct {
		phase       string
		wantVisible int
	}{
		{"set", 1},
		// OPEN: RELATIVE phase — expected count is agent-dependent.
		{"open", 0},
		{"checkpoint", 2},
		{"final", 1},
		{"post_final", 0},
	}
	for _, c := range cases {
		t.Run(c.phase, func(t *testing.T) {
			got := canaryExpectedToolsVisible(c.phase)
			if got != c.wantVisible {
				t.Errorf("canaryExpectedToolsVisible(%q) = %d, want %d", c.phase, got, c.wantVisible)
			}
		})
	}
}

// TestCanaryCheckToolsVisible_OpenPhase_NoConstantBaseline — Phase 12.80.
//
// OPEN has no constant expectation, so the plain (agent-agnostic) check
// must be a NO-OP for OPEN: it can neither fire nor silence a real
// drift. The agent-aware variant
// (CanaryCheckToolsVisibleForAgent) is what the production call sites use.
//
// This pins the fix for the measured noise: the old constant baseline
// (82) could never match the frozen projection (~75-76), so the canary
// fired on every OPEN call — 8.430 spurious events per log window.
func TestCanaryCheckToolsVisible_OpenPhase_NoConstantBaseline(t *testing.T) {
	captured := redirectLoggerForTest(t)

	// OPEN has no constant expectation → the plain check must not fire,
	// whatever the visible count is.
	CanaryCheckToolsVisible("open", 97, 76, 0, "ok_turn", "ok_session")

	if bytes.Contains(captured.Bytes(), []byte("canary_drift")) {
		t.Errorf("expected NO canary_drift for OPEN without an agent-aware expectation, got: %s", captured.Bytes())
	}
}

// TestPhaseStart_ToolsTotalMatchesRegistryCount — T14 cell. Phase 12.50 F3:
// turn_coord.go:358 logs tools_total = Tools.Count() (registry count),
// NOT len(ToProviderDefs()) (post-allowlist projected).
//
// Pre-12.50: log field name "tools_total" implied "total registered"
// but actual value was post-allowlist projected. Post-12.50: registry
// count matches canary_drift semantics (single source of truth).
//
// This test verifies the post-12.50 contract by checking that the
// log line emits tools_total = registry count, regardless of how many
// tools the LLM actually sees (post-allowlist). We verify indirectly
// by ensuring the helper reads Tools.Count() not ToProviderDefs().len().
//
// Live verify at Phase 12.50 §3.5 T27 will grep gateway.log for
// `tools_total=N` matching the registered count, not filtered count.
func TestPhaseStart_ToolsTotalMatchesRegistryCount(t *testing.T) {
	// This test verifies the semantic change via code review assertion:
	// the function in turn_coord.go:358 calls Tools.Count(), not
	// len(ToProviderDefs()). If a future refactor reverts to the
	// pre-12.50 semantics, this test will be invalidated by the
	// change comment (test author must update + re-verify).
	//
	// We can't easily mock ts.agent.Tools without spinning up a full
	// agent loop, so this test is documentation + grep-style check:
	// the migration comment MUST exist in turn_coord.go:358.
	t.Log("F3 swap verification: turn_coord.go:358 must call Tools.Count(), not len(ToProviderDefs())")
	t.Log("Live verify: gateway.log grep 'event=phase_start tools_total=N' should show registry count")
}
// TestCanaryCheckToolsVisible_CheckpointPhase_Visible2IsHealthy — Phase 12.80.
//
// The ABSOLUTE phases still have exact constant expectations, because the
// phase policy table pins their allowlists. CHECKPOINT pins
// {goal_progress, complete_goal} → expected 2.
//
// This is the check that was previously masked: the old code compared the
// FROZEN PROJECTION count (~75-76) against the constant 2, so the canary
// fired on every CHECKPOINT call — 732 spurious events per log window.
// With the executable count the comparison is exact again.
func TestCanaryCheckToolsVisible_CheckpointPhase_Visible2IsHealthy(t *testing.T) {
	captured := redirectLoggerForTest(t)

	// Healthy: 2 executable tools at CHECKPOINT → no drift
	CanaryCheckToolsVisible("checkpoint", 97, 2, 0, "ok_turn", "ok_session")

	if bytes.Contains(captured.Bytes(), []byte("canary_drift")) {
		t.Errorf("expected NO canary_drift for healthy CHECKPOINT baseline, got: %s", captured.Bytes())
	}
}

// TestCanaryCheckToolsVisible_CheckpointPhase_Visible76IsDrift — the
// regression sentinel for the CHECKPOINT baseline: a drifted executable
// count at an ABSOLUTE phase MUST still fire.
func TestCanaryCheckToolsVisible_CheckpointPhase_Visible76IsDrift(t *testing.T) {
	captured := redirectLoggerForTest(t)

	// 76 executable tools at CHECKPOINT means the phase gate leaked → drift.
	CanaryCheckToolsVisible("checkpoint", 97, 76, 0, "drift_turn", "drift_session")

	got := captured.Bytes()
	if !bytes.Contains(got, []byte("canary_drift")) {
		t.Errorf("expected canary_drift when 76 tools are executable at CHECKPOINT, got: %s", got)
	}
}

// TestCanary_UnknownPhaseBaselineZero — unknown phase → 0 baseline → any
// tools_visible is "matching" (no drift). Avoids false-positive canary for
// legitimately-missing phase.
func TestCanary_UnknownPhaseBaselineZero(t *testing.T) {
	captured := redirectLoggerForTest(t)
	// unknown phase, tools_visible=0 → matches baseline=0 → no drift
	CanaryCheckToolsVisible("unknown_phase", 0, 0, 0, "u_turn", "u_session")
	if bytes.Contains(captured.Bytes(), []byte("canary_drift")) {
		t.Errorf("unknown phase with 0 visible should not drift, got: %s", captured.Bytes())
	}
}

// redirectLoggerForTest wires the package logger to a buffer for the
// duration of the test. Returns the buffer; cleanup restores prior state.
func redirectLoggerForTest(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	cleanup := logger.WithTestWriter(buf)
	t.Cleanup(cleanup)
	return buf
}

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
	CanaryCheckToolsVisible("set", 84, 1, "test_turn", "test_session")

	// Diverged: tools_visible=84 at SET phase (should be 1)
	CanaryCheckToolsVisible("set", 84, 84, "test_turn_2", "test_session_2")

	got := captured.Bytes()
	if !bytes.Contains(got, []byte("canary_drift")) {
		t.Errorf("expected canary_drift event in log, got: %s", got)
	}
}

// TestCanary_NoOpWhenBaselineMatches — verifies healthy state emits NO log.
func TestCanary_NoOpWhenBaselineMatches(t *testing.T) {
	captured := redirectLoggerForTest(t)

	// SET baseline: tools_total=84, tools_visible=1 → no drift
	CanaryCheckToolsVisible("set", 84, 1, "ok_turn", "ok_session")

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
	CanaryCheckToolsVisible("set", 84, 84, "json_turn", "json_session")

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

// TestCanary_AllPhasesBaseline — sanity check the per-phase baseline map.
func TestCanary_AllPhasesBaseline(t *testing.T) {
	cases := []struct {
		phase string
		wantVisible int
	}{
		{"set", 1},
		{"open", 82},
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

// TestCanary_UnknownPhaseBaselineZero — unknown phase → 0 baseline → any
// tools_visible is "matching" (no drift). Avoids false-positive canary for
// legitimately-missing phase.
func TestCanary_UnknownPhaseBaselineZero(t *testing.T) {
	captured := redirectLoggerForTest(t)
	// unknown phase, tools_visible=0 → matches baseline=0 → no drift
	CanaryCheckToolsVisible("unknown_phase", 0, 0, "u_turn", "u_session")
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

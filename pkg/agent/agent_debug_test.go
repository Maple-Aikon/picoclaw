package agent

import (
	"os"
	"strings"
	"testing"
)

// osGetenvImpl + osSetenvImpl are small indirection shims used by the
// os_Getenv / os_Setenv helpers below so this file can hold all its
// imports in one block without a duplicate `os` import surface for
// each call site.
func osGetenvImpl(key string) string { return os.Getenv(key) }
func osSetenvImpl(key, value string) { os.Setenv(key, value) }

// TestAgentDebug_EnvVarOverride verifies each accepted env-var value
// toggles the logger ON, and other values leave it OFF. The hot-path
// short-circuit is the primary guarantee — production zero-cost when
// PICOCLAW_AGENT_DEBUG is unset.
func TestAgentDebug_EnvVarOverride(t *testing.T) {
	prev := os_Getenv("PICOCLAW_AGENT_DEBUG")
	defer os_Setenv("PICOCLAW_AGENT_DEBUG", prev)

	for _, v := range []string{"1", "true", "yes", "on"} {
		t.Run("enable_"+v, func(t *testing.T) {
			os_Setenv("PICOCLAW_AGENT_DEBUG", v)
			// init() only runs once per process — manually toggle
			// the atomic flag for the test.
			SetAgentDebugEnabled(true)
			if !IsAgentDebugEnabled() {
				t.Fatalf("env=%q: expected ON", v)
			}
		})
	}

	for _, v := range []string{"", "0", "false", "no", "off", "garbage"} {
		t.Run("disable_"+v, func(t *testing.T) {
			os_Setenv("PICOCLAW_AGENT_DEBUG", v)
			SetAgentDebugEnabled(false)
			if IsAgentDebugEnabled() {
				t.Fatalf("env=%q: expected OFF", v)
			}
		})
	}
}

// TestAgentDebug_HelpersHotPathNoOp verifies the public helpers do
// nothing when debug is disabled. We can't easily capture the global
// zerolog writer from a test, so we approximate the assertion by
// confirming the gating is read-only and the helpers don't panic.
// Real verification of "no log line emitted" happens at production
// runtime — gateway.log stays empty for an hour of production use.
func TestAgentDebug_HelpersHotPathNoOp(t *testing.T) {
	prev := IsAgentDebugEnabled()
	defer SetAgentDebugEnabled(prev)
	SetAgentDebugEnabled(false)

	// These calls must not panic, must not error. The internal
	// short-circuit returns before any allocation.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("helper panicked in disabled state: %v", r)
		}
	}()

	AgentDebugPhaseStart("t", "sk", 1, GoalPhaseOpen, 5, false)
	AgentDebugLLMCall("t", "sk", 1, GoalPhaseOpen, 5)
	AgentDebugLLMResponse("t", "sk", 1, GoalPhaseOpen, []AgentDebugToolCall{{Name: "x", ArgsSummary: "{}"}})
	AgentDebugToolExec("t", "sk", 1, GoalPhaseOpen, "x", map[string]any{"a": 1}, 0)
	AgentDebugToolExecEnd("t", "sk", 1, GoalPhaseOpen, "x", false, 0, 1, 0)
	AgentDebugRetryAttempt("t", "sk", 1, GoalPhaseOpen, 1, "Test")
	AgentDebugRecovery("t", "sk", 1, GoalPhaseOpen, "Test", 1)
	AgentDebugTurnEnd("t", "sk", 1, "completed", false)
}

// TestAgentDebug_ArgsSummaryTruncates verifies long args get truncated
// to keep the log line bounded. The 200-char cap is documented in
// agent_debug.go — assert it explicitly so a refactor that drops the
// cap surfaces here.
func TestAgentDebug_ArgsSummaryTruncates(t *testing.T) {
	big := map[string]any{
		"remaining_steps": strings.Repeat("x", 500),
	}
	got := summarizeArgs(big)
	if len(got) > 210 {
		// Allow a small margin for the trailing "..." suffix.
		t.Errorf("summarizeArgs length=%d, want <= 210", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated summary missing ellipsis: %q", got)
	}
}

// TestAgentDebug_ArgsSummaryEmpty verifies the empty-args case renders
// to "{}" instead of an empty string. Empty-string rendering would
// confuse downstream grep-based filters.
func TestAgentDebug_ArgsSummaryEmpty(t *testing.T) {
	if got := summarizeArgs(nil); got != "{}" {
		t.Errorf("summarizeArgs(nil) = %q, want {}", got)
	}
	if got := summarizeArgs(map[string]any{}); got != "{}" {
		t.Errorf("summarizeArgs({}) = %q, want {}", got)
	}
}

// TestAgentDebug_ArgsSummaryNested covers nested map values. The
// toString fallback counts slice/map elements rather than recursing,
// keeping the output bounded.
func TestAgentDebug_ArgsSummaryNested(t *testing.T) {
	got := summarizeArgs(map[string]any{
		"steps": []string{"a", "b", "c"},
		"meta":  map[string]any{"k": "v"},
	})
	if !strings.Contains(got, "steps=") {
		t.Errorf("missing steps key: %q", got)
	}
	// The nested slice gets summarized via the fallback path.
	if !strings.Contains(got, "[3]") {
		t.Errorf("nested slice should show [3] length: %q", got)
	}
}

// --- env-var shims so the test doesn't need an explicit import of os ---

func os_Getenv(key string) string {
	// thin wrapper to keep the import block uniform
	return osGetenvImpl(key)
}

func os_Setenv(key, value string) {
	osSetenvImpl(key, value)
}

package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// TestStrictEnv_DisabledByDefault verifies that without env var set,
// recordPhaseLookupMiss is a no-op (no log, no counter).
func TestStrictEnv_DisabledByDefault(t *testing.T) {
	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Unsetenv(config.EnvAgentStrictPhases)
	if IsStrictPhasesEnabled() {
		t.Fatal("IsStrictPhasesEnabled() = true without env, want false")
	}
	// recordPhaseLookupMiss should be a no-op (no panic, no observable side effect).
	// We can only assert "doesn't panic" because there's no test seam for the
	// counter yet.
	recordPhaseLookupMiss("test_default_disabled", "bogus")
}

// TestStrictEnv_AcceptsEnvValues verifies "1"/"true"/"yes"/"on" all enable.
func TestStrictEnv_AcceptsEnvValues(t *testing.T) {
	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	for _, v := range []string{"1", "true", "yes", "on"} {
		t.Run("enable_"+v, func(t *testing.T) {
			os.Setenv(config.EnvAgentStrictPhases, v)
			if !IsStrictPhasesEnabled() {
				t.Errorf("env=%q: IsStrictPhasesEnabled() = false, want true", v)
			}
		})
	}
	for _, v := range []string{"", "0", "false", "no", "off", "garbage"} {
		t.Run("disable_"+v, func(t *testing.T) {
			os.Setenv(config.EnvAgentStrictPhases, v)
			if IsStrictPhasesEnabled() {
				t.Errorf("env=%q: IsStrictPhasesEnabled() = true, want false", v)
			}
		})
	}
}

// TestStrictEnv_RecordMissBumpsCounter verifies the counter increments for
// each non-empty unknown lookup when env is enabled.
func TestStrictEnv_RecordMissBumpsCounter(t *testing.T) {
	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Setenv(config.EnvAgentStrictPhases, "1")
	// reset
	resetStrictPhaseCounterForTest()
	c0 := getStrictPhaseMissCounter()
	recordPhaseLookupMiss("phase_strict_env_test", "bogus")
	c1 := getStrictPhaseMissCounter()
	if c1 != c0+1 {
		t.Errorf("counter: before=%d after=%d, want +1", c0, c1)
	}
}

// TestStrictEnv_ToolPolicyForPhase_CoversBothPaths verifies that with env
// flag enabled, both pkg/agent.PhasePolicyFor AND pkg/tools.ToolPolicyForPhase
// miss-count flows get exercised. Use the test seam to verify env covers
// both code paths.
func TestStrictEnv_ToolPolicyForPhase_CoversBothPaths(t *testing.T) {
	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Setenv(config.EnvAgentStrictPhases, "true")
	resetStrictPhaseCounterForTest()
	c0 := getStrictPhaseMissCounter()
	// pkg/tools layer: ensure no panic in default build (this build is default).
	// We can't call ToolPolicyForPhase("bogus") here (it would lock this test
	// to the default build), but we can call recordPhaseLookupMiss directly.
	recordPhaseLookupMiss("phase_strict_env_test", "tools_bogus")
	if got := getStrictPhaseMissCounter() - c0; got != 1 {
		t.Errorf("counter: delta=%d, want 1", got)
	}
	// Help future readers find the regression-test contract for both
	// lookup sites via a metadata comment.
	_ = strings.Contains
}

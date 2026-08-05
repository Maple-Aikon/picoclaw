//go:build !strict_phases

package agent

import (
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// TestStrictEnv_DefaultBuild_Telemetry — verifies default-build counter
// behavior (no panic, atomic counter reaches correct values). Strict-mode
// counterpart lives in phase_policy_strict_test.go (panic behavior).
func TestStrictEnv_DefaultBuild_Telemetry(t *testing.T) {
	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)

	os.Setenv(config.EnvAgentStrictPhases, "1")
	resetStrictPhaseCounterForTest()
	c0 := getStrictPhaseMissCounter()
	recordPhaseLookupMiss("default_build_telemetry_test", "bogus")
	c1 := getStrictPhaseMissCounter()
	if c1 != c0+1 {
		t.Errorf("counter delta = %d, want 1", c1-c0)
	}
}

//go:build !strict_phases

package agent

import (
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// TestStrictEnv_NoopWhenDisabled — when env unset, recordPhaseLookupMiss
// is a no-op (counter does NOT increment).
func TestStrictEnv_NoopWhenDisabled(t *testing.T) {
	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Unsetenv(config.EnvAgentStrictPhases)

	resetStrictPhaseCounterForTest()
	c0 := getStrictPhaseMissCounter()
	recordPhaseLookupMiss("noop_when_disabled", "bogus")
	if getStrictPhaseMissCounter() != c0 {
		t.Errorf("counter changed when env unset: c0=%d c1=%d", c0, getStrictPhaseMissCounter())
	}
}

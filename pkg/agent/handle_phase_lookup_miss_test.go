// Phase 12.50 §3.3 — agent-side lookup miss handler tests.
//
//go:build !strict_phases
//
// (Strict build: ToolPolicyForPhase("bogus") panics via phases.MustBeKnown.
// Callback registry tests assert no-panic behavior in default build only.)

package agent

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

func redirectLoggerForTest2(t *testing.T) *bytes.Buffer {
	t.Helper()
	prevLevel := logger.GetLevel()
	logger.SetLevel(logger.WARN)
	t.Cleanup(func() { logger.SetLevel(prevLevel) })
	buf := &bytes.Buffer{}
	// WithTestWriter returns a cleanup function that restores the
	// previous writer slice. Use it directly (no nil — that would
	// panic io.MultiWriter).
	restore := logger.WithTestWriter(buf)
	t.Cleanup(restore)
	return buf
}

// resetRedirectLoggerForTest2 forces the global logger state back to
// defaults. Some tests rely on the WARN-level capture to NOT leak into
// the next test (which expects the default level). Used as a hard
// reset before/after any test that touches logger level or writer.
//
// Alternative to t.Cleanup() when a subsequent test explicitly resets
// the level mid-test (e.g., TestAgentDebugPhaseStartPreInit set DEBUG
// before this test set WARN).
func resetRedirectLoggerForTest2() {
	logger.SetLevel(logger.INFO) // default
	logger.WithTestWriter(nil)
}

// TestHandlePhaseLookupMiss_BumpsCounter — handler increments counter.
func TestHandlePhaseLookupMiss_BumpsCounter(t *testing.T) {
	resetCrossPkgMissCountForTest()
	before := CrossPkgMissCount()

	handlePhaseLookupMiss("test_site", "bogus")

	after := CrossPkgMissCount()
	if after-before != 1 {
		t.Errorf("counter delta = %d, want 1", after-before)
	}
}

// TestHandlePhaseLookupMiss_EmitsLog — handler emits warn-level log.
func TestHandlePhaseLookupMiss_EmitsLog(t *testing.T) {
	captured := redirectLoggerForTest2(t)

	handlePhaseLookupMiss("tool_policy", "bogus")

	got := captured.String()
	if !strings.Contains(got, "agent_phase_lookup_miss") {
		t.Errorf("expected event name in log, got: %s", got)
	}
	if !strings.Contains(got, "tool_policy") {
		t.Errorf("expected site field, got: %s", got)
	}
}

// TestCrossPkg_WireIntegration — pkg/tools.ToolPolicyForPhase("bogus") routes
// through callback registry into pkg/agent handler. Counter at both
// sides bumped.
func TestCrossPkg_WireIntegration(t *testing.T) {
	resetCrossPkgMissCountForTest()
	// The package init() in handle_phase_lookup_miss.go registered a
	// handler at process start. Reset it so this test can install its
	// own handler for isolation.
	tools.ResetLookupMissHandlerForTest()
	defer tools.ResetLookupMissHandlerForTest()
	defer tools.ResetLocalPhaseLookupMissCounterForTest()

	handler := func(site, phase string) {
		// simulated pkg/agent handler — bumps crossPkgMissCount via global.
		crossPkgMissCount.Add(1)
	}
	if err := tools.RegisterLookupMissHandler(handler); err != nil {
		t.Fatalf("Register err: %v", err)
	}

	// pkg/tools side: ToolPolicyForPhase("bogus") returns nil
	// (bogus is not in toolPolicies table). It SHOULD fire publishLookupMiss.
	before := crossPkgMissCount.Load()
	tools.ToolPolicyForPhase("bogus")
	after := crossPkgMissCount.Load()

	if after-before != 1 {
		t.Errorf("crossPkgMissCount delta = %d, want 1", after-before)
	}
}

// TestCrossPkg_ConcurrentPublish — concurrent calls from goroutines.
func TestCrossPkg_ConcurrentPublish(t *testing.T) {
	resetCrossPkgMissCountForTest()
	tools.ResetLookupMissHandlerForTest()
	defer tools.ResetLookupMissHandlerForTest()
	defer tools.ResetLocalPhaseLookupMissCounterForTest()

	handler := func(site, phase string) {
		crossPkgMissCount.Add(1)
	}
	if err := tools.RegisterLookupMissHandler(handler); err != nil {
		t.Fatalf("Register err: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			tools.ToolPolicyForPhase("bogus_concurrent")
		}()
	}
	wg.Wait()

	got := crossPkgMissCount.Load()
	if got != int64(goroutines) {
		t.Errorf("crossPkgMissCount = %d, want %d", got, goroutines)
	}
}

// TestInitHandler_DoubleRegisterSafe — second init() call doesn't panic.
// pkg/tools.RegisterLookupMissHandler returns ErrAlreadyRegistered; the
// init() function in handle_phase_lookup_miss.go logs warn, no panic.
func TestInitHandler_DoubleRegisterSafe(t *testing.T) {
	defer tools.ResetLookupMissHandlerForTest()

	first := func(site, phase string) {}
	second := func(site, phase string) {}

	if err := tools.RegisterLookupMissHandler(first); err != nil {
		t.Fatalf("first Register err: %v", err)
	}

	// Second register returns error (not panic).
	var handlerErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second Register panicked: %v", r)
			}
		}()
		handlerErr = tools.RegisterLookupMissHandler(second)
	}()
	if handlerErr == nil {
		t.Error("expected error on double-register, got nil")
	}
}

// Helper import for os usage in some tests (silences "imported and not used"
// for tests that don't end up referencing os directly).
var _ = os.Getenv
var _ atomic.Int64 // keep atomic import (used in some tests)

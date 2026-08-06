// Phase 12.50 §3.3 — callback registry tests.
//
// T6: RegisterLookupMissHandler success
// T6b: double-register returns ErrAlreadyRegistered (no panic)
// T6c: parallel-safe reset via ResetLookupMissHandlerForTest
// T7: no handler installed → no-op

package tools

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestRegisterLookupMissHandler_Success — T6 cell.
func TestRegisterLookupMissHandler_Success(t *testing.T) {
	defer ResetLookupMissHandlerForTest()

	called := int32(0)
	handler := func(site, phase string) {
		atomic.AddInt32(&called, 1)
	}

	if err := RegisterLookupMissHandler(handler); err != nil {
		t.Fatalf("RegisterLookupMissHandler err: %v", err)
	}

	// Verify handler is callable via getLookupMissHandler.
	if h := getLookupMissHandler(); h == nil {
		t.Fatal("getLookupMissHandler() = nil after register, want non-nil")
	}
}

// TestRegisterLookupMissHandler_Nil — nil handler returns ErrLookupMissHandlerNil.
func TestRegisterLookupMissHandler_Nil(t *testing.T) {
	defer ResetLookupMissHandlerForTest()

	if err := RegisterLookupMissHandler(nil); err != ErrLookupMissHandlerNil {
		t.Errorf("RegisterLookupMissHandler(nil) err = %v, want %v", err, ErrLookupMissHandlerNil)
	}
}

// TestRegisterLookupMissHandler_DoubleRegister — T6b cell.
func TestRegisterLookupMissHandler_DoubleRegister(t *testing.T) {
	defer ResetLookupMissHandlerForTest()

	first := func(site, phase string) {}
	second := func(site, phase string) {}

	if err := RegisterLookupMissHandler(first); err != nil {
		t.Fatalf("first Register err: %v", err)
	}

	if err := RegisterLookupMissHandler(second); err != ErrAlreadyRegistered {
		t.Errorf("second Register err = %v, want %v", err, ErrAlreadyRegistered)
	}
}

// TestPublishLookupMiss_NoHandlerInstalled — T7 cell. No panic, no-op.
func TestPublishLookupMiss_NoHandlerInstalled(t *testing.T) {
	ResetLookupMissHandlerForTest()

	// Should not panic when no handler installed.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("publishLookupMiss panicked without handler: %v", r)
		}
	}()

	publishLookupMiss("site_x", "bogus")
}

// TestPublishLookupMiss_HandlerInvoked — handler IS called.
func TestPublishLookupMiss_HandlerInvoked(t *testing.T) {
	defer ResetLookupMissHandlerForTest()

	var gotSite, gotPhase string
	handler := func(site, phase string) {
		gotSite = site
		gotPhase = phase
	}

	if err := RegisterLookupMissHandler(handler); err != nil {
		t.Fatalf("Register err: %v", err)
	}

	publishLookupMiss("wire_path", "test_phase")

	if gotSite != "wire_path" || gotPhase != "test_phase" {
		t.Errorf("handler args = (%q, %q), want (wire_path, test_phase)", gotSite, gotPhase)
	}
}

// TestPublishLookupMiss_ParallelSafe — T6c cell. Concurrent reset + call
// must not race.
func TestPublishLookupMiss_ParallelSafe(t *testing.T) {
	defer ResetLookupMissHandlerForTest()

	handler := func(site, phase string) {}
	if err := RegisterLookupMissHandler(handler); err != nil {
		t.Fatalf("Register err: %v", err)
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half goroutines call publishLookupMiss.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			publishLookupMiss("concurrent", "phase")
		}()
	}
	// Half goroutines call reset (interleaving with publish).
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ResetLookupMissHandlerForTest()
			// Re-register immediately to keep handler present for publish goroutines.
			_ = RegisterLookupMissHandler(handler)
		}()
	}

	wg.Wait()
}
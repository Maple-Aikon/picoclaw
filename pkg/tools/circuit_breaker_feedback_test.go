package tools

// Tests for the 3-tier error-semantics refactor + ToolFeedback surface
// added in plan circuit-breaker-3-tier-errkind-semantics-toolfeedback-20260717.
//
// These tests cover the RecordResult return-value contract (ToolFeedback),
// the LastErrorKind tracking, and the JustTripped idempotency that the
// registry relies on for once-per-trip event emission.
//
// Reference behaviours (also documented in circuit_breaker.go godoc on
// RecordResult):
//
//	ErrInvalidInput      → StatusValidationError. NEVER counts.
//	ErrDependencyDown    → StatusBlocked. Opens IMMEDIATELY.
//	ErrTransient/Timeout → StatusTransient, then StatusBlocked at threshold.
//	isError == false     → StatusOK. Resets failures, clears lastErrorKind.

import (
	"testing"
)

// --- 1. ErrInvalidInput must NOT count toward failures ---

func TestRecordResult_ValidationDoesNotIncrementFailures(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")

	// Drive 10× ErrInvalidInput — way past the failure threshold.
	for i := 0; i < 10; i++ {
		fb := cb.RecordResult("web_search", true, ErrInvalidInput)
		if fb.Status != StatusValidationError {
			t.Fatalf("call %d: Status=%v, want StatusValidationError", i+1, fb.Status)
		}
		if fb.JustTripped {
			t.Fatalf("call %d: JustTripped=true on validation (must NEVER trip)", i+1)
		}
		if fb.Message == "" {
			t.Fatalf("call %d: Message empty (validationHint must be surfaced)", i+1)
		}
	}

	// Breaker state must remain pristine — Closed, no failures, no lastErrKind.
	state, _, failures, lastErrKind := cb.Snapshot()
	if state != StateClosed {
		t.Fatalf("state after 10× ErrInvalidInput = %v, want StateClosed (validation must not trip)", state)
	}
	if failures != 0 {
		t.Fatalf("failures after 10× ErrInvalidInput = %d, want 0 (validation must not count)", failures)
	}
	if lastErrKind != "" {
		t.Fatalf("lastErrorKind after validation-only = %q, want empty", lastErrKind)
	}
}

// --- 2. ErrDependencyDown opens immediately on first occurrence ---

func TestRecordResult_DependencyDownOpensImmediately(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")

	fb := cb.RecordResult("web_search", true, ErrDependencyDown)

	if fb.Status != StatusBlocked {
		t.Fatalf("Status=%v, want StatusBlocked", fb.Status)
	}
	if !fb.JustTripped {
		t.Fatal("JustTripped=false on first ErrDependencyDown, want true (single call must trip)")
	}
	if fb.Message == "" {
		t.Fatal("Message empty (dependencyDownHint must be surfaced)")
	}

	state, _, _, lastErrKind := cb.Snapshot()
	if state != StateOpen {
		t.Fatalf("state after 1× ErrDependencyDown = %v, want StateOpen", state)
	}
	if lastErrKind != ErrDependencyDown {
		t.Fatalf("lastErrorKind=%q, want %q", lastErrKind, ErrDependencyDown)
	}
}

// --- 3. Subsequent ErrDependencyDown calls in Open state must be idempotent ---

func TestRecordResult_DependencyDownWhenAlreadyOpenIsIdempotent(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")

	first := cb.RecordResult("web_search", true, ErrDependencyDown)
	if !first.JustTripped {
		t.Fatal("first call: JustTripped=false, want true")
	}

	// Second call while already Open — JustTripped must be false to prevent
	// the registry from emitting duplicate runtime events.
	second := cb.RecordResult("web_search", true, ErrDependencyDown)
	if second.JustTripped {
		t.Fatal("second call: JustTripped=true, want false (idempotency)")
	}
	if second.Status != StatusBlocked {
		t.Fatalf("second call: Status=%v, want StatusBlocked", second.Status)
	}
	// Failures counter must remain 0 — dependency-down is "binary", not a
	// counter (see plan Q1 resolution + circuit_breaker.go godoc).
	state, _, failures, _ := cb.Snapshot()
	if state != StateOpen {
		t.Fatalf("state=%v, want StateOpen", state)
	}
	if failures != 0 {
		t.Fatalf("failures=%d, want 0 (dependency-down must not increment counter)", failures)
	}
}

// --- 4. ErrTransient at threshold must transition with JustTripped ---

func TestRecordResult_TransientReachesThreshold(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")

	// failureThreshold defaults to 3 in NewCircuitBreaker.
	threshold := cb.failureThreshold
	if threshold != 3 {
		t.Fatalf("setup: failureThreshold=%d, want 3 (changed default?)", threshold)
	}

	// First two calls: below threshold → StatusTransient, no trip.
	for i := 1; i <= threshold-1; i++ {
		fb := cb.RecordResult("web_search", true, ErrTransient)
		if fb.Status != StatusTransient {
			t.Fatalf("call %d: Status=%v, want StatusTransient (below threshold)",
				i, fb.Status)
		}
		if fb.JustTripped {
			t.Fatalf("call %d: JustTripped=true below threshold, want false", i)
		}
		if fb.Message == "" {
			t.Fatalf("call %d: Message empty (transientHint must be surfaced)", i)
		}
	}

	// Threshold-th call: trips the breaker.
	trip := cb.RecordResult("web_search", true, ErrTransient)
	if trip.Status != StatusBlocked {
		t.Fatalf("threshold call: Status=%v, want StatusBlocked", trip.Status)
	}
	if !trip.JustTripped {
		t.Fatal("threshold call: JustTripped=false, want true (first trip event)")
	}
	if trip.Message == "" {
		t.Fatal("threshold call: Message empty (escalationHint must be surfaced)")
	}

	state, _, failures, lastErrKind := cb.Snapshot()
	if state != StateOpen {
		t.Fatalf("state at threshold = %v, want StateOpen", state)
	}
	if failures != threshold {
		t.Fatalf("failures at threshold = %d, want %d", failures, threshold)
	}
	if lastErrKind != ErrTransient {
		t.Fatalf("lastErrorKind=%q, want %q", lastErrKind, ErrTransient)
	}

	// One more ErrTransient after trip — JustTripped must stay false.
	over := cb.RecordResult("web_search", true, ErrTransient)
	if over.JustTripped {
		t.Fatal("post-trip call: JustTripped=true, want false (no duplicate event)")
	}
}

// --- 5. Successful result must reset failures and clear lastErrorKind ---

func TestRecordResult_SuccessResetsAndClearsLastErrorKind(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")

	// Trip via transient.
	for i := 0; i < cb.failureThreshold; i++ {
		cb.RecordResult("web_search", true, ErrTransient)
	}
	if state, _, _, _ := cb.Snapshot(); state != StateOpen {
		t.Fatalf("setup: state=%v, want StateOpen", state)
	}

	// Recover via success — must clear lastErrorKind and reset failures.
	fb := cb.RecordResult("web_search", false, ErrTransient)
	if fb.Status != StatusOK {
		t.Fatalf("Status=%v, want StatusOK", fb.Status)
	}

	state, _, failures, lastErrKind := cb.Snapshot()
	if state != StateClosed {
		t.Fatalf("state after recovery = %v, want StateClosed", state)
	}
	if failures != 0 {
		t.Fatalf("failures after recovery = %d, want 0", failures)
	}
	if lastErrKind != "" {
		t.Fatalf("lastErrorKind after recovery = %q, want empty", lastErrKind)
	}
}

// --- 6. LastErrorKind getter matches Snapshot's 4th return value ---

func TestRecordResult_LastErrorKindGetterMatchesSnapshot(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")

	cases := []ErrorKind{ErrDependencyDown, ErrTransient, ErrTimeout}
	for _, k := range cases {
		// Reset between cases.
		cb.RecordResult("web_search", false, ErrTransient)
		cb.RecordResult("web_search", true, k)

		_, _, _, snapKind := cb.Snapshot()
		getterKind := cb.LastErrorKind()
		if snapKind != getterKind {
			t.Fatalf("ErrorKind=%q: Snapshot()=%q vs LastErrorKind()=%q (must agree)",
				k, snapKind, getterKind)
		}
		if snapKind != k {
			t.Fatalf("ErrorKind=%q: tracked=%q (mismatch)", k, snapKind)
		}
	}
}
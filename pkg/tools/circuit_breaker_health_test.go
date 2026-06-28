package tools

import (
	"testing"
	"time"
)

// Tests for the health-snapshot surface added in plan
// exception-handling-recovery-pattern-gap-closure-20260628 (Task B0).
//
// These tests cover Name() / Snapshot() / NewCircuitBreakerWithName — the
// read-side additions that let the future ToolHealthContributor ask
// "which tools are currently open?" without mutating breaker state. The
// per-session scope behaviour is already covered by
// circuit_breaker_session_test.go.

// --- 1. Name() ---

func TestCircuitBreaker_Name_EmptyForLegacyCtor(t *testing.T) {
	cb := NewCircuitBreaker()
	if got := cb.Name(); got != "" {
		t.Fatalf("NewCircuitBreaker() should produce an unnamed breaker, got Name()=%q", got)
	}
}

func TestCircuitBreaker_Name_PopulatedByNameCtor(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")
	if got := cb.Name(); got != "web_search" {
		t.Fatalf("Name()=%q, want %q", got, "web_search")
	}
}

func TestCircuitBreaker_Name_StableAcrossStateTransitions(t *testing.T) {
	cb := NewCircuitBreakerWithName("shell_exec")
	if got := cb.Name(); got != "shell_exec" {
		t.Fatalf("initial Name()=%q, want %q", got, "shell_exec")
	}
	// Trip via ErrDependencyDown — name must not change.
	cb.RecordResult("shell_exec", true, ErrDependencyDown)
	if got := cb.Name(); got != "shell_exec" {
		t.Fatalf("Name() after trip=%q, want %q (name must persist)", got, "shell_exec")
	}
	// Recover via success — name must still be set.
	cb.RecordResult("shell_exec", false, ErrTransient)
	if got := cb.Name(); got != "shell_exec" {
		t.Fatalf("Name() after recovery=%q, want %q", got, "shell_exec")
	}
}

// --- 2. Snapshot() ---

func TestCircuitBreaker_Snapshot_InitialState(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")
	state, openedAt, failures := cb.Snapshot()
	if state != StateClosed {
		t.Fatalf("initial state=%v, want StateClosed (%v)", state, StateClosed)
	}
	if !openedAt.IsZero() {
		t.Fatalf("initial openedAt=%v, want zero", openedAt)
	}
	if failures != 0 {
		t.Fatalf("initial failures=%d, want 0", failures)
	}
}

func TestCircuitBreaker_Snapshot_AfterDependencyDown(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")
	before := time.Now()
	cb.RecordResult("web_search", true, ErrDependencyDown)
	after := time.Now()

	state, openedAt, failures := cb.Snapshot()
	if state != StateOpen {
		t.Fatalf("state after ErrDependencyDown=%v, want StateOpen", state)
	}
	// ErrDependencyDown opens the circuit immediately WITHOUT incrementing
	// cb.failures (see RecordResult in circuit_breaker.go) — failures is
	// only counted for the consecutive-failure path. This is correct: a
	// dependency outage is "binary" (up or down), not a counter.
	if failures != 0 {
		t.Fatalf("failures after ErrDependencyDown=%d, want 0 (DependencyDown does not increment failures)", failures)
	}
	// openedAt should be set to roughly "now"; allow a small skew but reject
	// obviously-bogus timestamps so a regression that forgets to set it is
	// caught.
	if openedAt.Before(before) || openedAt.After(after) {
		t.Fatalf("openedAt=%v not in expected window [%v, %v]", openedAt, before, after)
	}
}

func TestCircuitBreaker_Snapshot_AfterThresholdFailures(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")
	for i := 0; i < cb.failureThreshold; i++ {
		cb.RecordResult("web_search", true, ErrTransient)
	}

	state, openedAt, failures := cb.Snapshot()
	if state != StateOpen {
		t.Fatalf("state after %d transient failures=%v, want StateOpen",
			cb.failureThreshold, state)
	}
	if failures != cb.failureThreshold {
		t.Fatalf("failures=%d, want %d", failures, cb.failureThreshold)
	}
	if openedAt.IsZero() {
		t.Fatalf("openedAt must be set when breaker opens, got zero value")
	}
}

func TestCircuitBreaker_Snapshot_HalfOpenAfterRecoveryTimeout(t *testing.T) {
	cb := NewCircuitBreakerWithName("web_search")

	// Trip via the consecutive-failure path so cb.failures is incremented.
	// (ErrDependencyDown opens immediately but does NOT increment failures;
	// using ErrTransient here is what populates the counter we want to
	// observe surviving the HalfOpen transition.)
	for i := 0; i < cb.failureThreshold; i++ {
		cb.RecordResult("web_search", true, ErrTransient)
	}

	// Force openedAt into the past so Allow() transitions to HalfOpen.
	cb.mu.Lock()
	cb.openedAt = time.Now().Add(-2 * cb.recoveryTimeout)
	cb.mu.Unlock()

	if !cb.Allow() {
		t.Fatal("setup: breaker should transition to HalfOpen after timeout")
	}

	state, _, failures := cb.Snapshot()
	if state != StateHalfOpen {
		t.Fatalf("state after HalfOpen transition=%v, want StateHalfOpen", state)
	}
	// failures must survive the HalfOpen transition: only RecordResult
	// (on success) resets it. This catches a regression where the HalfOpen
	// branch in Allow() accidentally zeroes the counter.
	if failures != cb.failureThreshold {
		t.Fatalf("failures after HalfOpen=%d, want %d (must survive transition)",
			failures, cb.failureThreshold)
	}
}

// --- 3. NewCircuitBreakerWithName() defaults ---

func TestCircuitBreaker_WithNameCtor_HasSameDefaultsAsLegacy(t *testing.T) {
	named := NewCircuitBreakerWithName("web_search")
	legacy := NewCircuitBreaker()

	// Default thresholds must match the legacy ctor.
	if named.failureThreshold != legacy.failureThreshold {
		t.Fatalf("failureThreshold mismatch: named=%d legacy=%d",
			named.failureThreshold, legacy.failureThreshold)
	}
	if named.recoveryTimeout != legacy.recoveryTimeout {
		t.Fatalf("recoveryTimeout mismatch: named=%v legacy=%v",
			named.recoveryTimeout, legacy.recoveryTimeout)
	}

	// A fresh named breaker must behave like a fresh legacy one.
	if !named.Allow() {
		t.Fatal("fresh named breaker should Allow()")
	}
	state, openedAt, failures := named.Snapshot()
	if state != StateClosed || failures != 0 || !openedAt.IsZero() {
		t.Fatalf("fresh named breaker not in initial state: state=%v failures=%d openedAt=%v",
			state, failures, openedAt)
	}
}

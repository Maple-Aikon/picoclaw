package tools

import (
	"context"
	"sync"
	"testing"
	"time"
)

// These tests cover the per-session circuit breaker scope change
// (plan: circuit-breaker-scope-20260620, Option 2). Each test exercises
// the breaker lookup logic directly via getCircuitBreaker / breakerKey —
// not the public ExecuteWithContext path — because the goal is to validate
// the new (channel, chatID, name) keying and lazy allocation behaviour in
// isolation from the rest of the execute pipeline.

// --- helpers ---

// failResult returns a non-DependencyDown error so RecordResult counts it
// against the consecutive-failure threshold (3 by default).
func failResult(text string) *ToolResult {
	return ErrorResult(text).WithErrorKind(ErrTransient)
}

// okResult returns a successful result so RecordResult resets the breaker.
func okResult() *ToolResult {
	return &ToolResult{ForLLM: "ok", ForUser: "ok"}
}

// tripBreaker drives a breaker past its failure threshold so Allow()
// returns false on the next call. Uses ErrTransient (not
// ErrDependencyDown) because the consecutive-failure path is the one we
// want to exercise for "3 fails in a row".
func tripBreaker(t *testing.T, cb *CircuitBreaker, name string) {
	t.Helper()
	for i := 0; i < cb.failureThreshold; i++ {
		cb.RecordResult(name, true, ErrTransient)
	}
}

// --- 1. breakerKey ---

func TestBreakerKey_HappyPath(t *testing.T) {
	got := breakerKey("telegram", "5680819959", "web_search")
	want := "telegram:5680819959:web_search"
	if got != want {
		t.Fatalf("breakerKey happy-path = %q, want %q", got, want)
	}
}

func TestBreakerKey_EmptyContextFallsBackToAnon(t *testing.T) {
	// Both empty: must land in _anon namespace so legacy callers cannot
	// trip a real session's breaker.
	got := breakerKey("", "", "web_search")
	want := "_anon:web_search"
	if got != want {
		t.Fatalf("breakerKey empty = %q, want %q", got, want)
	}

	// One side empty but not both: treat as a real session (the convention
	// is "both empty → anon"; mixed-empty is still a real key).
	got2 := breakerKey("telegram", "", "web_search")
	if got2 == want {
		t.Fatalf("breakerKey(telegram,\"\",web_search) should not collapse to _anon key, got %q", got2)
	}
}

// --- 2. getCircuitBreaker: per-session isolation ---

func TestGetCircuitBreaker_DifferentSessionsAreIndependent(t *testing.T) {
	r := NewToolRegistry()

	// Session A trips its breaker on "web_search".
	cbA := r.getCircuitBreaker("telegram", "111", "web_search")
	tripBreaker(t, cbA, "web_search")
	if cbA.Allow() {
		t.Fatalf("session A breaker should be Open after 3 failures")
	}

	// Session B must still be Closed for the same tool.
	cbB := r.getCircuitBreaker("telegram", "222", "web_search")
	if !cbB.Allow() {
		t.Fatalf("session B breaker should still Allow (independent of session A)")
	}
}

func TestGetCircuitBreaker_DifferentToolsInSameSessionAreIndependent(t *testing.T) {
	r := NewToolRegistry()

	cbSearch := r.getCircuitBreaker("telegram", "111", "web_search")
	tripBreaker(t, cbSearch, "web_search")
	if cbSearch.Allow() {
		t.Fatalf("web_search breaker should be Open after 3 failures")
	}

	// Same session, different tool — must not be affected.
	cbExec := r.getCircuitBreaker("telegram", "111", "shell_exec")
	if !cbExec.Allow() {
		t.Fatalf("shell_exec breaker in same session should still Allow")
	}
}

func TestGetCircuitBreaker_EmptyContextUsesAnonNamespace(t *testing.T) {
	r := NewToolRegistry()

	// Caller forgot to pass channel/chatID (legacy Execute path).
	cbAnon := r.getCircuitBreaker("", "", "web_search")
	tripBreaker(t, cbAnon, "web_search")

	// Even a real session must NOT be tripped by _anon failures.
	cbReal := r.getCircuitBreaker("telegram", "111", "web_search")
	if !cbReal.Allow() {
		t.Fatalf("real-session breaker must remain Closed despite _anon failures")
	}

	// A second _anon caller should hit the SAME breaker (shared namespace).
	cbAnon2 := r.getCircuitBreaker("", "", "web_search")
	if cbAnon2 != cbAnon {
		t.Fatalf("_anon namespace must be shared across callers (got distinct breaker)")
	}
}

// --- 3. Same-session breaker: open → recovery timeout → half-open → success ---

func TestGetCircuitBreaker_OpenThenRecoverAfterRecoveryTimeout(t *testing.T) {
	r := NewToolRegistry()

	cb := r.getCircuitBreaker("telegram", "111", "web_search")
	tripBreaker(t, cb, "web_search")
	if cb.Allow() {
		t.Fatalf("breaker should be Open immediately after trip")
	}

	// Force the breaker to look like it opened > recoveryTimeout ago.
	cb.mu.Lock()
	cb.openedAt = time.Now().Add(-2 * cb.recoveryTimeout)
	cb.mu.Unlock()

	// Next Allow() should transition to HalfOpen and return true.
	if !cb.Allow() {
		t.Fatalf("breaker should transition to HalfOpen after recovery timeout")
	}

	// A successful result must close it.
	cb.RecordResult("web_search", false, ErrTransient)
	if !cb.Allow() {
		t.Fatalf("breaker should be Closed after successful recovery")
	}

	// And a parallel session must not have been affected by any of this.
	cbB := r.getCircuitBreaker("telegram", "222", "web_search")
	if !cbB.Allow() {
		t.Fatalf("session B should still be Closed throughout session A's recovery")
	}
}

// --- 4. Race: concurrent lazy allocation must not panic or lose entries ---

func TestGetCircuitBreaker_ConcurrentLazyAllocationIsSafe(t *testing.T) {
	r := NewToolRegistry()

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// All goroutines request the same key — they must all get the same
	// breaker back (no duplicates) and the registry must contain exactly
	// one entry for that key afterwards.
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			cb := r.getCircuitBreaker("telegram", "111", "web_search")
			if cb == nil {
				t.Error("getCircuitBreaker returned nil under concurrency")
			}
		}()
	}
	wg.Wait()

	r.cbMu.Lock()
	defer r.cbMu.Unlock()
	if len(r.breakers) != 1 {
		t.Fatalf("expected exactly 1 breaker after concurrent requests, got %d", len(r.breakers))
	}
	for k := range r.breakers {
		if k != "telegram:111:web_search" {
			t.Fatalf("unexpected key in breakers map: %q", k)
		}
	}
}

// --- 5. Clone() does not inherit breaker state ---

func TestClone_StartsWithEmptyBreakerMap(t *testing.T) {
	parent := NewToolRegistry()

	// Trip a breaker on the parent so its state is visibly Open.
	parentCb := parent.getCircuitBreaker("telegram", "111", "web_search")
	tripBreaker(t, parentCb, "web_search")
	if parentCb.Allow() {
		t.Fatalf("setup: parent breaker should be Open")
	}

	clone := parent.Clone()

	// The clone's breakers map must be empty — subagents must NOT inherit
	// the parent's transient failure state.
	clone.cbMu.Lock()
	cloneLen := len(clone.breakers)
	clone.cbMu.Unlock()
	if cloneLen != 0 {
		t.Fatalf("Clone should start with empty breakers map, got %d entries", cloneLen)
	}

	// First lookup on the clone should allocate a fresh Closed breaker,
	// not share the parent's Open one.
	cloneCb := clone.getCircuitBreaker("telegram", "111", "web_search")
	if cloneCb == nil {
		t.Fatal("clone.getCircuitBreaker returned nil")
	}
	if cloneCb == parentCb {
		t.Fatal("clone must not share the parent's breaker instance")
	}
	if !cloneCb.Allow() {
		t.Fatal("clone's first breaker must start Closed")
	}
}

// --- 6. End-to-end through ExecuteWithContext: failure path uses per-session key ---

func TestExecuteWithContext_OpenBreakerBlocksOnlyItsOwnSession(t *testing.T) {
	r := NewToolRegistry()

	// Tool that always fails with ErrTransient.
	failTool := &mockRegistryTool{
		name:   "flaky",
		desc:   "always fails",
		params: map[string]any{"type": "object", "properties": map[string]any{}},
		result: failResult("boom"),
	}
	r.Register(failTool)

	// Drive session A past the threshold via the public execute path.
	for i := 0; i < 3; i++ {
		res := r.ExecuteWithContext(context.Background(), "flaky", nil, "telegram", "111", nil)
		if res == nil || !res.IsError {
			t.Fatalf("attempt %d: expected error result, got %+v", i+1, res)
		}
	}

	// Session A's 4th call must be blocked by the breaker (Allow() == false).
	resA := r.ExecuteWithContext(context.Background(), "flaky", nil, "telegram", "111", nil)
	if resA == nil {
		t.Fatal("session A 4th call returned nil")
	}
	if !resA.IsError || resA.ErrKind != ErrDependencyDown {
		t.Fatalf(
			"session A 4th call should be blocked by breaker (ErrDependencyDown), got IsError=%v ErrKind=%v ForLLM=%q",
			resA.IsError, resA.ErrKind, resA.ForLLM,
		)
	}

	// Session B must still get through to the underlying tool (and fail
	// normally, NOT be blocked).
	resB := r.ExecuteWithContext(context.Background(), "flaky", nil, "telegram", "222", nil)
	if resB == nil {
		t.Fatal("session B call returned nil")
	}
	if resB.ErrKind == ErrDependencyDown {
		t.Fatalf("session B should NOT see circuit-open, got ErrDependencyDown. ForLLM=%q", resB.ForLLM)
	}
	// Underlying tool still fails — but it fails for its own reason,
	// not because of session A's breaker.
	if !resB.IsError {
		t.Fatal("session B should still see the underlying failure")
	}
}
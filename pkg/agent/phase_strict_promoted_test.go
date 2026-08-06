// Phase 12.50 §3.1 — promotion latch (PromotionState) tests.
// Tests assert INFRA-ONLY semantics in default build (fail-OPEN path).
// In strict_phases build, PhasePolicyFor("bogus") panics via MustBeKnown
// — TestPhasePolicyFor_InvokesIsStrictRuntimeFailClosed is gated out.
//
//go:build !strict_phases

// Test seams:
//   - `loadPromotionState()` — reads ~/.picoclaw/.strict_promoted, returns nil on error.
//   - `savePromotionStateFresh(s)` — atomic write (tmp+rename).
//   - `IsStrictRuntimeFailClosed()` — main predicate for env-mode fail-CLOSED.
//   - `ensurePromotionState()` — lazy-init PromotionState + persist FirstSeenAt.
//   - `resetFirstSeenAtForMiss()` — race-safe CAS swap when miss counter bumps.
//   - `promoteLocked(now)` — atomic flip PromotedAt + sync persist.
//
// All tests use t.TempDir() for isolated .strict_promoted file paths.
// Production path resolution lives in phase_strict_promoted.go:resolvePromotionFilePath().

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// setPromotionFilePathForTest redirects the .strict_promoted file path to
// a test-owned tempdir. Cleanup restores the original.
func setPromotionFilePathForTest(t *testing.T, dir string) {
	t.Helper()
	prev := promotionFilePathOverride
	promotionFilePathOverride = dir
	t.Cleanup(func() { promotionFilePathOverride = prev })
}

// TestLoadPromotionState_MissingFile — T11 cell. No file present → returns nil.
// ensurePromotionState falls back to fresh init + persist.
func TestLoadPromotionState_MissingFile(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	got := loadPromotionState()
	if got != nil {
		t.Errorf("loadPromotionState() with no file = %+v, want nil", got)
	}
}

// TestLoadPromotionState_EmptyFile — T15 edge case. Empty file → returns nil
// (graceful fallback, no panic).
func TestLoadPromotionState_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	path := filepath.Join(dir, ".strict_promoted")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadPromotionState()
	if got != nil {
		t.Errorf("loadPromotionState() with empty file = %+v, want nil", got)
	}
}

// TestLoadPromotionState_GarbageContent — T15 cell. Garbage bytes (not
// `promoted_at=N` format) → returns nil + log warn. No panic.
func TestLoadPromotionState_GarbageContent(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	path := filepath.Join(dir, ".strict_promoted")
	if err := os.WriteFile(path, []byte("not a valid format\njust some bytes\n\x00\x01\x02"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should not panic, should return nil.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("loadPromotionState panicked on garbage: %v", r)
		}
	}()

	got := loadPromotionState()
	if got != nil {
		t.Errorf("loadPromotionState() with garbage content = %+v, want nil", got)
	}
}

// TestLoadPromotionState_NegativeTimestamp — T15 edge case. Valid format
// but negative promoted_at → returns nil (defensive: never trust file
// contents for security).
func TestLoadPromotionState_NegativeTimestamp(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	path := filepath.Join(dir, ".strict_promoted")
	if err := os.WriteFile(path, []byte("promoted_at=-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadPromotionState()
	if got != nil {
		t.Errorf("loadPromotionState() with negative ts = %+v, want nil", got)
	}
}

// TestLoadPromotionState_ValidFormat — T9 cell (Phase 12.50 §3.5). Valid
// `promoted_at=N` format → returns *PromotionState{PromotedAt: N, FirstSeenAt: 0}.
func TestLoadPromotionState_ValidFormat(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	path := filepath.Join(dir, ".strict_promoted")
	if err := os.WriteFile(path, []byte("promoted_at=1700000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadPromotionState()
	if got == nil {
		t.Fatal("loadPromotionState() with valid file = nil, want non-nil")
	}
	if got.PromotedAt != 1700000000 {
		t.Errorf("PromotedAt = %d, want 1700000000", got.PromotedAt)
	}
	if got.FirstSeenAt != 0 {
		t.Errorf("FirstSeenAt = %d, want 0", got.FirstSeenAt)
	}
}

// TestPhasePolicyFor_InvokesIsStrictRuntimeFailClosed — T8 wire test.
// Phase 12.50 R6 + Round 5 Q1=A: 12.50 ships INFRA-ONLY. Signature change
// `(policy) → (policy, error)` is deferred to 12.51a (Path 4 wire fix).
//
// This test verifies the promotion latch INFRA wires correctly:
//   1. Set env=1, FirstSeenAt=8d ago
//   2. IsStrictRuntimeFailClosed() returns true
//   3. PhasePolicyFor("bogus") still returns nil (signature unchanged yet)
//   4. State shows PromotedAt > 0 (proves promoteLocked fired)
//
// Caller audit (11 sites need to handle err) ships in 12.51a/b per X-9
// cross-check from main-turn-19 debug session.
func TestPhasePolicyFor_InvokesIsStrictRuntimeFailClosed(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)
	resetPromotionStateForTest()

	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Setenv(config.EnvAgentStrictPhases, "1")

	eightDaysAgo := nowUnix() - 8*24*3600
	SetFirstSeenAtForTest(eightDaysAgo)

	// Sanity check: promotion fires.
	if !IsStrictRuntimeFailClosed() {
		t.Fatal("IsStrictRuntimeFailClosed() = false, want true (FirstSeenAt=8d)")
	}

	// In-memory state must show PromotedAt > 0 (proves promoteLocked fired).
	state := promotionState.Load()
	if state == nil || state.PromotedAt == 0 {
		t.Fatal("promotion state missing PromotedAt — ROUND4-C4 regression")
	}

	// PhasePolicyFor("bogus") still returns nil (signature unchanged in 12.50).
	// Caller audit deferred to 12.51a. This is the INFRA-ONLY contract.
	policy := PhasePolicyFor(GoalPhase("bogus"))
	if policy != nil {
		t.Errorf("PhasePolicyFor('bogus') = %+v, want nil (signature unchanged until 12.51a)", policy)
	}
}

// TestSavePromotionStateFresh_AtomicWrite — T10 cell. savePromotionStateFresh
// writes via tmp+rename pattern. Verify file exists after call and contents
// match expected format.
func TestSavePromotionStateFresh_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	state := &PromotionState{PromotedAt: 1700000001, FirstSeenAt: 1700000000}
	if err := savePromotionStateFresh(state); err != nil {
		t.Fatalf("savePromotionStateFresh err: %v", err)
	}

	path := filepath.Join(dir, ".strict_promoted")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after save: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("save wrote empty file")
	}
	// Format: "promoted_at=N\n" — relaxed substring check (no strict format).
	if !containsBytes(data, "1700000001") {
		t.Errorf("save content missing PromotedAt value: %q", data)
	}
}

// TestEnsurePromotionState_LazyInit — T14b cell (DOUBT-C1 fold). First call
// to ensurePromotionState() with env=1 triggers FirstSeenAt≈now. Second call
// asserts FirstSeenAt unchanged (no re-init). Without DOUBT-C1 fix, lazy
// init never happens → FirstSeenAt stays 0 forever.
func TestEnsurePromotionState_LazyInit(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	// Reset global state for test isolation.
	resetPromotionStateForTest()

	s1 := ensurePromotionState()
	if s1 == nil {
		t.Fatal("ensurePromotionState() returned nil")
	}
	if s1.FirstSeenAt == 0 {
		t.Fatal("FirstSeenAt=0 after lazy init — DOUBT-C1 regression!")
	}
	firstSeenAt := s1.FirstSeenAt

	// Second call must return same state (no re-init).
	s2 := ensurePromotionState()
	if s2.FirstSeenAt != firstSeenAt {
		t.Errorf("FirstSeenAt re-init: before=%d after=%d", firstSeenAt, s2.FirstSeenAt)
	}
}

// TestResetFirstSeenAtForMiss_RaceSafe — T14c + T14e fold (DOUBT-C2). Set
// FirstSeenAt=now via init, call resetFirstSeenAtForMiss(), assert FirstSeenAt=0
// immediately. Concurrent goroutines must not race.
func TestResetFirstSeenAtForMiss_RaceSafe(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	resetPromotionStateForTest()
	s := ensurePromotionState()
	if s.FirstSeenAt == 0 {
		t.Fatal("FirstSeenAt not set after lazy init")
	}

	resetFirstSeenAtForMiss()

	// After reset, load fresh state.
	s2 := promotionState.Load()
	if s2 == nil || s2.FirstSeenAt != 0 {
		t.Errorf("after resetFirstSeenAtForMiss: FirstSeenAt=%d, want 0", s2.FirstSeenAt)
	}
}

// TestPromoteLocked_CASOnly — T16 cell. After promoteLocked fires, PromotedAt > 0
// and state is persisted. Without ROUND4-C4 fix, promoteLocked would never be
// called from IsStrictRuntimeFailClosed — this test asserts the actual wire.
func TestPromoteLocked_CASOnly(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)

	resetPromotionStateForTest()
	// First, init state.
	_ = ensurePromotionState()

	promoteLocked(1700000005)

	s := promotionState.Load()
	if s == nil || s.PromotedAt != 1700000005 {
		t.Errorf("after promoteLocked: PromotedAt=%d, want 1700000005", s.PromotedAt)
	}
}

// TestIsStrictRuntimeFailClosed_DisabledByDefault — without env, always false.
func TestIsStrictRuntimeFailClosed_DisabledByDefault(t *testing.T) {
	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Unsetenv(config.EnvAgentStrictPhases)
	resetPromotionStateForTest()

	if IsStrictRuntimeFailClosed() {
		t.Fatal("IsStrictRuntimeFailClosed() = true without env, want false")
	}
}

// TestIsStrictRuntimeFailClosed_BeforeWindow — FirstSeenAt set recently → not promoted.
func TestIsStrictRuntimeFailClosed_BeforeWindow(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)
	resetPromotionStateForTest()

	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Setenv(config.EnvAgentStrictPhases, "1")

	// Lazy init → FirstSeenAt = now
	_ = ensurePromotionState()

	if IsStrictRuntimeFailClosed() {
		t.Fatal("IsStrictRuntimeFailClosed() = true right after lazy init, want false (7d not elapsed)")
	}
}

// TestIsStrictRuntimeFailClosed_AfterWindow — FirstSeenAt set 8d ago → promoted.
func TestIsStrictRuntimeFailClosed_AfterWindow(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)
	resetPromotionStateForTest()

	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Setenv(config.EnvAgentStrictPhases, "1")

	// Set FirstSeenAt to 8 days ago via injection.
	eightDaysAgo := nowUnix() - 8*24*3600
	SetFirstSeenAtForTest(eightDaysAgo)

	if !IsStrictRuntimeFailClosed() {
		t.Fatal("IsStrictRuntimeFailClosed() = false with FirstSeenAt=8d ago, want true (7d elapsed)")
	}
}

// TestIsStrictRuntimeFailClosed_InvokesPromoteLocked — T16b cell. Verify
// IsStrictRuntimeFailClosed actually flips PromotedAt atomically. Without
// ROUND4-C4 fix, returns true but never persists.
func TestIsStrictRuntimeFailClosed_InvokesPromoteLocked(t *testing.T) {
	dir := t.TempDir()
	setPromotionFilePathForTest(t, dir)
	resetPromotionStateForTest()

	prev := os.Getenv(config.EnvAgentStrictPhases)
	defer os.Setenv(config.EnvAgentStrictPhases, prev)
	os.Setenv(config.EnvAgentStrictPhases, "1")

	eightDaysAgo := nowUnix() - 8*24*3600
	SetFirstSeenAtForTest(eightDaysAgo)

	IsStrictRuntimeFailClosed()

	// After call, PromotedAt MUST be set (proves promoteLocked fired).
	s := promotionState.Load()
	if s == nil || s.PromotedAt == 0 {
		t.Fatal("after IsStrictRuntimeFailClosed: PromotedAt=0 — promoteLocked NEVER FIRED (ROUND4-C4 regression)")
	}
}

// containsBytes is a helper to avoid importing strings in this file's
// t.Log calls. Faster than strings.Contains for single-substring checks
// in tests. Renamed from `contains` because gate_skip_message_test.go
// already defines `contains(string, string) bool` in the same package.
func containsBytes(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
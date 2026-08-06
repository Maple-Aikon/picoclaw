// Phase 12.50 §3.1 — promotion latch (PromotionState) for strict-mode
// runtime fail-CLOSED behavior.
//
// Q1=A: auto-promote after ≥7 days of zero `agent_phase_lookup_miss`
// counter. Q2=C: persist FirstSeenAt to file (write once on lazy-init,
// not per-miss) so "7 calendar days" survives picoclaw restart.
// Q4=A: savePromotionStateFresh is sync IO inside CAS-success branch
// (acceptable brief block — promotion only fires once per 7d).
// Q5=C: CAS-first then sync persist (in-memory = source of truth,
// file = durability fallback).
//
// File format: "promoted_at=<unix_seconds>\n" — simple key=value,
// no schema versioning needed for a single boolean.
//
// Three state-modifying helpers (CAS-only invariant per ROUND3-F01):
//   - ensurePromotionState()        lazy-init + persist FirstSeenAt
//   - resetFirstSeenAtForMiss()     CAS-swap with FirstSeenAt=0 (DOUBT-C2)
//   - promoteLocked(now)            CAS-swap with PromotedAt=now + persist (ROUND4-C4)
// Direct field mutation = race condition. NEVER add `state.X = Y` outside
// these 3 helpers.

package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// PromotionWindowSec is the 7-day zero-counter window required before
// strict-mode promotes to runtime fail-CLOSED.
const PromotionWindowSec = 7 * 24 * 3600

// PromotionState carries the persisted promotion clock. PromotedAt > 0
// means promotion has fired (fail-CLOSED on next env=1 startup).
// FirstSeenAt > 0 means the strict-mode counter has been observed at
// least once (window starts from FirstSeenAt, NOT from process start).
type PromotionState struct {
	PromotedAt  int64 // unix seconds; 0 = not promoted
	FirstSeenAt int64 // unix seconds; 0 = never observed
}

// promotionFilePathOverride is the test seam for the .strict_promoted
// file path. When set, loadPromotionState + savePromotionStateFresh
// resolve against this directory instead of ~/.picoclaw/.
//
// Production path resolution: $PICOCLAW_HOME/.strict_promoted, falling
// back to ~/.picoclaw/.strict_promoted if env unset.
var promotionFilePathOverride string

// promotionState is the in-memory PromotionState. atomic.Pointer for
// lock-free reads (matches pkg/credential/store.go:11 pattern).
var promotionState atomic.Pointer[PromotionState]

// nowOverrideForTest is the test-only override for `time.Now().Unix()`.
// Production callers always read time.Now() directly via nowUnix().
var nowOverrideForTest atomic.Int64

// nowUnix returns time.Now().Unix() or the test override if set > 0.
func nowUnix() int64 {
	if o := nowOverrideForTest.Load(); o > 0 {
		return o
	}
	return time.Now().Unix()
}

// resolvePromotionFilePath returns the directory used for
// .strict_promoted persistence. Test override wins; production reads
// PICOCLAW_HOME env (matches existing picoclaw config layout).
func resolvePromotionFilePath() string {
	if promotionFilePathOverride != "" {
		return promotionFilePathOverride
	}
	// Production: $PICOCLAW_HOME/.strict_promoted.
	home := os.Getenv("PICOCLAW_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".picoclaw")
	}
	return home
}

// loadPromotionState reads ~/.picoclaw/.strict_promoted. Returns nil if
// the file is missing, empty, or contains garbage content. Never panics
// on malformed input (DOUBT-C3 graceful fallback).
//
// Format: "promoted_at=<unix_seconds>\n". Future fields are ignored.
// Negative promoted_at treated as garbage (defensive).
func loadPromotionState() *PromotionState {
	dir := resolvePromotionFilePath()
	path := filepath.Join(dir, ".strict_promoted")
	data, err := os.ReadFile(path)
	if err != nil {
		// Missing file = fresh init. Permission errors fall through silently
		// (test isolation + production FS write at first save will retry).
		return nil
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return nil
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "promoted_at=") {
			continue
		}
		val := strings.TrimPrefix(line, "promoted_at=")
		ts, err := strconv.ParseInt(val, 10, 64)
		if err != nil || ts <= 0 {
			logger.WarnCF("agent", "strict-mode promotion file has invalid promoted_at value",
				map[string]any{
					"event":  "agent_phase_promotion_load_invalid",
					"path":   path,
					"value":  val,
					"reason": "parse_error_or_nonpositive",
				})
			return nil
		}
		return &PromotionState{PromotedAt: ts}
	}
	// No promoted_at line found → treated as garbage.
	logger.WarnCF("agent", "strict-mode promotion file has no promoted_at line",
		map[string]any{
			"event": "agent_phase_promotion_load_malformed",
			"path":  path,
		})
	return nil
}

// savePromotionStateFresh writes state atomically via tmp + rename.
// On FS error, returns the error (caller logs warn, accepts in-memory).
//
// Q4=A: sync IO inside CAS-success branch. Q5=C: CAS-first then sync
// persist; on persist failure, in-memory already promoted via CAS,
// log warn, accept in-memory (in-memory = source of truth).
//
// File format: "promoted_at=<unix_seconds>\n".
func savePromotionStateFresh(state *PromotionState) error {
	dir := resolvePromotionFilePath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	target := filepath.Join(dir, ".strict_promoted")
	tmp := target + ".tmp"

	content := fmt.Sprintf("promoted_at=%d\n", state.PromotedAt)
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		// Cleanup tmp on rename failure (avoid leaving half-written file).
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, target, err)
	}
	return nil
}

// ensurePromotionState lazy-inits the in-memory PromotionState.
//
// Q2=C (round 5): try load first; if file exists with FirstSeenAt>0, use
// that. Else fresh init with FirstSeenAt=time.Now().Unix() + persist
// (write-once at lazy init, NOT per-miss — avoids FS hot-path cost).
//
// DOUBT-C1 (round 2): must be called from IsStrictRuntimeFailClosed
// before the `state == nil` early return. Without this call, FirstSeenAt
// stays 0 forever (production would never promote).
func ensurePromotionState() *PromotionState {
	if s := promotionState.Load(); s != nil {
		return s
	}
	// Try load from file first (handles restart case per Q2=C).
	if loaded := loadPromotionState(); loaded != nil {
		if promotionState.CompareAndSwap(nil, loaded) {
			return loaded
		}
		return promotionState.Load()
	}
	// Fresh init: FirstSeenAt = now, persist immediately.
	fresh := &PromotionState{FirstSeenAt: nowUnix()}
	if !promotionState.CompareAndSwap(nil, fresh) {
		return promotionState.Load()
	}
	if err := savePromotionStateFresh(fresh); err != nil {
		logger.WarnCF("agent", "strict-mode FirstSeenAt persist failed (in-memory OK)",
			map[string]any{
				"event":        "agent_phase_firstseen_persist_failed",
				"first_seen_at": fresh.FirstSeenAt,
				"err":          err.Error(),
			})
	}
	return fresh
}

// resetFirstSeenAtForMiss mutates the FirstSeenAt clock to 0 when a
// miss is recorded (T3: "misses reset the clock").
//
// DOUBT-C2 (round 2): race-safe via CAS swap of a NEW struct (not direct
// field mutation of the shared pointer). Reading code, easy to "tiện tay
// set field" — DO NOT. Lock down: any write to PromotionState MUST go
// through one of 3 helpers.
func resetFirstSeenAtForMiss() {
	for {
		old := promotionState.Load()
		if old == nil || old.FirstSeenAt == 0 {
			return // nothing to reset
		}
		fresh := &PromotionState{
			PromotedAt:  old.PromotedAt,
			FirstSeenAt: 0,
		}
		if promotionState.CompareAndSwap(old, fresh) {
			return
		}
		// lost race, retry
	}
}

// promoteLocked atomically flips PromotedAt=now + sync persist.
// ROUND4-C4: must be invoked from IsStrictRuntimeFailClosed right
// before returning true. Without this call, state.PromotedAt stays 0
// forever even after 7d elapsed.
//
// Q5=C: CAS-swap in-memory first (atomic.Pointer pointer change).
// Then sync persist (tmp+rename, blocks briefly). On write failure,
// log warn but accept in-memory promotion (in-memory = source of truth;
// file = durability fallback).
func promoteLocked(now int64) {
	for {
		old := promotionState.Load()
		if old == nil {
			return // nothing to promote
		}
		if old.PromotedAt > 0 {
			return // already promoted
		}
		fresh := &PromotionState{
			PromotedAt:  now,
			FirstSeenAt: old.FirstSeenAt,
		}
		if promotionState.CompareAndSwap(old, fresh) {
			if err := savePromotionStateFresh(fresh); err != nil {
				logger.WarnCF("agent", "strict-mode promotion persist failed (in-memory OK)",
					map[string]any{
						"event":      "agent_phase_promotion_persist_failed",
						"promoted_at": fresh.PromotedAt,
						"err":        err.Error(),
					})
			}
			return
		}
	}
}

// IsStrictRuntimeFailClosed returns true iff:
//   - PICOCLAW_AGENT_STRICT_PHASES is set ("1"/"true"/"yes"/"on"), AND
//   - the 7-day window has elapsed since first seen, AND
//   - no misses were recorded in the window (FirstSeenAt is the clock anchor).
//
// DOUBT-C1 (round 2): ensurePromotionState MUST be called inside this
// function before the `state == nil` early return, otherwise FirstSeenAt
// stays 0 forever (production would never promote).
//
// ROUND4-C4 (round 4): promoteLocked must be invoked BEFORE returning
// true. Without this call, state.PromotedAt stays 0 forever even after
// 7d elapsed → fail-CLOSED never flips on (only the in-memory check
// would return true; persistence + subsequent restarts would fail).
//
// Round 5 Q1=A: signature returns only `bool` (NO `(*Policy, error)`).
// The 11 production callers silently fall through when err is returned,
// so changing to error return is a no-op for now. Caller audit + err
// propagation deferred to 12.51a/b (Path 4 wire fix + escape hatch).
func IsStrictRuntimeFailClosed() bool {
	if !IsStrictPhasesEnabled() {
		return false
	}
	state := ensurePromotionState()
	if state.PromotedAt > 0 {
		return true // already promoted, no-op
	}
	now := nowUnix()
	if state.FirstSeenAt > 0 && now-state.FirstSeenAt >= PromotionWindowSec {
		// 7d elapsed → promote atomically + persist before returning true
		promoteLocked(now)
		return true
	}
	return false
}

// SetFirstSeenAtForTest is a test-only seam that injects a specific
// FirstSeenAt value (unix seconds). Used by tests that need to simulate
// "7+ days elapsed since first seen" without waiting in real time.
//
// Implementation note: we set FirstSeenAt explicitly and keep `now` at
// real time, so `now - FirstSeenAt` is the simulated elapsed duration.
// The nowOverrideForTest atomic is reset to 0 (real time) — only
// FirstSeenAt is synthetic.
func SetFirstSeenAtForTest(unixSec int64) {
	nowOverrideForTest.Store(0) // use real time for "now"
	resetPromotionStateForTest()
	fresh := &PromotionState{FirstSeenAt: unixSec}
	promotionState.Store(fresh)
}

// resetPromotionStateForTest is a test-only seam that clears the
// global promotionState pointer. Must be called at the start of every
// test that uses SetFirstSeenAtForTest or expects fresh state.
func resetPromotionStateForTest() {
	promotionState.Store(nil)
}
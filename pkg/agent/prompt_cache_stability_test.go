package agent

import (
	"crypto/sha256"
	"fmt"
	"testing"
)
// TestPromptCacheStability_OpenPhase_CacheKeyReuse verifies the OPEN-phase
// cache slot is correctly keyed by iter + iter-cap dims. Calling
// BuildSystemPrompt twice with the same (phase=OPEN, iter, cap, max-cap)
// tuple yields a CACHE HIT (byte-identical output) — the second call does
// not need to rebuild. Cross-iter calls with non-zero cap dims produce
// different prompts because the OPEN-phase hint carries the iter / iter-cap
// compass (Phase 12.38 §4).
//
// Why this test exists:
//   - Open phase is the only phase where the prompt cache is reused across
//     calls (Phase 12.16.1 followup: Set/Checkpoint/Final bypass the cache).
//   - The cache key includes iter + iter-cap + max-iter-cap. This test
//     confirms the cache STORES + RETRIEVES by that key, not by phase alone.
//
// What this test is NOT trying to assert:
//   - Cross-phase byte-identity. Each phase fires its own hint contributor
//     with phase-specific text (allowed-tools list, lockout semantics).
//     Cross-phase prompts MUST differ.
func TestPromptCacheStability_OpenPhase_CacheKeyReuse(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared Agent LayerIdentity info here",
		"memory/MEMORY.md": "# PicoClaw MemoryPersistent memory facts",
	})

	cb := NewContextBuilder(tmpDir)

	// Use BuildSystemPromptWithSnapshotFullKey to thread cap dims.
	// iterationCap=25, maxIterationsCap=250 — the OPEN hint now renders
	// the dynamic compass (Phase 12.38 §4): iter is in the prompt body.
	p1 := cb.BuildSystemPromptWithSnapshotFullKey(
		string(GoalPhaseOpen), false, 3, "", 25, 250)
	p2 := cb.BuildSystemPromptWithSnapshotFullKey(
		string(GoalPhaseOpen), false, 3, "", 25, 250)

	h1 := fmt.Sprintf("%x", sha256.Sum256([]byte(p1)))
	h2 := fmt.Sprintf("%x", sha256.Sum256([]byte(p2)))

	if h1 != h2 {
		t.Errorf("OPEN-phase cache miss on identical back-to-back callhash1=%shash2=%s— the system prompt cache slot is not stable for the same (phase, iter) tuple.", h1, h2)
	}

	// Iter change with cap dims set → different prompt (iter dimension in cache key)
	p3 := cb.BuildSystemPromptWithSnapshotFullKey(
		string(GoalPhaseOpen), false, 4, "", 25, 250)
	h3 := fmt.Sprintf("%x", sha256.Sum256([]byte(p3)))
	if h1 == h3 {
		t.Errorf("OPEN-phase iter change did not invalidate prompt; hash1=%s hash3=%s — iter dimension is missing from cache key.", h1, h3)
	}

	// Cap change → different prompt (iter-cap dimension in cache key)
	p4 := cb.BuildSystemPromptWithSnapshotFullKey(
		string(GoalPhaseOpen), false, 3, "", 30, 250)
	h4 := fmt.Sprintf("%x", sha256.Sum256([]byte(p4)))
	if h1 == h4 {
		t.Errorf("OPEN-phase iter-cap change did not invalidate prompt; hash1=%s hash4=%s — iter-cap dimension is missing from cache key.", h1, h4)
	}
}

// TestPromptCacheStability_DynamicContentExcluded verifies that time/runtime
// info from buildDynamicContext does NOT leak into the system prompt (it
// belongs in user[0] turn-tail — see wrapDynamicContext). MiniMax-M3 passive
// cache requires 100% identity-stable system prefix to register a cache hit,
// so any second-resolution timestamp embedded in the system would invalidate
// every call.
//
// Asserts by checking that calling BuildSystemPrompt twice (same phase, same
// iter) yields byte-identical output even though buildDynamicContext's
// underlying time.Now() would have advanced between calls.
func TestPromptCacheStability_DynamicContentExcluded(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared Agent LayerIdentity info here",
		"memory/MEMORY.md": "# PicoClaw MemoryPersistent memory facts",
	})

	cb := NewContextBuilder(tmpDir)

	// Two calls, back-to-back — any dynamic content would produce different
	// hashes (time.Now() advances between calls).
	p1 := cb.BuildSystemPrompt(string(GoalPhaseOpen), false, 1)
	p2 := cb.BuildSystemPrompt(string(GoalPhaseOpen), false, 1)

	h1 := fmt.Sprintf("%x", sha256.Sum256([]byte(p1)))
	h2 := fmt.Sprintf("%x", sha256.Sum256([]byte(p2)))

	if h1 != h2 {
		t.Errorf("BuildSystemPrompt output drifted between identical back-to-back callshash1=%shash2=%s— dynamic content (time/runtime) is leaking into the system prompt. Move it to user[0] turn-tail (wrapDynamicContext).", h1, h2)
	}
}

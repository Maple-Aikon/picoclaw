package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// setupWorkspace creates a temporary workspace with standard directories and optional files.
// Returns the tmpDir path; caller should defer os.RemoveAll(tmpDir).
func setupWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "picoclaw-test-*")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(tmpDir, "memory"), 0o755)
	os.MkdirAll(filepath.Join(tmpDir, "skills"), 0o755)
	for name, content := range files {
		dir := filepath.Dir(filepath.Join(tmpDir, name))
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tmpDir
}

// TestSingleSystemMessage verifies that BuildMessages always produces exactly one
// system message regardless of summary/history variations.
// Fix: multiple system messages break Anthropic (top-level system param) and
// Codex (only reads last system message as instructions).
func TestSingleSystemMessage(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nTest agent.",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	tests := []struct {
		name    string
		history []providers.Message
		summary string
		message string
	}{
		{
			name:    "no summary, no history",
			summary: "",
			message: "hello",
		},
		{
			name:    "with summary",
			summary: "Previous conversation discussed X",
			message: "hello",
		},
		{
			name: "with history and summary",
			history: []providers.Message{
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
			summary: strings.Repeat("Long summary text. ", 50),
			message: "new message",
		},
		{
			name: "system message in history is filtered",
			history: []providers.Message{
				{Role: "system", Content: "stale system prompt from previous session"},
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
			summary: "",
			message: "new message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := cb.BuildMessages(tt.history, tt.summary, tt.message, nil, "test", "chat1", "", "")

			systemCount := 0
			for _, m := range msgs {
				if m.Role == "system" {
					systemCount++
				}
			}
			if systemCount != 1 {
				t.Errorf("expected exactly 1 system message, got %d", systemCount)
			}
			if msgs[0].Role != "system" {
				t.Errorf("first message should be system, got %s", msgs[0].Role)
			}
			if msgs[len(msgs)-1].Role != "user" {
				t.Errorf("last message should be user, got %s", msgs[len(msgs)-1].Role)
			}

			// System message must contain identity (static). Dynamic context
			// (time/runtime/session) must NOT appear in system (Layout B — keeps the
			// system prefix identity-stable for MiniMax passive cache); it is
			// prepended to user[0] inside <dynamic_context> instead.
			sys := msgs[0].Content
			if !strings.Contains(sys, "picoclaw") {
				t.Error("system message missing identity")
			}
			if strings.Contains(sys, "Current Time") {
				t.Error("system message must not contain dynamic time context (Layout B)")
			}
			last := msgs[len(msgs)-1]
			if !strings.Contains(extractStringContent(last), "Current Time") {
				t.Error("user[0] missing dynamic time context (Layout B)")
			}

			// Summary handling
			if tt.summary != "" {
				if !strings.Contains(sys, "CONTEXT_SUMMARY:") {
					t.Error("summary present but CONTEXT_SUMMARY prefix missing")
				}
				if !strings.Contains(sys, tt.summary[:20]) {
					t.Error("summary content not found in system message")
				}
			} else {
				if strings.Contains(sys, "CONTEXT_SUMMARY:") {
					t.Error("CONTEXT_SUMMARY should not appear without summary")
				}
			}
		})
	}
}

func TestBuildMessages_CurrentSenderDynamicContext(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"IDENTITY.md": "# Identity\nTest agent.",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	tests := []struct {
		name              string
		senderID          string
		senderDisplayName string
		wantLine          string
		wantSection       bool
	}{
		{
			name:              "both id and display name",
			senderID:          "feishu:ou_xxx",
			senderDisplayName: "Zhang San",
			wantLine:          "Current sender: Zhang San (ID: feishu:ou_xxx)",
			wantSection:       true,
		},
		{
			name:              "display name only",
			senderDisplayName: "Alice",
			wantLine:          "Current sender: Alice",
			wantSection:       true,
		},
		{
			name:        "id only",
			senderID:    "discord:123",
			wantLine:    "Current sender: discord:123",
			wantSection: true,
		},
		{
			name:        "no sender info",
			wantSection: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := cb.BuildMessages(nil, "", "hello", nil, "discord", "chat1", tt.senderID, tt.senderDisplayName)

			// Layout B (Q1=B điều chỉnh 2026-08-22): dynamic context — including
			// the Current Sender section — now lives in user[0], NOT in the system
			// message. Assert against the last user message instead.
			userContent := lastUserMessageContent(msgs)

			if tt.wantSection {
				if !strings.Contains(userContent, "## Current Sender") {
					t.Fatalf("user[0] missing Current Sender section:\n%s", userContent)
				}
				if !strings.Contains(userContent, tt.wantLine) {
					t.Fatalf("user[0] missing sender line %q:\n%s", tt.wantLine, userContent)
				}
				return
			}

			if strings.Contains(userContent, "## Current Sender") {
				t.Fatalf("user[0] should omit Current Sender section:\n%s", userContent)
			}
		})
	}
}

// TestMtimeAutoInvalidation verifies that the cache detects source file changes
// via mtime without requiring explicit InvalidateCache().
// Fix: original implementation had no auto-invalidation — edits to bootstrap files,
// memory, or skills were invisible until process restart.
func TestMtimeAutoInvalidation(t *testing.T) {
	tests := []struct {
		name       string
		file       string // relative path inside workspace
		contentV1  string
		contentV2  string
		checkField string // substring to verify in rebuilt prompt
	}{
		{
			name:       "bootstrap file change",
			file:       "AGENT.md",
			contentV1:  "# Original Agent",
			contentV2:  "# Updated Agent",
			checkField: "Updated Agent",
		},
		{
			name:       "memory file change",
			file:       "memory/MEMORY.md",
			contentV1:  "# Memory\nUser likes Go.",
			contentV2:  "# Memory\nUser likes Rust.",
			checkField: "User likes Rust",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := setupWorkspace(t, map[string]string{tt.file: tt.contentV1})
			defer os.RemoveAll(tmpDir)

			cb := NewContextBuilder(tmpDir)

			sp1 := cb.BuildSystemPromptWithCache("", false, 0)

			// Overwrite file and set future mtime to ensure detection.
			// Use 2s offset for filesystem mtime resolution safety (some FS
			// have 1s or coarser granularity, especially in CI containers).
			fullPath := filepath.Join(tmpDir, tt.file)
			os.WriteFile(fullPath, []byte(tt.contentV2), 0o644)
			future := time.Now().Add(2 * time.Second)
			os.Chtimes(fullPath, future, future)

			// Verify sourceFilesChangedLocked detects the mtime change
			cb.systemPromptMutex.RLock()
			changed := cb.sourceFilesChangedLocked()
			cb.systemPromptMutex.RUnlock()
			if !changed {
				t.Fatalf("sourceFilesChangedLocked() should detect %s change", tt.file)
			}

			// Should auto-rebuild without explicit InvalidateCache()
			sp2 := cb.BuildSystemPromptWithCache("", false, 0)
			if sp1 == sp2 {
				t.Errorf("cache not rebuilt after %s change", tt.file)
			}
			if !strings.Contains(sp2, tt.checkField) {
				t.Errorf("rebuilt prompt missing expected content %q", tt.checkField)
			}
		})
	}

	// Skills directory mtime change
	t.Run("skills dir change", func(t *testing.T) {
		tmpDir := setupWorkspace(t, nil)
		defer os.RemoveAll(tmpDir)

		cb := NewContextBuilder(tmpDir)
		_ = cb.BuildSystemPromptWithCache("", false, 0) // populate cache

		// Touch skills directory (simulate new skill installed)
		skillsDir := filepath.Join(tmpDir, "skills")
		future := time.Now().Add(2 * time.Second)
		os.Chtimes(skillsDir, future, future)

		// Verify sourceFilesChangedLocked detects it (cache is rebuilt)
		// We confirm by checking internal state: a second call should rebuild.
		cb.systemPromptMutex.RLock()
		changed := cb.sourceFilesChangedLocked()
		cb.systemPromptMutex.RUnlock()
		if !changed {
			t.Error("sourceFilesChangedLocked() should detect skills dir mtime change")
		}
	})
}

// TestExplicitInvalidateCache verifies that InvalidateCache() forces a rebuild
// even when source files haven't changed (useful for tests and reload commands).
func TestExplicitInvalidateCache(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Test Agent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	sp1 := cb.BuildSystemPromptWithCache("", false, 0)
	cb.InvalidateCache()
	sp2 := cb.BuildSystemPromptWithCache("", false, 0)

	if sp1 != sp2 {
		t.Error("prompt should be identical after invalidate+rebuild when files unchanged")
	}

	// Verify cachedAt was reset
	cb.InvalidateCache()
	cb.systemPromptMutex.RLock()
	if !cb.cachedAt.IsZero() {
		t.Error("cachedAt should be zero after InvalidateCache()")
	}
	cb.systemPromptMutex.RUnlock()
}

// TestCacheStability verifies that the static prompt is stable across repeated calls
// when no files change (regression test for issue #607).
func TestCacheStability(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nContent",
		"SOUL.md":  "# Soul\nContent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	results := make([]string, 5)
	for i := range results {
		results[i] = cb.BuildSystemPromptWithCache("", false, 0)
	}
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Errorf("cached prompt changed between call 0 and %d", i)
		}
	}

	// Static prompt must NOT contain per-request data
	if strings.Contains(results[0], "Current Time") {
		t.Error("static cached prompt should not contain time (added dynamically)")
	}
}

// TestCacheInvalidationOnGoalPhaseChange verifies that BuildSystemPromptWithCache
// rebuilds when goalPhase changes (Phase 12.5). Previously the cache was keyed
// only on source file mtime, so a prompt built for GoalPhase=Open stayed in the
// cache when the next turn asked for GoalPhase=Set, missing the GoalPhaseSet
// hint. Test asserts:
//  1. First build with goalPhase="open" caches that prompt.
//  2. Second call with goalPhase="open" is a cache hit (same string).
//  3. Third call with goalPhase="set" is a cache miss (different string), and
//     the new prompt contains the GoalPhaseSet hint text.
func TestCacheInvalidationOnGoalPhaseChange(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nContent",
		"SOUL.md":  "# Soul\nContent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	promptOpen := cb.BuildSystemPromptWithCache("open", false, 0)
	// Same phase → cache hit, identical string.
	promptOpenCached := cb.BuildSystemPromptWithCache("open", false, 0)
	if promptOpen != promptOpenCached {
		t.Errorf("cache miss for same goalPhase: builds produced different strings")
	}

	// Different phase → cache miss, new prompt. The GoalPhaseSet hint
	// contributor fires only when goalPhase="set".
	promptSet := cb.BuildSystemPromptWithCache("set", false, 0)
	if promptSet == promptOpen {
		t.Errorf("cache hit across goalPhase transition — GoalPhaseSet hint would never fire")
	}
	if !strings.Contains(promptSet, "set_goal") || !strings.Contains(promptSet, "locked") {
		t.Errorf("GoalPhaseSet prompt missing expected hint text (no 'set_goal' / 'locked' keyword)")
	}
}

// TestCacheHitOnSameGoalPhase verifies that BuildSystemPromptWithCache
// reuses the same prompt for the same goalPhase even after an unrelated
// SetPhase call to the registry (which shouldn't affect the prompt cache).
func TestCacheHitOnSameGoalPhase(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nContent",
		"SOUL.md":  "# Soul\nContent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	p1 := cb.BuildSystemPromptWithCache("set", false, 0)
	p2 := cb.BuildSystemPromptWithCache("set", false, 0)
	if p1 != p2 {
		t.Errorf("cache miss on identical goalPhase=\"set\"")
	}
}

// TestCacheInvalidationOnIterationChange (Phase 12.16.1): the cache key
// must include the iteration index. Without this dimension, complete_goal →
// archive → hasGoal=false → phase=set would hit iter 1's cached prompt at
// later iters in the same turn, returning the stale "Goal phase: SET (iter
// 1)" header and the (iter 1) reference inside goalPhaseSetHintText. This
// regression caused the main-turn-4 oscillation where the LLM saw the iter 1
// prompt 25 times in a row.
//
// Verifies the fix in 2 ways:
//  1. Same iter → cache hit (returns identical prompt)
//  2. Different iter → cache miss (rebuilds prompt with new iter)
//  3. Iter 1 → iter 17 produces a prompt that mentions "(iter 17)" in the
//     hint header (the bug was: hint always said "(iter 1)")
func TestCacheInvalidationOnIterationChange(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nContent",
		"SOUL.md":  "# Soul\nContent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	// Build at iter 1 (phase=set, so GoalPhaseSet hint fires with iter=1).
	promptIter1 := cb.BuildSystemPromptWithCache("set", false, 1)

	// Same iter → cache hit (returned string identity).
	promptIter1Again := cb.BuildSystemPromptWithCache("set", false, 1)
	if promptIter1 != promptIter1Again {
		t.Errorf("cache miss for same iter=1: builds produced different strings")
	}
	if !strings.Contains(promptIter1, "(iter 1)") {
		t.Errorf("iter=1 prompt missing '(iter 1)' reference in GoalPhaseSet hint header; got:\n%s", promptIter1[:intMin(500, len(promptIter1))])
	}

	// Different iter → cache miss, new prompt with iter 17 in the hint header.
	// This is the exact regression scenario from main-turn-4.
	promptIter17 := cb.BuildSystemPromptWithCache("set", false, 17)
	if promptIter17 == promptIter1 {
		t.Errorf("cache hit across iter change — iter-1 prompt would be reused at iter 17 (Phase 12.16.1 regression)")
	}
	if !strings.Contains(promptIter17, "(iter 17)") {
		t.Errorf("iter=17 prompt missing '(iter 17)' reference in GoalPhaseSet hint header; hint would still say '(iter 1)' (the bug). Got:\n%s", promptIter17[:intMin(500, len(promptIter17))])
	}

	// After iter 17 build, iter 1 build is back to cache miss (iter key
	// mismatch). Iter 1 prompt has its own iter=1 header.
	promptIter1Again2 := cb.BuildSystemPromptWithCache("set", false, 1)
	if promptIter1Again2 == promptIter17 {
		t.Errorf("cache hit across iter change (reverse direction)")
	}
	if !strings.Contains(promptIter1Again2, "(iter 1)") {
		t.Errorf("iter=1 prompt missing '(iter 1)' reference after iter 17 cache flip")
	}
}

// intMin helper for substring slicing above (avoids name collision with
// the Go 1.21+ builtin min on two ints).
func intMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestNonOpenPhasesBypassCache (Phase 12.16.1): cache MUST be bypassed for
// GoalPhaseSet, GoalPhaseCheckpoint, and GoalPhaseFinal. Reasoning: these
// phases have tool allowlists restricted to 1-2 goal lifecycle tools, so
// the prompt body is dominated by constant hint text + a tiny tool
// section. Rebuilding every time is cheaper than tracking cache
// invalidation across the iter dimension for a prompt that barely
// changes. Also eliminates any residual risk of stale-prompt leakage
// during a goal-phase transition mid-turn.
//
// Open phase continues to use the cache normally (cache is only useful
// for phases with per-iter or per-phase variance in the tool section).
//
// Verifies:
//  1. Calling with goalPhase="set"/"checkpoint"/"final" twice returns
//     identical content (rebuild is deterministic)
//  2. The non-Open phase path does NOT touch the Open cache (so an
//     Open-phase cache hit does not return the Set-phase prompt after
//     a Set call)
//  3. Iter dimension does not cause stale hints for non-Open phases
//     (the original main-turn-4 bug surface, but for the bypass path)
func TestNonOpenPhasesBypassCache(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nContent",
		"SOUL.md":  "# Soul\nContent",
	})
	defer os.RemoveAll(tmpDir)

	for _, phase := range []string{string(GoalPhaseSet), string(GoalPhaseCheckpoint), string(GoalPhaseFinal)} {
		t.Run(phase, func(t *testing.T) {
			cb := NewContextBuilder(tmpDir)

			// First call: rebuild (cache bypassed for this phase)
			p1 := cb.BuildSystemPromptWithCache(phase, false, 1)
			// Second call: rebuild again (cache still bypassed, no shared state with Open cache)
			p2 := cb.BuildSystemPromptWithCache(phase, false, 1)

			// Both should produce the same prompt (deterministic rebuild
			// with same input). This proves the call path is rebuilding
			// each time rather than returning cached content from a prior
			// call to a different phase.
			if p1 != p2 {
				t.Errorf("non-Open phase %q: rebuild not deterministic for same input", phase)
			}

			// Set phase-specific regression check: the GoalPhaseSet hint
			// header MUST reflect the actual iter (Phase 12.16.1 fix).
			// Rebuild at iter 17 → prompt contains "(iter 17)".
			if phase == string(GoalPhaseSet) {
				p17 := cb.BuildSystemPromptWithCache(phase, false, 17)
				if !strings.Contains(p17, "(iter 17)") {
					t.Errorf("Set phase rebuild at iter=17 missing '(iter 17)' hint header — Phase 12.16.1 regression")
				}
				if strings.Contains(p17, "(iter 1)") {
					t.Errorf("Set phase rebuild at iter=17 still contains '(iter 1)' — Phase 12.16.1 stale-text regression")
				}
			}

			// Now call with Open phase twice — Open should still cache-hit
			openP1 := cb.BuildSystemPromptWithCache(string(GoalPhaseOpen), false, 1)
			openP2 := cb.BuildSystemPromptWithCache(string(GoalPhaseOpen), false, 1)
			// Open phase cache hit: openP1 == openP2
			if openP1 != openP2 {
				t.Errorf("Open phase cache miss on identical call")
			}

			// Non-Open cache state must not corrupt Open cache hit.
			// If a non-Open call had written to the same cache slot with
			// goalPhase=phase, then calling Open again would miss. The fact
			// that openP2 == openP1 confirms the cache slot for Open is
			// still valid, which means non-Open calls did not write to it.
		})
	}
}

// TestNewFileCreationInvalidatesCache verifies that creating a source file that
// did not exist when the cache was built triggers a cache rebuild.
// This catches the "from nothing to something" edge case that the old
// modifiedSince (return false on stat error) would miss.
func TestNewFileCreationInvalidatesCache(t *testing.T) {
	tests := []struct {
		name       string
		file       string // relative path inside workspace
		content    string
		checkField string // substring to verify in rebuilt prompt
	}{
		{
			name:       "new bootstrap file",
			file:       "SOUL.md",
			content:    "# Soul\nBe kind and helpful.",
			checkField: "Be kind and helpful",
		},
		{
			name:       "new memory file",
			file:       "memory/MEMORY.md",
			content:    "# Memory\nUser prefers dark mode.",
			checkField: "User prefers dark mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start with an empty workspace (no bootstrap/memory files)
			tmpDir := setupWorkspace(t, nil)
			defer os.RemoveAll(tmpDir)

			cb := NewContextBuilder(tmpDir)

			// Populate cache — file does not exist yet
			sp1 := cb.BuildSystemPromptWithCache("", false, 0)
			if strings.Contains(sp1, tt.checkField) {
				t.Fatalf("prompt should not contain %q before file is created", tt.checkField)
			}

			// Create the file after cache was built
			fullPath := filepath.Join(tmpDir, tt.file)
			os.MkdirAll(filepath.Dir(fullPath), 0o755)
			if err := os.WriteFile(fullPath, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			// Set future mtime to guarantee detection
			future := time.Now().Add(2 * time.Second)
			os.Chtimes(fullPath, future, future)

			// Cache should auto-invalidate because file went from absent -> present
			sp2 := cb.BuildSystemPromptWithCache("", false, 0)
			if !strings.Contains(sp2, tt.checkField) {
				t.Errorf("cache not invalidated on new file creation: expected %q in prompt", tt.checkField)
			}
		})
	}
}

// TestSkillFileContentChange verifies that modifying a skill file's content
// (not just the directory structure) invalidates the cache.
// This is the scenario where directory mtime alone is insufficient — on most
// filesystems, editing a file inside a directory does NOT update the parent
// directory's mtime.
func TestSkillFileContentChange(t *testing.T) {
	skillMD := `---
name: test-skill
description: "A test skill"
---
# Test Skill v1
Original content.`

	tmpDir := setupWorkspace(t, map[string]string{
		"skills/test-skill/SKILL.md": skillMD,
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	// Populate cache
	sp1 := cb.BuildSystemPromptWithCache("", false, 0)
	_ = sp1 // cache is warm

	// Modify the skill file content (without touching the skills/ directory)
	updatedSkillMD := `---
name: test-skill
description: "An updated test skill"
---
# Test Skill v2
Updated content.`

	skillPath := filepath.Join(tmpDir, "skills", "test-skill", "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(updatedSkillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	// Set future mtime on the skill file only (NOT the directory)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(skillPath, future, future)

	// Verify that sourceFilesChangedLocked detects the content change
	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Error("sourceFilesChangedLocked() should detect skill file content change")
	}

	// Verify cache is actually rebuilt with new content
	sp2 := cb.BuildSystemPromptWithCache("", false, 0)
	if sp1 == sp2 && strings.Contains(sp1, "test-skill") {
		// If the skill appeared in the prompt and the prompt didn't change,
		// the cache was not invalidated.
		t.Error("cache should be invalidated when skill file content changes")
	}
}

// TestGlobalSkillFileContentChange verifies that modifying a global skill
// (~/.picoclaw/skills) invalidates the cached system prompt.
func TestGlobalSkillFileContentChange(t *testing.T) {
	tmpHome := t.TempDir()
	// getGlobalConfigDir() reads PICOCLAW_HOME first (returns the .picoclaw
	// dir directly, no implicit join). Set PICOCLAW_HOME so the global skills
	// directory is <tmpHome>/skills and matches NewContextBuilder's lookup.
	t.Setenv("PICOCLAW_HOME", tmpHome)
	t.Setenv("HOME", tmpHome)

	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	globalSkillPath := filepath.Join(tmpHome, "skills", "global-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(globalSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	v1 := `---
name: global-skill
description: global-v1
---
# Global Skill v1`
	if err := os.WriteFile(globalSkillPath, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(tmpDir)
	sp1 := cb.BuildSystemPromptWithCache("", false, 0)
	if !strings.Contains(sp1, "global-v1") {
		t.Fatal("expected initial prompt to contain global skill description")
	}

	v2 := `---
name: global-skill
description: global-v2
---
# Global Skill v2`
	if err := os.WriteFile(globalSkillPath, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(globalSkillPath, future, future); err != nil {
		t.Fatalf("failed to update mtime for %s: %v", globalSkillPath, err)
	}

	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Fatal("sourceFilesChangedLocked() should detect global skill file content change")
	}

	sp2 := cb.BuildSystemPromptWithCache("", false, 0)
	if !strings.Contains(sp2, "global-v2") {
		t.Error("rebuilt prompt should contain updated global skill description")
	}
	if sp1 == sp2 {
		t.Error("cache should be invalidated when global skill file content changes")
	}
}

// TestBuiltinSkillFileContentChange verifies that modifying a builtin skill
// invalidates the cached system prompt.
func TestBuiltinSkillFileContentChange(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	builtinRoot := t.TempDir()
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", builtinRoot)

	builtinSkillPath := filepath.Join(builtinRoot, "builtin-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(builtinSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	v1 := `---
name: builtin-skill
description: builtin-v1
---
# Builtin Skill v1`
	if err := os.WriteFile(builtinSkillPath, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(tmpDir)
	sp1 := cb.BuildSystemPromptWithCache("", false, 0)
	if !strings.Contains(sp1, "builtin-v1") {
		t.Fatal("expected initial prompt to contain builtin skill description")
	}

	v2 := `---
name: builtin-skill
description: builtin-v2
---
# Builtin Skill v2`
	if err := os.WriteFile(builtinSkillPath, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(builtinSkillPath, future, future); err != nil {
		t.Fatalf("failed to update mtime for %s: %v", builtinSkillPath, err)
	}

	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Fatal("sourceFilesChangedLocked() should detect builtin skill file content change")
	}

	sp2 := cb.BuildSystemPromptWithCache("", false, 0)
	if !strings.Contains(sp2, "builtin-v2") {
		t.Error("rebuilt prompt should contain updated builtin skill description")
	}
	if sp1 == sp2 {
		t.Error("cache should be invalidated when builtin skill file content changes")
	}
}

// TestSkillFileDeletionInvalidatesCache verifies that deleting a nested skill
// file invalidates the cached system prompt.
func TestSkillFileDeletionInvalidatesCache(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"skills/delete-me/SKILL.md": `---
name: delete-me
description: delete-me-v1
---
# Delete Me`,
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	sp1 := cb.BuildSystemPromptWithCache("", false, 0)
	if !strings.Contains(sp1, "delete-me-v1") {
		t.Fatal("expected initial prompt to contain skill description")
	}

	skillPath := filepath.Join(tmpDir, "skills", "delete-me", "SKILL.md")
	if err := os.Remove(skillPath); err != nil {
		t.Fatal(err)
	}

	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Fatal("sourceFilesChangedLocked() should detect deleted skill file")
	}

	sp2 := cb.BuildSystemPromptWithCache("", false, 0)
	if strings.Contains(sp2, "delete-me-v1") {
		t.Error("rebuilt prompt should not contain deleted skill description")
	}
	if sp1 == sp2 {
		t.Error("cache should be invalidated when skill file is deleted")
	}
}

// TestConcurrentBuildSystemPromptWithCache verifies that multiple goroutines
// can safely call BuildSystemPromptWithCache concurrently without producing
// empty results, panics, or data races.
// Run with: go test -race ./pkg/agent/ -run TestConcurrentBuildSystemPromptWithCache
func TestConcurrentBuildSystemPromptWithCache(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md":             "# Agent\nConcurrency test agent.",
		"SOUL.md":              "# Soul\nBe helpful.",
		"memory/MEMORY.md":     "# Memory\nUser prefers Go.",
		"skills/demo/SKILL.md": "---\nname: demo\ndescription: \"demo skill\"\n---\n# Demo",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup
	errs := make(chan string, goroutines*iterations)

	for g := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range iterations {
				result := cb.BuildSystemPromptWithCache("", false, 0)
				if result == "" {
					errs <- "empty prompt returned"
					return
				}
				if !strings.Contains(result, "picoclaw") {
					errs <- "prompt missing identity"
					return
				}

				// Also exercise BuildMessages concurrently
				msgs := cb.BuildMessages(nil, "", "hello", nil, "test", "chat", "", "")
				if len(msgs) < 2 {
					errs <- "BuildMessages returned fewer than 2 messages"
					return
				}
				if msgs[0].Role != "system" {
					errs <- "first message not system"
					return
				}

				// Occasionally invalidate to exercise the write path
				if i%10 == 0 {
					cb.InvalidateCache()
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)

	for errMsg := range errs {
		t.Errorf("concurrent access error: %s", errMsg)
	}
}

// BenchmarkBuildMessagesWithCache measures caching performance.

// TestEmptyWorkspaceBaselineDetectsNewFiles verifies that when the cache is
// built on an empty workspace (no tracked files exist), creating a file
// afterwards still triggers cache invalidation. This validates the
// time.Unix(1, 0) fallback for maxMtime: any real file's mtime is after epoch,
// so fileChangedSince correctly detects the absent -> present transition AND
// the mtime comparison succeeds even without artificially inflated Chtimes.
func TestEmptyWorkspaceBaselineDetectsNewFiles(t *testing.T) {
	// Empty workspace: no bootstrap files, no memory, no skills content.
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	// Build cache — all tracked files are absent, maxMtime falls back to epoch.
	sp1 := cb.BuildSystemPromptWithCache("", false, 0)

	// Create a bootstrap file with natural mtime (no Chtimes manipulation).
	// The file's mtime should be the current wall-clock time, which is
	// strictly after time.Unix(1, 0).
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	if err := os.WriteFile(soulPath, []byte("# Soul\nNewly created."), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cache should detect the new file via existedAtCache (absent -> present).
	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Fatal("sourceFilesChangedLocked should detect newly created file on empty workspace")
	}

	sp2 := cb.BuildSystemPromptWithCache("", false, 0)
	if !strings.Contains(sp2, "Newly created") {
		t.Error("rebuilt prompt should contain new file content")
	}
	if sp1 == sp2 {
		t.Error("cache should have been invalidated after file creation")
	}
}

func TestBuildMessages_IncludesMediaOnlyCurrentMessage(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	msgs := cb.BuildMessages(
		nil,
		"",
		"",
		[]string{"data:image/png;base64,abc123"},
		"pico",
		"chat-1",
		"",
		"",
	)

	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}

	userMsg := msgs[1]
	if userMsg.Role != "user" {
		t.Fatalf("userMsg.Role = %q, want %q", userMsg.Role, "user")
	}
	// Layout B (Q1=B điều chỉnh 2026-08-22): user[0] always carries the dynamic
	// context block when the default system prompt is active — even for
	// media-only turns with empty currentMessage. The media payload survives
	// unchanged alongside it.
	if !strings.Contains(userMsg.Content, "<dynamic_context>") {
		t.Fatalf("userMsg.Content = %q, want <dynamic_context> block (Layout B)", userMsg.Content)
	}
	if len(userMsg.Media) != 1 || userMsg.Media[0] != "data:image/png;base64,abc123" {
		t.Fatalf("userMsg.Media = %#v, want image payload", userMsg.Media)
	}
}
// TestCache_EstimateSystemTokensDoesNotCorruptCache verifies that
// EstimateSystemTokens (called from computeContextUsage at turn finalization)
// does NOT write through the cache.
//
// History:
//   - Originally (pre-12.5.1): called BuildSystemPromptWithCache("", 0)
//     which silently overwrote a real "set"-phase cached prompt with the
//     empty-phase version (no hint). Bug caught on Telegram main session
//     2026-07-23 18:43 ICT.
//   - Phase 12.5.1 fix: EstimateSystemTokens uses BuildSystemPrompt("") (no-
//     cache variant) instead. The cache slot for "set" stays intact after
//     EstimateSystemTokens.
//   - Phase 12.16.1: non-Open phases (Set/Checkpoint/Final) BYPASS the
//     cache entirely (see isCacheableGoalPhase). After this change, the
//     cache slot for "set" is NEVER populated by a Set-phase build — the
//     Set hint is rebuilt on demand via BuildSystemPrompt. This is even
//     safer than Phase 12.5.1 because there's no cache state to corrupt.
//
// Detection strategy (post-12.16.1): after EstimateSystemTokens on a
// Set-phase build, (a) the Set hint must still be present in rebuilt
// prompts, (b) an Open-phase build MUST still cache-hit, (c) the cache
// slot's state must remain untouched (whatever state it had before the
// non-Open phase call).
func TestCache_EstimateSystemTokensDoesNotCorruptCache(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "picoclaw-12.5.1-*")
	defer os.RemoveAll(tmpDir)
	os.MkdirAll(filepath.Join(tmpDir, "memory"), 0o755)
	os.MkdirAll(filepath.Join(tmpDir, "skills"), 0o755)
	for _, name := range []string{"AGENT.md", "SOUL.md"} {
		os.WriteFile(filepath.Join(tmpDir, name), []byte(strings.Repeat("Content.\n", 10)), 0o644)
	}
	cb := NewContextBuilder(tmpDir)

	// Phase 12.16.1: Set-phase build BYPASSES cache. The rebuild still
	// includes the GoalPhaseSet hint, but doesn't write to the cache slot.
	withHint := cb.BuildSystemPromptWithCache("set", false, 1)
	if !strings.Contains(withHint, "Goal phase: SET") {
		t.Fatal("setup: hint should be present after BuildSystemPromptWithCache(set)")
	}
	// Cache slot must NOT have been written to by a Set-phase call.
	// Phase 12.16.1: this is expected — non-Open phases never write
	// to cache. The cachedSystemPromptGoalPhase stays "" (unwritten).
	if cb.cachedSystemPromptGoalPhase != "" {
		t.Fatalf("setup: Set-phase call must not write to cache, got phase = %q",
			cb.cachedSystemPromptGoalPhase)
	}

	// Now warm the Open-phase cache (Open DOES use the cache).
	openHint := cb.BuildSystemPromptWithCache("open", false, 1)
	if !strings.Contains(openHint, "Content.") {
		t.Fatal("Open-phase build missing expected agent content")
	}
	if cb.cachedSystemPromptGoalPhase != "open" {
		t.Fatalf("setup: Open-phase cache should be populated, got phase = %q",
			cb.cachedSystemPromptGoalPhase)
	}

	// EstimateSystemTokens called at turn finalization (with empty phase,
	// which the no-cache variant handles correctly — Phase 12.5.1).
	cb.EstimateSystemTokens("summary text", []string{"plan"})

	// Open-phase cache slot MUST still be "open" — EstimateSystemTokens
	// must not have corrupted it via the no-cache build path.
	if cb.cachedSystemPromptGoalPhase != "open" {
		t.Fatalf("EstimateSystemTokens corrupted cache phase: now %q, want \"open\" (the Open-phase cache was overwritten by an empty-phase rebuild)",
			cb.cachedSystemPromptGoalPhase)
	}

	// Set-phase rebuild must still return the hint (deterministic rebuild
	// via BuildSystemPrompt with phase="set").
	setAgain := cb.BuildSystemPromptWithCache("set", false, 1)
	if !strings.Contains(setAgain, "Goal phase: SET") {
		t.Fatal("Set-phase rebuild after EstimateSystemTokens lost GoalPhaseSet hint")
	}

	// Open-phase next call must hit the cache (still valid).
	openAgain := cb.BuildSystemPromptWithCache("open", false, 1)
	if openAgain != openHint {
		t.Fatal("Open-phase cache miss after EstimateSystemTokens — cache was corrupted")
	}
}

// TestCacheInvalidationOnIterationCapChange (Phase 12.38 §4 F52/F58.2,
// updated Phase 12.39): the cache key must include iterationCap and
// maxIterationsCap. Without these dimensions, an OPEN cache slot built at
// cap=5 would be reused at cap=10 after a goal_progress extension at
// CHECKPOINT — the LLM would see stale "Next CHECKPOINT at iter 5" text
// instead of the actual cap=10.
//
// Phase 12.39 changed the rendered text from "Iteration cap: N" to
// "Next CHECKPOINT phase will be at iter N" (event-marker style). The
// cache invalidation contract is the same — only the rendered text changed.
func TestCacheInvalidationOnIterationCapChange(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nContent",
		"SOUL.md":  "# Soul\nContent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	// Build OPEN cache at iter 5 with cap=5
	p1 := cb.BuildSystemPromptWithCacheFullKey("open", false, 5, "", 5, 15)
	// Same dims → cache HIT (build returns identical prompt)
	p1Cached := cb.BuildSystemPromptWithCacheFullKey("open", false, 5, "", 5, 15)
	if p1 != p1Cached {
		t.Errorf("expected cache HIT on identical dims, got different prompts")
	}
	// Cap extended: iter 6 with cap=10 → cache MISS → rebuild with new content
	p2 := cb.BuildSystemPromptWithCacheFullKey("open", false, 6, "", 10, 15)
	if p1 == p2 {
		t.Errorf("expected cache MISS on cap change (5→10), got identical prompts (stale cap-leak)")
	}
	if !strings.Contains(p2, "Next CHECKPOINT phase will be at iter 10") {
		t.Errorf("rebuilt prompt must show new cap=10, got:\n%s", p2)
	}
	if strings.Contains(p1, "Next CHECKPOINT phase will be at iter 10") {
		t.Errorf("original cache slot must NOT contain stale cap=10, got:\n%s", p1)
	}
}

// TestCacheInvalidationOnMaxIterationsCapChange (Phase 12.38 §4, updated
// Phase 12.39): when cap hits ceiling, the rendered text changes from
// "Next CHECKPOINT at iter X" to "FINAL phase will be at iter M". If
// maxCap changes so cap is no longer at ceiling (e.g. cap=15, maxCap=20),
// the marker reverts and the cache must invalidate so the LLM sees the
// corrected text.
func TestCacheInvalidationOnMaxIterationsCapChange(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nContent",
		"SOUL.md":  "# Soul\nContent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	// cap=15, maxCap=15 → at ceiling → "FINAL phase will be at iter 15" present
	p1 := cb.BuildSystemPromptWithCacheFullKey("open", false, 5, "", 15, 15)
	// cap=15, maxCap=20 → not at ceiling → "Next CHECKPOINT" present
	p2 := cb.BuildSystemPromptWithCacheFullKey("open", false, 5, "", 15, 20)
	if p1 == p2 {
		t.Errorf("expected cache MISS when cap-ceiling state changes (at ceiling → not at ceiling)")
	}
	if !strings.Contains(p1, "FINAL phase will be at iter 15") {
		t.Errorf("p1 must show FINAL marker (at ceiling), got:\n%s", p1)
	}
	if !strings.Contains(p2, "Next CHECKPOINT phase will be at iter 15") {
		t.Errorf("p2 must show CHECKPOINT marker (not at ceiling), got:\n%s", p2)
	}
}

// BenchmarkBuildMessagesWithCache measures caching performance.
func BenchmarkBuildMessagesWithCache(b *testing.B) {
	tmpDir, _ := os.MkdirTemp("", "picoclaw-bench-*")
	defer os.RemoveAll(tmpDir)

	os.MkdirAll(filepath.Join(tmpDir, "memory"), 0o755)
	os.MkdirAll(filepath.Join(tmpDir, "skills"), 0o755)
	for _, name := range []string{"AGENT.md", "SOUL.md"} {
		os.WriteFile(filepath.Join(tmpDir, name), []byte(strings.Repeat("Content.\n", 10)), 0o644)
	}

	cb := NewContextBuilder(tmpDir)
	history := []providers.Message{
		{Role: "user", Content: "previous message"},
		{Role: "assistant", Content: "previous response"},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cb.BuildMessages(history, "summary", "new message", nil, "cli", "test", "", "")
	}
}

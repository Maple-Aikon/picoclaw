package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMain isolates all prompt-history writes + Graphiti queue writes from
// production paths.
//
// Phase 12.45 F1: the Go-side recall writer (logReplayPromptBlock) targets
// the same prompt_history.log as the JS hook. Any agent test that triggers
// a RecallLLM call (recovery paths, wire tests) would otherwise append
// [RECALL] blocks to the REAL production log. Redirecting at TestMain level
// covers every test — including fixtures that don't go through
// setupRecallTestTurnState (e.g. phase12_42 wire setups).
//
// Phase 12.70: defense-in-depth against the same leak class hitting
// apps/graphiti-mcp/queue/episodes.db (production). The seahorse graphiti
// singleton resolves its DB path via GRAPHITI_QUEUE_DB env var
// (graphiti.go:111); defaulting to ~/.picoclaw/workspace/apps/graphiti-mcp/queue/episodes.db.
// Any agent test that triggers compact→enqueueGraphitiEpisode would otherwise
// append a "test-..." session_key row to the LIVE production DB. Setting
// GRAPHITI_QUEUE_DB here means: (a) tests never touch prod DB even if a
// fixture forgets to override GraphitiQueuePath; (b) cleanup auto via
// os.RemoveAll(tmpDir) after m.Run().
func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "picoclaw-agent-test-*")
	if err != nil {
		panic("TestMain: MkdirTemp: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)
	os.Setenv("PICOCLAW_HOOK_LOG_FILE", filepath.Join(tmpDir, "prompt.log"))
	os.Setenv("GRAPHITI_QUEUE_DB", filepath.Join(tmpDir, "episodes.db"))

	// Phase 12.55: shrink the recovery backoff schedule for tests. Wire
	// tests that drive handleGoalRecovery / retryExecuteToolChain would
	// otherwise sleep 3s+6s+10s per exhaustion. Tests that specifically
	// measure delay (T8) override the var locally and restore it.
	oldDelays := recoveryBackoffDelays
	recoveryBackoffDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	defer func() { recoveryBackoffDelays = oldDelays }()

	code := m.Run()
	os.Exit(code)
}

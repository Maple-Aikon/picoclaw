package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMain isolates prompt-history writes from production paths.
//
// Phase 12.45 F1: the Go-side recall writer (logReplayPromptBlock) targets
// the same prompt_history.log as the JS hook. Any agent test that triggers
// a RecallLLM call (recovery paths, wire tests) would otherwise append
// [RECALL] blocks to the REAL production log. Redirecting at TestMain level
// covers every test — including fixtures that don't go through
// setupRecallTestTurnState (e.g. phase12_42 wire setups).
//
// Phase 12.70 (REMOVED 2026-09-15 in feat/remove-sqlite-queue): the prior
// version of this file also set GRAPHITI_QUEUE_DB to a per-process tmp
// file to defense-in-depth against the SQLite queue leak class. That
// path has been removed from pkg/seahorse/graphiti.go — the HTTP seam
// (janus-graph-daemon /episodes) is now the sole ingestion route, so
// there is no production DB to defend against.
//
// Phase 12.55: shrink the recovery backoff schedule for tests. Wire
// tests that drive handleGoalRecovery / retryExecuteToolChain would
// otherwise sleep 3s+6s+10s per exhaustion. Tests that specifically
// measure delay (T8) override the var locally and restore it.
func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "picoclaw-agent-test-*")
	if err != nil {
		panic("TestMain: MkdirTemp: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)
	os.Setenv("PICOCLAW_HOOK_LOG_FILE", filepath.Join(tmpDir, "prompt.log"))

	oldDelays := recoveryBackoffDelays
	recoveryBackoffDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	defer func() { recoveryBackoffDelays = oldDelays }()

	code := m.Run()
	os.Exit(code)
}

package seahorse

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates the Graphiti queue writes from the production DB.
//
// Defense-in-depth (matches pkg/agent/test_main_test.go Phase 12.70):
// the seahorse graphiti singleton resolves its DB path via the
// GRAPHITI_QUEUE_DB env var (graphiti.go:111), defaulting to the live
// production queue at ~/.picoclaw/workspace/apps/graphiti-mcp/queue/episodes.db.
// Any seahorse test that triggers rememberInGraphiti (via short_compaction,
// summarization, or direct graphiti_test invocations) would otherwise
// append "test-..." session_key rows to the LIVE production DB. Setting
// GRAPHITI_QUEUE_DB here to a per-process tmp file means: (a) cleanup
// auto via os.RemoveAll after m.Run(); (b) fixtures that explicitly
// override GraphitiQueuePath via SetGraphitiConfig still win (higher
// precedence — resolveGraphitiDBPathLocked checks configuredQueuePath
// first before falling through to the env var).
func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "picoclaw-seahorse-test-*")
	if err != nil {
		panic("TestMain: MkdirTemp: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)
	os.Setenv("GRAPHITI_QUEUE_DB", filepath.Join(tmpDir, "episodes.db"))

	code := m.Run()
	os.Exit(code)
}

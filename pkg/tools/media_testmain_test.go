package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates PICOCLAW_MEDIA_DIR to a per-process temp directory so media-store
// tests don't collide with stale /tmp/picoclaw_media owned by another uid. The env override
// is read by media.TempDir() (see pkg/media/tempdir.go).
//
// Affected tests (pre-existing failures from cross-uid /tmp/picoclaw_media ownership):
//   - TestToolRegistry_ExecuteWithContext_ExtractsInlineMediaDataURL
//   - any other test that exercises inline data-URL or media store persistence
//
// Without this, CreateTemp on /tmp/picoclaw_media returns "permission denied" when the
// runtime owner is not the test runner (common in fresh sandboxes or after prior runs).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "picoclaw-media-tools-")
	if err != nil {
		panic("picoclaw media TestMain: MkdirTemp failed: " + err.Error())
	}
	if err := os.Setenv("PICOCLAW_MEDIA_DIR", filepath.Join(dir, "media")); err != nil {
		panic("picoclaw media TestMain: Setenv failed: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
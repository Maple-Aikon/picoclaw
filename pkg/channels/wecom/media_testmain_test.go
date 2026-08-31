package wecom

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain isolates PICOCLAW_MEDIA_DIR to a per-process temp directory so media-store
// tests don't collide with stale /tmp/picoclaw_media owned by another uid.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "picoclaw-media-wecom-")
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

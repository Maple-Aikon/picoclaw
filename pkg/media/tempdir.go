package media

import (
	"os"
	"path/filepath"
)

const TempDirName = "picoclaw_media"

// envMediaDirOverride lets tests redirect the media temp dir to an isolated
// t.TempDir() so they don't fight over perms on the shared /tmp/picoclaw_media
// path (which may be owned by a different uid from a prior runtime session).
// Production callers leave this unset and get the default path.
const envMediaDirOverride = "PICOCLAW_MEDIA_DIR"

// TempDir returns the temporary directory used for downloaded media.
//
// In production this is /tmp/picoclaw_media. Tests can override it by setting
// PICOCLAW_MEDIA_DIR in the environment (typically via t.Setenv together with
// t.TempDir()) to avoid permission collisions with stale runtime-owned dirs.
func TempDir() string {
	if v := os.Getenv(envMediaDirOverride); v != "" {
		return v
	}
	return filepath.Join(os.TempDir(), TempDirName)
}

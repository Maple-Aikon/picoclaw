package tools

// zz_modea_1113_exec_prune_test.go — probe whether the 2026-11-13 lesson
// appended to the live `exec` entry is actually VISIBLE in the auto-load
// digest (2026-11-13).
//
// WHY THIS FILE EXISTS
//
// The lesson was APPENDED to memory/tool_knowledge/exec.md, which is
// ~265 KB — far above the 2 KB cap. A file this size takes the
// headings-only fallback (Regime B), which keeps the HEAD of the file and
// DROPS THE TAIL. An appended lesson at the very end of the file is
// therefore NOT auto-loadable unless the digest happens to reach it.
//
// This probe measures the REAL production path — LoadForEscalation, i.e.
// Read -> renderKnowledgeFile -> digestSections -> extractBody — and
// asserts the lesson's needle is present in the rendered digest.
// It reads only; it never writes.
//
// Run with:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace \
//     go test ./pkg/tools/ -run TestZZExecPruneLessonVisible -count=1 -v

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZZExecPruneLessonVisible(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	path := filepath.Join(store.Root(), "exec.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exec.md: %v", err)
	}
	t.Logf("exec.md on disk: %d bytes (cap %d)", len(raw), KnowledgeSizeCap)

	// THE REAL production path used for auto-load.
	digest := store.LoadForEscalation("exec")
	t.Logf("LoadForEscalation output: %d bytes", len(digest))

	needles := []string{
		"pmc stop",
		"nanobot gateway stop",
		"READ-ONLY",
		"prune_dream_sessions",
		"custom_deny_patterns",
	}

	missing := 0
	for _, n := range needles {
		inSource := strings.Contains(string(raw), n)
		inDigest := strings.Contains(digest, n)
		t.Logf("needle %-24q source=%v digest=%v", n, inSource, inDigest)
		if inSource && !inDigest {
			missing++
		}
	}

	if missing > 0 {
		t.Errorf("%d lesson needle(s) present in source but NOT visible in the auto-load digest", missing)
	}
}

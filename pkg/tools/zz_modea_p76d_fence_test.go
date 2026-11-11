package tools

// zz_modea_p76d_fence_test.go — Mode A p76d (2026-11-11): which files
// are EXPOSED to the extractBody truncation bug?
//
// extractBody strips a leading frontmatter fence. When a file does NOT
// start with the fence, the function falls back to a bare
// strings.Index for the first horizontal rule and DROPS everything
// before it.
//
// writeLocked ALWAYS writes a frontmatter fence (renderKnowledgeFile).
// So a file without the fence on disk was either:
//   (a) hand-written / merged by a script that did not add one, or
//   (b) produced by an older version.
//
// Those are exactly the files where the truncation bites.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const p76dNL = string(rune(10))

func p76dCountH2(s string) int {
	n := 0
	for _, line := range strings.Split(s, p76dNL) {
		if strings.HasPrefix(line, "## ") {
			n++
		}
	}
	return n
}

func TestP76dFenceAudit(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	names, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	noFence := 0
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(store.Root(), name+".md"))
		if err != nil {
			continue
		}
		content := string(raw)
		hasFence := strings.HasPrefix(content, "---")
		real := strings.TrimSpace(content)
		prod := extractBody(content)
		realH2 := p76dCountH2(real)
		prodH2 := p76dCountH2(prod)
		lostH2 := realH2 - prodH2
		if hasFence {
			if lostH2 != 0 {
				fmt.Printf("FENCED_BUT_LOSSING entry=%-46s lostH2=%d%s", name, lostH2, p76dNL)
			}
			continue
		}
		noFence++
		fmt.Printf("NO_FENCE entry=%-46s bytes=%7d realH2=%3d prodH2=%3d lostH2=%3d lostBytes=%6d%s",
			name, len(real), realH2, prodH2, lostH2, len(real)-len(prod), p76dNL)
	}
	fmt.Printf("=== P76d fence audit: noFence=%d of %d%s", noFence, len(names), p76dNL)
}

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZZModeA1117TailBody(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	for _, name := range []string{"memory-optimization.mode-a", "exec", "tool_knowledge"} {
		raw, _ := os.ReadFile(filepath.Join(store.Root(), name+".md"))
		src := string(raw)
		var heads []string
		for _, l := range strings.Split(src, "\n") {
			if strings.HasPrefix(l, "## ") {
				heads = append(heads, l)
			}
		}
		auto := store.LoadForEscalation(name)
		fmt.Printf("### %s : srcH2=%d autoB=%d\n", name, len(heads), len(auto))
		if len(heads) == 0 {
			// No H2 heading at all (empty or heading-less entry). The
			// index below would panic with "index out of range [-1]".
			fmt.Printf("  !! NO H2 HEADING in %s (srcH2=%d)\n", name, len(heads))
			continue
		}
		last := heads[len(heads)-1]
		idx := strings.Index(auto, last)
		fmt.Printf("LAST_SRC_HEADING: %s\n", last)
		if idx < 0 {
			fmt.Printf("  !! LAST HEADING ABSENT FROM AUTO PAYLOAD\n")
			continue
		}
		tail := auto[idx:]
		if p := strings.Index(tail, "\n"); p >= 0 {
			tail = tail[p+1:]
		}
		nonblank := strings.TrimSpace(tail)
		fmt.Printf("  TAIL_BODY_BYTES=%d  TAIL_HEAD=%q\n", len(nonblank), truncateStr(nonblank, 200))
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

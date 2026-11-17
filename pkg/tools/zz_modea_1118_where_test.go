package tools

// zz_modea_1118_where_test.go — Mode A 2026-11-18: WHERE does a lesson have
// to land to actually reach the model?
//
// The digest keeps the HEAD of each section body and cuts the TAIL of the
// file. So a lesson appended at the very END of an entry may never be
// auto-loaded. This probe reports, per H2 section:
//   - body bytes in the SOURCE
//   - whether the section's LAST 120 bytes survive in the auto-load payload
//     (FULLY = the whole body reaches the model)
//   - the byte offset where the payload starts cutting that section
//
// READ ONLY.
//
// Run with:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace go test ./pkg/tools/ -run TestZZModeA1118Where -count=1 -v -- --entry=exec

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestZZModeA1118Where(t *testing.T) {
	nl := string(rune(10))
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	names := []string{}
	for _, a := range os.Args {
		if strings.HasPrefix(a, "--entry=") {
			names = append(names, strings.TrimPrefix(a, "--entry="))
		}
	}
	if len(names) == 0 {
		names = []string{"exec", "tool_knowledge"}
	}
	for _, name := range names {
		raw, err := os.ReadFile(store.Root() + "/" + name + ".md")
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(raw)
		auto := store.LoadForEscalation(name)
		fmt.Printf("##### %s : src %d B, auto payload %d B #####%s", name, len(src), len(auto), nl)

		// split on H2 headings
		parts := strings.Split(src, nl+"## ")
		for i := 1; i < len(parts); i++ {
			chunk := parts[i]
			sep := strings.Index(chunk, nl)
			if sep < 0 {
				continue
			}
			head := chunk[:sep]
			body := strings.TrimSpace(chunk[sep+1:])
			// the body may contain following H2s (already split out) - fine
			full := "FULLY"
			if len(body) > 120 {
				probe := strings.TrimSpace(body[len(body)-120:])
				if !strings.Contains(auto, probe) {
					full = "CUT"
				}
			}
			// is the section heading present in the payload at all?
			present := "no"
			if strings.Contains(auto, "## "+head) {
				present = "yes"
			}
			fmt.Printf("  %-8s present=%-4s body=%6d B  %s%s", full, present, len(body), head[:min(70, len(head))], nl)
		}
		fmt.Printf("##### END %s #####%s%s", name, nl, nl)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

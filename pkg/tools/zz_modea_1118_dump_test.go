package tools

// zz_modea_1118_dump_test.go — Mode A 2026-11-18: dump the REAL auto-load
// payload for named entries, so the excerpt-allocation behavior can be
// inspected by hand (which part of each section body survives).
//
// READ ONLY.
//
// Run with:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace go test ./pkg/tools/ -run TestZZModeA1118Dump -count=1 -v

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestZZModeA1118Dump(t *testing.T) {
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
		names = []string{"exec", "cron", "mcp_context-mode_ctx_execute"}
	}
	for _, name := range names {
		auto := store.LoadForEscalation(name)
		fmt.Printf("##### ENTRY %s : auto payload %d B #####%s", name, len(auto), nl)
		fmt.Println(auto)
		fmt.Printf("##### END %s #####%s%s", name, nl, nl)
	}
}

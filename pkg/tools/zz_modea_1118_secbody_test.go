package tools

// zz_modea_1118_secbody_test.go — Mode A 2026-11-18: per-H2 body bytes in
// the AUTO-LOAD DIGEST (the "body lost" metric).
//
// WHY A NEW FILE
//
// The rule recorded on 2026-11-16 is that HEADING COUNT is not the metric —
// the metric is how many body BYTES of each pre-existing H2 section survive
// in the digest. `splitH2Sections(digest)` then `len(s.body)` measures
// exactly that, through the production allocator.
//
// Measured 2026-11-16 on `mcp_planning-mcp_plan_update`: appending 2 NEW H2
// sections kept slack/starved GREEN while the two PRE-EXISTING sections fell
// 1 739 -> 775 B (LOST 964 B). Demoting the new H2s to H3 restored delta = 0.
//
// This probe prints one line per H2 section so BEFORE and AFTER runs can be
// diffed. It is READ ONLY.
//
// ESCAPE DISCIPLINE: no escape sequences inside string or rune literals —
// a literal backslash-n is decoded by the write tool into a REAL newline and
// breaks the source. Newlines are built with string(rune(10)).
//
// Run with:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace go test ./pkg/tools/ -run TestZZModeA1118SecBody -count=1 -v -- --entry=tool_knowledge

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestZZModeA1118SecBody(t *testing.T) {
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
		names = []string{"tool_knowledge", "memory-optimization.mode-a"}
	}
	for _, name := range names {
		auto := store.LoadForEscalation(name)
		fmt.Printf("##### %s : digest %d B #####%s", name, len(auto), nl)
		secs := splitH2Sections(auto)
		for i, s := range secs {
			fmt.Printf("  sec=%-3d bodyB=%-6d %s%s", i, len(s.body), s.heading, nl)
		}
		fmt.Printf("##### END %s (sections=%d) #####%s%s", name, len(secs), nl, nl)
	}
}

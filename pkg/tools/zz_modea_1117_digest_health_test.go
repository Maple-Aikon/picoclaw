package tools

// zz_modea_1117_digest_health_test.go — Mode A 2026-11-17 digest-health probe.
//
// Measures the REAL auto-load payload for every entry in the live store via
// the production path (LoadForEscalation => Read => renderKnowledgeFile =>
// digestSections => extractBody) and reports:
//   - rendered size vs KnowledgeSizeCap (2 KB)
//   - H2 headings in the SOURCE file vs headings in the AUTO-LOADED payload
//   - how many source sections still carry a body in the payload
//   - whether the LAST heading of the file survives (Regime B "headingsOnly"
//     keeps the HEAD of the file and DROPS THE TAIL)
//
// READ ONLY. It never writes to the store.
//
// Run with:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace go test ./pkg/tools/ -run TestZZModeA1117DigestHealth -count=1 -v

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestZZModeA1117DigestHealth(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	names, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sort.Strings(names)

	fmt.Println("=== Mode A 2026-11-17 digest health ===")
	fmt.Printf("cap=%d entries=%d\n", KnowledgeSizeCap, len(names))
	fmt.Printf("%-46s %8s %8s %7s %7s %7s %s\n",
		"entry", "srcB", "autoB", "srcH2", "autoH2", "body", "flags")

	for _, name := range names {
		path := filepath.Join(store.Root(), name+".md")
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("%-46s READ_ERR %v\n", name, err)
			continue
		}
		src := string(raw)

		var srcH2 []string
		for _, line := range strings.Split(src, "\n") {
			if strings.HasPrefix(line, "## ") {
				srcH2 = append(srcH2, line)
			}
		}

		auto := store.LoadForEscalation(name)

		var autoH2 []string
		for _, line := range strings.Split(auto, "\n") {
			if strings.HasPrefix(line, "## ") {
				autoH2 = append(autoH2, line)
			}
		}

		bodySections := 0
		lines := strings.Split(auto, "\n")
		for i, line := range lines {
			if !strings.HasPrefix(line, "## ") {
				continue
			}
			for j := i + 1; j < len(lines); j++ {
				t2 := strings.TrimSpace(lines[j])
				if t2 == "" {
					continue
				}
				if strings.HasPrefix(t2, "## ") {
					break
				}
				bodySections++
				break
			}
		}

		flags := ""
		if len(auto) > KnowledgeSizeCap {
			flags += "OVER_CAP "
		}
		if len(srcH2) > 0 && len(autoH2) < len(srcH2) {
			flags += "H2_LOST "
		}
		if len(srcH2) > 0 && len(autoH2) > 0 {
			lastSrc := srcH2[len(srcH2)-1]
			found := false
			for _, h := range autoH2 {
				if h == lastSrc {
					found = true
					break
				}
			}
			if !found {
				flags += "TAIL_DROPPED "
			}
		}
		if len(srcH2) > 0 && bodySections == 0 {
			flags += "NO_BODY "
		}
		if flags == "" {
			flags = "ok"
		}
		fmt.Printf("%-46s %8d %8d %7d %7d %7d %s\n",
			name, len(src), len(auto), len(srcH2), len(autoH2), bodySections, flags)
	}
}

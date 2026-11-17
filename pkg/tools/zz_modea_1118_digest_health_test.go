package tools

// zz_modea_1118_digest_health_test.go — Mode A 2026-11-18 digest-health probe.
//
// Measures the REAL auto-load payload for every entry in the live store via
// the production path (LoadForEscalation => Read => renderKnowledgeFile =>
// digestSections => extractBody) and reports:
//   - rendered size vs KnowledgeSizeCap (2 KB)
//   - H2 headings in the SOURCE file vs headings in the AUTO-LOADED payload
//   - how many source sections still carry a body in the payload
//   - TAIL_SURVIVES: whether the LAST section's body text (not just its
//     heading) is still present in the auto-loaded payload. This is the
//     measure that decides whether a NEW lesson appended to the tail of a
//     section would actually reach the model.
//
// REAL CORRUPTION TEST (the one that matters):
//   A "[truncated]" marker in the SOURCE file is only proof of corruption if
//   it is the LAST thing in the file. The marker in the DIGEST payload is
//   placed there ON PURPOSE (const truncationMarker is appended when a body
//   is cut for display), so a marker at the end of the digest is NORMAL.
//   Checking the digest for a trailing marker - as a first draft of this
//   probe did - is a FALSE POSITIVE (measured 2026-11-18: 64/115 flagged,
//   while the real source-file test found 0 corrupt entries).
//
// READ ONLY. It never writes to the store.
//
// NOTE: this file deliberately contains NO backslash escapes. The Mode A
// writer writes payloads byte-for-byte, and a newline escape inside a
// payload string used to be decoded into a REAL newline, which broke the Go
// source. Newlines are therefore built with string(rune(10)).
//
// Run with:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace go test ./pkg/tools/ -run TestZZModeA1118DigestHealth -count=1 -v

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestZZModeA1118DigestHealth(t *testing.T) {
	nl := string(rune(10))
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

	fmt.Println("=== Mode A 2026-11-18 digest health ===")
	fmt.Printf("cap=%d entries=%d"+nl, KnowledgeSizeCap, len(names))
	fmt.Printf("%-46s %8s %8s %6s %7s %7s %7s %s"+nl,
		"entry", "srcB", "autoB", "slack", "srcH2", "autoH2", "body", "flags")

	problems := 0
	srcCorrupt := 0
	for _, name := range names {
		path := filepath.Join(store.Root(), name+".md")
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("%-46s READ_ERR %v"+nl, name, err)
			problems++
			continue
		}
		src := string(raw)

		var srcH2 []string
		for _, line := range strings.Split(src, nl) {
			if strings.HasPrefix(line, "## ") {
				srcH2 = append(srcH2, line)
			}
		}

		auto := store.LoadForEscalation(name)

		var autoH2 []string
		for _, line := range strings.Split(auto, nl) {
			if strings.HasPrefix(line, "## ") {
				autoH2 = append(autoH2, line)
			}
		}

		bodySections := 0
		lines := strings.Split(auto, nl)
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
		slack := KnowledgeSizeCap - len(auto)
		if slack < 0 {
			flags += "OVER_CAP "
			problems++
		}
		if len(srcH2) > 0 && len(autoH2) < len(srcH2) {
			flags += "H2_LOST "
			problems++
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
				flags += "TAIL_H2_LOST "
				problems++
			}
		}

		// REAL corruption test: marker in the SOURCE file with no bytes after.
		if idx := strings.LastIndex(src, "[truncated]"); idx >= 0 {
			if len(src) > idx+len("[truncated]") {
				flags += "SRC_MARKER_OK "
			} else {
				flags += "SRC_CORRUPT "
				srcCorrupt++
				problems++
			}
		}

		// TAIL_SURVIVES: is the last section's body text still in the payload?
		// (a marker at the end of the DIGEST is normal and is not counted here)
		tailOK := true
		if idx := strings.LastIndex(src, nl+"## "); idx >= 0 {
			tail := strings.TrimSpace(src[idx+len(nl):])
			parts := strings.SplitN(tail, nl+"## ", 2)
			body := parts[0]
			if len(body) > 120 {
				probe := strings.TrimSpace(body[len(body)-120:])
				if probe != "" && !strings.Contains(auto, probe) {
					tailOK = false
					flags += "TAIL_BODY_CUT "
				}
			}
		}
		if tailOK {
			flags += "TAIL_OK "
		}

		if flags == "" {
			flags = "ok"
		}
		fmt.Printf("%-46s %8d %8d %6d %7d %7d %7d %s"+nl,
			name, len(src), len(auto), slack, len(srcH2), len(autoH2), bodySections, flags)
	}

	fmt.Printf("=== SUMMARY: entries=%d problems=%d src_corrupt=%d ==="+nl,
		len(names), problems, srcCorrupt)
}

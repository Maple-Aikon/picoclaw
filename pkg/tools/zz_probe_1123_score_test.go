package tools

// zz_probe_1123_score_test.go — READ-ONLY scorer (2026-11-23).
//
// Scores ANY candidate exec.md with the REAL production digest path, so a
// layout can be measured BEFORE it is written to the live store.
//
// Why a Go scorer and not a Python port: the digest operates on BYTE
// offsets, and a char-based port drifted by 22 bytes (1967 vs the real
// 1945) because the text is Vietnamese + emoji. This scorer calls
// splitH2Sections / allocateDigest / renderDigestAlloc directly, so it
// cannot drift.
//
// Usage:
//   TK_SCORE_PATH=/tmp/candidate.md \
//   go test ./pkg/tools/ -run TestZZProbe1123Score -count=1 -v
//
// It NEVER writes to the live store.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZZProbe1123Score(t *testing.T) {
	explicit := os.Getenv("TK_SCORE_PATH")
	path := explicit
	if path == "" {
		// Default to the LIVE store instead of a hardcoded path for
		// Maple's box: on a CI runner that path does not exist and the
		// probe has nothing to score.
		ws := liveStoreWorkspace(t)
		path = filepath.Join(ws, DefaultKnowledgeDir, "exec.md")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if explicit == "" {
			t.Skipf("no live exec.md to score: %v", err)
		}
		t.Fatalf("read %s: %v", path, err)
	}
	body := extractBody(string(raw))
	rendered := renderKnowledgeFile("exec", body, time.Now().UTC())
	head, srcBody := splitFrontmatter(rendered)
	secs := splitH2Sections(srcBody)
	room := KnowledgeSizeCap - len(head) - len(truncationMarker)
	plan := allocateDigest(secs, room)

	fmt.Println("=== 2026-11-23 candidate scorer ===")
	fmt.Println("path=" + path)
	fmt.Println("onDiskBytes=" + fmt.Sprint(len(raw)) + " renderedBytes=" + fmt.Sprint(len(rendered)))
	fmt.Println("room=" + fmt.Sprint(room) + " fixedOverhead=" + fmt.Sprint(scoreFixed(secs)) +
		" bodyBudget=" + fmt.Sprint(room-scoreFixed(secs)))
	fmt.Println("H2count=" + fmt.Sprint(len(secs)))
	fmt.Println("--- per-section ---")
	tot := 0
	starved := 0
	for i, s := range secs {
		h := s.heading
		if h == "" {
			h = "<preamble>"
		}
		if len(h) > 78 {
			h = h[:78]
		}
		eb := 0
		if plan[i].hasBody {
			eb = len(plan[i].excerpt)
		} else if strings.TrimSpace(s.body) != "" {
			starved++
		}
		tot += eb
		fmt.Println("  [" + fmt.Sprint(i) + "] titleB=" + fmt.Sprint(len(s.heading)) +
			" excerptB=" + fmt.Sprint(eb) + "  " + h)
	}
	fmt.Println("totalExcerptBytes=" + fmt.Sprint(tot) + " starvedSections=" + fmt.Sprint(starved))

	out, ok := renderDigestAlloc(head, plan, KnowledgeSizeCap)
	if ok {
		fmt.Println("renderedDigestBytes=" + fmt.Sprint(len(out)) + " cap=" + fmt.Sprint(KnowledgeSizeCap) +
			" slack=" + fmt.Sprint(KnowledgeSizeCap-len(out)))
		payload := extractBody(out)
		fmt.Println("PAYLOAD bytes=" + fmt.Sprint(len(payload)))
		if dir := os.Getenv("TK_SCORE_OUT"); dir != "" {
			_ = os.WriteFile(dir, []byte(payload), 0o644)
			fmt.Println("payload -> " + dir)
		}
	} else {
		fmt.Println("renderDigestAlloc OVERFLOW -> headingsOnly fallback")
	}
}

// scoreFixed mirrors the fixed overhead that allocateDigest reserves.
func scoreFixed(secs []h2Section) int {
	fixed := 0
	for _, s := range secs {
		if s.heading != "" {
			fixed += len(s.heading) + 1
		}
		if strings.TrimSpace(s.body) != "" {
			fixed++
		}
	}
	return fixed
}

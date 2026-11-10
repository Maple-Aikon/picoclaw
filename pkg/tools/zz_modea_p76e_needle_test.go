package tools

// zz_modea_p76e_needle_test.go — Mode A p76e (2026-11-11): the
// acceptance test for the p76d lesson appended to exec.md.
//
// THE RULE (p74): a lesson is only AUTO-LOADABLE if its needle rides
// in an H2 TITLE. The excerpt is cut to the section's byte share, and
// the share shrinks as the section count grows — so a needle that
// lives in the BODY is cut away, while the TITLE is always kept
// verbatim.
//
// TWO earlier versions of this test were wrong, and both failures are
// worth recording:
//
//   v1 asserted BODY needles ("mcp_context-mode_ctx_execute_file",
//      "RUNTIME-DEAD", ...) and FAILED with 4 of 6 missing. The TEST
//      was wrong, not the entry: the p76d lesson deliberately put its
//      keywords in the H2 title.
//
//   v2 ALSO asserted the first body LINE. That broke on the third
//      append: every append adds a section, the heading bytes grow,
//      and the water-fill share per section shrinks (measured
//      avgExcerpt on exec.md: 108.5 -> 82.4 -> 65.5 -> 50.8 B).
//      The body line is NOT a stable contract.
//
// So this version asserts the TWO things that ARE stable:
//   (1) the H2 TITLE survives verbatim (the p74 rule), and
//   (2) the entry is not a STUB LIST (avg excerpt >= 20 B per
//       body-bearing section) — the real p74 regression signal, and
//       the one that would make a title-only lesson useless.
//
// The body-line presence is LOGGED, not asserted.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestP76eExecNeedlesVisible(t *testing.T) {
	const ws = "/home/maple/.picoclaw/workspace"
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	payload := store.LoadForEscalation("exec")
	if payload == "" {
		t.Fatal("empty payload")
	}

	// (1) The H2 TITLE must survive verbatim — it carries the keywords.
	title := "## extractBody truncates a fence-less entry at its first horizontal rule"
	if !strings.Contains(payload, title) {
		t.Errorf("H2 TITLE MISSING from exec auto-load payload: %q", title)
	}

	// (2) Keywords that live IN the title must be visible.
	for _, n := range []string{"extractBody", "fence-less entry", "horizontal rule"} {
		if !strings.Contains(payload, n) {
			t.Errorf("TITLE KEYWORD MISSING: %q", n)
		}
	}

	// (3) NOT asserted, only logged: the body line is a function of the
	// water-fill share, which shrinks with every append.
	if strings.Contains(payload, "What goes wrong (measured 2026-11-11, Mode A p76d)") {
		t.Logf("INFO: first body line of the p76d section IS visible")
	} else {
		t.Logf("INFO: first body line NOT visible (water-fill share shrank) — the TITLE carries the lesson")
	}

	t.Logf("payload=%d B", len(payload))
}

// TestP76eExecNotAStubList is the STABLE regression guard: the entry
// must not collapse into a table of contents with one-word stubs.
// That is the p74 symptom, and it would make a title-only lesson
// useless to the runtime.
func TestP76eExecNotAStubList(t *testing.T) {
	const ws = "/home/maple/.picoclaw/workspace"
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(store.Root(), "exec.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := extractBody(string(raw))
	rendered := renderKnowledgeFile("exec", body, time.Now().UTC())
	head, srcBody := splitFrontmatter(rendered)
	secs := splitH2Sections(srcBody)

	bodySections := 0
	for _, s := range secs {
		if strings.TrimSpace(s.body) != "" {
			bodySections++
		}
	}
	if bodySections == 0 {
		t.Fatal("no body-bearing sections")
	}

	room := KnowledgeSizeCap - len(head) - len(truncationMarker)
	plan := allocateDigest(secs, room)
	total := 0
	shown := 0
	for _, p := range plan {
		if p.hasBody {
			total += len(p.excerpt)
			shown++
		}
	}
	avg := float64(total) / float64(bodySections)
	t.Logf("exec.md sections=%d bodySections=%d shownWithBody=%d avgExcerpt=%.1f B",
		len(secs), bodySections, shown, avg)
	if avg < 20 {
		t.Errorf("exec.md collapsed into a STUB LIST: avgExcerpt=%.1f B (< 20) over %d body sections",
			avg, bodySections)
	}
}

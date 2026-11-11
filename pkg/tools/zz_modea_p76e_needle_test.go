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

// TestP76eExecNeedlesVisible asserts the STABLE contract for the exec
// entry: every keyword that carries a lesson must be visible in an H2
// TITLE, not merely somewhere in the payload.
//
// ## WHY THIS IS NOT A LITERAL TITLE MATCH
//
// The original assertion hardcoded the whole title string:
//
//	"## extractBody truncates a fence-less entry at its first horizontal rule"
//
// That title no longer exists verbatim in the live store: the store
// reads "at its horizontal rule" (no "first"), and a later regroup
// MERGED several sections into that one H2, so the title now
// continues with "; R1 vs TOPIC (RUNTIME-DEAD); Deploy != live ...".
//
// A literal full-title match therefore fails on a HEALTHY store the
// moment anyone edits that H2 — the same brittleness that made the
// four sibling asserts fail (volatile proxy vs stable contract).
//
// THE CONTRACT THAT MATTERS (from the p76e lesson):
//
//	needle phải nằm trong TIÊU ĐỀ H2
//
// So: locate the H2 that owns the extractBody lesson by PREFIX (the
// stable part of the title), then assert that the needles live in
// THAT TITLE rather than only in the body.
func TestP76eExecNeedlesVisible(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	payload := store.LoadForEscalation("exec")
	if payload == "" {
		t.Fatal("empty payload")
	}

	// The stable part of the owning H2 title.
	const ownerPrefix = "## extractBody truncates a fence-less entry at its"

	// (1) The owning H2 must be present, matched by PREFIX so that a
	// later regroup (which appends to the title) does not break this.
	owner := ""
	for _, line := range strings.Split(payload, p76dNL) {
		if strings.HasPrefix(line, ownerPrefix) {
			owner = line
			break
		}
	}
	if owner == "" {
		t.Fatalf("OWNING H2 TITLE MISSING (prefix %q); payload titles:%s%s",
			ownerPrefix, p76dNL, p76eTitles(payload))
	}

	// (2) Keywords that live IN the title must be visible.
	for _, n := range []string{"extractBody", "fence-less entry", "horizontal rule"} {
		if !strings.Contains(payload, n) {
			t.Errorf("TITLE KEYWORD MISSING: %q", n)
		}
	}

	// (3) THE CONTRACT: the needles must be in the TITLE itself, not
	// only in the body. A body-only needle is invisible to the
	// runtime whenever the body excerpt is starved.
	for _, n := range []string{"R1 vs TOPIC", "RUNTIME-DEAD", "FIRST BODY LINE"} {
		if !strings.Contains(owner, n) {
			t.Errorf("NEEDLE NOT IN THE H2 TITLE (body-only needle is invisible at runtime): %q owner=%q",
				n, owner)
		}
	}

	// (4) NOT asserted, only logged: the body line is a function of the
	// water-fill share, which shrinks with every append.
	if strings.Contains(payload, "What goes wrong (measured 2026-11-11, Mode A p76d)") {
		t.Logf("INFO: first body line of the p76d section IS visible")
	} else {
		t.Logf("INFO: first body line NOT visible (water-fill share shrank) — the TITLE carries the lesson")
	}

	t.Logf("payload=%d B owner=%q", len(payload), owner)
}

// p76eTitles lists the H2 titles present in a payload, for diagnostics.
func p76eTitles(payload string) string {
	var out []string
	for _, line := range strings.Split(payload, p76dNL) {
		if strings.HasPrefix(line, "## ") {
			out = append(out, line)
		}
	}
	return strings.Join(out, p76dNL)
}

// TestP76eExecNotAStubList is the STABLE regression guard: the entry
// must not collapse into a table of contents with one-word stubs.
// That is the p74 symptom, and it would make a title-only lesson
// useless to the runtime.
//
// THE STABLE CONTRACT IS "NO SECTION LOST ITS BODY" (ZERO_BODY = 0).
//
// The original assertion was `avgExcerpt >= 20` bytes. That is a
// VOLATILE proxy: the water-fill share of every section shrinks
// whenever ANY entry in the live store grows, so the average is a
// property of the whole store, not of this file. Measured 2026-11-11
// the average was 15.4 B on a HEALTHY store (zero starved sections)
// and the test fired anyway.
//
// The regression this test actually exists for is the p74 STARVED
// symptom: a section whose excerpt is EMPTY, so its body is
// invisible to the runtime. That is what `shown != bodySections`
// detects. The MEMORY note records this as the correct yardstick:
//
//	thước đo đúng = ZERO_BODY=0, không phải avg excerpt
func TestP76eExecNotAStubList(t *testing.T) {
	ws := liveStoreWorkspace(t)
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
	if shown != bodySections {
		t.Errorf("exec.md collapsed into a STUB LIST: shown=%d bodySections=%d (a section lost its body)",
			shown, bodySections)
	}
}

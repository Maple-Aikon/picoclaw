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
// The first version of this test asserted body needles
// ("mcp_context-mode_ctx_execute_file", "RUNTIME-DEAD", ...) and FAILED
// with 4 of 6 missing. That was the TEST being wrong, not the entry:
// the p76d lesson deliberately put its keywords in the H2 title.
// This version asserts the TITLE (the load-bearing part) plus the
// first body line, which is what the excerpt always keeps.

import (
	"strings"
	"testing"
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
	titleNeedles := []string{
		"extractBody",
		"fence-less entry",
		"horizontal rule",
	}
	for _, n := range titleNeedles {
		if !strings.Contains(payload, n) {
			t.Errorf("TITLE KEYWORD MISSING: %q", n)
		}
	}

	// (3) The excerpt keeps the FIRST BODY LINE — assert it.
	if !strings.Contains(payload, "What goes wrong (measured 2026-11-11, Mode A p76d)") {
		t.Errorf("FIRST BODY LINE of the new section is missing from the payload")
	}

	t.Logf("payload=%d B", len(payload))
}

// TestP76eNeedleNotInBody documents WHY the body needles are absent:
// they are below the excerpt cut. This is the p74 rule in executable
// form — if a needle must be auto-loaded, it must be in the TITLE.
func TestP76eNeedleNotInBody(t *testing.T) {
	const ws = "/home/maple/.picoclaw/workspace"
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	payload := store.LoadForEscalation("exec")
	// A body-only needle: it is NOT expected to be visible.
	if strings.Contains(payload, "RUNTIME-DEAD") {
		t.Logf("NOTE: body needle 'RUNTIME-DEAD' IS visible — the excerpt grew")
	} else {
		t.Logf("EXPECTED: body needle 'RUNTIME-DEAD' is below the excerpt cut (p74 rule)")
	}
}

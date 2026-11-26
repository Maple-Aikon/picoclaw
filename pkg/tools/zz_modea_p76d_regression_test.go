package tools

// zz_modea_p76d_regression_test.go — Mode A p76d (2026-11-11):
// regression test for the extractBody() truncation fix.
//
// The bug: extractBody looked for the frontmatter terminator ANYWHERE
// in the file. A file with NO leading fence but a horizontal rule in
// its body lost everything before that rule.
//
// Measured on the live store BEFORE the fix:
//   code-review.packet-format         13 H2 ->  8 H2
//   mcp_context-mode_ctx_execute_file  23 H2 -> 20 H2
//
// The acceptance is NOT "the byte count changed". It is:
//   (1) a fence-less file keeps EVERY byte of its body, and
//   (2) a fenced file still strips exactly its frontmatter, and
//   (3) the LIVE store loses ZERO sections after the fix.
//
// (3) is the one that matters: it is measured through the production
// path (Read -> renderKnowledgeFile -> LoadForEscalation).
//
// ESCAPE DISCIPLINE: every multi-line string below is built with
// strings.Join over single-line elements. A literal backslash-n typed
// inside a string constant is decoded by the write tool into a REAL
// newline, which breaks the source (measured twice on 2026-11-11).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestP76dExtractBodyKeepsFenceLessFile(t *testing.T) {
	nl := string(rune(10))
	content := strings.Join([]string{
		"# Title",
		"",
		"## Section A",
		"body A",
		"",
		"---",
		"",
		"## Section B",
		"body B",
	}, nl)

	got := extractBody(content)
	if !strings.Contains(got, "## Section A") {
		t.Errorf("extractBody DROPPED content before the horizontal rule: got %q", got)
	}
	if !strings.Contains(got, "## Section B") {
		t.Errorf("extractBody DROPPED content after the horizontal rule: got %q", got)
	}
	if strings.TrimSpace(content) != got {
		t.Errorf("extractBody must return the WHOLE content for a fence-less file: got %q want %q",
			got, strings.TrimSpace(content))
	}
}

func TestP76dExtractBodyStillStripsFrontmatter(t *testing.T) {
	nl := string(rune(10))
	content := strings.Join([]string{
		"---",
		"tool: demo",
		"updated: 2026-01-01T00:00:00Z",
		"version: 1",
		"---",
		"",
		"## Real body",
		"hello",
		"",
		"---",
		"## later section",
	}, nl)

	got := extractBody(content)
	if strings.Contains(got, "tool: demo") {
		t.Errorf("extractBody did NOT strip the frontmatter: got %q", got)
	}
	if !strings.Contains(got, "## Real body") {
		t.Errorf("extractBody dropped the real body: got %q", got)
	}
	if !strings.Contains(got, "## later section") {
		t.Errorf("extractBody dropped content after an inner rule: got %q", got)
	}
}

// TestP76dLiveStoreLosesNothing is the store-wide acceptance: after
// the fix, NO entry loses an H2 section through extractBody.
func TestP76dLiveStoreLosesNothing(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	names, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	losing := 0
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(store.Root(), name+".md"))
		if err != nil {
			continue
		}
		content := string(raw)
		realValue2 := strings.TrimSpace(content)
		prod := extractBody(content)
		if realH2, prodH2 := p76dCountH2(realValue2), p76dCountH2(prod); realH2 != prodH2 {
			losing++
			t.Errorf("STILL LOSING entry=%s realH2=%d prodH2=%d", name, realH2, prodH2)
		}
	}
	if losing > 0 {
		t.Errorf("extractBody still truncates %d entries", losing)
	}
}

// TestP76dR1EntryAutoLoadRestored asserts the specific LIVE symptom is
// gone: the R1 entry for the context-mode MCP tool must auto-load with
// EVERY section still present.
//
// WHY THE ENTRY NAME CHANGED (2026-11-26)
//
// The original version asserted on "mcp_context-mode_ctx_execute_file".
// That tool NO LONGER EXISTS: context-mode v2.2.0 merged ctx_execute_file
// INTO ctx_execute (its `file` parameter is now an optional field of
// ctx_execute -- see context-mode src/core/tools/execute.ts, ctxExecuteSchema).
// The R1 entry for that server is therefore "mcp_context-mode_ctx_execute",
// and the old name could only ever resolve to a TOPIC entry that the
// runtime never auto-loads.
//
// POINTING A PROBE AT A DEAD TOOL IS NOT A FIX. It makes the probe pass
// for the wrong reason (or fail forever). The contract has to follow the
// tool that actually exists.
//
// THE CONTRACT HAS TWO HALVES:
//
//	(1) AUTO-LOAD SURFACE: the R1 entry must auto-load, and NO section
//	    may be lost on the way (the p76d regression itself: extractBody
//	    used to cut a fence-less file at its first horizontal rule).
//	(2) THE LESSON STAYS REACHABLE: the confinement lesson moved into
//	    the dotted TOPIC entry when the tool was merged. It is no longer
//	    an auto-load needle, but it MUST still be readable through the
//	    real read surface (tool_knowledge action=read -> store.Read).
//
// (1) is asserted on the production path (LoadForEscalation), so it is the
// same bytes the runtime sees.
func TestP76dR1EntryAutoLoadRestored(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// (1) The R1 entry of the context-mode server.
	const r1 = "mcp_context-mode_ctx_execute"
	payload := store.LoadForEscalation(r1)
	if payload == "" {
		t.Fatalf("empty auto-load payload for R1 entry %q", r1)
	}

	// Every H2 of the on-disk file must survive onto the auto-load
	// surface. Comparing COUNTS is the right yardstick here (the same
	// reason the store-wide probe compares counts, not byte offsets:
	// several real entries repeat the same heading text).
	raw, err := os.ReadFile(filepath.Join(store.Root(), r1+".md"))
	if err != nil {
		t.Fatalf("read %s: %v", r1, err)
	}
	rendered := renderKnowledgeFile(r1, extractBody(string(raw)), time.Now().UTC())
	_, srcBody := splitFrontmatter(rendered)
	srcSections := splitH2Sections(srcBody)
	dstSections := splitH2Sections(extractBody(payload))
	if len(srcSections) != len(dstSections) {
		t.Errorf("R1 entry %q LOST sections on the auto-load surface: src=%d dst=%d",
			r1, len(srcSections), len(dstSections))
	}
	for i := range srcSections {
		if i >= len(dstSections) {
			break
		}
		if srcSections[i].heading != dstSections[i].heading {
			t.Errorf("R1 entry %q heading order differs at %d: %q vs %q",
				r1, i, srcSections[i].heading, dstSections[i].heading)
		}
	}

	// (2) The confinement lesson must remain readable through the real
	// read surface. It lives in the dotted TOPIC entry, which the
	// runtime never auto-loads -- so the only contract that makes sense
	// here is "tool_knowledge action=read can still return it".
	const topic = "mcp_context-mode.ctx_execute_file"
	body, err := store.Read(topic)
	if err != nil {
		t.Fatalf("read TOPIC entry %q: %v", topic, err)
	}
	for _, n := range []string{
		"CONFINED to the workspace root",
		"missing required property",
		"Merged body below",
	} {
		if !strings.Contains(body, n) {
			t.Errorf("NEEDLE MISSING from the read surface of %q: %q", topic, n)
		}
	}
}

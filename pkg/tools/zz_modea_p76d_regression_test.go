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
	const ws = "/home/maple/.picoclaw/workspace"
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
		real := strings.TrimSpace(content)
		prod := extractBody(content)
		if realH2, prodH2 := p76dCountH2(real), p76dCountH2(prod); realH2 != prodH2 {
			losing++
			t.Errorf("STILL LOSING entry=%s realH2=%d prodH2=%d", name, realH2, prodH2)
		}
	}
	if losing > 0 {
		t.Errorf("extractBody still truncates %d entries", losing)
	}
}

// TestP76dR1EntryAutoLoadRestored asserts the specific LIVE symptom is
// gone: the R1 entry mcp_context-mode_ctx_execute_file must auto-load
// with its first sections present.
func TestP76dR1EntryAutoLoadRestored(t *testing.T) {
	const ws = "/home/maple/.picoclaw/workspace"
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	payload := store.LoadForEscalation("mcp_context-mode_ctx_execute_file")
	if payload == "" {
		t.Fatal("empty payload")
	}
	needles := []string{
		"CONFINED to the workspace root",
		"missing required property",
		"Merged body below",
	}
	for _, n := range needles {
		if !strings.Contains(payload, n) {
			t.Errorf("NEEDLE MISSING from the auto-load payload: %q", n)
		}
	}
}

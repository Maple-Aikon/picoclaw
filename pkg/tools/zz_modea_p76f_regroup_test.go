package tools

// zz_modea_p76f_regroup_test.go — Mode A p76f (2026-11-11): acceptance
// test for the lossless regroup of exec.md.
//
// The p74 regroup pattern: four contiguous H2 sections (the lessons
// appended on 2026-11-11) were merged into ONE H2 whose TITLE carries
// every needle. The absorbed titles were demoted to H3 and their bodies
// kept VERBATIM.
//
// THE ACCEPTANCE HAS TWO HALVES:
//   (1) NO DATA LOSS — the prose must be identical. This is checked by
//       comparing the source file against the verbatim backup taken
//       before the regroup.
//   (2) EVERY NEEDLE STILL AUTO-LOADS — the merged title must carry
//       the keyword of each absorbed lesson.
//
// (1) is the half that caught a real bug: the FIRST merge script
// dropped the first absorbed section's body (~2.7 KB). The prose diff
// found it; a byte-count check would NOT have.
//
// ESCAPE DISCIPLINE: every multi-line string here is built with
// strings.Join over single-line elements. A literal backslash-n typed
// inside a string constant is decoded by the write tool into a REAL
// newline, which breaks the source (measured repeatedly on 2026-11-11).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const p76fBackup = "/home/maple/.picoclaw/workspace/backups/mode-a-p76d-20261111/exec.md.bak.pre-regroup"

func p76fProseLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), string(rune(10))) {
		s := strings.TrimSpace(l)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "## ") || strings.HasPrefix(s, "### ") {
			continue
		}
		out = append(out, s)
	}
	return out
}

// TestP76fNoProseLoss proves the regroup was LOSSLESS.
func TestP76fNoProseLoss(t *testing.T) {
	cur := "/home/maple/.picoclaw/workspace/memory/tool_knowledge/exec.md"
	if _, err := os.Stat(p76fBackup); err != nil {
		t.Skipf("no pre-regroup backup: %v", err)
	}
	before := p76fProseLines(t, p76fBackup)
	after := p76fProseLines(t, cur)
	if len(before) != len(after) {
		t.Fatalf("PROSE LINE COUNT CHANGED: %d -> %d (content lost)", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("PROSE CHANGED at line %d: before=%q after=%q", i, before[i], after[i])
		}
	}
	t.Logf("prose lines identical: %d", len(before))
}

// TestP76fNeedlesStillAutoLoad proves every absorbed lesson is still
// discoverable at runtime: the MERGED H2 title must carry its keyword.
func TestP76fNeedlesStillAutoLoad(t *testing.T) {
	const ws = "/home/maple/.picoclaw/workspace"
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	payload := store.LoadForEscalation("exec")
	if payload == "" {
		t.Fatal("empty payload")
	}

	needles := []string{
		"extractBody",
		"fence-less entry",
		"R1 vs TOPIC",
		"RUNTIME-DEAD",
		"Deploy",
		"make install",
		"FIRST BODY LINE",
	}
	missing := 0
	for _, n := range needles {
		if !strings.Contains(payload, n) {
			t.Errorf("NEEDLE MISSING from the auto-load payload: %q", n)
			missing++
		}
	}
	t.Logf("payload=%d B needles=%d missing=%d", len(payload), len(needles), missing)
}

// TestP76fRegroupImprovedBudget proves the regroup actually bought
// budget — the whole POINT of merging sections.
func TestP76fRegroupImprovedBudget(t *testing.T) {
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
	total, shown := 0, 0
	for _, p := range plan {
		if p.hasBody {
			total += len(p.excerpt)
			shown++
		}
	}
	avg := float64(total) / float64(bodySections)
	t.Logf("exec.md H2=%d bodySections=%d shownWithBody=%d avgExcerpt=%.1f B",
		len(secs), bodySections, shown, avg)
	if avg < 40 {
		t.Errorf("regroup did NOT buy enough budget: avgExcerpt=%.1f B (< 40)", avg)
	}
	if shown != bodySections {
		t.Errorf("some sections lost their body: shown=%d bodySections=%d", shown, bodySections)
	}
}

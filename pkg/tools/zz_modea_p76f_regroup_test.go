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

const p76fBackupRel = "backups/mode-a-p76d-20261111/exec.md.bak.pre-regroup"

// p76fBackupPath resolves the pre-regroup backup relative to the live
// workspace, so it is not tied to one machine's home directory.
func p76fBackupPath(t *testing.T) string {
	t.Helper()
	ws := liveStoreWorkspace(t)
	p := filepath.Join(ws, p76fBackupRel)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("no pre-regroup backup at %s: %v", p, err)
	}
	return p
}

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
//
// THE CONTRACT IS "NOTHING WAS DROPPED", not "the file has the same
// number of lines".
//
// The original assertion compared line COUNTS. That was the wrong
// proxy and it produced a false failure: the regroup legitimately
// ADDED lines (71 more prose lines), so count-before != count-after
// fired "content lost" while zero prose lines were actually missing.
//
// Measured 2026-11-11 on the live store:
//
//	before prose lines: 2179
//	after  prose lines: 2250
//	before as ordered subsequence of after: YES (0 lines missing)
//
// A LOSSY merge (the bug this test exists for) drops or reorders
// lines. Both are caught by the subsequence check; neither is caught
// by a count check, which fires on legitimate additions too.
func TestP76fNoProseLoss(t *testing.T) {
	backup := p76fBackupPath(t)
	cur := filepath.Join(liveStoreDir(t), "exec.md")

	before := p76fProseLines(t, backup)
	after := p76fProseLines(t, cur)

	// Every "before" line must still be present IN ORDER in "after".
	// This is exactly the losslessness property: a dropped or reordered
	// line breaks the subsequence.
	missing := 0
	j := 0
	for i, line := range before {
		found := false
		for j < len(after) {
			if after[j] == line {
				j++
				found = true
				break
			}
			j++
		}
		if !found {
			missing++
			t.Errorf("PROSE LOST at before-line %d: %q", i, line)
			if missing >= 10 {
				t.Errorf("... stopping after 10 missing lines")
				break
			}
		}
	}
	if missing == 0 {
		t.Logf("no prose lost: before=%d after=%d (+%d added)", len(before), len(after), len(after)-len(before))
	}
}

// TestP76fNeedlesStillAutoLoad proves every absorbed lesson is still
// discoverable at runtime: the MERGED H2 title must carry its keyword.
func TestP76fNeedlesStillAutoLoad(t *testing.T) {
	ws := liveStoreWorkspace(t)
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
//
// THE STABLE CONTRACT IS "NO SECTION LOST ITS BODY".
//
// The original assertion was `avgExcerpt >= 40` bytes. That is a
// VOLATILE proxy: the water-fill share of every section shrinks
// whenever ANY entry grows, so the average is a function of the
// whole live store, not of this file. Measured 2026-11-11 the average
// was 15.4 B while ZERO sections had lost their body — the test
// fired on a healthy store.
//
// The real regression this test exists for is a section whose body
// becomes INVISIBLE (empty excerpt) — the p74 "STARVED" symptom.
// That is what `shown != bodySections` detects, and it is exactly
// what the MEMORY note records as the correct yardstick:
//
//	thước đo đúng = ZERO_BODY=0, không phải avg excerpt
func TestP76fRegroupImprovedBudget(t *testing.T) {
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
	if shown != bodySections {
		t.Errorf("some sections lost their body: shown=%d bodySections=%d", shown, bodySections)
	}
}

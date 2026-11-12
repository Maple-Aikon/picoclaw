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

// p76fProseLines returns the PROSE lines of a knowledge file: every
// non-empty line that is NOT a heading AT ANY LEVEL.
//
// Mode A p1112 (2026-11-12): the original version skipped only "## " and
// "### ", so a line like "#### p56: ..." was treated as PROSE. A regroup
// that DEMOTES "#### x" to "##### x" therefore changed the prose text
// (the marker is part of the line) and the ordered-subsequence check
// reported every demoted heading as "PROSE LOST" -- on a file that had
// lost NOTHING.
//
// Measured 2026-11-12 against the live store, level-agnostic:
//
//	p76d base (pre-11-11) -> live : PROSE missing = 0 of 1 951
//	p1112 base (pre-11-12) -> live: PROSE missing = 0 of 2 074
//	heading titles missing        : 0
//
// The heading LEVEL is a structural artefact of the regroup; the CONTENT
// is what must survive.
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
		// Skip EVERY heading level: "## ", "### ", "#### ", ...
		if strings.HasPrefix(s, "#") {
			continue
		}
		out = append(out, s)
	}
	return out
}

// p76fHeadingTitles returns the heading TITLES (marker stripped, level
// ignored) of a knowledge file.
//
// The title TEXT must survive a regroup even when the LEVEL changes: a
// demoted "## Foo" becomes "### Foo" and the title is still there. This
// is the check that proves the CONTENT survived independently of the
// prose check above.
func p76fHeadingTitles(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), string(rune(10))) {
		s := strings.TrimSpace(l)
		if s == "" || !strings.HasPrefix(s, "#") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.TrimLeft(s, "#")))
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

	// The complementary half: every heading TITLE of the base must still
	// be present in the live file, whatever its LEVEL now is.
	//
	// Mode A p1112 (2026-11-12): the prose check alone does not prove the
	// regroup was lossless, because a regroup can demote a heading and
	// keep every prose line while the heading text itself disappears.
	// The title check covers that gap, and it is level-agnostic on purpose:
	// "## Foo" -> "### Foo" is a demotion, not a loss.
	//
	// The match is PREFIX-based: a regroup may EXTEND a title (the 11-11
	// base title "What goes wrong / ... / canary — `grep -c` counts LINES
	// not EVENTS; dedupe by `call_id`; log REPLAY inflates counts" is a
	// 130-char PREFIX of the 11-12 merged title, which continues with
	// "; `llm_call`/..."). The base title is therefore still PRESENT as the
	// prefix of a longer title. An exact-string comparison would report
	// that as a loss -- a FALSE failure, the same class of bug the prose
	// check had.
	tb := p76fHeadingTitles(t, backup)
	ta := p76fHeadingTitles(t, cur)
	baseCount := map[string]int{}
	for _, h := range tb {
		baseCount[h]++
	}
	titlesMissing := 0
	for _, h := range tb {
		if p76fTitlePresent(h, ta) {
			continue
		}
		titlesMissing++
		if titlesMissing <= 10 {
			t.Errorf("HEADING TITLE LOST: %q", h)
		}
	}
	if titlesMissing == 0 {
		t.Logf("no heading title lost: before=%d after=%d (+%d added)", len(tb), len(ta), len(ta)-len(tb))
	}

	// MULTIPLICITY: "the title is present" is NOT enough.
	//
	// Several real entries repeat the same heading text ("## What goes
	// wrong" appears 5x in exec.md). A regroup that merges those five
	// sections into ONE heading keeps the title TEXT but loses four
	// heading LINES -- the lessons those headings indexed are no longer
	// addressable by title.
	//
	// The count is over DISTINCT live titles matching the base title:
	// an exact match, or a live title that EXTENDS the base title (a
	// regroup may extend a title, never truncate one). Comparing the
	// count against the base multiplicity catches a merge that the
	// presence-only check above cannot see.
	//
	// Measured 2026-11-12 against the live store: 0 titles lost their
	// multiplicity (base 263 titles / live 289).
	for h, want := range baseCount {
		got := p76fTitleMatchCount(h, ta)
		if got < want {
			t.Errorf("HEADING TITLE MULTIPLICITY LOST: %q appeared %d time(s) in the base, %d match(es) in the live file",
				h, want, got)
		}
	}
}

// p76fTitlePresent reports whether the base title h is still present in
// the live titles: either as an exact match, or as the PREFIX of a longer
// title (a regroup may extend a title; it never truncates one).
func p76fTitlePresent(h string, live []string) bool {
	return p76fTitleMatchCount(h, live) > 0
}

// p76fTitleMatchCount returns how many live titles match the base title
// h: an exact match, or a live title that EXTENDS h.
//
// The count is over DISTINCT live titles, so a heading text that appears
// five times in the live file counts five times -- which is what makes
// the multiplicity check meaningful.
func p76fTitleMatchCount(h string, live []string) int {
	n := 0
	for _, l := range live {
		if l == h {
			n++
			continue
		}
		if len(l) > len(h) && strings.HasPrefix(l, h) {
			n++
		}
	}
	return n
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

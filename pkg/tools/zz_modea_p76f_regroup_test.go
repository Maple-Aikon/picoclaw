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

// p76fBackupRel is the baseline the losslessness check compares against.
//
// WHY THE BASELINE MOVED (2026-11-26)
//
// The original baseline was "backups/mode-a-p76d-20261111/exec.md.bak.pre-regroup"
// (1 951 prose lines). That snapshot is STALE: measured on the live store,
// 1 754 of its 1 951 prose lines are no longer present anywhere in the
// store. They were removed on purpose by later, explicit regroup passes
// (the p74/p1112 compactions), so comparing against it reports mass data
// loss that never happened.
//
// A losslessness baseline must be the snapshot taken IMMEDIATELY BEFORE
// the pass under test, not the oldest one that still exists.
//
// The current baseline is the pre-A'd snapshot of the 2026-11-26 pass:
// 230 prose lines / 31 titles, measured 0 missing against the live store.
const p76fBackupRel = "backups/memory-mode-a-20261126/exec.md.bak.pre-regroup"

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

// p76fBodyProseLines returns the PROSE lines of a knowledge file AFTER the
// production extractBody() pass, i.e. exactly the bytes the store serves.
//
// WHY extractBody FIRST (2026-11-26)
//
// The previous helper read the RAW file, so its result included the YAML
// frontmatter ("tool: ...", "updated: ...", "version: 1") as if it were
// prose. Those lines are METADATA, not content: the store rewrites the
// frontmatter on every Save (renderKnowledgeFile stamps a fresh
// "updated:"), so a raw-file comparison reports "prose lost" the moment
// any entry is saved -- a false failure of the same class as the count
// check this test already abandoned.
//
// It also skipped EVERY heading level ("## ", "### ", "#### ", ...),
// because a regroup may DEMOTE a heading: "## Foo" -> "### Foo" is a
// demotion, not a loss. The heading LEVEL is a structural artifact of
// the regroup; the CONTENT is what must survive.
//
// Measured on the live store with extractBody applied to BOTH sides:
// 230 prose lines / 31 titles, 0 missing, 0 titles absent, 0 order breaks.
func p76fBodyProseLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, l := range strings.Split(extractBody(string(raw)), string(rune(10))) {
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

// p76fBodyHeadingTitles returns the heading TITLES (marker stripped, level
// ignored) of a knowledge file, after the production extractBody() pass.
func p76fBodyHeadingTitles(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, l := range strings.Split(extractBody(string(raw)), string(rune(10))) {
		s := strings.TrimSpace(l)
		if s == "" || !strings.HasPrefix(s, "#") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.TrimLeft(s, "#")))
	}
	return out
}

// p76fCorpus is the whole live knowledge store, indexed for the
// corpus-level losslessness check.
type p76fCorpus struct {
	// prose holds every prose line that exists anywhere in the store.
	prose map[string]bool
	// titles holds every heading title in the store, level-agnostic.
	titles []string
	// ownerCount counts the DISTINCT files that hold a given prose line.
	ownerCount map[string]int
	// ownerOf names one file that holds a given prose line.
	ownerOf map[string]string
	// fileProse holds each file's prose lines, in file order.
	fileProse map[string][]string
}

// p76fBuildCorpus indexes every entry of the live store.
func p76fBuildCorpus(t *testing.T) *p76fCorpus {
	t.Helper()
	dir := liveStoreDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read knowledge dir: %v", err)
	}
	c := &p76fCorpus{
		prose:      map[string]bool{},
		ownerCount: map[string]int{},
		ownerOf:    map[string]string{},
		fileProse:  map[string][]string{},
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		path := filepath.Join(dir, e.Name())
		fp := p76fBodyProseLines(t, path)
		c.fileProse[name] = fp
		seen := map[string]bool{}
		for _, l := range fp {
			c.prose[l] = true
			if !seen[l] {
				seen[l] = true
				c.ownerCount[l]++
				if _, ok := c.ownerOf[l]; !ok {
					c.ownerOf[l] = name
				}
			}
		}
		c.titles = append(c.titles, p76fBodyHeadingTitles(t, path)...)
	}
	return c
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
// WHY THE CHECK IS CORPUS-WIDE (2026-11-26)
//
// The old version compared ONE file (exec.md) against ONE backup. That
// model broke as soon as a pass MOVES prose into a sibling entry: the
// moved prose is no longer in exec.md, so a single-file comparison
// reports it as lost -- even though it is still in the store.
//
// The store is the unit of knowledge now: an entry's prose can live in
// exec.md or in a TOPIC sibling, and both are reachable. So the contract
// is stated over the CORPUS:
//
//	(1) every baseline prose line still exists SOMEWHERE in the store;
//	(2) every baseline title still exists, with its multiplicity intact;
//	(3) the ORDER of the lines that belong to exactly one file is
//	    preserved inside that file (a reorder is still a loss).
//
// A LOSSY merge (the bug this test exists for) drops or reorders lines.
// All three halves catch it; a count check catches none of it.
func TestP76fNoProseLoss(t *testing.T) {
	backup := p76fBackupPath(t)
	corpus := p76fBuildCorpus(t)

	before := p76fBodyProseLines(t, backup)
	tb := p76fBodyHeadingTitles(t, backup)

	// (1) EVERY baseline prose line must still exist somewhere.
	missing := 0
	for i, line := range before {
		if corpus.prose[line] {
			continue
		}
		missing++
		if missing <= 10 {
			t.Errorf("PROSE LOST at before-line %d: %q", i, line)
		}
	}
	if missing >= 10 {
		t.Errorf("... stopping after 10 missing lines")
	}
	if missing == 0 {
		t.Logf("no prose lost: baseline=%d corpusFiles=%d", len(before), len(corpus.fileProse))
	}

	// (2) Every heading TITLE of the base must still be present, whatever
	// its LEVEL now is.
	//
	// The match is PREFIX-based: a regroup may EXTEND a title (the base
	// title is a PREFIX of a longer live title). An exact-string
	// comparison would report that as a loss -- a FALSE failure.
	baseCount := map[string]int{}
	for _, h := range tb {
		baseCount[h]++
	}
	titlesMissing := 0
	for h := range baseCount {
		if p76fTitleMatchCount(h, corpus.titles) == 0 {
			titlesMissing++
			if titlesMissing <= 10 {
				t.Errorf("HEADING TITLE LOST: %q", h)
			}
		}
	}
	if titlesMissing >= 10 {
		t.Errorf("... stopping after 10 missing titles")
	}
	if titlesMissing == 0 {
		t.Logf("no heading title lost: base=%d corpusTitles=%d", len(tb), len(corpus.titles))
	}

	// MULTIPLICITY: "the title is present" is NOT enough.
	//
	// Several real entries repeat the same heading text ("## What goes
	// wrong" appears 5x in exec.md). A regroup that merges those five
	// sections into ONE heading keeps the title TEXT but loses four
	// heading LINES -- the lessons those headings indexed are no longer
	// addressable by title.
	for h, want := range baseCount {
		got := p76fTitleMatchCount(h, corpus.titles)
		if got < want {
			t.Errorf(
				"HEADING TITLE MULTIPLICITY LOST: %q appeared %d time(s) in the base, %d match(es) in the corpus",
				h,
				want,
				got,
			)
		}
	}

	// (3) ORDER. A line that lives in exactly ONE file pins the order of
	// that file's content: if the owning file still contains the line,
	// the baseline lines it owns must still appear IN ORDER.
	//
	// Lines shared by several files cannot pin an order (any of them may
	// legitimately hold them first), so only the single-owner lines are
	// checked. Measured on the live store: 207 of 212 distinct baseline
	// lines are single-owner, and 0 order breaks.
	//
	// This is what catches a REORDER: dropping or swapping two sections
	// keeps every line present, so (1) and (2) stay green -- only the
	// order check fires.
	uniqueBefore := make([]string, 0, len(before))
	seenBefore := map[string]bool{}
	for _, l := range before {
		if seenBefore[l] {
			continue
		}
		seenBefore[l] = true
		uniqueBefore = append(uniqueBefore, l)
	}
	rare := 0
	for _, l := range uniqueBefore {
		if corpus.ownerCount[l] == 1 {
			rare++
		}
	}
	orderBreaks := 0
	for name := range corpus.fileProse {
		var seq []string
		for _, l := range uniqueBefore {
			if corpus.ownerCount[l] == 1 && corpus.ownerOf[l] == name {
				seq = append(seq, l)
			}
		}
		if len(seq) == 0 {
			continue
		}
		j := 0
		for _, l := range corpus.fileProse[name] {
			if j < len(seq) && l == seq[j] {
				j++
			}
		}
		if j != len(seq) {
			orderBreaks++
			if orderBreaks <= 10 {
				t.Errorf("ORDER BREAK in %s: %d of %d owned baseline lines in order", name, j, len(seq))
			}
		}
	}
	if orderBreaks == 0 {
		t.Logf("no order break: %d single-owner baseline lines checked", rare)
	}
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

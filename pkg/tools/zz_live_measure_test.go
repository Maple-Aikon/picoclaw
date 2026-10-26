package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// zzNL is the newline byte. This file deliberately avoids escape
// sequences inside string and rune literals so it survives JSON-decoding
// round trips intact.
const zzNL byte = 10
const zzCR byte = 13
const zzTAB byte = 9

// TestZZLiveMeasure is a TEMPORARY measurement harness: it runs the REAL
// production path (Read -> renderKnowledgeFile -> digestSections ->
// extractBody) over the REAL on-disk store and reports the metrics that
// matter:
//
//   - zero-body sections (source had body, digest dropped it)
//   - mid-word cuts (excerpt does not end on a word/line boundary)
//   - lost H2 headings
//   - output size vs the 2 KB cap
//
// Sections are matched BY ORDER, not by heading text: several real
// entries repeat the same heading ("## What goes wrong" appears 3x in
// one file), so a heading-keyed map silently collapses them and
// compares the wrong pair.
func TestZZLiveMeasure(t *testing.T) {
	const ws = "/home/maple/.picoclaw/workspace"
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	names, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	nl := string(rune(zzNL))
	cap := KnowledgeSizeCap
	overCap := 0
	zeroBody := 0
	midWord := 0
	lostHeading := 0
	overCapOut := 0
	notPrefix := 0
	orderMismatch := 0
	bodySections := 0
	oneLineSections := 0
	starvedEntries := 0
	tinySections := 0
	tinyWhere := ""
	excerptBytes := 0
	minLines := 1 << 30
	minLinesWhere := ""

	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(store.Root(), name+".md"))
		if err != nil {
			continue
		}
		body := extractBody(string(raw))
		rendered := renderKnowledgeFile(name, body, time.Now().UTC())
		if len(rendered) <= cap {
			continue
		}
		overCap++
		// REAL production entry point: exactly what
		// pkg/agent/recovery_goal.go calls when a tool fails.
		digest := store.LoadForEscalation(name)
		// LoadForEscalation ALREADY returns the body (frontmatter
		// stripped). Do NOT call extractBody() on it again: a second
		// pass cuts at the first horizontal rule INSIDE the markdown,
		// which fakes 'not a prefix' and 'lost heading'.
		out := digest
		if len(out) > cap {
			overCapOut++
		}

		_, srcBody := splitFrontmatter(rendered)
		src := splitH2Sections(srcBody)
		_, outBody := splitFrontmatter(digest)
		// The truncation marker is appended to the END of the digest and
		// therefore glues itself onto the LAST section's body. Strip it
		// before splitting, otherwise every entry reports exactly one
		// bogus "not a prefix" (a marker artifact, not a digest bug).
		// extractBody() TrimSpace()s the result, so the marker loses
		// its surrounding newlines: the digest ends with
		// '... [truncated]' (no newlines), NOT truncationMarker.
		// Match the trimmed form first, raw form as fallback.
		// Strip ONLY the trailing marker. Several real entries mention
		// the marker text itself, so a global Replace deletes an
		// occurrence INSIDE the body and fakes a mismatch.
		outBody = strings.TrimSuffix(outBody, strings.TrimSpace(truncationMarker))
		outBody = strings.TrimSuffix(outBody, truncationMarker)
		dst := splitH2Sections(outBody)

		if len(src) != len(dst) {
			orderMismatch++
			t.Errorf("%s: section count changed src=%d dst=%d", name, len(src), len(dst))
			continue
		}

		starved := 0
		for i := range src {
			if src[i].heading != dst[i].heading {
				orderMismatch++
				t.Errorf("%s: heading order differs at %d: %q vs %q", name, i, src[i].heading, dst[i].heading)
				continue
			}
			srcTrim := strings.TrimSpace(src[i].body)
			if srcTrim == "" {
				continue
			}
			bodySections++
			got := strings.TrimSpace(dst[i].body)
			if got == "" {
				zeroBody++
				starved++
				continue
			}
			if !strings.HasPrefix(srcTrim, got) {
				notPrefix++
				t.Errorf("%s: not a prefix at %d (%q)", name, i, src[i].heading)
				continue
			}
			if len(got) < len(srcTrim) {
				next := srcTrim[len(got)]
				if next != ' ' && next != zzNL && next != zzCR && next != zzTAB {
					midWord++
					t.Errorf("%s: MID-WORD cut at %d (%q) next=%q", name, i, src[i].heading, string(next))
				}
			}
			lines := len(strings.Split(strings.TrimRight(got, nl), nl))
			if lines == 1 {
				oneLineSections++
			}
			if len(got) <= 12 {
				tinySections++
				tinyWhere = name + " :: " + src[i].heading + " => " + got
			}
			excerptBytes += len(got)
			if lines < minLines {
				minLines = lines
				minLinesWhere = name + " :: " + src[i].heading
			}
		}
		if starved > 0 {
			starvedEntries++
		}

		for i := range src {
			if src[i].heading == "" {
				continue
			}
			if !strings.Contains(out, src[i].heading) {
				lostHeading++
				t.Errorf("%s: LOST heading %q", name, src[i].heading)
			}
		}
	}

	t.Logf("ENTRIES=%d OVER_CAP=%d BODY_SECTIONS=%d", len(names), overCap, bodySections)
	t.Logf("ZERO_BODY=%d (starved entries=%d)", zeroBody, starvedEntries)
	t.Logf("MID_WORD=%d", midWord)
	t.Logf("LOST_HEADINGS=%d", lostHeading)
	t.Logf("NOT_PREFIX=%d", notPrefix)
	t.Logf("ORDER_MISMATCH=%d", orderMismatch)
	t.Logf("OUTPUT_OVER_CAP=%d", overCapOut)
	t.Logf("ONE_LINE_SECTIONS=%d (of %d body sections)", oneLineSections, bodySections)
	t.Logf("TINY_SECTIONS(<=12B)=%d avg_excerpt_bytes=%.1f", tinySections, float64(excerptBytes)/float64(max(bodySections, 1)))
	if tinyWhere != "" {
		t.Logf("TINY_EXAMPLE: %s", tinyWhere)
	}
	if minLines < (1 << 30) {
		t.Logf("MIN_BODY_LINES=%d (in %s)", minLines, minLinesWhere)
	} else {
		t.Logf("MIN_BODY_LINES=n/a")
	}
}

func TestZZDebugOne(t *testing.T) {
	const ws = "/home/maple/.picoclaw/workspace"
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tool_knowledge.recovery-forensics", "edit_file.mem-anchor"} {
		raw, err := os.ReadFile(filepath.Join(store.Root(), name+".md"))
		if err != nil {
			continue
		}
		body := extractBody(string(raw))
		rendered := renderKnowledgeFile(name, body, time.Now().UTC())
		digest := store.LoadForEscalation(name)
		_, srcBody := splitFrontmatter(rendered)
		src := splitH2Sections(srcBody)
		_, outBody := splitFrontmatter(digest)
		dst := splitH2Sections(outBody)
		t.Logf("=== %s: srcSections=%d dstSections=%d", name, len(src), len(dst))
		for i := 0; i < len(src) && i < len(dst); i++ {
			s := strings.TrimSpace(src[i].body)
			d := strings.TrimSpace(dst[i].body)
			if s == "" {
				continue
			}
			if !strings.HasPrefix(s, d) {
				t.Logf("  MISMATCH idx=%d heading=%q", i, src[i].heading)
				t.Logf("    srcLen=%d dstLen=%d", len(s), len(d))
				t.Logf("    SRC[0:120]=%q", s[:min(120, len(s))])
				t.Logf("    DST[0:120]=%q", d[:min(120, len(d))])
			}
		}
	}
}

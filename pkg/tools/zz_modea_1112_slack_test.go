package tools

// zz_modea_1112_slack_test.go — Mode A p1112 (2026-11-12): the SLACK
// metric for the tool_knowledge store.
//
// THE QUESTION THIS FILE ANSWERS
//
// A knowledge entry is "healthy" when the production digest keeps every
// section body visible. zz_live_measure_test.go reports that as a
// LAGGING indicator (ZERO_BODY: did it already break?). This file
// reports the LEADING indicator: how much room is LEFT before it breaks.
//
//	slack = room - fixed - SUM(minExcerpt over body-bearing sections)
//
//	room       = KnowledgeSizeCap - len(frontmatter) - len(truncationMarker)
//	fixed      = SUM over sections of (heading bytes + 1), plus 1 per
//	             body-bearing section  (the digest's own overhead)
//	minExcerpt = FIRST WORD of the FIRST BODY LINE  (the cheapest
//	             excerpt that is never cut mid-word)
//
// slack < 0 => the minimum excerpts NO LONGER FIT => sections lose
//	             their body (they become STARVED, invisible to the LLM).
// slack ~ 0 => the entry is about to starve: one more heading, or one
//	             appended lesson, pushes it over.
//
// WHY THE FORMULA IS CROSS-CHECKED AGAINST PRODUCTION
//
// The formula above MIRRORS allocateDigest's fixed-cost arithmetic.
// That mirror is a DRIFT HAZARD: production could change its overhead
// accounting and this file would keep reporting "healthy" from a
// formula that no longer describes reality.
//
// So this file does NOT trust its own arithmetic. It calls the real
// allocateDigest() and compares the two answers. If they ever disagree,
// the test FAILS instead of quietly ranking entries by a stale metric.
//
// THE KNOWN GAP (stated, not hidden)
//
// slack predicts the allocator's MINIMUM-EXCERPT starvation only. It
// does NOT predict the headingsOnly fallback: when the rendered digest
// does not fit the cap, digestSections drops EVERY body, whatever the
// slack says. Assert 3 below closes that gap.
//
// MEASURED 2026-11-12 against the live store: 64 over-cap entries,
// NEGATIVE_SLACK=0, ZERO_BODY=0. The tightest entry is `exec`
// (slack=17, 10 H2 sections) -- one more heading, or one lesson whose
// first word is longer, would starve it.
//
// ESCAPE DISCIPLINE: this file avoids escape sequences inside string
// and rune literals so it survives JSON-decoding round trips intact.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// zzSlackRow is one entry's slack accounting.
type zzSlackRow struct {
	name     string
	fileB    int
	h2       int
	bodySect int
	fixed    int
	sumMin   int
	slack    int
	room     int
	starved  int
	fallback bool
}

func TestZZModea1112Slack(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	names, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	nl := string(rune(digestNL))
	var rows []zzSlackRow
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(store.Root(), name+".md"))
		if err != nil {
			continue
		}
		body := extractBody(string(raw))
		rendered := renderKnowledgeFile(name, body, time.Now().UTC())
		if len(rendered) <= KnowledgeSizeCap {
			continue
		}
		head, bodyOnly := splitFrontmatter(rendered)
		sections := splitH2Sections(bodyOnly)
		room := KnowledgeSizeCap - len(head) - len(truncationMarker)
		fixed := 0
		sumMin := 0
		bodySect := 0
		for _, s := range sections {
			if s.heading != "" {
				fixed += len(s.heading) + 1
			}
			bt := strings.TrimSpace(s.body)
			if bt != "" {
				fixed++
				bodySect++
				firstLine := strings.Split(bt, nl)[0]
				sumMin += len(digestMinWordSafeExcerpt(firstLine))
			}
		}
		// PRODUCTION GROUND TRUTH: ask the real allocator which sections
		// keep a body. plan[i] is index-aligned with sections[i].
		plan := allocateDigest(sections, room)
		starved := 0
		for i, p := range plan {
			if strings.TrimSpace(sections[i].body) == "" {
				continue
			}
			if !p.hasBody {
				starved++
			}
		}
		// The headingsOnly fallback: when the plan does not fit the cap,
		// digestSections drops EVERY body regardless of slack.
		_, fits := renderDigestAlloc(head, plan, KnowledgeSizeCap)
		rows = append(rows, zzSlackRow{
			name:     name,
			fileB:    len(raw),
			h2:       len(sections),
			bodySect: bodySect,
			fixed:    fixed,
			sumMin:   sumMin,
			slack:    room - fixed - sumMin,
			room:     room,
			starved:  starved,
			fallback: !fits,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].slack < rows[j].slack })
	t.Logf("OVER_CAP=%d (rows sorted by slack asc)", len(rows))
	for i, r := range rows {
		if i >= 12 {
			break
		}
		t.Logf("slack=%-7d file=%-7d h2=%-3d bodySect=%-3d room=%-5d fixed=%-5d sumMin=%-5d starved=%-3d fallback=%-5v %s",
			r.slack, r.fileB, r.h2, r.bodySect, r.room, r.fixed, r.sumMin, r.starved, r.fallback, r.name)
	}
	neg := 0
	for _, r := range rows {
		if r.slack < 0 {
			neg++
		}
	}
	t.Logf("NEGATIVE_SLACK=%d of %d over-cap entries", neg, len(rows))

	// ASSERT 1 -- the formula and the production allocator must AGREE.
	// If production changes its overhead accounting, this fails loudly
	// instead of the metric silently going stale.
	for _, r := range rows {
		predicted := 0
		if r.slack < 0 {
			predicted = 1
		}
		actual := 0
		if r.starved > 0 {
			actual = 1
		}
		if predicted != actual {
			t.Errorf("FORMULA DRIFT %s: slack=%d predicts starvation=%d but the production allocator starved %d section(s)",
				r.name, r.slack, predicted, r.starved)
		}
	}

	// ASSERT 2 -- no entry may be past the edge. A negative slack means
	// the production digest DROPS section bodies for that entry, i.e.
	// the lessons inside become invisible to the LLM.
	for _, r := range rows {
		if r.slack < 0 {
			t.Errorf("STARVED ENTRY %s: slack=%d (room=%d fixed=%d sumMin=%d) -- %d section(s) lose their body",
				r.name, r.slack, r.room, r.fixed, r.sumMin, r.starved)
		}
	}

	// ASSERT 3 -- close the gap the slack metric does not cover: a plan
	// that does not fit the cap falls back to headingsOnly, which drops
	// EVERY body even though slack may be positive.
	for _, r := range rows {
		if r.fallback {
			t.Errorf("HEADINGS_ONLY FALLBACK %s: the digest plan does not fit the cap -> every section body is dropped (slack=%d)",
				r.name, r.slack)
		}
	}
}

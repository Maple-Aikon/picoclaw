package tools

// zz_modea_1112_stale_sequencer_test.go — Mode A p1112 (2026-11-12):
// the stale-cherry-pick lesson must be AUTO-LOADABLE.
//
// THE CONTRACT (from the p76e/p76f lessons):
//
//	a lesson is only auto-loadable if its needle rides in the H2 TITLE,
//	because the body excerpt is a water-fill share that shrinks as the
//	entry grows.
//
// The stale-sequencer lesson was appended as a NEW H2 section on
// 2026-11-12, which pushed exec.md's fixed overhead past the room and
// starved 4 sections (slack -18 => ZERO_BODY 4). The fix compacted the
// over-long H2 titles (fixed 1928 -> 1603, slack -18 -> +307) and
// moved the full original titles to H3 inside their own section.
//
// This test asserts the lesson is visible on the REAL runtime path
// (LoadForEscalation), and that the entry as a whole is not starved.
//
// ESCAPE DISCIPLINE: this file avoids escape sequences inside string
// and rune literals so it survives JSON-decoding round trips intact.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestP1112StaleSequencerLessonAutoLoads(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	payload := store.LoadForEscalation("exec")
	if payload == "" {
		t.Fatal("empty payload")
	}

	// (1) The owning H2 must exist, matched by PREFIX so that a later
	// regroup (which appends to the title) does not break this.
	const ownerPrefix = "## Stale git cherry-pick sequencer"
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

	// (2) THE CONTRACT: the needles must be in the TITLE itself, not
	// only in the body. A body-only needle is invisible at runtime
	// whenever the body excerpt is starved.
	for _, n := range []string{"`--abort`", "`--continue`"} {
		if !strings.Contains(owner, n) {
			t.Errorf("NEEDLE NOT IN THE H2 TITLE (body-only needle is invisible at runtime): %q owner=%q",
				n, owner)
		}
	}

	// (3) The title-bytes lesson must also ride a title: it is the
	// rule that keeps this entry from starving again.
	foundFixedOverhead := false
	for _, line := range strings.Split(payload, p76dNL) {
		if strings.HasPrefix(line, "## ") && strings.Contains(line, "FIXED OVERHEAD") {
			foundFixedOverhead = true
			break
		}
	}
	if !foundFixedOverhead {
		t.Errorf("TITLE-BYTES RULE NOT VISIBLE in any H2 title; payload titles:%s%s",
			p76dNL, p76eTitles(payload))
	}

	// (4) NO section may be starved. This is the regression this whole
	// compaction exists for: slack was -18, so four sections lost their
	// body and their lessons became invisible at runtime.
	raw, err := os.ReadFile(filepath.Join(store.Root(), "exec.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := extractBody(string(raw))
	rendered := renderKnowledgeFile("exec", body, time.Now().UTC())
	head, srcBody := splitFrontmatter(rendered)
	secs := splitH2Sections(srcBody)
	room := KnowledgeSizeCap - len(head) - len(truncationMarker)
	plan := allocateDigest(secs, room)
	starved := 0
	for i, p := range plan {
		if strings.TrimSpace(secs[i].body) == "" {
			continue
		}
		if !p.hasBody {
			starved++
			t.Errorf("STARVED section [%d] %q -- its body is invisible at runtime",
				i, secs[i].heading)
		}
	}
	if starved != 0 {
		t.Errorf("STARVED sections=%d (want 0)", starved)
	}

	t.Logf("payload=%d B owner=%q", len(payload), owner)
}

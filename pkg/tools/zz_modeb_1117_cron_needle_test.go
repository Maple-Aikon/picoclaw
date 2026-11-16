package tools

// zz_modeb_1117_cron_needle_test.go — Mode B 2026-11-17 needle probe.
//
// Question this run must answer with a MEASUREMENT, not an assumption:
//   does the new cron.md lesson (the "cron says ok but the turn died" trap)
//   actually SURVIVE the 2 KB auto-load digest?
//
// The store's own rule (tool_knowledge.md, 2026-11-11) says a lesson placed
// in an H3 does NOT survive the digest: the digest renders H2 + the FIRST
// body line of each H2, so H3 content is dropped.  Mode A 2026-11-17
// measured the opposite constraint: a NEW H2 costs fixed overhead and can
// flip slack positive -> negative.
//
// So the lesson was added as an H3 inside an EXISTING H2 (H2 count stayed
// at 3).  This probe measures whether the needles are still visible in the
// auto-load payload, and how much body each section kept.
//
// READ ONLY.  Run:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace go test ./pkg/tools/ -run TestZZModeB1117CronNeedle -count=1 -v

import (
	"fmt"
	"strings"
	"testing"
)

func TestZZModeB1117CronNeedle(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	entry := "cron"
	auto := store.LoadForEscalation(entry)

	fmt.Println("=== Mode B 2026-11-17 cron.md needle probe ===")
	fmt.Println("entry=" + entry + "  autoBytes=" + fmt.Sprint(len(auto)) +
		"  cap=" + fmt.Sprint(KnowledgeSizeCap))

	// The needles that MUST be visible if the lesson auto-loads.
	needles := []string{
		"lastStatus",
		"cooldown",
		"Token Plan",
		"goal/archive",
		"UTC",
		"20261116T172928Z",
	}
	fmt.Println("--- needles in the AUTO-LOAD payload ---")
	missing := 0
	for _, n := range needles {
		ok := strings.Contains(auto, n)
		if !ok {
			missing++
		}
		fmt.Println("  " + n + " => " + fmt.Sprint(ok))
	}

	// How much body did each H2 section keep?
	fmt.Println("--- body bytes per H2 section in the AUTO-LOAD payload ---")
	secs := splitH2Sections(auto)
	for i, s := range secs {
		h := s.heading
		if h == "" {
			h = "<preamble>"
		}
		if len(h) > 62 {
			h = h[:62]
		}
		fmt.Println("  [" + fmt.Sprint(i) + "] " + fmt.Sprint(len(s.body)) + " B  " + h)
	}

	// The lesson must also be findable via the explicit read action
	// (action=read returns the FULL body, no cap).
	full, _ := store.Read(entry)
	fmt.Println("--- store.Read(" + entry + ") bytes=" + fmt.Sprint(len(full)) +
		" (full body, no cap) ---")
	fmt.Println("  lesson present in full body: " +
		fmt.Sprint(strings.Contains(full, "Token Plan")))

	if missing > 0 {
		fmt.Println("RESULT: " + fmt.Sprint(missing) + "/" + fmt.Sprint(len(needles)) +
			" needles MISSING from the auto-load payload")
	} else {
		fmt.Println("RESULT: all " + fmt.Sprint(len(needles)) +
			" needles PRESENT in the auto-load payload")
	}
}

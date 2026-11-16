package tools

// zz_modeb_1117_cron_layout_test.go — Mode B 2026-11-17 LAYOUT PROBE.
//
// The first attempt at this run added the cron lesson as an H3 inside an
// existing H2 (H2 count unchanged at 3) and the needle probe showed
// 6/6 needles MISSING from the auto-load payload.  So the H3 placement does
// NOT work for cron.md.
//
// Why: allocateDigest gives each section the FIRST BODY LINE as its
// minimum excerpt and water-fills from the TOP of the body.  An H3 lesson
// placed at the END of a section is never the first body line, so it never
// reaches the digest.
//
// Mode A 2026-11-17 measured the other constraint: a NEW H2 costs FIXED
// overhead (heading bytes are charged before any body excerpt), which can
// flip slack positive -> negative.
//
// The two rules conflict for cron.md.  This probe MEASURES the candidate
// layouts instead of guessing:
//   L0 = current file (H3 at the end of the last H2)
//   L1 = lesson as the FIRST body line of the LAST H2 section
//   L2 = lesson as a NEW H2 section at the END of the file
//   L3 = lesson as a NEW H2 section in the MIDDLE
//
// READ ONLY.  It computes digests; it never writes to the store.
//
// Run:
//   PICOCLAW_TEST_WORKSPACE=/home/maple/.picoclaw/workspace go test ./pkg/tools/ -run TestZZModeB1117CronLayout -count=1 -v

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const modebNL = string(rune(10))

func modebLayoutNeedles() []string {
	return []string{
		"lastStatus",
		"cooldown",
		"Token Plan",
		"goal/archive",
	}
}

// modebOldNeedles are the PRE-EXISTING lessons in cron.md.  A layout that
// makes the new lesson visible but starves the old ones is a REGRESSION:
// the digest is a fixed-size budget, so every byte of the new lesson is
// taken from an old one.
//
// NOTE: these were derived from the PRISTINE file (the pre-edit backup),
// not from a guess.  An earlier revision listed "rate_limit_error", which
// was NEVER in cron.md -- that made the baseline look like 3/6 instead of
// the true 4/6 and would have hidden a real regression.
func modebOldNeedles() []string {
	return []string{
		"agent_turn",
		"fd/255",
		"deleteAfterRun",
		"dlq_recent",
		"window-21",
	}
}

// modebLessonHead is the condensed lesson body used by the candidate layouts.
const modebLessonHead = "**Cron `lastStatus` KHÔNG phản ánh trạng thái turn** — turn LLM chết ở iteration 1 (HTTP 429 \"Token Plan usage limit reached\" ⇒ cả 3 candidate `skipped (cooldown)`) ⇒ `complete_goal` không chạy ⇒ **KHÔNG có file trong `memory/goal/archive/`** ⇒ đó mới là dấu hiệu đáng tin, KHÔNG phải `lastStatus`. Đo 11-17: Mode A **và** Mode B đều chết cùng lúc 3 đêm (11-14/15/16) ⇒ 3 ngày không có memory-optimization nào chạy, mà cron vẫn báo `ok`. 🪤 Filename goal-archive mang timestamp **UTC** (cron expr là giờ ICT ⇒ lệch 7 giờ). 🪤 `sessions/<key>.jsonl` CÓ mặt khác turn thành công (file ghi lúc turn START). 📌 Đếm bằng goal-archive, đừng tin `lastStatus`. Chi tiết: `memory/202611/20261117.md` + R1 `tool_knowledge/cron.md`."

// modebLessonShort is a much more compact version of the same lesson.
// The digest has only ~2 KB to spend, so the lesson cost matters.
const modebLessonShort = "**Cron `lastStatus` ≠ trạng thái turn** — turn chết ở iteration 1 (HTTP 429 \"Token Plan usage limit reached\" ⇒ cả 3 candidate `skipped (cooldown)`) ⇒ `complete_goal` không chạy ⇒ **KHÔNG có file trong `memory/goal/archive/`** ⇒ đó mới là dấu hiệu đáng tin. Đo 11-17: Mode A **và** Mode B chết cùng lúc 3 đêm (11-14/15/16) ⇒ 3 ngày không có memory-optimization nào chạy, mà cron vẫn báo `ok`. 🪤 Filename goal-archive là **UTC** (cron expr giờ ICT ⇒ lệch 7 giờ). 🪤 `sessions/*.jsonl` CÓ mặt ≠ turn thành công (ghi lúc turn START). 📌 Đếm bằng goal-archive, đừng tin `lastStatus`. Chi tiết: daily 20261117."

func modebScore(t *testing.T, label, body string) {
	t.Helper()
	rendered := renderKnowledgeFile("cron", body, time.Unix(0, 0).UTC())
	dig := rendered
	if len(dig) > KnowledgeSizeCap {
		dig = digestSections(dig, KnowledgeSizeCap)
	}
	present := 0
	for _, n := range modebLayoutNeedles() {
		if strings.Contains(dig, n) {
			present++
		}
	}
	oldPresent := 0
	for _, n := range modebOldNeedles() {
		if strings.Contains(dig, n) {
			oldPresent++
		}
	}
	fmt.Println("--- " + label + " ---")
	fmt.Println("  srcBytes=" + fmt.Sprint(len(rendered)) +
		"  digestBytes=" + fmt.Sprint(len(dig)) +
		"  cap=" + fmt.Sprint(KnowledgeSizeCap) +
		"  NEWneedles=" + fmt.Sprint(present) + "/" + fmt.Sprint(len(modebLayoutNeedles())) +
		"  OLDneedles=" + fmt.Sprint(oldPresent) + "/" + fmt.Sprint(len(modebOldNeedles())))
	secs := splitH2Sections(dig)
	for i, s := range secs {
		h := s.heading
		if h == "" {
			h = "<preamble>"
		}
		if len(h) > 50 {
			h = h[:50]
		}
		fmt.Println("    [" + fmt.Sprint(i) + "] body=" + fmt.Sprint(len(s.body)) + " B  " + h)
	}
}

func TestZZModeB1117CronLayout(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	path := filepath.Join(store.Root(), "cron.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	src := string(raw)
	body := extractBody(src)

	fmt.Println("=== Mode B 2026-11-17 cron.md LAYOUT PROBE ===")
	fmt.Println("current file bytes=" + fmt.Sprint(len(src)))

	// L0 = current layout (lesson is an H3 at the end of the last H2)
	modebScore(t, "L0 current (H3 at end of last H2)", body)

	// L1 = lesson as the FIRST body line of the LAST H2 section
	l1 := modebInsertFirstBodyLine(body, modebLessonHead)
	modebScore(t, "L1 lesson as FIRST body line of last H2", l1)

	// L2 = lesson as a NEW H2 section at the END of the file
	l2 := body + modebNL + "## 2026-11-17 — cron `lastStatus` KHÔNG phản ánh trạng thái turn (429 ⇒ 3 đêm chết âm thầm)" + modebNL + modebLessonHead + modebNL
	modebScore(t, "L2 lesson as NEW H2 at END", l2)

	// L3 = lesson as a NEW H2 section in the MIDDLE (before the last H2)
	block := "## 2026-11-17 — cron `lastStatus` KHÔNG phản ánh trạng thái turn (429 ⇒ 3 đêm chết âm thầm)" + modebNL + modebLessonHead + modebNL
	l3 := modebInsertH2BeforeLast(body, block)
	modebScore(t, "L3 lesson as NEW H2 in MIDDLE", l3)

	// L1a = lesson as the FIRST body line of the FIRST H2 ("What goes wrong")
	l1a := modebInsertFirstBodyLineOfIndex(body, modebLessonHead, 0)
	modebScore(t, "L1a lesson as FIRST body line of H2 #1", l1a)

	// L1b = lesson as the FIRST body line of the SECOND H2
	l1b := modebInsertFirstBodyLineOfIndex(body, modebLessonHead, 1)
	modebScore(t, "L1b lesson as FIRST body line of H2 #2", l1b)

	// L4 = SHORT lesson as a NEW H2 at the END
	l4 := body + modebNL + "## 2026-11-17 — cron `lastStatus` KHÔNG phản ánh trạng thái turn (429 ⇒ 3 đêm chết âm thầm)" + modebNL + modebLessonShort + modebNL
	modebScore(t, "L4 SHORT lesson as NEW H2 at END", l4)

	// L5 = SHORT lesson as the FIRST body line of H2 #1
	l5 := modebInsertFirstBodyLineOfIndex(body, modebLessonShort, 0)
	modebScore(t, "L5 SHORT lesson as FIRST body line of H2 #1", l5)

	// L6 = SHORT lesson as a NEW H2 in the MIDDLE
	block6 := "## 2026-11-17 — cron `lastStatus` KHÔNG phản ánh trạng thái turn (429 ⇒ 3 đêm chết âm thầm)" + modebNL + modebLessonShort + modebNL
	l6 := modebInsertH2BeforeLast(body, block6)
	modebScore(t, "L6 SHORT lesson as NEW H2 in MIDDLE", l6)
}

// modebInsertFirstBodyLineOfIndex inserts text as the first body line of the
// idx-th "## " section (0-based).
func modebInsertFirstBodyLineOfIndex(body, text string, idx int) string {
	lines := strings.Split(body, modebNL)
	h2s := []int{}
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") {
			h2s = append(h2s, i)
		}
	}
	if idx < 0 || idx >= len(h2s) {
		return body
	}
	at := h2s[idx]
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:at+1]...)
	out = append(out, text)
	out = append(out, lines[at+1:]...)
	return strings.Join(out, modebNL)
}

// modebInsertFirstBodyLine inserts text as the first body line of the LAST
// "## " section.
func modebInsertFirstBodyLine(body, text string) string {
	lines := strings.Split(body, modebNL)
	lastH2 := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") {
			lastH2 = i
		}
	}
	if lastH2 < 0 {
		return body
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:lastH2+1]...)
	out = append(out, text)
	out = append(out, lines[lastH2+1:]...)
	return strings.Join(out, modebNL)
}

// modebInsertH2BeforeLast inserts a block just before the LAST "## " section.
func modebInsertH2BeforeLast(body, block string) string {
	lines := strings.Split(body, modebNL)
	lastH2 := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") {
			lastH2 = i
		}
	}
	if lastH2 < 0 {
		return body
	}
	out := make([]string, 0, len(lines)+2)
	out = append(out, lines[:lastH2]...)
	out = append(out, strings.Split(strings.TrimRight(block, modebNL), modebNL)...)
	out = append(out, lines[lastH2:]...)
	return strings.Join(out, modebNL)
}

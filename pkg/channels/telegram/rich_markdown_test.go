package telegram

import "testing"

// TestNormalizeRichMarkdown_MainTurn13Pattern reproduces the exact failing
// markdown from main-turn-13 iter 12 (Post-Final): a code span containing an
// escaped pipe inside a table cell. Telegram's parser falls back to a
// paragraph for this input; the normalizer must unescape pipes ONLY inside
// code spans of table rows.
func TestNormalizeRichMarkdown_MainTurn13Pattern(t *testing.T) {
	in := "### Verification\n" +
		"\n" +
		"| File | `1300\\|2000\\|MEMORY.md` hits |\n" +
		"|---|---|\n" +
		"| `SKILL.md` | 1 (line 108 — the new entry) |\n" +
		"| `structural-jobs.md` | 0 ✓ |"
	want := "### Verification\n" +
		"\n" +
		"| File | `1300|2000|MEMORY.md` hits |\n" +
		"|---|---|\n" +
		"| `SKILL.md` | 1 (line 108 — the new entry) |\n" +
		"| `structural-jobs.md` | 0 ✓ |"
	if got := normalizeRichMarkdown(in); got != want {
		t.Errorf("normalizeRichMarkdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestNormalizeRichMarkdown_PlainEscapedPipeUntouched: escaped pipes in a
// PLAIN cell (no backticks) parse fine on Telegram — must stay untouched.
func TestNormalizeRichMarkdown_PlainEscapedPipeUntouched(t *testing.T) {
	in := "| File | MEMORY.md hits |\n|---|---|\n| SKILL.md | 1 (line 108) |\n| 1300\\|2000\\|MEMORY.md | 0 |"
	if got := normalizeRichMarkdown(in); got != in {
		t.Errorf("plain-cell escaped pipes must stay untouched\n got: %q\nwant: %q", got, in)
	}
}

// TestNormalizeRichMarkdown_NonTableUntouched: a prose line with a code span
// containing \| but NO table separator must not change.
func TestNormalizeRichMarkdown_NonTableUntouched(t *testing.T) {
	in := "use `a\\|b` here please"
	if got := normalizeRichMarkdown(in); got != in {
		t.Errorf("non-table line must stay untouched\n got: %q\nwant: %q", got, in)
	}
}

// TestNormalizeRichMarkdown_SingleLineTableUntouched: a table written on ONE
// line (no newlines between rows) is not detected (no row/separator lines).
func TestNormalizeRichMarkdown_SingleLineTableUntouched(t *testing.T) {
	in := "| a | b | |---|---| | `c\\|d` | 1 |"
	if got := normalizeRichMarkdown(in); got != in {
		t.Errorf("single-line table must stay untouched\n got: %q\nwant: %q", got, in)
	}
}

// TestNormalizeRichMarkdown_MultiRowAllCells: every code span in every table
// row gets unescaped; separator variants (colon-aligned) still detected.
func TestNormalizeRichMarkdown_MultiRowAllCells(t *testing.T) {
	in := "| a | b |\n" +
		"|:---|---:|\n" +
		"| `x\\|y` | `p\\|q` |\n" +
		"| `m\\|n` | 0 |"
	want := "| a | b |\n" +
		"|:---|---:|\n" +
		"| `x|y` | `p|q` |\n" +
		"| `m|n` | 0 |"
	if got := normalizeRichMarkdown(in); got != want {
		t.Errorf("multi-row table mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestNormalizeRichMarkdown_Idempotent: applying twice yields the same output.
func TestNormalizeRichMarkdown_Idempotent(t *testing.T) {
	in := "| File | `1300\\|2000\\|MEMORY.md` hits |\n|---|---|\n| `SKILL.md` | 1 |"
	once := normalizeRichMarkdown(in)
	twice := normalizeRichMarkdown(once)
	if twice != once {
		t.Errorf("normalizer is not idempotent\n once: %q\ntwice: %q", once, twice)
	}
}

// TestNormalizeRichMarkdown_NoTableNoChange: ordinary prose with \| inside
// backticks (no table) is preserved verbatim.
func TestNormalizeRichMarkdown_NoTableNoChange(t *testing.T) {
	in := "run: `grep -n \"1300\\|2000\" file`"
	if got := normalizeRichMarkdown(in); got != in {
		t.Errorf("no-table markdown must stay untouched\n got: %q\nwant: %q", got, in)
	}
}

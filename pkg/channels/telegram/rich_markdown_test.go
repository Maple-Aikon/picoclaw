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

// TestNormalizeRichMarkdown_SingleNewlineToParagraph — Telegram rich mode
// renders a single \n as a space (verified 2026-08-09). Single newlines
// between prose lines must become a paragraph break.
func TestNormalizeRichMarkdown_SingleNewlineToParagraph(t *testing.T) {
	in := "line one\nline two"
	want := "line one\n\nline two"
	if got := normalizeRichMarkdown(in); got != want {
		t.Errorf("single newline must become paragraph break\n got: %q\nwant: %q", got, want)
	}
}

// TestNormalizeRichMarkdown_NewlineRunCollapses — runs of 2+ newlines
// collapse to exactly one paragraph break (idempotent target form).
func TestNormalizeRichMarkdown_NewlineRunCollapses(t *testing.T) {
	in := "a\n\n\nb\n\n\n\nc"
	want := "a\n\nb\n\nc"
	if got := normalizeRichMarkdown(in); got != want {
		t.Errorf("newline runs must collapse to one paragraph break\n got: %q\nwant: %q", got, want)
	}
}

// TestNormalizeRichMarkdown_ProseAroundTable — prose + table + prose: only
// single newlines OUTSIDE the table become paragraph breaks; table rows
// stay contiguous; a blank line between tables is preserved as a break.
func TestNormalizeRichMarkdown_ProseAroundTable(t *testing.T) {
	in := "intro\n| a |\n|---|---|\n| b | 1 |\n\noutro"
	want := "intro\n\n| a |\n|---|---|\n| b | 1 |\n\noutro"
	if got := normalizeRichMarkdown(in); got != want {
		t.Errorf("prose/table mix mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestNormalizeRichMarkdown_TwoTablesSeparatedByBlankLine — blank line
// between two tables must stay a paragraph break (not merge the tables).
func TestNormalizeRichMarkdown_TwoTablesSeparatedByBlankLine(t *testing.T) {
	in := "| a |\n|---|---|\n| 1 |\n\n| b |\n|---|---|\n| 2 |"
	want := "| a |\n|---|---|\n| 1 |\n\n| b |\n|---|---|\n| 2 |"
	if got := normalizeRichMarkdown(in); got != want {
		t.Errorf("two tables must stay separated\n got: %q\nwant: %q", got, want)
	}
}

// TestNormalizeRichMarkdown_ProseIdempotent — applying twice to prose with
// single newlines yields the same (already-normalized) output.
func TestNormalizeRichMarkdown_ProseIdempotent(t *testing.T) {
	in := "a\nb\n\nc"
	once := normalizeRichMarkdown(in)
	twice := normalizeRichMarkdown(once)
	if twice != once {
		t.Errorf("prose normalizer is not idempotent\n once: %q\ntwice: %q", once, twice)
	}
	if once != "a\n\nb\n\nc" {
		t.Errorf("unexpected normalized form: %q", once)
	}
}

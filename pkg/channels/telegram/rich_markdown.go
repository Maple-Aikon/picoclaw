package telegram

import (
	"regexp"
	"strings"
)

// richTableRowRE matches a markdown table row: pipe-delimited line that
// starts and ends with a pipe (allowing surrounding whitespace).
var richTableRowRE = regexp.MustCompile(`^\s*\|.*\|\s*$`)

// richTableSepRE matches a GFM table separator row (|---|---|, |:---:|, ...):
// starts and ends with a pipe, contains only spaces/tabs/colons/pipes and at
// least one dash.
var richTableSepRE = regexp.MustCompile(`^\s*\|[\s:|-]*\-[\s:|-]*\|\s*$`)

// normalizeRichMarkdown fixes a Telegram rich-markdown parser limitation:
// a table cell containing an inline code span that includes an escaped pipe
// (e.g. `1300\|2000\|MEMORY.md`) makes the whole table fall back to a plain
// paragraph (verified empirically 2026-08-09, main-turn-13). Inside detected
// table regions only, pipes escaped inside backtick code spans are unescaped
// (`\|` -> `|`), which is what the LLM meant anyway. Everything else is left
// byte-identical, so the transform is idempotent and safe for prose.
func normalizeRichMarkdown(md string) string {
	lines := strings.Split(md, "\n")

	// Pass 1: mark table rows and unescape pipes inside their code spans
	// (existing behavior).
	tableRow := make([]bool, len(lines))
	inTable := false
	for i := 0; i < len(lines); i++ {
		row := richTableRowRE.MatchString(lines[i])
		if inTable && row {
			lines[i] = unescapePipesInCodeSpans(lines[i])
			tableRow[i] = true
			continue
		}
		if !inTable && row && i+1 < len(lines) && richTableSepRE.MatchString(lines[i+1]) {
			// Header row of a new table; the separator follows.
			lines[i] = unescapePipesInCodeSpans(lines[i])
			tableRow[i] = true
			inTable = true
			continue
		}
		inTable = false
	}

	// Pass 2: collapse newline runs to exactly one paragraph break ("\n\n")
	// OUTSIDE table rows. Telegram rich mode renders a single "\n" as a
	// space (user-visible bug, 2026-08-09), so single newlines between
	// prose lines must become "\n\n". Table rows keep their single
	// newlines so the table stays contiguous. Idempotent: any run of 1+
	// newlines becomes exactly one "\n\n", and "\n\n" is unchanged.
	var b strings.Builder
	b.Grow(len(md) + 16)
	prevContent := -1 // index of the last non-empty line written
	pendingEmpty := false
	for i := 0; i < len(lines); i++ {
		if lines[i] == "" {
			pendingEmpty = true
			continue
		}
		if prevContent >= 0 {
			if !pendingEmpty && tableRow[prevContent] && tableRow[i] {
				b.WriteByte('\n') // adjacent table rows: keep contiguous
			} else {
				b.WriteString("\n\n")
			}
		}
		b.WriteString(lines[i])
		prevContent = i
		pendingEmpty = false
	}
	return b.String()
}

// unescapePipesInCodeSpans replaces `\|` with `|` only inside backtick code
// spans of a single line. A span is opened by a backtick run of length n and
// closed by the next run of exactly n backticks; if no closing run exists the
// rest of the line is treated as span content (defensive, mirrors how code
// spans behave in practice for LLM output).
func unescapePipesInCodeSpans(line string) string {
	if !strings.Contains(line, "`") || !strings.Contains(line, `\|`) {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	i := 0
	for i < len(line) {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		// Opening backtick run.
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		n := j - i
		b.WriteString(line[i:j])
		i = j
		closed := false
		for i < len(line) {
			k := strings.IndexByte(line[i:], '`')
			if k < 0 {
				break // unclosed span: content runs to end of line
			}
			pos := i + k
			m := pos
			for m < len(line) && line[m] == '`' {
				m++
			}
			if m-pos == n {
				// Matching closing run: unescape the content between.
				b.WriteString(strings.ReplaceAll(line[i:pos], `\|`, "|"))
				b.WriteString(line[pos:m])
				i = m
				closed = true
				break
			}
			// Different-length run: it is span content, keep scanning.
			i = m
		}
		if !closed {
			b.WriteString(strings.ReplaceAll(line[i:], `\|`, "|"))
			i = len(line)
		}
	}
	return b.String()
}

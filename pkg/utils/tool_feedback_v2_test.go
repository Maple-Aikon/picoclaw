package utils

import (
	"strconv"
	"strings"
	"testing"
)

// Phase 12.58.3 -- tests for the pendingExplanation parameter.
// Want strings built as concatenation of:
//   - backtick raw string literals (safe for newlines + unicode)
//   - double-quote strings containing literal backticks (tool names)
//
// Actual FormatToolFeedbackMessage joins body lines with single
// newlines and joins header+iterContext+body with double newlines.

func TestFormatToolFeedbackMessage_V2_EmptyPendingExplanationOmitsDivider(t *testing.T) {
	got := FormatToolFeedbackMessage(
		"read_file",
		"reading plan",
		`{"path":"/tmp/x"}`,
		"📊 #3/250 · Goal-Checkpoint",
		"",
	)
	want := `🔧 ` + "`" + `read_file` + "`" + `

📊 #3/250 · Goal-Checkpoint

reading plan
` + "`" + "`" + "`" + `json
{"path":"/tmp/x"}
` + "`" + "`" + "`"
	if got != want {
		t.Errorf(
			`mismatch
` +
			`got:  %q
` +
			`want: %q
` +
			`diff: %s`,
			got, want, diffStrings(got, want),
		)
	}
}

func TestFormatToolFeedbackMessage_V2_PendingExplanationAppendsBelowArgs(t *testing.T) {
	got := FormatToolFeedbackMessage(
		"web_search",
		"searching docs",
		`{"query":"go escape sequences"}`,
		"",
		"Continuing the current task.",
	)
	want := `🔧 ` + "`" + `web_search` + "`" + `

searching docs
` + "`" + "`" + "`" + `json
{"query":"go escape sequences"}
` + "`" + "`" + "`" + `
──────────
💬 Continuing the current task.`
	if got != want {
		t.Errorf(
			`mismatch
` +
			`got:  %q
` +
			`want: %q
` +
			`diff: %s`,
			got, want, diffStrings(got, want),
		)
	}
}

func TestFormatToolFeedbackMessage_V2_PendingExplanationTrimsWhitespace(t *testing.T) {
	got := FormatToolFeedbackMessage(
		"exec",
		"running test",
		`{"cmd":"go test"}`,
		"",
		`   
   Continuing the current task.  
  `,
	)
	want := `🔧 ` + "`" + `exec` + "`" + `

running test
` + "`" + "`" + "`" + `json
{"cmd":"go test"}
` + "`" + "`" + "`" + `
──────────
💬 Continuing the current task.`
	if got != want {
		t.Errorf(
			`mismatch
` +
			`got:  %q
` +
			`want: %q
` +
			`diff: %s`,
			got, want, diffStrings(got, want),
		)
	}
}

func TestFormatToolFeedbackMessage_V2_PendingExplanationWithIterContext(t *testing.T) {
	got := FormatToolFeedbackMessage(
		"mcp__signet__memory_search",
		"recalling prior decisions",
		`{"query":"tool feedback ordering"}`,
		"📊 #7/250",
		"tool feedback ordering resolved in 12.58.3 -- folded into tracked card",
	)
	want := `🔧 ` + "`" + `mcp__signet__memory_search` + "`" + `

📊 #7/250

recalling prior decisions
` + "`" + "`" + "`" + `json
{"query":"tool feedback ordering"}
` + "`" + "`" + "`" + `
──────────
💬 tool feedback ordering resolved in 12.58.3 -- folded into tracked card`
	if got != want {
		t.Errorf(
			`mismatch
` +
			`got:  %q
` +
			`want: %q
` +
			`diff: %s`,
			got, want, diffStrings(got, want),
		)
	}
}

func TestFormatToolFeedbackMessage_V2_PendingExplanationOnlyToolName(t *testing.T) {
	got := FormatToolFeedbackMessage(
		"cron",
		"",
		"",
		"",
		"recurring reminder set",
	)
	want := `🔧 ` + "`" + `cron` + "`" + `

──────────
💬 recurring reminder set`
	if got != want {
		t.Errorf(
			`mismatch
` +
			`got:  %q
` +
			`want: %q
` +
			`diff: %s`,
			got, want, diffStrings(got, want),
		)
	}
	if !strings.Contains(got, "──────────") {
		t.Errorf("expected divider line, got %q", got)
	}
	if !strings.Contains(got, "💬 recurring reminder set") {
		t.Errorf("expected speech-balloon line, got %q", got)
	}
}

func diffStrings(got, want string) string {
	var b strings.Builder
	min := len(got)
	if len(want) < min {
		min = len(want)
	}
	for i := 0; i < min; i++ {
		if got[i] != want[i] {
			b.WriteString("first diff at byte ")
			b.WriteString(strconv.Itoa(i))
			b.WriteString(": got=")
			b.WriteString(strconv.QuoteRune(rune(got[i])))
			b.WriteString(" want=")
			b.WriteString(strconv.QuoteRune(rune(want[i])))
			break
		}
	}
	return b.String()
}

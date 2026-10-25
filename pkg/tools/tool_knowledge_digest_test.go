package tools

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// buildManySectionBody builds a body with n H2 sections, each with a
// heading and a body paragraph long enough to be truncated by the digest.
func buildManySectionBody(n int, paraLen int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("## Section ")
		b.WriteString(itoaTest(i))
		b.WriteString("\n")
		b.WriteString(strings.Repeat("x", paraLen))
		b.WriteString("\n\n")
	}
	return b.String()
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// TestDigestSections_NeverExceedsCap is the core invariant: whatever the
// input, the digest output INCLUDING the truncation marker must fit the
// cap. The old hardTruncate violated this (it cut to cap and THEN
// appended the marker, producing 2057 bytes for a 2048-byte cap).
func TestDigestSections_NeverExceedsCap(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"many sections", renderKnowledgeFile("exec", buildManySectionBody(31, 900), time.Now())},
		{"no frontmatter", buildManySectionBody(20, 500)},
		{"horizontal rules inside", "## A\n" + strings.Repeat("y", 400) + "\n\n---\n" + "## B\n" + strings.Repeat("z", 400)},
		{"no h2 at all", strings.Repeat("plain text line\n", 500)},
		{"single huge section", "## Only\n" + strings.Repeat("q", 9000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := digestSections(tc.content, KnowledgeSizeCap)
			if len(out) > KnowledgeSizeCap {
				t.Errorf("digest exceeded cap: got=%d cap=%d", len(out), KnowledgeSizeCap)
			}
			if !utf8.ValidString(out) {
				t.Errorf("digest produced invalid UTF-8")
			}
		})
	}
}

// TestDigestSections_KeepsEveryH2Heading is the lesson-survivability
// invariant. The old head-truncation dropped every section after the
// first ~2 KB -- and since lessons are APPENDED, the newest lesson was
// exactly what got lost.
func TestDigestSections_KeepsEveryH2Heading(t *testing.T) {
	body := buildManySectionBody(31, 900)
	content := renderKnowledgeFile("exec", body, time.Now())
	out := digestSections(content, KnowledgeSizeCap)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		if !strings.Contains(out, line) {
			t.Errorf("digest dropped H2 heading %q", line)
		}
	}
}

// TestDigestSections_KeepsFrontmatter verifies the YAML header survives
// so extractBody can still find the body.
func TestDigestSections_KeepsFrontmatter(t *testing.T) {
	content := renderKnowledgeFile("fs.read", buildManySectionBody(10, 400), time.Now())
	out := digestSections(content, KnowledgeSizeCap)
	if !strings.HasPrefix(out, "---\ntool: fs.read\n") {
		t.Errorf("digest lost the frontmatter header: %.80q", out)
	}
	if extractBody(out) == "" {
		t.Errorf("body unrecoverable after digest")
	}
}

// TestDigestSections_KeepsFrontmatterWhenFileHasNoFrontmatter guards the
// opposite direction: a file that does NOT start with "---" must not be
// treated as frontmatter. A bare search for the terminator finds
// horizontal rules mid-document and would swallow the whole file.
func TestDigestSections_NoFalseFrontmatter(t *testing.T) {
	content := "## A\n" + strings.Repeat("y", 400) + "\n\n---\n" +
		"## B\n" + strings.Repeat("z", 400) + "\n" +
		"## C\n" + strings.Repeat("w", 400)
	out := digestSections(content, KnowledgeSizeCap)
	if !strings.Contains(out, "## A") || !strings.Contains(out, "## B") || !strings.Contains(out, "## C") {
		t.Errorf("digest lost headings when the document contains a horizontal rule")
	}
}

// TestDigestSections_MarksTruncation pins the marker so callers (and the
// LLM) can tell the body was cut.
func TestDigestSections_MarksTruncation(t *testing.T) {
	content := renderKnowledgeFile("exec", buildManySectionBody(31, 900), time.Now())
	out := digestSections(content, KnowledgeSizeCap)
	if !strings.Contains(out, "... [truncated]") {
		t.Errorf("digest is missing its truncation marker")
	}
}

// TestDigestSections_UnderCapIsUnchanged pins that small entries are NOT
// digested -- the digest only applies to oversized ones.
func TestDigestSections_UnderCapIsUnchanged(t *testing.T) {
	content := "## Small\njust a little text\n"
	out := digestSections(content, KnowledgeSizeCap)
	if out != content {
		t.Errorf("under-cap content was modified: %q", out)
	}
}

// TestLoadForEscalation_OverCapEntryKeepsAllHeadings is the END-TO-END
// invariant for the read path: an oversized entry must surface every
// lesson heading, not just the first ones.
func TestLoadForEscalation_OverCapEntryKeepsAllHeadings(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}
	body := buildManySectionBody(31, 900)
	if _, _, err := s.Replace("exec", body); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got := s.LoadForEscalation("exec")
	if got == "" {
		t.Fatalf("LoadForEscalation returned empty despite save")
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		if !strings.Contains(got, line) {
			t.Errorf("LoadForEscalation dropped H2 heading %q", line)
		}
	}
}

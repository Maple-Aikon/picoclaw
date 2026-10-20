package tools

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Regression tests for the two defects measured on 2026-10-20.
//
// Defect A: Save() applied smartTruncate BEFORE os.WriteFile, so a body over
// the cap was silently shortened on disk while Save returned nil error and
// doSave printed a success line. Measured live: 300 sentinel strings in,
// 151 out, 149 lost, "Saved knowledge ... (2065 bytes)".
//
// Defect B: smartTruncate cut at a raw byte offset, which can split a
// multi-byte UTF-8 rune and write an invalid file to disk. Measured with
// Vietnamese text: 4/8 and 5/8 shift offsets produced invalid UTF-8.
//
// NOTE: multi-line Go string literals below are raw (backtick) strings on
// purpose. An interpreted "..." literal cannot contain a real newline, and
// writing one as an escape sequence gets mangled by the tool transport.

func timeForTest() time.Time {
	return time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestKnowledgeStore_Save_DoesNotLoseBodyAboveCap is the core regression.
// Save must persist the body verbatim. Truncation is a read-path concern.
func TestKnowledgeStore_Save_DoesNotLoseBodyAboveCap(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	body := "## What goes wrong" + `
` + strings.TrimSpace(strings.Repeat("SENTINEL-RULE payload text. ", 120))
	wantSent := strings.Count(body, "SENTINEL-RULE")

	path, size, err := s.Replace("fs.read", body)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if size <= KnowledgeSizeCap {
		t.Fatalf("test body is not above the cap: size=%d cap=%d", size, KnowledgeSizeCap)
	}

	raw := readFileString(t, path)
	gotSent := strings.Count(raw, "SENTINEL-RULE")
	if gotSent != wantSent {
		t.Errorf("Save lost body: sentinels in=%d out=%d (lost %d)",
			wantSent, gotSent, wantSent-gotSent)
	}
	if strings.Contains(raw, "... [truncated]") {
		t.Errorf("Save wrote a truncation marker to disk; body was not persisted verbatim")
	}
}

// TestKnowledgeStore_Save_BodySurvivesRoundTrip proves the same thing through
// the public Read path, which is what a future turn actually sees.
func TestKnowledgeStore_Save_BodySurvivesRoundTrip(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	const marker = "TAIL-SENTINEL-LAST-LINE-MUST-SURVIVE"
	body := `## What goes wrong
` + strings.Repeat("filler line for the cap. ", 150) + `
` + marker

	if _, _, err := s.Replace("web.fetch", body); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Read("web.fetch")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(got, marker) {
		t.Errorf("Read lost the tail of a saved body; marker absent. body=%d cap=%d",
			len(body), KnowledgeSizeCap)
	}
}

// TestKnowledgeStore_Save_AlwaysWritesValidUTF8 pins defect B at the write
// boundary: whatever the body contains, the file on disk must parse.
func TestKnowledgeStore_Save_AlwaysWritesValidUTF8(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	// U+0111 is 2 bytes; U+1ED9 and U+1E63 are 3 bytes.
	for _, r := range []rune{'đ', 'ộ', 'ậ'} {
		for shift := 0; shift < 8; shift++ {
			body := `## What goes wrong
` + strings.Repeat("x", shift) + strings.Repeat(string(r), 2000) + `
`
			path, _, err := s.Replace("fs.read", body)
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			raw := readFileString(t, path)
			if !utf8.ValidString(raw) {
				t.Errorf("Save wrote invalid UTF-8: char=%q shift=%d", string(r), shift)
			}
		}
	}
}

// TestSmartTruncate_UTF8Safe pins defect B directly at the helper level:
// whatever byteCap is given, the result must be valid UTF-8.
func TestSmartTruncate_UTF8Safe(t *testing.T) {
	for _, r := range []rune{'đ', 'ộ', 'ậ'} {
		for shift := 0; shift < 8; shift++ {
			body := `## What goes wrong
` + strings.Repeat("x", shift) + strings.Repeat(string(r), 2000) + `
`
			content := renderKnowledgeFile("fs.read", body, timeForTest())
			for _, cap := range []int{
				KnowledgeSizeCap, KnowledgeSizeCap + 1,
				KnowledgeSizeCap + 2, KnowledgeSizeCap + 3,
			} {
				got := smartTruncate(content, cap)
				if !utf8.ValidString(got) {
					t.Errorf("smartTruncate produced invalid UTF-8: char=%q shift=%d cap=%d",
						string(r), shift, cap)
				}
			}
		}
	}
}

// TestSmartTruncate_NeverExceedsCap is the property the read path still needs.
func TestSmartTruncate_NeverExceedsCap(t *testing.T) {
	content := renderKnowledgeFile("fs.read", strings.Repeat("x", 5000), timeForTest())
	got := smartTruncate(content, KnowledgeSizeCap)
	if len(got) > KnowledgeSizeCap+32 {
		t.Errorf("smartTruncate exceeded cap by more than the marker: len=%d cap=%d",
			len(got), KnowledgeSizeCap)
	}
	if !strings.Contains(got, "... [truncated]") {
		t.Errorf("truncated output is missing its marker; callers rely on it")
	}
}

// TestSmartTruncate_KeepsFrontmatter is the property the read path needs most:
// the YAML header must survive so extractBody can still find the body.
func TestSmartTruncate_KeepsFrontmatter(t *testing.T) {
	body := strings.Repeat("payload line"+`
`, 400)
	content := renderKnowledgeFile("fs.read", body, timeForTest())
	got := smartTruncate(content, KnowledgeSizeCap)
	if !strings.HasPrefix(got, `---
tool: fs.read
`) {
		t.Errorf("smartTruncate lost the frontmatter header: %.60q", got)
	}
	if extractBody(got) == "" {
		t.Errorf("body is unrecoverable after truncate")
	}
}

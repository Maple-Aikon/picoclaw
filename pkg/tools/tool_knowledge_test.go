package tools

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// tempWorkspace returns the test's t.TempDir() — Go cleans it up automatically.
func tempWorkspace(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestKnowledgeStore_SaveAndRead(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	body := "## What goes wrong\n- Missing path returns: 'no such file'\n\n## What works instead\n- Check path with fs.stat first"
	path, size, err := s.Save("fs.read", body)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if path == "" {
		t.Fatalf("expected non-empty path")
	}
	if size <= 0 {
		t.Fatalf("expected positive size, got %d", size)
	}

	got, err := s.Read("fs.read")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(got, "What goes wrong") {
		t.Errorf("Read missing expected heading; got %q", got)
	}
	if !strings.Contains(got, "fs.stat first") {
		t.Errorf("Read missing expected lesson; got %q", got)
	}
	// Frontmatter must be stripped by Read.
	if strings.HasPrefix(got, "---") {
		t.Errorf("Read returned body with frontmatter prefix; got %q", got[:20])
	}
}

func TestKnowledgeStore_RenderWithFrontmatter(t *testing.T) {
	// Snapshot the canonical format so accidental format drift breaks
	// the build (every consumer of these files relies on the layout).
	body := "lesson body"
	now, err := time.Parse(time.RFC3339, "2026-07-18T08:00:00Z")
	if err != nil {
		t.Fatalf("time.Parse: %v", err)
	}
	rendered := renderKnowledgeFile("fs.read", body, now)

	if !strings.HasPrefix(rendered, "---\ntool: fs.read\nupdated: 2026-07-18T08:00:00Z\nversion: 1\n---\n\n") {
		t.Errorf("rendered frontmatter drift; got:\n%s", rendered)
	}
	if !strings.HasSuffix(rendered, "lesson body\n") {
		t.Errorf("rendered body suffix drift; got:\n%s", rendered)
	}
}

func TestKnowledgeStore_ExtractBody_NoFrontmatter(t *testing.T) {
	// extractBody must be safe on input without frontmatter (defensive —
	// LoadForEscalation can re-encounter the lesson body in odd states).
	in := "plain lesson\nno frontmatter here"
	got := extractBody(in)
	if got != in {
		t.Errorf("expected verbatim passthrough; got %q", got)
	}
}

func TestKnowledgeStore_SanitizeToolName_RejectsTraversal(t *testing.T) {
	cases := []string{
		"../escape", // parent dir traversal
		"foo/bar",   // slash
		"foo\\bar",  // backslash
		"a..b",      // contains ..
		"",          // empty
		"   ",       // whitespace only
		"foo bar",   // space
		"foo$bar",   // disallowed char
	}
	for _, c := range cases {
		_, err := sanitizeToolName(c)
		if !errors.Is(err, ErrKnowledgeToolNameInvalid) {
			t.Errorf("sanitizeToolName(%q): expected ErrKnowledgeToolNameInvalid, got %v", c, err)
		}
	}
}

func TestKnowledgeStore_SanitizeToolName_NormalizeLowercase(t *testing.T) {
	cases := map[string]string{
		"FS.READ":     "fs.read",
		"  fs.read  ": "fs.read",
		"web_Fetch.2": "web_fetch.2",
	}
	for in, want := range cases {
		got, err := sanitizeToolName(in)
		if err != nil {
			t.Errorf("sanitizeToolName(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("sanitizeToolName(%q): want %q got %q", in, want, got)
		}
	}
}

func TestKnowledgeStore_Delete_Idempotent(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	// Delete on missing file → nil (no error).
	if err := s.Delete("never-saved"); err != nil {
		t.Errorf("Delete on missing should be no-op; got %v", err)
	}

	// Save then Delete → file gone, second Delete → nil.
	if _, _, err := s.Save("fs.read", "lesson"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete("fs.read"); err != nil {
		t.Errorf("Delete after Save: %v", err)
	}
	if err := s.Delete("fs.read"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestKnowledgeStore_List_Sorted(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	tools := []string{"web.fetch", "fs.read", "shell.run"}
	for _, name := range tools {
		if _, _, err := s.Save(name, "lesson for "+name); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"fs.read", "shell.run", "web.fetch"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("List not sorted; got %v want %v", got, want)
	}
}

func TestKnowledgeStore_LoadForEscalation_NoFile(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	got := s.LoadForEscalation("never-saved")
	if got != "" {
		t.Errorf("LoadForEscalation on missing file: want '' got %q", got)
	}
}

func TestKnowledgeStore_LoadForEscalation_TruncatesOverCap(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	// Build a body that exceeds 2KB after rendering. Save truncates
	// down to cap; LoadForEscalation must return truncated body too.
	big := strings.Repeat("line of text\n", 200)
	if _, _, err := s.Save("fs.read", big); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := s.LoadForEscalation("fs.read")
	if got == "" {
		t.Fatalf("LoadForEscalation returned empty despite save")
	}
	// The truncation must drop the file under (or near) cap.
	// We don't pin to exact byte count because smart-truncate preserves
	// the head; just assert it's substantially smaller than the original.
	if len(got) >= len(big) {
		t.Errorf("LoadForEscalation did not truncate; len(got)=%d len(big)=%d", len(got), len(big))
	}
}

func TestKnowledgeStore_ConcurrentSaveSameTool(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			body := "lesson from goroutine " + string(rune('a'+i%26))
			if _, _, err := s.Save("fs.read", body); err != nil {
				t.Errorf("goroutine Save: %v", err)
			}
		}()
	}
	wg.Wait()

	// After all races, one body should have won. Read must succeed and
	// return one of the written bodies (no corruption that breaks Read).
	got, err := s.Read("fs.read")
	if err != nil {
		t.Fatalf("Read after concurrent saves: %v", err)
	}
	if !strings.HasPrefix(got, "lesson from goroutine ") {
		t.Errorf("Read returned unexpected body: %q", got)
	}
}

func TestKnowledgeStore_PathFor_RejectsTraversal(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}

	// Valid name → path under root.
	p, err := s.PathFor("fs.read")
	if err != nil {
		t.Fatalf("PathFor valid: %v", err)
	}
	if !strings.HasSuffix(p, "fs.read.md") {
		t.Errorf("PathFor valid: unexpected suffix %q", p)
	}
	if !strings.HasPrefix(p, ws) {
		t.Errorf("PathFor valid: not under workspace; got %q", p)
	}

	// Invalid name → ErrKnowledgeToolNameInvalid (NEVER a path that escapes).
	if _, err := s.PathFor("../escape"); !errors.Is(err, ErrKnowledgeToolNameInvalid) {
		t.Errorf("PathFor traversal: want ErrKnowledgeToolNameInvalid, got %v", err)
	}
}

func TestAppendKnowledgeSection_Format(t *testing.T) {
	// Snapshot the canonical markers so the escalation wire in
	// registry.go can rely on the format.
	got := AppendKnowledgeSection("the lesson body")
	want := "=== Saved Knowledge ===\nthe lesson body\n=== End Knowledge ==="
	if got != want {
		t.Errorf("AppendKnowledgeSection drift:\n got %q\nwant %q", got, want)
	}
}

// stringSlicesEqual compares two slices ignoring nil vs empty distinction.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
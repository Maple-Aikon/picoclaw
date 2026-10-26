package tools

import (
	"strings"
	"testing"
	"time"
)

// nlByte is the newline byte. The test file deliberately avoids newline
// escapes in string literals so the file survives JSON-decoding round
// trips intact.
const nlByte byte = 10

// buildDigestWordSections builds a body with n H2 sections. Every section
// body is `lines` long lines; each line is a run of space-separated unique
// tokens so a test can detect a mid-word cut PRECISELY.
//
// It returns the body and the per-section original body text (same order).
func buildDigestWordSections(n, lines int) (string, []string) {
	var b strings.Builder
	bodies := make([]string, 0, n)
	for i := 0; i < n; i++ {
		b.WriteString("## Section ")
		b.WriteString(itoaTest(i))
		b.WriteString(string(rune(nlByte)))
		var sb strings.Builder
		for j := 0; j < lines; j++ {
			sb.WriteString("tok")
			sb.WriteString(itoaTest(i))
			sb.WriteString("_")
			sb.WriteString(itoaTest(j))
			sb.WriteString("_alpha beta gamma delta epsilon zeta eta theta")
			if j < lines-1 {
				sb.WriteString(string(rune(nlByte)))
			}
		}
		b.WriteString(sb.String())
		b.WriteString(string(rune(nlByte)))
		bodies = append(bodies, sb.String())
	}
	return b.String(), bodies
}

// digestSectionBodies returns the body text the digest kept for each H2
// heading, keyed by heading.
//
// The truncation marker is stripped: it is appended at the very END of the
// digest, so it lands inside the LAST section's body when the digest is
// re-split. The marker is a digest-level artifact, not part of any
// section's excerpt, so it must not count against the excerpt.
func digestSectionBodies(digest string) map[string]string {
	out := map[string]string{}
	_, body := splitFrontmatter(digest)
	for _, s := range splitH2Sections(body) {
		if s.heading == "" {
			continue
		}
		text := strings.TrimSpace(s.body)
		text = strings.TrimSpace(strings.TrimSuffix(text, "... [truncated]"))
		out[s.heading] = text
	}
	return out
}

// originalSectionBodies does the same for the PRE-digest content.
func originalSectionBodies(content string) map[string]string {
	out := map[string]string{}
	_, body := splitFrontmatter(content)
	for _, s := range splitH2Sections(body) {
		if s.heading == "" {
			continue
		}
		out[s.heading] = strings.TrimSpace(s.body)
	}
	return out
}

// assertExcerptWordSafe fails when the excerpt is not a prefix of the
// original, or when it was cut in the middle of a word.
//
// A word-safe cut leaves the excerpt ending exactly at a word or line
// boundary, so the NEXT byte of the original must be a space or newline.
func assertExcerptWordSafe(t *testing.T, heading, original, excerpt string) {
	t.Helper()
	if excerpt == "" {
		return
	}
	if !strings.HasPrefix(original, excerpt) {
		t.Errorf("%s: excerpt is not a prefix of the original body: %.60q", heading, excerpt)
		return
	}
	if len(excerpt) == len(original) {
		return
	}
	next := original[len(excerpt)]
	if next != ' ' && next != nlByte {
		t.Errorf("%s: excerpt was cut mid-word (next byte %q): %.60q",
			heading, string(rune(next)), excerpt)
	}
}

// TestDigestSections_EverySectionKeepsReadableBody is the RED test.
//
// The old digest allocated ONE fixed per-section byte budget to every
// section. Because the fixed overhead (frontmatter + EVERY heading +
// marker) grows with the section count, that uniform budget either
// overflowed the cap (so the ladder fell through to budget=0 and wiped
// EVERY body) or was small enough that the raw byte cut landed in the
// middle of a word.
//
// The invariant: for an oversized entry, every body-bearing section
// keeps a NON-EMPTY excerpt, and every excerpt is cut on a word or line
// boundary -- never mid-word.
func TestDigestSections_EverySectionKeepsReadableBody(t *testing.T) {
	cases := []struct {
		name  string
		sects int
		lines int
	}{
		// 40 sections: the old ladder resolved this one, but with a raw
		// 25-byte cut that lands mid-word.
		{"40 sections", 40, 3},
		// 60 sections: the old ladder could not fit ANY non-zero budget,
		// so it fell through to budget=0 and wiped every body.
		{"60 sections", 60, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := buildDigestWordSections(tc.sects, tc.lines)
			content := renderKnowledgeFile("exec", body, time.Now())
			if len(content) <= KnowledgeSizeCap {
				t.Fatalf("test fixture is not over the cap: %d bytes", len(content))
			}
			out := digestSections(content, KnowledgeSizeCap)

			if len(out) > KnowledgeSizeCap {
				t.Fatalf("digest exceeded cap: got=%d cap=%d", len(out), KnowledgeSizeCap)
			}
			orig := originalSectionBodies(content)
			got := digestSectionBodies(out)

			starved := 0
			midWord := 0
			for heading, original := range orig {
				excerpt, present := got[heading]
				if !present {
					t.Errorf("digest dropped H2 heading %q", heading)
					continue
				}
				if excerpt == "" {
					starved++
					continue
				}
				assertExcerptWordSafe(t, heading, original, excerpt)
				if !strings.HasPrefix(original, excerpt) ||
					(len(excerpt) < len(original) &&
						original[len(excerpt)] != ' ' && original[len(excerpt)] != nlByte) {
					midWord++
				}
			}
			if starved > 0 {
				t.Errorf("%d/%d sections were left with NO body text at all", starved, len(orig))
			}
			if midWord > 0 {
				t.Errorf("%d/%d section excerpts were cut mid-word", midWord, len(orig))
			}
		})
	}
}

// TestLoadForEscalation_OverCapEntryKeepsReadableBody is the END-TO-END
// invariant for the read path: what the runtime actually auto-loads on a
// tool failure must still carry the lesson text, not just the headings.
//
// The body must be word-safe too -- a mid-word cut produces text like
// "1. **`go version -m <bin>" which reads as garbage.
func TestLoadForEscalation_OverCapEntryKeepsReadableBody(t *testing.T) {
	ws := tempWorkspace(t)
	s, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}
	body, _ := buildDigestWordSections(40, 3)
	if _, _, err := s.Replace("exec", body); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got := s.LoadForEscalation("exec")
	if got == "" {
		t.Fatalf("LoadForEscalation returned empty despite save")
	}
	orig := originalSectionBodies(renderKnowledgeFile("exec", body, time.Now()))
	digest := digestSectionBodies(got)
	starved := 0
	for heading, original := range orig {
		excerpt, present := digest[heading]
		if !present {
			t.Errorf("LoadForEscalation dropped H2 heading %q", heading)
			continue
		}
		if excerpt == "" {
			starved++
			continue
		}
		assertExcerptWordSafe(t, heading, original, excerpt)
	}
	if starved > 0 {
		t.Errorf("%d/%d sections were left with NO body text at all", starved, len(orig))
	}
}

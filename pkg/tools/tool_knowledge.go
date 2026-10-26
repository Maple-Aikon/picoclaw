package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ToolKnowledgeStore is a per-workspace, per-tool persistent memory of
// "lessons learned" the LLM has explicitly saved. Lives at:
//
//	{workspace}/memory/tool_knowledge/{tool_name}.md
//
// File format (deterministic, hashable, human-readable):
//
//	---
//	tool: exec
//	updated: 2026-07-18T08:59:00Z
//	version: 1
//	---
//
//	## What goes wrong
//	- Missing path returns: "no such file"
//
//	## What works instead
//	- Check path with list_dir first
//
// Thread-safety: per-tool mutex; safe for concurrent append/read.
type ToolKnowledgeStore struct {
	root string // workspace/memory/tool_knowledge/

	mu          sync.Mutex
	perFileLock map[string]*sync.Mutex // keyed by tool name
}

// DefaultKnowledgeDir is the conventional location under the workspace.
const DefaultKnowledgeDir = "memory/tool_knowledge"

// KnowledgeFileMode is the file mode for newly created knowledge files.
// 0600 = owner read/write only; knowledge can be sensitive (paths, args).
const KnowledgeFileMode = 0o600

// KnowledgeSizeCap bounds what a knowledge file may contribute to a PROMPT,
// not what may be stored. It is applied on the read path (Read,
// LoadForEscalation) so an oversized entry does not flood the context window.
//
// It must never be applied on the write path: Save persists the body verbatim.
// A cap that truncates at save time is silent, unrecoverable data loss — the
// caller gets a success message and the tail is gone (regression test
// TestKnowledgeStore_Save_DoesNotLoseBodyAboveCap, 2026-10-20).
const KnowledgeSizeCap = 2 * 1024 // 2KB

// MinBodyChars is the warning threshold — a body shorter than this
// triggers a "consider expanding" hint in the tool's response.
const MinBodyChars = 50

// Soft-prompt constants (Phase 3) — surfaced by registry.go at the
// execution site based on (fb.Status, tracker.Count(key)). The strings
// stay terse to preserve prompt budget; the full action is described
// in tool_knowledge_tool.go's Description.
const (
	SoftPromptFirstSuccess = "\n\n[Tip] First successful pattern for this tool/signature. " +
		"If you learned anything generalizable, consider saving it via `tool_knowledge` " +
		"(action=replace, tool=<this>, body='<lesson>')."
	SoftPromptRepeatedFailure = "\n\n[Tip] Repeated failure on same signature. " +
		"If you discovered a workaround, save it via `tool_knowledge` " +
		"(action=replace, tool=<this>, body='<lesson>') so future turns benefit."
)

// Knowledge section markers appended to escalation messages (Phase 2).
// Stable so snapshot tests can pin the format.
const (
	KnowledgeSectionHeader = "=== Saved Knowledge ==="
	KnowledgeSectionFooter = "=== End Knowledge ==="
)

// AppendKnowledgeSection wraps a lesson body with the canonical markers
// and returns the composite string ready to be appended to an escalation
// message. Pure function — exported for snapshot tests.
func AppendKnowledgeSection(lesson string) string {
	return KnowledgeSectionHeader + "\n" + lesson + "\n" + KnowledgeSectionFooter
}

// ErrKnowledgeEmpty is returned when the LLM tries to save empty content.
var ErrKnowledgeEmpty = errors.New("knowledge body must be non-empty")

// ErrKnowledgeToolNameInvalid is returned when the LLM picks an unsafe
// tool name (path traversal, slashes, etc.).
var ErrKnowledgeToolNameInvalid = errors.New("tool name must be alphanumeric, dot, dash, or underscore (no slashes, no '..')")

// NewToolKnowledgeStore resolves {workspace}/{dir} and ensures it exists.
// Pass empty string for dir to use DefaultKnowledgeDir.
func NewToolKnowledgeStore(workspace, dir string) (*ToolKnowledgeStore, error) {
	if workspace == "" {
		return nil, fmt.Errorf("workspace is required")
	}
	if dir == "" {
		dir = DefaultKnowledgeDir
	}
	root := filepath.Join(workspace, dir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create knowledge dir: %w", err)
	}
	return &ToolKnowledgeStore{
		root:        root,
		perFileLock: make(map[string]*sync.Mutex),
	}, nil
}

// Root returns the absolute directory where knowledge files live.
// Useful for diagnostics and tests.
func (s *ToolKnowledgeStore) Root() string {
	return s.root
}

// fileLock returns a stable per-tool mutex (lazily created).
func (s *ToolKnowledgeStore) fileLock(tool string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lk, ok := s.perFileLock[tool]; ok {
		return lk
	}
	lk := &sync.Mutex{}
	s.perFileLock[tool] = lk
	return lk
}

// sanitizeToolName rejects path-traversal attempts and slashes so the LLM
// cannot escape the knowledge dir. Normalized to lower-case for stable lookups.
func sanitizeToolName(tool string) (string, error) {
	t := strings.TrimSpace(strings.ToLower(tool))
	if t == "" {
		return "", ErrKnowledgeToolNameInvalid
	}
	if strings.Contains(t, "..") || strings.ContainsAny(t, "/\\") {
		return "", ErrKnowledgeToolNameInvalid
	}
	for _, r := range t {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '.' || r == '-' || r == '_' {
			continue
		}
		return "", ErrKnowledgeToolNameInvalid
	}
	return t, nil
}

// PathFor returns the absolute file path for a tool's knowledge file.
// Exposed for diagnostics + tests; tool itself uses Save/Read/List/Delete.
func (s *ToolKnowledgeStore) PathFor(tool string) (string, error) {
	safe, err := sanitizeToolName(tool)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.root, safe+".md"), nil
}

// Replace overwrites the knowledge body for the given tool.
// Body must be non-empty. Returns the final path written and the byte size.
//
// This is the explicit-destruction path: whatever was there before is gone.
// Callers that want to add to an entry must use Append instead — an
// accidental Replace silently destroys context that only existed in this file
// (measured 2026-10-20: a save-shaped overwrite dropped a recovered entry).
//
// Concurrency: per-tool mutex. Two concurrent writes to the same tool
// serialize; writes to different tools do not block each other.
// writeLocked renders and writes an entry. The caller MUST already hold the
// per-tool lock for safe. Splitting this out of Replace is what lets Append
// create a missing entry without deadlocking on the same mutex (2026-10-20).
func (s *ToolKnowledgeStore) writeLocked(safe, body string) (string, int, error) {
	path := filepath.Join(s.root, safe+".md")
	// No truncation here. The cap is a prompt-budget concern, enforced by the
	// read path; applying it before the write silently destroyed body tails
	// while still reporting success (see KnowledgeSizeCap).
	final := renderKnowledgeFile(safe, body, time.Now().UTC())

	if err := os.WriteFile(path, []byte(final), KnowledgeFileMode); err != nil {
		return "", 0, fmt.Errorf("write knowledge file: %w", err)
	}
	return path, len(final), nil
}

// Replace overwrites the knowledge body for the given tool.
// Body must be non-empty. Returns the final path written and the byte size.
//
// This is the explicit-destruction path: whatever was there before is gone.
// Callers that want to add to an entry must use Append instead — an
// accidental Replace silently destroys context that only existed in this file
// (measured 2026-10-20: a save-shaped overwrite dropped a recovered entry).
//
// Concurrency: per-tool mutex. Two concurrent writes to the same tool
// serialize; writes to different tools do not block each other.
func (s *ToolKnowledgeStore) Replace(tool, body string) (string, int, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", 0, ErrKnowledgeEmpty
	}
	safe, err := sanitizeToolName(tool)
	if err != nil {
		return "", 0, err
	}
	lk := s.fileLock(safe)
	lk.Lock()
	defer lk.Unlock()

	return s.writeLocked(safe, body)
}

// Append adds body to the end of the existing knowledge for the given tool,
// keeping every byte already stored. Returns the final path written and the
// resulting byte size.
//
// If no entry exists yet, Append behaves like Replace — creating the file is
// not destructive, so there is nothing to protect.
//
// Concurrency: the read-modify-write happens entirely under the per-tool
// mutex, so a concurrent Replace on the same tool serializes against it
// instead of silently dropping one of the two bodies.
// Append adds body to the end of the existing knowledge for the given tool,
// keeping every byte already stored. Returns the final path written and the
// resulting byte size.
//
// If no entry exists yet, Append behaves like Replace — creating the file is
// not destructive, so there is nothing to protect.
//
// Concurrency: the read-modify-write happens entirely under the per-tool
// mutex, so a concurrent Replace on the same tool serializes against it
// instead of silently dropping one of the two bodies. This is also why the
// missing-entry branch below calls the UNLOCKED writeLocked rather than
// Replace: calling a lock-taking method while holding that same non-reentrant
// mutex deadlocks forever (caught by
// TestToolKnowledge_AppendCreatesMissingEntry, 2026-10-20).
func (s *ToolKnowledgeStore) Append(tool, body string) (string, int, error) {
	add := strings.TrimSpace(body)
	if add == "" {
		return "", 0, ErrKnowledgeEmpty
	}
	safe, err := sanitizeToolName(tool)
	if err != nil {
		return "", 0, err
	}
	lk := s.fileLock(safe)
	lk.Lock()
	defer lk.Unlock()

	path := filepath.Join(s.root, safe+".md")
	existing, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			// No entry yet — creating one destroys nothing.
			return s.writeLocked(safe, add)
		}
		return "", 0, fmt.Errorf("read knowledge file: %w", readErr)
	}

	merged := string(existing)
	if !strings.HasSuffix(merged, "\n") {
		merged += "\n"
	}
	merged += add + "\n"
	// Same guarantee as Replace: the composed file is written verbatim. The
	// read-path cap must never be applied to a write.
	if err := os.WriteFile(path, []byte(merged), KnowledgeFileMode); err != nil {
		return "", 0, fmt.Errorf("append knowledge file: %w", err)
	}
	return path, len(merged), nil
}

// Read returns the body (without frontmatter) for the given tool.
// Returns ErrKnowledgeNotFound if no knowledge file exists yet.
func (s *ToolKnowledgeStore) Read(tool string) (string, error) {
	safe, err := sanitizeToolName(tool)
	if err != nil {
		return "", err
	}
	lk := s.fileLock(safe)
	lk.Lock()
	defer lk.Unlock()

	path := filepath.Join(s.root, safe+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrKnowledgeNotFound
		}
		return "", fmt.Errorf("read knowledge file: %w", err)
	}
	return extractBody(string(data)), nil
}

// ErrKnowledgeNotFound is returned by Read when no knowledge has been saved.
var ErrKnowledgeNotFound = errors.New("no knowledge saved for this tool yet")

// Delete removes the knowledge file for the given tool.
// Idempotent — returns nil even if no file exists.
func (s *ToolKnowledgeStore) Delete(tool string) error {
	safe, err := sanitizeToolName(tool)
	if err != nil {
		return err
	}
	lk := s.fileLock(safe)
	lk.Lock()
	defer lk.Unlock()

	path := filepath.Join(s.root, safe+".md")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete knowledge file: %w", err)
	}
	return nil
}

// List returns the set of tool names with saved knowledge, sorted.
func (s *ToolKnowledgeStore) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("list knowledge dir: %w", err)
	}

	var tools []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".md") {
			tools = append(tools, strings.TrimSuffix(name, ".md"))
		}
	}
	return tools, nil
}

// LoadForEscalation returns the body for the given tool iff:
//  1. a knowledge file exists,
//  2. its rendered form fits within KnowledgeSizeCap after digesting.
//
// Returns "" otherwise — callers (Phase 2 wire in registry.go) treat empty
// as "no prior knowledge to surface". Pure read path — no frontmatter
// mutation, no event emission.
//
// Phase 12.62 (2026-10-25): oversized entries are now rendered as an
// H2-heading digest instead of a head-truncated blob. The old
// smartTruncate kept only the FIRST ~2 KB of the body, so for the 48/102
// real entries that exceed the cap the tail was unreachable — and since
// lessons are always APPENDED, the newest lesson (the one most likely to
// describe the failure that just happened) was exactly what got dropped.
// The digest keeps EVERY H2 heading (so every lesson stays discoverable)
// and spends the remaining budget on a bounded excerpt of each section.
//
// The full body remains available through `tool_knowledge action=read`,
// which does not apply the cap (tool_knowledge_tool.go → store.Read).
func (s *ToolKnowledgeStore) LoadForEscalation(tool string) string {
	body, err := s.Read(tool)
	if err != nil {
		return ""
	}
	// Re-render and apply the digest to mirror Save's cap behaviour.
	rendered := renderKnowledgeFile(tool, body, time.Now().UTC())
	if len(rendered) > KnowledgeSizeCap {
		rendered = digestSections(rendered, KnowledgeSizeCap)
	}
	// Strip the frontmatter block — caller only wants the lesson body.
	return extractBody(rendered)
}

// renderKnowledgeFile builds the canonical file: YAML frontmatter + body.
// Pure function — exported via tests for snapshot.
func renderKnowledgeFile(tool, body string, now time.Time) string {
	frontmatter := fmt.Sprintf(
		"---\ntool: %s\nupdated: %s\nversion: 1\n---\n\n",
		tool,
		now.UTC().Format(time.RFC3339),
	)
	return frontmatter + body + "\n"
}

// extractBody returns the body portion (everything after the closing "---").
// If no frontmatter is present, returns the input verbatim.
func extractBody(content string) string {
	const marker = "\n---\n"
	// Skip the opening "---\n" if present.
	start := 0
	if strings.HasPrefix(content, "---\n") {
		start = 4
	}
	idx := strings.Index(content[start:], marker)
	if idx < 0 {
		return strings.TrimSpace(content)
	}
	body := content[start+idx+len(marker):]
	return strings.TrimSpace(body)
}

// splitFrontmatter separates a leading YAML frontmatter block from the
// body.
//
// Phase 12.62: the frontmatter is only recognised when the content
// STARTS with "---". A bare strings.Index for the frontmatter terminator finds
// horizontal rules in the middle of a markdown document (measured:
// memory/tool_knowledge/code-review.packet-format.md has seven of them)
// and would treat everything up to the LAST one as "frontmatter" --
// a huge bogus head that pushes the digest over the cap and silently
// drops the tail headings.
func splitFrontmatter(content string) (head, body string) {
	const fmEnd = "\n---\n\n"
	if !strings.HasPrefix(content, "---") {
		return "", content
	}
	fmIdx := strings.Index(content, fmEnd)
	if fmIdx < 0 {
		return "", content
	}
	fmEndIdx := fmIdx + len(fmEnd)
	return content[:fmEndIdx], content[fmEndIdx:]
}

// digestSections renders an H2-heading digest of an oversized knowledge
// file so the result NEVER exceeds byteCap -- including the truncation
// marker.
//
// Phase 12.62 (2026-10-25). The problem it replaces: smartTruncate kept
// the HEAD of the body, so for an entry with 31 H2 sections only the
// first ~2 KB survived -- every lesson after that point was invisible to
// the LLM. Lessons are appended, so the newest lesson was always the one
// dropped.
//
// The digest keeps EVERY "## " heading verbatim (so no lesson becomes
// undiscoverable) and fills the remaining budget with a bounded excerpt
// of each section body. Two passes per budget:
//
//  1. Prefer keeping the FIRST headed section in full (it usually holds
//     the "What goes wrong" lesson, which is the part an LLM needs most),
//     then digest the rest.
//  2. If that still overflows the cap, fall back to digesting ALL
//     sections including the first.
//
// If even the headings alone exceed the cap, the headings are truncated
// so the result still respects the cap.
//
// Frontmatter is preserved verbatim when present. A file WITHOUT
// frontmatter is still digested by its H2 headings (measured 2026-10-25:
// 7 of the 48 real over-cap entries have no frontmatter, and the old
// fallback truncated them to a blind head-slice).
//
// The output is valid UTF-8 by construction: excerpts are cut on rune
// boundaries via trimToUTF8LineBoundary.
func digestSections(content string, byteCap int) string {
	if len(content) <= byteCap {
		return content
	}
	head, body := splitFrontmatter(content)

	sections := splitH2Sections(body)
	if len(sections) == 0 {
		// No H2 structure at all - nothing to digest by.
		return truncateWithinCap(content, byteCap)
	}

	// Phase 12.63 (2026-10-26): global water-filling allocation.
	//
	// The old implementation handed the SAME per-section byte budget to
	// every section and tried a ladder of shrinking budgets. That is
	// arithmetically hopeless: the digest's fixed overhead (frontmatter +
	// EVERY heading + marker) grows with the section count, so for a
	// section-heavy entry NO uniform budget fits, the ladder fell through
	// to budget=0, and every body was wiped -- the digest degenerated
	// into a table of contents. Measured on the real store: a 60-section
	// entry lost the body of 58/60 sections.
	//
	// The fix computes the room that is ACTUALLY left after the fixed
	// overhead, gives every body-bearing section a word-safe minimum
	// excerpt, and then water-fills the surplus across the sections.
	plan := allocateDigest(sections, byteCap-len(head)-len(truncationMarker))
	out, ok := renderDigestAlloc(head, plan, byteCap)
	if ok {
		return out
	}

	// Headings alone exceeded the cap. Keep the frontmatter and as many
	// headings as fit, dropping the tail -- still respecting the cap.
	return headingsOnly(head, sections, byteCap)
}

// allocateDigest splits the available room across the sections.
//
// room is the number of bytes left AFTER the frontmatter and the
// truncation marker have been accounted for. The function reserves:
//
//   - one line for each heading (heading + newline),
//   - one line for each section that carries body text,
//   - a word-safe minimum excerpt for each body-bearing section.
//
// Any surplus is then water-filled: sections that still have unshown
// body text receive an equal share until the room is exhausted.
//
// Every excerpt is cut on a word or line boundary, so the rendered
// digest never contains a half-written word.
func allocateDigest(sections []h2Section, room int) []digestSectionPlan {
	plan := make([]digestSectionPlan, 0, len(sections))

	// Fixed cost, counted BEFORE any excerpt is handed out:
	//   - every heading costs its bytes plus a newline,
	//   - every body-bearing section costs one extra newline for its
	//     excerpt block.
	//
	// Phase 12.63: the first version added the body-block newline while
	// iterating, AFTER `remaining` had been computed -- so the budget was
	// short by up to N bytes, the render could exceed the cap, and the
	// caller silently fell back to headingsOnly (losing every body).
	// Every byte the digest can spend must be accounted for first.
	fixed := 0
	for _, s := range sections {
		if s.heading != "" {
			fixed += len(s.heading) + 1
		}
		if strings.TrimSpace(s.body) != "" {
			fixed++
		}
	}

	remaining := room - fixed

	// Give every body-bearing section a word-safe minimum excerpt.
	type pendingSection struct {
		sectionIdx int
		minBytes   int
	}
	var pending []pendingSection
	for i, s := range sections {
		p := digestSectionPlan{heading: s.heading}
		bodyText := strings.TrimSpace(s.body)
		if bodyText != "" {
			p.hasBody = true
			firstLine := strings.Split(bodyText, string(rune(digestNL)))[0]
			minExcerpt := digestMinWordSafeExcerpt(firstLine)
			p.excerpt = minExcerpt
			pending = append(pending, pendingSection{sectionIdx: i, minBytes: len(minExcerpt)})
		}
		plan = append(plan, p)
	}

	// Reserve the minimum excerpts. When the room cannot cover every
	// minimum, the LAST sections lose their body first -- earlier
	// sections hold the older lessons, and a section that keeps its
	// heading but loses its body is still discoverable.
	for i := range pending {
		si := pending[i].sectionIdx
		if remaining < pending[i].minBytes {
			plan[si].hasBody = false
			plan[si].excerpt = ""
			continue
		}
		remaining -= pending[i].minBytes
	}

	// Water-fill the surplus: hand out equal shares to the sections that
	// still have unshown text, until the room is exhausted.
	for {
		var eligible []pendingSection
		for _, p := range pending {
			if !plan[p.sectionIdx].hasBody {
				continue
			}
			bodyText := strings.TrimSpace(sections[p.sectionIdx].body)
			if len(plan[p.sectionIdx].excerpt) < len(bodyText) {
				eligible = append(eligible, p)
			}
		}
		if len(eligible) == 0 || remaining <= 0 {
			break
		}
		share := remaining / len(eligible)
		if share <= 0 {
			break
		}
		progressed := false
		for _, e := range eligible {
			si := e.sectionIdx
			bodyText := strings.TrimSpace(sections[si].body)
			cur := len(plan[si].excerpt)
			newLen := extendExcerpt(bodyText, cur, cur+share)
			if newLen <= cur {
				// The next word does not fit inside this section's
				// share. Allow exactly ONE more word -- and only when
				// the GLOBAL room can pay for it.
				//
				// Phase 12.63: an earlier version retried with an
				// unbounded window (cur+share+len(body)), so every
				// section could grow past the cap. The render then
				// overflowed and the caller silently fell back to
				// headingsOnly -- which drops EVERY body. The extra
				// word must be paid for out of the global room.
				next := nextWordEnd(bodyText, cur)
				if next <= cur || next-cur > remaining {
					continue
				}
				newLen = next
			}
			plan[si].excerpt = bodyText[:newLen]
			remaining -= newLen - cur
			progressed = true
		}
		if !progressed {
			break
		}
	}

	return plan
}

// truncateWithinCap truncates content so that the result INCLUDING the
// truncation marker never exceeds byteCap.
//
// Phase 12.62: the older hardTruncate cut to byteCap and THEN appended
// the marker, so its output could exceed the cap by len(marker) bytes
// (measured: 2057 bytes for a 2048-byte cap). Every cap-respecting
// caller must use this variant instead.
func truncateWithinCap(content string, byteCap int) string {
	if len(content) <= byteCap {
		return content
	}
	if byteCap <= len(truncationMarker) {
		// Even the marker does not fit - fall back to a raw byte slice.
		if byteCap <= 0 {
			return ""
		}
		return trimToUTF8LineBoundary(content[:byteCap])
	}
	room := byteCap - len(truncationMarker)
	return trimToUTF8LineBoundary(content[:room]) + truncationMarker
}

// h2Section is one "## " heading plus the body lines that follow it.
type h2Section struct {
	heading string
	body    string
}

// splitH2Sections splits a body into its H2 sections. Text before the
// first H2 (a preamble) is returned as a leading section with an empty
// heading so it is not silently dropped.
func splitH2Sections(body string) []h2Section {
	var (
		sections []h2Section
		cur      *h2Section
		preamble strings.Builder
	)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			sections = append(sections, h2Section{heading: line})
			cur = &sections[len(sections)-1]
			continue
		}
		if cur == nil {
			preamble.WriteString(line)
			preamble.WriteString("\n")
			continue
		}
		cur.body += line
		cur.body += "\n"
	}
	if p := strings.TrimSpace(preamble.String()); p != "" {
		sections = append([]h2Section{{body: p + "\n"}}, sections...)
	}
	return sections
}

// firstHeadedIndex returns the index of the first section that carries an
// H2 heading, or -1 when the document has none.
func firstHeadedIndex(sections []h2Section) int {
	for i, s := range sections {
		if s.heading != "" {
			return i
		}
	}
	return -1
}

// renderDigest renders the digest and reports whether it fits the cap.
// keepFirstFull keeps the first HEADED section's body verbatim instead of
// excerpting it.
func renderDigest(head string, sections []h2Section, budget int, keepFirstFull bool, byteCap int) (string, bool) {
	fullIdx := -1
	if keepFirstFull {
		fullIdx = firstHeadedIndex(sections)
	}
	var b strings.Builder
	b.WriteString(head)
	for i, s := range sections {
		if s.heading != "" {
			b.WriteString(s.heading)
			b.WriteString("\n")
		}
		body := strings.TrimSpace(s.body)
		if body != "" {
			if i == fullIdx {
				b.WriteString(body)
			} else if budget > 0 {
				cut := body
				if len(cut) > budget {
					cut = cut[:budget]
				}
				b.WriteString(trimToUTF8LineBoundary(cut))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(truncationMarker)
	out := b.String()
	if len(out) > byteCap {
		return "", false
	}
	return out, true
}

// headingsOnly renders just the headings of the given sections, dropping
// every body. Used as the last-resort fallback when even the headings
// overflow the cap.
func headingsOnly(head string, sections []h2Section, byteCap int) string {
	var b strings.Builder
	b.WriteString(head)
	for _, s := range sections {
		if s.heading != "" {
			b.WriteString(s.heading)
			b.WriteString("\n")
		}
	}
	b.WriteString(truncationMarker)
	return truncateWithinCap(b.String(), byteCap)
}

// smartTruncate keeps the frontmatter intact and preserves as much of the body
// as fits within byteCap. READ PATH ONLY - never call this before a write.
//
// The cut is UTF-8 safe: the body is sliced at byteCap, then walked back byte by
// byte until the result is valid UTF-8, then snapped back to the last newline so
// the output never ends mid-line. Without those two steps the result can end
// inside a multi-byte rune (measured 2026-10-20: 4/8 and 5/8 offsets on
// Vietnamese text produced invalid UTF-8) or mid-line. Mirrors
// truncateDiffPreview in shared/diff_result.go.
func smartTruncate(content string, byteCap int) string {
	if len(content) <= byteCap {
		return content
	}
	const fmEnd = "\n---\n\n"
	fmIdx := strings.Index(content, fmEnd)
	if fmIdx < 0 {
		// No frontmatter - just hard truncate.
		return hardTruncate(content, byteCap)
	}
	fmEndIdx := fmIdx + len(fmEnd)
	head := content[:fmEndIdx]
	body := content[fmEndIdx:]
	remaining := byteCap - len(head)
	if remaining <= 0 {
		// Frontmatter alone exceeded cap (should not happen with sane
		// frontmatter); just hard-truncate the whole thing.
		return hardTruncate(content, byteCap)
	}
	if remaining >= len(body) {
		return content // would fit, recompute elsewhere if needed
	}
	// Reserve room for the marker so the result stays inside byteCap.
	tail := body[:remaining]
	if room := remaining - len(truncationMarker); room > 0 {
		tail = body[:room]
	}
	return head + trimToUTF8LineBoundary(tail) + truncationMarker
}

// truncationMarker is appended whenever a body is cut for display. Stable so
// snapshot tests can pin the format.
const truncationMarker = "\n... [truncated]\n"

// hardTruncate truncates content that has no recognisable frontmatter.
func hardTruncate(content string, byteCap int) string {
	if byteCap > len(content) {
		byteCap = len(content)
	}
	return trimToUTF8LineBoundary(content[:byteCap]) + truncationMarker
}

// trimToUTF8LineBoundary walks the tail back until it ends on a valid UTF-8
// rune AND a newline, so the result is both parseable and line-complete.
// ValidString on the whole slice is the rune check; a rune boundary alone can
// still leave a half-written line, which is why the newline snap is separate.
func trimToUTF8LineBoundary(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	if nl := strings.LastIndexByte(s, '\n'); nl >= 0 {
		s = s[:nl]
	}
	return s
}

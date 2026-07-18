package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ToolKnowledgeStore is a per-workspace, per-tool persistent memory of
// "lessons learned" the LLM has explicitly saved. Lives at:
//
//	{workspace}/memory/tool_knowledge/{tool_name}.md
//
// File format (deterministic, hashable, human-readable):
//
//	---
//	tool: fs.read
//	updated: 2026-07-18T08:59:00Z
//	version: 1
//	---
//
//	## What goes wrong
//	- Missing path returns: "no such file"
//
//	## What works instead
//	- Check path with fs.stat first
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

// KnowledgeSizeCap is the soft cap per knowledge file (bytes).
// Exceeding it triggers smart truncation that preserves frontmatter + headings.
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
		"(action=save, tool=<this>, body='<lesson>')."
	SoftPromptRepeatedFailure = "\n\n[Tip] Repeated failure on same signature. " +
		"If you discovered a workaround, save it via `tool_knowledge` " +
		"(action=save, tool=<this>, body='<lesson>') so future turns benefit."
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
		if r >= 'a' && r <= 'z' { continue }
		if r >= '0' && r <= '9' { continue }
		if r == '.' || r == '-' || r == '_' { continue }
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

// Save writes (or overwrites) the knowledge body for the given tool.
// Body must be non-empty. Returns the final path written and the byte size.
//
// Concurrency: per-tool mutex. Two concurrent saves to the same tool
// serialize; saves to different tools do not block each other.
func (s *ToolKnowledgeStore) Save(tool, body string) (string, int, error) {
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

	path := filepath.Join(s.root, safe+".md")
	final := renderKnowledgeFile(safe, body, time.Now().UTC())

	// Smart truncate if over cap (Q4 decision).
	if len(final) > KnowledgeSizeCap {
		final = smartTruncate(final, KnowledgeSizeCap)
	}

	if err := os.WriteFile(path, []byte(final), KnowledgeFileMode); err != nil {
		return "", 0, fmt.Errorf("write knowledge file: %w", err)
	}
	return path, len(final), nil
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
//  2. its rendered form fits within KnowledgeSizeCap after smart-truncate.
//
// Returns "" otherwise — callers (Phase 2 wire in registry.go) treat empty
// as "no prior knowledge to surface". Pure read path — no frontmatter
// mutation, no event emission.
func (s *ToolKnowledgeStore) LoadForEscalation(tool string) string {
	body, err := s.Read(tool)
	if err != nil {
		return ""
	}
	// Re-render and apply smart-truncate to mirror Save's cap behaviour.
	rendered := renderKnowledgeFile(tool, body, time.Now().UTC())
	if len(rendered) > KnowledgeSizeCap {
		rendered = smartTruncate(rendered, KnowledgeSizeCap)
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

// smartTruncate keeps the frontmatter intact and preserves the first
// markdown heading + as much of the body as fits within byteCap.
func smartTruncate(content string, byteCap int) string {
	if len(content) <= byteCap {
		return content
	}
	const fmEnd = "\n---\n\n"
	fmIdx := strings.Index(content, fmEnd)
	if fmIdx < 0 {
		// No frontmatter — just hard truncate.
		return content[:byteCap] + "\n... [truncated]\n"
	}
	fmEndIdx := fmIdx + len(fmEnd)
	head := content[:fmEndIdx]
	body := content[fmEndIdx:]
	remaining := byteCap - len(head)
	if remaining <= 0 {
		// Frontmatter alone exceeded cap (shouldn't happen with sane
		// frontmatter); just hard-truncate the whole thing.
		return content[:byteCap] + "\n... [truncated]\n"
	}
	if remaining >= len(body) {
		return content // would fit, recompute elsewhere if needed
	}
	return head + body[:remaining] + "\n... [truncated]\n"
}

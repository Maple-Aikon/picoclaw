package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/tools"
)

// Tests for ToolHealthContributor — plan:
// exception-handling-recovery-pattern-gap-closure-20260628 (Task B1).
//
// The contributor is a Steering-layer prompt source that emits a transient
// "tool unavailable" directive whenever at least one circuit breaker is open.
// Zero-cost when all tools are healthy (returns nil parts).

func TestToolHealthContributor_NoOpenTools_ReturnsNil(t *testing.T) {
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo { return nil },
	}
	parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parts != nil {
		t.Fatalf("parts = %+v, want nil (zero-cost path when no tools are open)", parts)
	}
}

func TestToolHealthContributor_OneOpenTool_EmitsDirective(t *testing.T) {
	// Use 125s ago so the formatter renders "2m5s" — robust against
	// millisecond-level test jitter (a 1s delay still shows as "2m5s" or
	// "2m6s", both of which contain "2m").
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo {
			return []tools.OpenToolInfo{
				{
					Name:     "web_search",
					OpenedAt: time.Now().Add(-125 * time.Second),
					Failures: 3,
				},
			}
		},
	}
	parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts count = %d, want 1", len(parts))
	}
	p := parts[0]
	if p.Layer != PromptLayerTurn {
		t.Errorf("Layer = %q, want %q", p.Layer, PromptLayerTurn)
	}
	if p.Slot != PromptSlotSteering {
		t.Errorf("Slot = %q, want %q", p.Slot, PromptSlotSteering)
	}
	if p.Cache != PromptCacheNone {
		t.Errorf("Cache = %q, want %q (transient steering must not cache)",
			p.Cache, PromptCacheNone)
	}
	if p.Stable {
		t.Errorf("Stable = true, want false (transient by nature)")
	}
	if !strings.Contains(p.Content, "web_search") {
		t.Errorf("Content missing tool name: %q", p.Content)
	}
	if !strings.Contains(p.Content, "2m") {
		t.Errorf("Content missing age value (expected '2m...' for 125s): %q", p.Content)
	}
	if !strings.Contains(p.Content, "3") {
		t.Errorf("Content missing failure count: %q", p.Content)
	}
	lower := strings.ToLower(p.Content)
	if !strings.Contains(lower, "unavailable") &&
		!strings.Contains(lower, "do not call") {
		t.Errorf("Content missing directive language: %q", p.Content)
	}
}

func TestToolHealthContributor_MultipleOpenTools_AllListed(t *testing.T) {
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo {
			return []tools.OpenToolInfo{
				{
					Name:     "tavily_search",
					OpenedAt: time.Now().Add(-125 * time.Second),
					Failures: 5,
				},
				{
					Name:     "web_search",
					OpenedAt: time.Now().Add(-65 * time.Second),
					Failures: 3,
				},
			}
		},
	}
	parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts count = %d, want 1 (single Steering part listing all)", len(parts))
	}
	if !strings.Contains(parts[0].Content, "tavily_search") {
		t.Errorf("Content missing tavily_search: %q", parts[0].Content)
	}
	if !strings.Contains(parts[0].Content, "web_search") {
		t.Errorf("Content missing web_search: %q", parts[0].Content)
	}
}

func TestToolHealthContributor_PromptSource_DeclaresSteeringSlot(t *testing.T) {
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo { return nil },
	}
	desc := c.PromptSource()
	if desc.ID != PromptSourceToolHealth {
		t.Errorf("ID = %q, want %q", desc.ID, PromptSourceToolHealth)
	}
	if len(desc.Allowed) != 1 {
		t.Fatalf("Allowed count = %d, want 1", len(desc.Allowed))
	}
	p := desc.Allowed[0]
	if p.Layer != PromptLayerTurn {
		t.Errorf("Placement.Layer = %q, want %q", p.Layer, PromptLayerTurn)
	}
	if p.Slot != PromptSlotSteering {
		t.Errorf("Placement.Slot = %q, want %q", p.Slot, PromptSlotSteering)
	}
	if desc.StableByDefault {
		t.Error("StableByDefault = true, want false (transient steering directive)")
	}
}

// TestToolHealthContributor_ContextBuilderIntegration covers plan Task B2:
// after RegisterPromptContributor, the system prompt produced by a real
// ContextBuilder must contain the Steering directive text when at least one
// tool is open.
func TestToolHealthContributor_ContextBuilderIntegration(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())

	err := cb.RegisterPromptContributor(&ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo {
			return []tools.OpenToolInfo{
				{
					Name:     "tavily_search",
					OpenedAt: time.Now().Add(-125 * time.Second),
					Failures: 5,
				},
			}
		},
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage: "thời tiết Huế hôm nay?",
	})

	if len(messages) == 0 {
		t.Fatal("BuildMessagesFromPrompt returned no messages")
	}
	content := messages[0].Content
	if !strings.Contains(content, "Tool Availability") {
		t.Errorf("system prompt missing 'Tool Availability' heading. content head: %q",
			truncateForLog(content, 400))
	}
	if !strings.Contains(content, "tavily_search") {
		t.Errorf("system prompt missing tool name 'tavily_search'. content head: %q",
			truncateForLog(content, 400))
	}
	if !strings.Contains(content, "unavailable") {
		t.Errorf("system prompt missing 'unavailable' directive. content head: %q",
			truncateForLog(content, 400))
	}
}

// TestToolHealthContributor_ContextBuilderIntegration_Healthy omits the
// contributor → system prompt must NOT contain the directive. Guards against
// false positives where the directive leaks in even when no breaker is open.
func TestToolHealthContributor_ContextBuilderIntegration_Healthy(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())

	// Register with empty listOpen to simulate a healthy system.
	err := cb.RegisterPromptContributor(&ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo { return nil },
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage: "hello",
	})
	if len(messages) == 0 {
		t.Fatal("BuildMessagesFromPrompt returned no messages")
	}
	if strings.Contains(messages[0].Content, "Tool Availability") {
		t.Errorf("healthy system should not emit Tool Availability directive. content head: %q",
			truncateForLog(messages[0].Content, 400))
	}
}

// truncateForLog keeps long system-prompt dumps readable in failure output.
// Returns the first n bytes plus a marker if the content was longer.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated " + itoaForLog(len(s)-n) + " bytes]"
}

func itoaForLog(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}// TestToolHealthContributor_LastErrorKindFormat verifies the per-tool
// `last_error_kind` suffix added in plan
// circuit-breaker-3-tier-errkind-semantics-toolfeedback-20260717:
//
//   - When OpenToolInfo.LastErrorKind is non-empty → format appends
//     `, last_error_kind: <kind>` so the LLM knows whether to wait (transient)
//     vs escalate (dependency down).
//   - When LastErrorKind is empty (legacy callers / pre-this-plan state) →
//     no suffix emitted (backward-compatible with existing fixtures).
//
// Also asserts the bypass-warning header text mentions auto-recovery so the
// LLM treats the directive as a transient steering signal, not a permanent
// capability removal.
func TestToolHealthContributor_LastErrorKindFormat(t *testing.T) {
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo {
			return []tools.OpenToolInfo{
				{
					Name:          "tavily_search",
					OpenedAt:      time.Now().Add(-125 * time.Second),
					Failures:      3,
					LastErrorKind: tools.ErrDependencyDown,
				},
				{
					Name:          "web_search",
					OpenedAt:      time.Now().Add(-65 * time.Second),
					Failures:      3,
					LastErrorKind: tools.ErrTransient,
				},
				{
					Name:     "legacy_tool", // pre-plan state, LastErrorKind unset
					OpenedAt: time.Now().Add(-30 * time.Second),
					Failures: 3,
				},
			}
		},
	}
	parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts count = %d, want 1", len(parts))
	}
	content := parts[0].Content

	// Auto-recovery hint in the bypass-warning header.
	lower := strings.ToLower(content)
	if !strings.Contains(lower, "auto-recover") && !strings.Contains(lower, "auto recover") {
		t.Errorf("Content missing auto-recovery bypass-warning header: %q", content)
	}

	// Per-tool suffix present when LastErrorKind is set.
	if !strings.Contains(content, "last_error_kind: dependency_down") {
		t.Errorf("Content missing 'last_error_kind: dependency_down' suffix for tavily_search: %q", content)
	}
	if !strings.Contains(content, "last_error_kind: transient") {
		t.Errorf("Content missing 'last_error_kind: transient' suffix for web_search: %q", content)
	}

	// No suffix for the legacy fixture (LastErrorKind empty).
	idx := strings.Index(content, "legacy_tool")
	if idx == -1 {
		t.Fatalf("Content missing legacy_tool: %q", content)
	}
	lineEnd := strings.Index(content[idx:], "\n")
	if lineEnd == -1 {
		lineEnd = len(content) - idx
	}
	line := content[idx : idx+lineEnd]
	if strings.Contains(line, "last_error_kind") {
		t.Errorf("legacy_tool line should NOT contain last_error_kind (LastErrorKind unset): %q", line)
	}
}
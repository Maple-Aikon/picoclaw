package tools

// =============================================================================
// Auto-load injection telemetry — pkg/tools side (escalate path)
//
// Plan: tool-knowledge-instrumentation-đếm-auto-load-injected-mục-3-tách-từ-plan-10-21-20261112
//
// What these tests pin:
//
//  1. The escalate counter fires ONLY when the escalation hint actually
//     carries a tool_knowledge lesson. An escalation with no lesson
//     (the tool has no stored entry) must NOT be counted — counting it
//     would inflate the total with escalations that carried nothing.
//
//  2. The counter measures INJECTIONS, not loads: a transient failure
//     below the escalation threshold loads the knowledge file but
//     injects nothing, so nothing is counted.
//
//  3. The key carries all four dimensions (channel, chatID, tool, path)
//     and an EMPTY channel/chatID becomes "<anon>" — never "<nil>", and
//     never a collision with a real session.
//
//  4. The byte dimensions are exact: body_bytes = the lesson body,
//     section_bytes = the full marker-wrapped section.
//
//  5. A lesson-less message is not miscounted as an injection.
// =============================================================================

import (
	"context"
	"strings"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// knowledgeBackedRegistry builds a registry whose tool "lesson_tool" has
// a stored lesson, so the escalation hint carries the knowledge section.
func knowledgeBackedRegistry(t *testing.T) *ToolRegistry {
	t.Helper()
	ws := t.TempDir()
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}
	lesson := "## What goes wrong\n" +
		"Lesson body that must reach the LLM when the tool fails."
	if _, _, err := store.Replace("lesson_tool", lesson); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	r := NewToolRegistry()
	r.SetToolKnowledgeStore(store)
	return r
}

// failingTool always fails with a transient error, so the signature
// tracker counts it toward the escalation threshold.
type failingTool struct {
	name string
}

func (m *failingTool) Name() string { return m.name }
func (m *failingTool) Description() string {
	return "mock tool that always fails transiently"
}
func (m *failingTool) Parameters() map[string]any {
	return map[string]any{"type": "object"}
}
func (m *failingTool) Execute(_ context.Context, _ map[string]any) *toolshared.ToolResult {
	return toolshared.ErrorResult("network blip").WithErrorKind(toolshared.ErrTransient)
}

// TestAutoLoadEscalate_CountedOnlyWhenLessonPresent asserts that an
// escalation carrying a lesson IS counted, with the exact byte
// dimensions and the full four-part key.
//
// Negative control: the same escalation for a tool WITHOUT a stored
// lesson must NOT be counted (the payload carries no lesson section).
func TestAutoLoadEscalate_CountedOnlyWhenLessonPresent(t *testing.T) {
	ResetToolsAutoLoadCounterForTest()
	t.Cleanup(ResetToolsAutoLoadCounterForTest)

	r := knowledgeBackedRegistry(t)
	r.Register(&failingTool{name: "lesson_tool"})

	// Failures #1 and #2 are BELOW the threshold (3) — the knowledge file
	// is LOADED (toolKnowledgeFor is evaluated as an argument) but
	// nothing is injected, so the counter must stay at 0.
	for i := 1; i <= 2; i++ {
		res := r.ExecuteWithContext(
			context.Background(), "lesson_tool", nil, "telegram", "chat-1", nil,
		)
		if strings.Contains(res.ForLLM, KnowledgeSectionHeader) {
			t.Fatalf("call #%d must not carry a lesson below threshold", i)
		}
		if got := ToolsAutoLoadCounter().Total.Load(); got != 0 {
			t.Fatalf("call #%d: counter must be 0 below the escalation threshold, got %d", i, got)
		}
	}

	// Call #3 reaches the threshold — the hint (and the lesson) is
	// injected into ForLLM.
	res := r.ExecuteWithContext(
		context.Background(), "lesson_tool", nil, "telegram", "chat-1", nil,
	)
	if !strings.Contains(res.ForLLM, KnowledgeSectionHeader) {
		t.Fatalf("call #3 must carry the lesson section, got %q", res.ForLLM)
	}

	stats := ToolsAutoLoadStatsSnapshot()
	if stats.Total != 1 {
		t.Fatalf("expected exactly 1 injection, got %d", stats.Total)
	}

	key := AutoLoadKey("telegram", "chat-1", "lesson_tool", AutoLoadPathEscalate)
	stat, ok := stats.ByKey[key]
	if !ok {
		t.Fatalf("key %q missing from snapshot: %v", key, stats.ByKey)
	}
	if stat.Count != 1 {
		t.Errorf("key count: expected 1, got %d", stat.Count)
	}
	body, ok := ExtractInjectedKnowledge(res.ForLLM)
	if !ok {
		t.Fatal("expected the lesson body to be extractable")
	}
	if stat.Bytes != int64(len(AppendKnowledgeSection(body))) {
		t.Errorf("section_bytes mismatch: got %d want %d",
			stat.Bytes, len(AppendKnowledgeSection(body)))
	}
	if stat.BodyBytes != int64(len(body)) {
		t.Errorf("body_bytes mismatch: got %d want %d", stat.BodyBytes, len(body))
	}
	if stat.BodyBytes <= 0 || stat.BodyBytes >= stat.Bytes {
		t.Errorf("body_bytes (%d) must be positive and smaller than section_bytes (%d)",
			stat.BodyBytes, stat.Bytes)
	}
	if stats.TotalBytes != stat.Bytes {
		t.Errorf("total bytes (%d) must equal the single key's bytes (%d)",
			stats.TotalBytes, stat.Bytes)
	}
}

// TestAutoLoadEscalate_NoLessonNotCounted asserts the negative control:
// an escalation for a tool WITHOUT a stored lesson injects the
// escalation directive but carries NO lesson section, so the counter
// must not move.
func TestAutoLoadEscalate_NoLessonNotCounted(t *testing.T) {
	ResetToolsAutoLoadCounterForTest()
	t.Cleanup(ResetToolsAutoLoadCounterForTest)

	r := NewToolRegistry()
	r.Register(&failingTool{name: "no_lesson_tool"})

	var last string
	for i := 1; i <= 3; i++ {
		res := r.ExecuteWithContext(
			context.Background(), "no_lesson_tool", nil, "telegram", "chat-2", nil,
		)
		last = res.ForLLM
	}
	if !strings.Contains(last, "has failed 3 times in this turn") {
		t.Fatalf("expected the escalation directive, got %q", last)
	}
	if got := ToolsAutoLoadCounter().Total.Load(); got != 0 {
		t.Fatalf("an escalation WITHOUT a lesson must not be counted, got %d", got)
	}
}

// TestAutoLoadEscalate_EmptyChannelChatID_AnonNotNil asserts that an
// empty channel/chatID (CLI, tests) becomes "<anon>" in the key — it
// never collides with a real session and never renders as "<nil>".
func TestAutoLoadEscalate_EmptyChannelChatID_AnonNotNil(t *testing.T) {
	ResetToolsAutoLoadCounterForTest()
	t.Cleanup(ResetToolsAutoLoadCounterForTest)

	r := knowledgeBackedRegistry(t)
	r.Register(&failingTool{name: "lesson_tool"})

	for i := 1; i <= 3; i++ {
		r.ExecuteWithContext(
			context.Background(), "lesson_tool", nil, "", "", nil,
		)
	}

	stats := ToolsAutoLoadStatsSnapshot()
	if stats.Total != 1 {
		t.Fatalf("expected 1 injection, got %d", stats.Total)
	}
	anonKey := AutoLoadKey("", "", "lesson_tool", AutoLoadPathEscalate)
	if !strings.Contains(anonKey, AutoLoadAnonValue) {
		t.Errorf("empty channel/chatID must become %q, got key %q", AutoLoadAnonValue, anonKey)
	}
	if strings.Contains(anonKey, "<nil>") {
		t.Errorf("key must never contain %q, got %q", "<nil>", anonKey)
	}
	if _, ok := stats.ByKey[anonKey]; !ok {
		t.Errorf("anon key %q missing from snapshot: %v", anonKey, stats.ByKey)
	}
	// The anonymous session must be a DIFFERENT key from a real one.
	realKey := AutoLoadKey("telegram", "chat-9", "lesson_tool", AutoLoadPathEscalate)
	if anonKey == realKey {
		t.Errorf("anonymous and real sessions must not share a key: %q", anonKey)
	}
}

// TestAutoLoadKey_AllFourDimensions asserts that every dimension
// changes the key — a counter that merged two dimensions would report
// two sessions as one.
func TestAutoLoadKey_AllFourDimensions(t *testing.T) {
	base := AutoLoadKey("telegram", "chat-1", "exec", AutoLoadPathRecovery)
	variants := map[string]string{
		"channel": AutoLoadKey("cli", "chat-1", "exec", AutoLoadPathRecovery),
		"chatID":  AutoLoadKey("telegram", "chat-2", "exec", AutoLoadPathRecovery),
		"tool":    AutoLoadKey("telegram", "chat-1", "read_file", AutoLoadPathRecovery),
		"path":    AutoLoadKey("telegram", "chat-1", "exec", AutoLoadPathEscalate),
	}
	seen := map[string]string{base: "base"}
	for dim, key := range variants {
		if key == base {
			t.Errorf("dimension %q does not change the key", dim)
		}
		if prev, dup := seen[key]; dup {
			t.Errorf("dimension %q collides with %q", dim, prev)
		}
		seen[key] = dim
	}
}

// TestExtractInjectedKnowledgeSection_ExactBytes asserts the byte
// dimensions are measured on the EXACT section text, not on a
// reconstruction.
func TestExtractInjectedKnowledgeSection_ExactBytes(t *testing.T) {
	body := "Lesson body text."
	section := AppendKnowledgeSection(body)
	msg := "prefix text\n\n" + section + "\n\n" + "suffix text"

	gotBody, gotSection, ok := ExtractInjectedKnowledgeSection(msg)
	if !ok {
		t.Fatal("expected the section to be detected")
	}
	if gotBody != body {
		t.Errorf("body mismatch: got %q want %q", gotBody, body)
	}
	if gotSection != section {
		t.Errorf("section mismatch: got %q want %q", gotSection, section)
	}
	if len(gotSection) != len(section) {
		t.Errorf("section length mismatch: got %d want %d", len(gotSection), len(section))
	}
}

// TestExtractInjectedKnowledgeSection_NoSection asserts a message
// without a lesson is not mistaken for an injection.
func TestExtractInjectedKnowledgeSection_NoSection(t *testing.T) {
	cases := []string{
		"",
		"Tool \"exec\" failed: boom",
		KnowledgeSectionHeader,
		KnowledgeSectionHeader + "\n",
		KnowledgeSectionHeader + "\n\n" + KnowledgeSectionFooter,
	}
	for _, msg := range cases {
		if _, _, ok := ExtractInjectedKnowledgeSection(msg); ok {
			t.Errorf("must NOT detect a lesson in %q", msg)
		}
	}
}

// TestAutoLoadToolNameFromMessage asserts the tool name is read from
// the recovery message shape and never guessed wrongly.
func TestAutoLoadToolNameFromMessage(t *testing.T) {
	cases := map[string]string{
		`Tool "exec" failed: boom`:             "exec",
		`Tool "mcp_janus-graph_search" failed`: "mcp_janus-graph_search",
		"not a recovery message":               AutoLoadUnknownTool,
		`Tool "" failed`:                       AutoLoadUnknownTool,
		"":                                     AutoLoadUnknownTool,
	}
	for msg, want := range cases {
		if got := AutoLoadToolNameFromMessage(msg); got != want {
			t.Errorf("tool name for %q: got %q want %q", msg, got, want)
		}
	}
}

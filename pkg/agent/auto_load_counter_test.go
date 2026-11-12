package agent

// =============================================================================
// Auto-load injection telemetry — pkg/agent side (recovery path)
//
// Plan: tool-knowledge-instrumentation-đếm-auto-load-injected-mục-3-tách-từ-plan-10-21-20261112
//
// What these tests pin:
//
//  1. OVER-COUNT GUARD (the important one). `checkToolExecErrorRecovery`
//     has TWO callsites (turn_coord.go:475 and retry_execute_tool_chain.go:348)
//     and `ts.toolExecRecoveryAttempts` is NOT reset between them, so the
//     same failure can BUILD the retry message twice. A counter placed at
//     the build site would count 2 for one injection. These tests assert
//     the counter stays at ONE per real injection.
//
//  2. The recovery path counts WITHOUT any escalation threshold — the
//     escalate path needs 3 identical failures, the recovery path needs
//     one. A counter that only worked on the escalate path would miss
//     the path that actually fires (~8/day per the 2026-11-12 baseline).
//
//  3. A recovery message WITHOUT a lesson is not counted.
//
//  4. The tool name is taken from the message shape; the key carries all
//     four dimensions; an empty channel/chatID becomes "<anon>".
// =============================================================================

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// seedKnowledgeLesson stores a lesson for `tool` and returns a registry
// wired to it.
func seedKnowledgeLesson(t *testing.T, tool string) *tools.ToolRegistry {
	t.Helper()
	ws := t.TempDir()
	store, err := tools.NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}
	lesson := "## What goes wrong\n" +
		"The lesson body that must reach the LLM on a tool failure."
	if _, _, err := store.Replace(tool, lesson); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	registry := tools.NewToolRegistry()
	registry.SetToolKnowledgeStore(store)
	return registry
}

// toolFailureExec builds an exec whose last message is a tool error for
// `tool`, so checkToolExecErrorRecovery can resolve the real tool name.
func toolFailureExec(tool, content string) *turnExecution {
	return &turnExecution{
		messages: []providers.Message{
			{
				Role: "assistant",
				ToolCalls: []providers.ToolCall{
					{ID: "call_abc_123_1", Name: tool},
				},
			},
			{Role: "tool", ToolCallID: "call_abc_123_1", Content: content},
		},
	}
}

// TestAutoLoadRecovery_CountsInjection_NotBuilds is the OVER-COUNT
// GUARD.
//
// The scenario: the SAME tool failure passes through BOTH callsites of
// checkToolExecErrorRecovery (turn_coord.go and retry_execute_tool_chain.go).
// The second pass BUILDS the retry message again — but the counter must
// only move once per INJECTION.
//
// How the test proves it: the build is done twice (exactly what the two
// callsites do), but the counter is only allowed to move when the message
// actually enters the LLM context. A counter placed at the build site
// would report 2 here; the injection-site counter reports 1.
func TestAutoLoadRecovery_CountsInjection_NotBuilds(t *testing.T) {
	ResetAgentAutoLoadCounterForTest()
	t.Cleanup(ResetAgentAutoLoadCounterForTest)

	ts := newPhase5TurnState(t)
	registry := seedKnowledgeLesson(t, "read_file")
	ts.agent.Tools = registry
	ts.channel = "telegram"
	ts.chatID = "chat-over"

	exec := toolFailureExec("read_file", "Tool execution failed: connection refused")

	// Pass 1 — the build happens.
	_, msg1 := checkToolExecErrorRecovery(ts, exec)
	if msg1 == "" {
		t.Fatal("expected a retry message from the first pass")
	}
	if !strings.Contains(msg1, tools.KnowledgeSectionHeader) {
		t.Fatalf("first pass must carry the lesson: %q", msg1)
	}
	// The BUILD alone must NOT move the counter — nothing reached the LLM.
	if got := AgentAutoLoadCounter().Total.Load(); got != 0 {
		t.Fatalf("a BUILD must not be counted as an injection, got %d", got)
	}

	// Pass 2 — the second callsite builds the message AGAIN. The recovery
	// counter is NOT reset between the two callsites, so the message is
	// rebuilt (over-count risk) even though only one injection happens.
	_, msg2 := checkToolExecErrorRecovery(ts, exec)
	if msg2 == "" {
		t.Fatal("expected a retry message from the second pass")
	}
	if got := AgentAutoLoadCounter().Total.Load(); got != 0 {
		t.Fatalf("a second BUILD must still not be counted, got %d", got)
	}

	// Now the message actually enters the LLM context — ONE injection.
	countRecoveryAutoLoadInject(ts, msg1)

	stats := AutoLoadStatsSnapshot()
	if stats.Total != 1 {
		t.Fatalf("expected exactly 1 injection after 2 builds + 1 injection, got %d", stats.Total)
	}

	key := tools.AutoLoadKey("telegram", "chat-over", "read_file", tools.AutoLoadPathRecovery)
	stat, ok := stats.ByKey[key]
	if !ok {
		t.Fatalf("key %q missing from snapshot: %v", key, stats.ByKey)
	}
	if stat.Count != 1 {
		t.Errorf("key count: expected 1, got %d", stat.Count)
	}
	if stat.Bytes <= 0 {
		t.Errorf("section bytes must be positive, got %d", stat.Bytes)
	}
	if stat.BodyBytes <= 0 || stat.BodyBytes >= stat.Bytes {
		t.Errorf("body_bytes (%d) must be positive and smaller than section_bytes (%d)",
			stat.BodyBytes, stat.Bytes)
	}
}

// TestAutoLoadRecovery_CountsWithoutThreshold asserts the recovery path
// counts on the FIRST failure — no escalation threshold is involved.
// The escalate path needs 3 identical failures; the recovery path does
// not, and this test pins that difference.
func TestAutoLoadRecovery_CountsWithoutThreshold(t *testing.T) {
	ResetAgentAutoLoadCounterForTest()
	t.Cleanup(ResetAgentAutoLoadCounterForTest)

	ts := newPhase5TurnState(t)
	registry := seedKnowledgeLesson(t, "read_file")
	ts.agent.Tools = registry
	ts.channel = "telegram"
	ts.chatID = "chat-threshold"

	exec := toolFailureExec("read_file", "Tool execution failed: connection refused")
	_, msg := checkToolExecErrorRecovery(ts, exec)
	if msg == "" {
		t.Fatal("expected a retry message on the FIRST failure")
	}
	countRecoveryAutoLoadInject(ts, msg)

	if got := AgentAutoLoadCounter().Total.Load(); got != 1 {
		t.Fatalf("recovery path must count on the first failure (no threshold), got %d", got)
	}
}

// TestAutoLoadRecovery_NoLessonNotCounted asserts the negative control:
// a recovery message with NO lesson section is not counted. Otherwise
// the total would be "recoveries", not "recoveries that carried a
// lesson" — which is the number the baseline probe could not separate.
func TestAutoLoadRecovery_NoLessonNotCounted(t *testing.T) {
	ResetAgentAutoLoadCounterForTest()
	t.Cleanup(ResetAgentAutoLoadCounterForTest)

	ts := newPhase5TurnState(t)
	// No knowledge store at all -> the retry message carries no lesson.
	ts.agent.Tools = tools.NewToolRegistry()
	ts.channel = "telegram"
	ts.chatID = "chat-nolesson"

	exec := toolFailureExec("read_file", "Tool execution failed: connection refused")
	_, msg := checkToolExecErrorRecovery(ts, exec)
	if msg == "" {
		t.Fatal("expected a retry message")
	}
	if strings.Contains(msg, tools.KnowledgeSectionHeader) {
		t.Fatalf("this fixture must NOT carry a lesson: %q", msg)
	}

	countRecoveryAutoLoadInject(ts, msg)
	if got := AgentAutoLoadCounter().Total.Load(); got != 0 {
		t.Fatalf("a recovery WITHOUT a lesson must not be counted, got %d", got)
	}
}

// TestAutoLoadRecovery_EmptyChannelChatID_AnonNotNil asserts the key
// normalises an empty channel/chatID to "<anon>" — never "<nil>", never
// a collision with a real session.
func TestAutoLoadRecovery_EmptyChannelChatID_AnonNotNil(t *testing.T) {
	ResetAgentAutoLoadCounterForTest()
	t.Cleanup(ResetAgentAutoLoadCounterForTest)

	ts := newPhase5TurnState(t)
	registry := seedKnowledgeLesson(t, "read_file")
	ts.agent.Tools = registry
	ts.channel = ""
	ts.chatID = ""

	exec := toolFailureExec("read_file", "Tool execution failed: connection refused")
	_, msg := checkToolExecErrorRecovery(ts, exec)
	countRecoveryAutoLoadInject(ts, msg)

	stats := AutoLoadStatsSnapshot()
	if stats.Total != 1 {
		t.Fatalf("expected 1 injection, got %d", stats.Total)
	}
	anonKey := tools.AutoLoadKey("", "", "read_file", tools.AutoLoadPathRecovery)
	if !strings.Contains(anonKey, tools.AutoLoadAnonValue) {
		t.Errorf("empty channel/chatID must become %q, got %q", tools.AutoLoadAnonValue, anonKey)
	}
	if strings.Contains(anonKey, "<nil>") {
		t.Errorf("key must never contain %q, got %q", "<nil>", anonKey)
	}
	if _, ok := stats.ByKey[anonKey]; !ok {
		t.Errorf("anon key %q missing from snapshot: %v", anonKey, stats.ByKey)
	}
}

// TestAutoLoadRecovery_ToolNameFromMessage asserts the counter keys on
// the REAL tool name carried by the recovery message, not on a guess.
func TestAutoLoadRecovery_ToolNameFromMessage(t *testing.T) {
	ResetAgentAutoLoadCounterForTest()
	t.Cleanup(ResetAgentAutoLoadCounterForTest)

	ts := newPhase5TurnState(t)
	registry := seedKnowledgeLesson(t, "read_file")
	ts.agent.Tools = registry
	ts.channel = "telegram"
	ts.chatID = "chat-tool"

	exec := toolFailureExec("read_file", "Tool execution failed: connection refused")
	_, msg := checkToolExecErrorRecovery(ts, exec)
	countRecoveryAutoLoadInject(ts, msg)

	stats := AutoLoadStatsSnapshot()
	realKey := tools.AutoLoadKey("telegram", "chat-tool", "read_file", tools.AutoLoadPathRecovery)
	if _, ok := stats.ByKey[realKey]; !ok {
		t.Errorf("expected key %q in snapshot, got %v", realKey, stats.ByKey)
	}
	unknownKey := tools.AutoLoadKey("telegram", "chat-tool", tools.AutoLoadUnknownTool, tools.AutoLoadPathRecovery)
	if _, ok := stats.ByKey[unknownKey]; ok {
		t.Errorf("tool name must be read from the message, not defaulted to %q: %v",
			tools.AutoLoadUnknownTool, stats.ByKey)
	}
}

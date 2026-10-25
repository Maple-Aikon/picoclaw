// Phase 12.62 (2026-10-25) — tool-name resolution for tool_knowledge
// auto-load. The recovery layer used the tool-call ID as the tool name;
// call IDs carry NO tool name (call_<turnID>_<unixNano>_<seq>), so every
// tool_knowledge lookup was keyed by a string that can never match a
// knowledge file.

package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// TestToolNameFromMessages_ResolvesRealToolName is the core Q1
// invariant: the assistant message that issued the call carries
// ToolCalls[{ID, Name}], so the real tool name is recoverable from the
// message history.
func TestToolNameFromMessages_ResolvesRealToolName(t *testing.T) {
	messages := []providers.Message{
		{Role: "user", Content: "do the thing"},
		{
			Role: "assistant",
			ToolCalls: []providers.ToolCall{
				{ID: "call_abc_123_1", Name: "read_file"},
				{ID: "call_abc_123_2", Name: "exec"},
			},
		},
		{Role: "tool", ToolCallID: "call_abc_123_2", Content: "boom"},
	}
	if got := toolNameFromMessages(messages, "call_abc_123_2"); got != "exec" {
		t.Fatalf("expected tool name %q, got %q", "exec", got)
	}
	if got := toolNameFromMessages(messages, "call_abc_123_1"); got != "read_file" {
		t.Fatalf("expected tool name %q, got %q", "read_file", got)
	}
}

// TestToolNameFromMessages_UnknownCallID verifies the helper returns ""
// for an ID that is not in any assistant message — the caller then falls
// back to the call-id (pre-Phase-12.62 behaviour).
func TestToolNameFromMessages_UnknownCallID(t *testing.T) {
	messages := []providers.Message{
		{
			Role:      "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_abc_123_1", Name: "read_file"}},
		},
	}
	if got := toolNameFromMessages(messages, "call_other_999_1"); got != "" {
		t.Fatalf("expected empty for unknown call id, got %q", got)
	}
	if got := toolNameFromMessages(messages, ""); got != "" {
		t.Fatalf("expected empty for empty call id, got %q", got)
	}
	if got := toolNameFromMessages(nil, "call_abc_123_1"); got != "" {
		t.Fatalf("expected empty for nil messages, got %q", got)
	}
}

// TestToolNameFromMessages_PrefersMostRecentAssistantMessage verifies
// the backwards scan picks the LAST assistant message that mentions the
// ID (a later turn reusing an ID must win).
func TestToolNameFromMessages_PrefersMostRecentAssistantMessage(t *testing.T) {
	messages := []providers.Message{
		{
			Role:      "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_x_1_1", Name: "old_name"}},
		},
		{Role: "tool", ToolCallID: "call_x_1_1", Content: "boom"},
		{
			Role:      "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_x_1_1", Name: "new_name"}},
		},
	}
	if got := toolNameFromMessages(messages, "call_x_1_1"); got != "new_name" {
		t.Fatalf("expected %q, got %q", "new_name", got)
	}
}

// TestCheckToolExecErrorRecovery_UsesRealToolName is the WIRING test:
// the recovery helper must key its per-tool counter and its knowledge
// lookup by the REAL tool name, not by the call-id.
func TestCheckToolExecErrorRecovery_UsesRealToolName(t *testing.T) {
	ts := newPhase5TurnState(t)
	exec := &turnExecution{
		messages: []providers.Message{
			{
				Role: "assistant",
				ToolCalls: []providers.ToolCall{
					{ID: "call_abc_123_1", Name: "read_file"},
				},
			},
			{Role: "tool", ToolCallID: "call_abc_123_1", Content: "Tool execution failed: connection refused"},
		},
	}
	tool, _ := checkToolExecErrorRecovery(ts, exec)
	if tool != "" {
		t.Fatalf("expected retry (no archive) on first error, got tool=%q", tool)
	}
	if ts.toolExecRecoveryAttempts["read_file"] != 1 {
		t.Fatalf("expected attempt count keyed by real tool name, got map=%v", ts.toolExecRecoveryAttempts)
	}
	if _, ok := ts.toolExecRecoveryAttempts["call_abc_123_1"]; ok {
		t.Fatalf("attempt counter must NOT be keyed by call-id: %v", ts.toolExecRecoveryAttempts)
	}
}

// TestCheckToolExecErrorRecovery_NegativeControl_NoAssistantMessage pins
// the FAIL-SAFE fallback: when the assistant message is missing (e.g.
// dropped by compaction) the helper falls back to the call-id instead of
// returning "" and silently skipping recovery.
func TestCheckToolExecErrorRecovery_NegativeControl_NoAssistantMessage(t *testing.T) {
	ts := newPhase5TurnState(t)
	exec := &turnExecution{
		messages: []providers.Message{
			{Role: "tool", ToolCallID: "call_abc_123_1", Content: "Tool execution failed: connection refused"},
		},
	}
	tool, _ := checkToolExecErrorRecovery(ts, exec)
	if tool != "" {
		t.Fatalf("expected retry (no archive) on first error, got tool=%q", tool)
	}
	if ts.toolExecRecoveryAttempts["call_abc_123_1"] != 1 {
		t.Fatalf("expected fallback to call-id keying, got map=%v", ts.toolExecRecoveryAttempts)
	}
}

// TestCheckToolExecErrorRecovery_AutoLoadsLessonByRealToolName is the
// END-TO-END NEGATIVE CONTROL for Q1: a knowledge entry for the failing
// tool MUST appear in the retry message. Before Phase 12.62 the lookup
// was keyed by the call-id, so the lesson was NEVER injected and the LLM
// kept repeating the same mistake.
func TestCheckToolExecErrorRecovery_AutoLoadsLessonByRealToolName(t *testing.T) {
	ws := t.TempDir()
	store, err := tools.NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}
	lesson := "## What goes wrong\n" +
		"The lesson body that must reach the LLM on a tool failure."
	if _, _, err := store.Replace("read_file", lesson); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	registry := tools.NewToolRegistry()
	registry.SetToolKnowledgeStore(store)

	ts := newPhase5TurnState(t)
	ts.agent.Tools = registry
	exec := &turnExecution{
		messages: []providers.Message{
			{
				Role: "assistant",
				ToolCalls: []providers.ToolCall{
					{ID: "call_abc_123_1", Name: "read_file"},
				},
			},
			{Role: "tool", ToolCallID: "call_abc_123_1", Content: "Tool execution failed: connection refused"},
		},
	}
	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool != "" {
		t.Fatalf("expected retry (no archive), got tool=%q", tool)
	}
	if !strings.Contains(msg, "The lesson body that must reach the LLM") {
		t.Fatalf("lesson was NOT auto-loaded into the retry message: %q", msg)
	}
	if !strings.Contains(msg, `"read_file"`) {
		t.Fatalf("retry message must name the real tool: %q", msg)
	}
}

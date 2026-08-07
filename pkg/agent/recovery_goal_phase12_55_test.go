// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	tools "github.com/sipeed/picoclaw/pkg/tools"
	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// Phase 12.55 T4: ErrDependencyDown becomes recoverable (Q1). Previously
// it was fail-fast (CB open after 1 failure); now it joins the common
// tool-exec retry ×3. checkToolExecErrorRecovery must return a non-empty
// msg (retry hint) for ErrDependencyDown at OPEN.
func TestCheckToolExecErrorRecovery_ErrDependencyDown_Recoverable(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)
	// setupRetryChainTestTurnState seeds an ACTIVE goal at GoalPhaseOpen.

	// Simulate the last message being a tool error result with
	// ErrKind=ErrDependencyDown (e.g. MCP upstream failure).
	exec.messages = append(exec.messages, toolRoleMessage("web_search", "Tool execution failed: upstream 403"))
	ts.lastToolResult = &tools.ToolResult{ErrKind: toolshared.ErrDependencyDown}

	toolName, msg := checkToolExecErrorRecovery(ts, exec)
	if toolName != "" {
		t.Errorf("toolName = %q, want \"\" (no archive at OPEN)", toolName)
	}
	if msg == "" {
		t.Error("msg = \"\", want non-empty retry hint for ErrDependencyDown (Q1)")
	}
}

// Regression guard: unknown ErrKind with no prefix marker stays
// non-recoverable (empty ErrKind + non-prefix content → no retry).
func TestCheckToolExecErrorRecovery_UnknownKind_NoMarkerStillNonRecoverable(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)

	exec.messages = append(exec.messages, toolRoleMessage("web_search", "random failure text without marker"))
	ts.lastToolResult = &tools.ToolResult{ErrKind: ""}

	toolName, msg := checkToolExecErrorRecovery(ts, exec)
	if toolName != "" || msg != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\") for unknown kind + no marker", toolName, msg)
	}
}

func TestCheckToolExecErrorRecovery_ErrInvalidInput_StillRecoverable(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)

	exec.messages = append(exec.messages, toolRoleMessage("complete_goal", "invalid arguments for tool"))
	ts.lastToolResult = &tools.ToolResult{ErrKind: toolshared.ErrInvalidInput}

	toolName, msg := checkToolExecErrorRecovery(ts, exec)
	if toolName != "" {
		t.Errorf("toolName = %q, want \"\" (no archive at OPEN)", toolName)
	}
	if msg == "" {
		t.Error("msg = \"\", want non-empty retry hint for ErrInvalidInput")
	}
}

func TestCheckToolExecErrorRecovery_UnknownKind_FallsBackToPrefix(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)

	// Empty ErrKind + executor prefix → legacy prefix heuristic applies.
	exec.messages = append(exec.messages, toolRoleMessage("web_search", "Tool execution failed: connection refused"))
	ts.lastToolResult = nil

	toolName, msg := checkToolExecErrorRecovery(ts, exec)
	if toolName != "" {
		t.Errorf("toolName = %q, want \"\"", toolName)
	}
	if msg == "" {
		t.Error("msg = \"\", want non-empty retry hint via prefix fallback")
	}
}

func TestCheckToolExecErrorRecovery_ErrTimeout_IsTransient(t *testing.T) {
	provider := &recordingProvider{}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRetryChainTestTurnState(t, al, pipeline)

	exec.messages = append(exec.messages, toolRoleMessage("web_search", "Tool execution failed: timeout"))
	ts.lastToolResult = &tools.ToolResult{ErrKind: toolshared.ErrTimeout}

	toolName, msg := checkToolExecErrorRecovery(ts, exec)
	if toolName != "" {
		t.Errorf("toolName = %q, want \"\"", toolName)
	}
	if msg == "" {
		t.Error("msg = \"\", want non-empty retry hint for ErrTimeout")
	}
}

// toolRoleMessage builds a tool-role message with ToolCallID used as the
// tool name by checkToolExecErrorRecovery.
func toolRoleMessage(toolCallID, content string) providers.Message {
	return providers.Message{Role: "tool", ToolCallID: toolCallID, Content: content}
}

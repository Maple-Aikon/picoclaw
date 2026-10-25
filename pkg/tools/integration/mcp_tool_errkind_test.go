// Phase 12.62 (2026-10-25) — MCP tool errors must carry a typed ErrKind.
//
// Before this change two of the three MCP failure paths left ErrKind
// EMPTY. An empty ErrKind sends the agent recovery layer down the legacy
// "Tool execution failed:" prefix heuristic — a prefix that NO MCP error
// ever emits (they say "MCP tool returned error:" / "MCP tool execution
// failed:") — so recovery never fired and the tool_knowledge lesson for
// the failing MCP tool was never surfaced to the LLM.

package integrationtools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPTool_Execute_ServerError_HasErrKind pins that a non-transient
// tool-level error is classified as invalid-input (a permanent failure of
// that call) rather than left untyped.
func TestMCPTool_Execute_ServerError_HasErrKind(t *testing.T) {
	manager := &MockMCPManager{
		callToolFunc: func(ctx context.Context, serverName, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Invalid API key"}},
				IsError: true,
			}, nil
		},
	}
	mcpTool := NewMCPTool(manager, "test_server", &mcp.Tool{Name: "test_tool"})

	result := mcpTool.Execute(context.Background(), map[string]any{})
	if result == nil {
		t.Fatal("result should not be nil")
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}
	if result.ErrKind == "" {
		t.Fatalf("MCP tool-level error has EMPTY ErrKind; the agent recovery layer " +
			"cannot classify it and the tool_knowledge lesson is never surfaced")
	}
	if !strings.Contains(result.ForLLM, "MCP tool returned error") {
		t.Errorf("error message should mention the server error, got: %s", result.ForLLM)
	}
}

// TestMCPTool_Execute_TransportError_HasErrKind pins that a transport
// failure which is neither a session-loss nor a known transient marker
// still gets a typed ErrKind.
func TestMCPTool_Execute_TransportError_HasErrKind(t *testing.T) {
	manager := &MockMCPManager{
		callToolFunc: func(ctx context.Context, serverName, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
			return nil, errors.New("some opaque transport failure")
		},
	}
	mcpTool := NewMCPTool(manager, "test_server", &mcp.Tool{Name: "test_tool"})

	result := mcpTool.Execute(context.Background(), map[string]any{})
	if result == nil {
		t.Fatal("result should not be nil")
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}
	if result.ErrKind == "" {
		t.Fatalf("MCP transport error has EMPTY ErrKind; recovery cannot fire")
	}
}

// TestMCPTool_Execute_NilResult_HasErrKind pins the nil-result path.
func TestMCPTool_Execute_NilResult_HasErrKind(t *testing.T) {
	manager := &MockMCPManager{
		callToolFunc: func(ctx context.Context, serverName, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
			return nil, nil
		},
	}
	mcpTool := NewMCPTool(manager, "test_server", &mcp.Tool{Name: "test_tool"})

	result := mcpTool.Execute(context.Background(), map[string]any{})
	if result == nil {
		t.Fatal("result should not be nil")
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}
	if result.ErrKind == "" {
		t.Fatalf("MCP nil-result error has EMPTY ErrKind; recovery cannot fire")
	}
}

// TestMCPTool_Execute_TransientToolError_StaysTransient verifies the
// pre-existing transient classification still wins over the new
// invalid-input default (SearXNG 403 / timeout style messages).
func TestMCPTool_Execute_TransientToolError_StaysTransient(t *testing.T) {
	manager := &MockMCPManager{
		callToolFunc: func(ctx context.Context, serverName, toolName string, arguments map[string]any) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "upstream returned http 503"}},
				IsError: true,
			}, nil
		},
	}
	mcpTool := NewMCPTool(manager, "test_server", &mcp.Tool{Name: "test_tool"})

	result := mcpTool.Execute(context.Background(), map[string]any{})
	if result == nil {
		t.Fatal("result should not be nil")
	}
	if result.ErrKind == "" {
		t.Fatalf("transient MCP error must carry an ErrKind")
	}
	if result.ErrKind == "invalid_input" {
		t.Fatalf("transient MCP error must NOT be classified as invalid_input, got %q", result.ErrKind)
	}
}

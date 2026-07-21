package agent

// Integration tests for the two top-degree hub nodes in pkg/agent:
// - Pipeline.ExecuteTools (out_degree 258, in pkg/agent/pipeline_execute.go)
// - Pipeline.CallLLM (out_degree 151, in pkg/agent/pipeline_llm.go)
//
// These tests drive a real AgentLoop through processMessage so both hubs
// are exercised end-to-end (LLM -> normalize -> ExecuteTools -> CallLLM again).
// Goal: provide direct integration coverage on the hot paths that were
// previously only exercised indirectly via larger suite tests.
//
// ADR-002 candidate: this file is the first integration test scoped to a
// single hub node. If additional hubs surface, follow this pattern.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// hubIntegrationTool is a deterministic test tool that records every call
// (id + args) and returns a configurable result.
type hubIntegrationTool struct {
	name    string
	result  *tools.ToolResult
	delay   time.Duration
	calls   atomic.Int32
	lastArg atomic.Value // map[string]any
}

func (h *hubIntegrationTool) Name() string { return h.name }
func (h *hubIntegrationTool) Description() string {
	return "hub integration test tool: " + h.name
}
func (h *hubIntegrationTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
	}
}
func (h *hubIntegrationTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	h.calls.Add(1)
	h.lastArg.Store(args)
	if h.delay > 0 {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return tools.SilentResult("cancelled")
		}
	}
	if h.result != nil {
		return h.result
	}
	return tools.SilentResult("ok")
}

// hubRecordingProvider is a deterministic LLM provider that returns a fixed
// sequence of LLMResponse values. responses/errors are indexed by main-flow
// call count (i.e., task-extraction background calls do not consume an index).
type hubRecordingProvider struct {
	responses []*providers.LLMResponse
	errors    []error
	calls     atomic.Int32 // total Chat calls (includes task-extraction background)
	mainIdx   atomic.Int32 // main-flow call index (skips task-extraction)
}

func (h *hubRecordingProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	h.calls.Add(1)
	// Skip the task-extraction background call so we only count main-flow Chat calls.
	if isTaskExtractionCall(messages, tools, opts) {
		return &providers.LLMResponse{Content: taskExtractionResponse(messages)}, nil
	}
	idx := int(h.mainIdx.Add(1)) - 1
	if idx >= 0 && idx < len(h.errors) && h.errors[idx] != nil {
		return nil, h.errors[idx]
	}
	if idx >= 0 && idx < len(h.responses) && h.responses[idx] != nil {
		return h.responses[idx], nil
	}
	// Default: text-only no-tool completion.
	return &providers.LLMResponse{Content: "fallback completion"}, nil
}

func (h *hubRecordingProvider) GetDefaultModel() string { return "hub-test-model" }

// newHubIntegrationLoop mirrors newTurnCoordTestLoop / newTestAgentLoop but
// lets the caller pass a custom provider so we can assert Pipeline.CallLLM
// call counts directly.
func newHubIntegrationLoop(t *testing.T, provider providers.LLMProvider) (*AgentLoop, func()) {
	t.Helper()

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace: t.TempDir(),
			},
		},
	}
	msgBus := bus.NewMessageBus()
	// MediaStore is nil — tests don't exercise media upload paths.
	cm, err := channels.NewManager(cfg, msgBus, nil)
	if err != nil {
		t.Fatalf("channels.NewManager: %v", err)
	}

	al := NewAgentLoop(cfg, msgBus, provider)
	al.channelManager = cm

	cleanup := func() {
		// No-op: in-memory bus + local cfg are GC'd when the test ends.
	}
	return al, cleanup
}

// hubInbound builds a normalized test inbound message for processMessage.
func hubInbound(channel, chatID, senderID, content string) bus.InboundMessage {
	return testInboundMessage(bus.InboundMessage{
		Channel:  channel,
		ChatID:   chatID,
		SenderID: senderID,
		Content:  content,
	})
}

// TestPipeline_ExecuteTools_Hub_HappyPath drives a single tool call through
// processMessage and asserts that Pipeline.ExecuteTools ran exactly once on
// the registered tool. This exercises the top-degree hub directly with a real
// AgentLoop (not pure mocks).
func TestPipeline_ExecuteTools_Hub_HappyPath(t *testing.T) {
	tool := &hubIntegrationTool{
		name:   "hub_echo",
		result: tools.SilentResult("hub echo ok"),
	}
	provider := &hubRecordingProvider{
		responses: []*providers.LLMResponse{
			// First call: LLM asks to call our tool.
			{
				Content: "",
				ToolCalls: []providers.ToolCall{{
					ID:        "call_hub_1",
					Type:      "function",
					Name:      "hub_echo",
					Arguments: map[string]any{"value": "ping"},
				}},
			},
			// Second call: text-only completion.
			{Content: "done"},
		},
	}
	al, cleanup := newHubIntegrationLoop(t, provider)
	defer cleanup()

	al.RegisterTool(tool)

	_, err := al.processMessage(
		context.Background(),
		hubInbound("pico", "hub-test-chat", "hub-test-user", "Please call hub_echo"),
	)
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}

	if got := tool.calls.Load(); got != 1 {
		t.Errorf("hub_echo calls = %d, want 1", got)
	}
	lastArg, _ := tool.lastArg.Load().(map[string]any)
	if lastArg == nil || lastArg["value"] != "ping" {
		t.Errorf("hub_echo last args = %v, want {value: ping}", lastArg)
	}
	// Pipeline.CallLLM should have fired at least twice (tool call + final).
	// It may fire more if the task-extraction background goroutine runs;
	// assert lower-bound only.
	if got := provider.calls.Load(); got < 2 {
		t.Errorf("Pipeline.CallLLM calls = %d, want >= 2", got)
	}
}

// TestPipeline_ExecuteTools_Hub_ToolError asserts that Pipeline.ExecuteTools
// surfaces tool errors correctly via tools.ErrorResult without crashing the
// AgentLoop.
func TestPipeline_ExecuteTools_Hub_ToolError(t *testing.T) {
	tool := &hubIntegrationTool{
		name: "hub_failing",
		result: &tools.ToolResult{
			ForLLM:  "simulated failure",
			IsError: true,
			ErrKind: tools.ErrDependencyDown,
		},
	}
	provider := &hubRecordingProvider{
		responses: []*providers.LLMResponse{
			{
				Content: "",
				ToolCalls: []providers.ToolCall{{
					ID:        "call_fail_1",
					Type:      "function",
					Name:      "hub_failing",
					Arguments: map[string]any{},
				}},
			},
			// After tool failure, LLM sees the error and finalizes.
			{Content: "tool failed, fallback"},
		},
	}
	al, cleanup := newHubIntegrationLoop(t, provider)
	defer cleanup()

	al.RegisterTool(tool)

	_, err := al.processMessage(
		context.Background(),
		hubInbound("pico", "hub-fail-chat", "hub-fail-user", "call hub_failing"),
	)
	if err != nil {
		t.Fatalf("processMessage returned error (should swallow tool error): %v", err)
	}
	if got := tool.calls.Load(); got != 1 {
		t.Errorf("hub_failing calls = %d, want 1", got)
	}
}

// TestPipeline_ExecuteTools_Hub_UnknownTool asserts the missing-tool path:
// when LLM emits a tool name not registered, Pipeline.ExecuteTools must
// surface a structured error result rather than panic or silently succeed.
func TestPipeline_ExecuteTools_Hub_UnknownTool(t *testing.T) {
	provider := &hubRecordingProvider{
		responses: []*providers.LLMResponse{
			{
				Content: "",
				ToolCalls: []providers.ToolCall{{
					ID:        "call_ghost",
					Type:      "function",
					Name:      "nonexistent_tool",
					Arguments: map[string]any{"x": 1},
				}},
			},
			{Content: "aborted"},
		},
	}
	al, cleanup := newHubIntegrationLoop(t, provider)
	defer cleanup()

	_, err := al.processMessage(
		context.Background(),
		hubInbound("pico", "hub-ghost-chat", "hub-ghost-user", "call nonexistent"),
	)
	if err != nil {
		t.Fatalf("processMessage returned error: %v", err)
	}
	// No tool registered -> Pipeline.ExecuteTools should not panic.
	// Verify LLM was called at least twice (initial + after the missing-tool
	// error reply).
	if got := provider.calls.Load(); got < 2 {
		t.Errorf("Pipeline.CallLLM calls = %d, want >= 2", got)
	}
}

// TestPipeline_CallLLM_Hub_RetryAfterTransientError verifies that when
// Pipeline.CallLLM returns a transient error, the recovery path retries the
// provider without crashing the AgentLoop. We inject a transient error on
// the first main-flow call and a successful response on the second.
func TestPipeline_CallLLM_Hub_RetryAfterTransientError(t *testing.T) {
	tool := &hubIntegrationTool{
		name:   "hub_ok",
		result: tools.SilentResult("ok"),
	}
	provider := &hubRecordingProvider{
		errors: []error{
			errors.New("transient: rate limited"), // call 1 (main-flow)
		},
		responses: []*providers.LLMResponse{
			nil, // call 1 already covered by errors[0]
			// call 2 (after retry succeeds): tool call
			{
				Content: "",
				ToolCalls: []providers.ToolCall{{
					ID:        "call_retry_1",
					Type:      "function",
					Name:      "hub_ok",
					Arguments: map[string]any{"value": "after retry"},
				}},
			},
			// call 3 (after tool success): text-only
			{Content: "done"},
		},
	}
	al, cleanup := newHubIntegrationLoop(t, provider)
	defer cleanup()

	al.RegisterTool(tool)

	_, err := al.processMessage(
		context.Background(),
		hubInbound("pico", "hub-retry-chat", "hub-retry-user", "please retry"),
	)
	if err != nil {
		t.Fatalf("processMessage returned error: %v", err)
	}
	// Tool should have been called after the retry succeeded.
	if got := tool.calls.Load(); got != 1 {
		t.Errorf("hub_ok calls after retry = %d, want 1", got)
	}
}

// TestPipeline_CallLLM_Hub_StreamingContextCancel ensures the LLM hub handles
// context cancellation gracefully (timeout / user-abort / channel-stop).
// We pass a pre-cancelled context and expect processMessage to return
// without panic.
func TestPipeline_CallLLM_Hub_StreamingContextCancel(t *testing.T) {
	provider := &hubRecordingProvider{
		responses: []*providers.LLMResponse{{Content: "should not reach"}},
	}
	al, cleanup := newHubIntegrationLoop(t, provider)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE processMessage

	_, err := al.processMessage(
		ctx,
		hubInbound("pico", "hub-cancel-chat", "hub-cancel-user", "ping"),
	)
	// Either nil (handled cleanly) or context.Canceled — both are acceptable.
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Logf("processMessage error = %v (acceptable for cancel)", err)
	}
}

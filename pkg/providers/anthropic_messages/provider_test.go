// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package anthropicmessages

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestBuildRequestBody(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		tools    []ToolDefinition
		model    string
		options  map[string]any
		want     map[string]any
		wantErr  bool
	}{
		{
			name: "basic user message",
			messages: []Message{
				{Role: "user", Content: "Hello, world!"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			want: map[string]any{
				"model":      "test-model",
				"max_tokens": int64(8192),
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "Hello, world!",
					},
				},
			},
		},
		{
			name: "user and assistant messages",
			messages: []Message{
				{Role: "user", Content: "What is 2+2?"},
				{Role: "assistant", Content: "4"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			want: map[string]any{
				"model":      "test-model",
				"max_tokens": int64(8192),
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "What is 2+2?",
					},
					map[string]any{
						"role": "assistant",
						"content": []any{
							map[string]any{
								"type": "text",
								"text": "4",
							},
						},
					},
				},
			},
		},
		{
			name: "with system message",
			messages: []Message{
				{Role: "system", Content: "You are a helpful assistant."},
				{Role: "user", Content: "Hello"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			want: map[string]any{
				"model":      "test-model",
				"max_tokens": int64(8192),
				"system":     "You are a helpful assistant.",
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "Hello",
					},
				},
			},
		},
		{
			name: "with custom max_tokens and temperature",
			messages: []Message{
				{Role: "user", Content: "Test"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens":  2048,
				"temperature": 0.5,
			},
			want: map[string]any{
				"model":       "test-model",
				"max_tokens":  int64(2048),
				"temperature": 0.5,
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "Test",
					},
				},
			},
		},
		{
			name: "missing max_tokens returns error",
			messages: []Message{
				{Role: "user", Content: "Test"},
			},
			model:   "test-model",
			options: map[string]any{},
			want:    nil,
			wantErr: true,
		},
		{
			name: "with tools",
			messages: []Message{
				{Role: "user", Content: "What's the weather?"},
			},
			tools: []ToolDefinition{
				{
					Function: ToolFunctionDefinition{
						Name:        "get_weather",
						Description: "Get current weather",
						Parameters: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"location": map[string]any{
									"type":        "string",
									"description": "City name",
								},
							},
						},
					},
				},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			want: map[string]any{
				"model":      "test-model",
				"max_tokens": int64(8192),
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": "What's the weather?",
					},
				},
				"tools": []any{
					map[string]any{
						"name":        "get_weather",
						"description": "Get current weather",
						"input_schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"location": map[string]any{
									"type":        "string",
									"description": "City name",
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildRequestBody(tt.messages, tt.tools, tt.model, tt.options)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildRequestBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				wantJSON, _ := json.MarshalIndent(tt.want, "", "  ")
				t.Errorf("buildRequestBody() mismatch:\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
			}
		})
	}
}

func TestParseResponseBody(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		want    *LLMResponse
		wantErr bool
	}{
		{
			name: "basic text response",
			body: []byte(`{
				"id": "msg-123",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Hello, how can I help?"}
				],
				"stop_reason": "end_turn",
				"model": "test-model",
				"usage": {
					"input_tokens": 10,
					"output_tokens": 5
				}
			}`),
			want: &LLMResponse{
				Content:      "Hello, how can I help?",
				ToolCalls:    []ToolCall{},
				FinishReason: "stop",
				Usage: &UsageInfo{
					PromptTokens:     10,
					CompletionTokens: 5,
					TotalTokens:      15,
				},
				Reasoning:        "",
				ReasoningDetails: nil,
			},
			wantErr: false,
		},
		{
			name: "response with tool use",
			body: []byte(`{
				"id": "msg-456",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "text", "text": "I'll check the weather for you."},
					{
						"type": "tool_use",
						"id": "toolu-123",
						"name": "get_weather",
						"input": {"location": "Tokyo"}
					}
				],
				"stop_reason": "tool_use",
				"model": "test-model",
				"usage": {
					"input_tokens": 20,
					"output_tokens": 15
				}
			}`),
			want: &LLMResponse{
				Content: "I'll check the weather for you.",
				ToolCalls: []ToolCall{
					{
						ID:   "toolu-123",
						Name: "get_weather",
						Arguments: map[string]any{
							"location": "Tokyo",
						},
						Function: &FunctionCall{
							Name:      "get_weather",
							Arguments: `{"location":"Tokyo"}`,
						},
					},
				},
				FinishReason: "tool_calls",
				Usage: &UsageInfo{
					PromptTokens:     20,
					CompletionTokens: 15,
					TotalTokens:      35,
				},
				Reasoning:        "",
				ReasoningDetails: nil,
			},
			wantErr: false,
		},
		{
			name:    "invalid JSON",
			body:    []byte(`invalid json`),
			want:    nil,
			wantErr: true,
		},
		{
			name: "max_tokens stop reason",
			body: []byte(`{
				"id": "msg-789",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Partial response"}
				],
				"stop_reason": "max_tokens",
				"model": "test-model",
				"usage": {
					"input_tokens": 100,
					"output_tokens": 4096
				}
			}`),
			want: &LLMResponse{
				Content:      "Partial response",
				ToolCalls:    []ToolCall{},
				FinishReason: "length",
				Usage: &UsageInfo{
					PromptTokens:     100,
					CompletionTokens: 4096,
					TotalTokens:      4196,
				},
				Reasoning:        "",
				ReasoningDetails: nil,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResponseBody(tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseResponseBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			// Compare individual fields
			if got.Content != tt.want.Content {
				t.Errorf("Content = %q, want %q", got.Content, tt.want.Content)
			}
			if got.FinishReason != tt.want.FinishReason {
				t.Errorf("FinishReason = %q, want %q", got.FinishReason, tt.want.FinishReason)
			}
			if got.Usage == nil && tt.want.Usage != nil {
				t.Errorf("Usage = nil, want non-nil")
			} else if got.Usage != nil && tt.want.Usage == nil {
				t.Errorf("Usage = non-nil, want nil")
			} else if got.Usage != nil && tt.want.Usage != nil {
				if got.Usage.PromptTokens != tt.want.Usage.PromptTokens {
					t.Errorf("Usage.PromptTokens = %d, want %d", got.Usage.PromptTokens, tt.want.Usage.PromptTokens)
				}
				if got.Usage.CompletionTokens != tt.want.Usage.CompletionTokens {
					t.Errorf("Usage.CompletionTokens = %d, want %d",
						got.Usage.CompletionTokens, tt.want.Usage.CompletionTokens)
				}
				if got.Usage.TotalTokens != tt.want.Usage.TotalTokens {
					t.Errorf("Usage.TotalTokens = %d, want %d", got.Usage.TotalTokens, tt.want.Usage.TotalTokens)
				}
			}
			if len(got.ToolCalls) != len(tt.want.ToolCalls) {
				t.Errorf("ToolCalls length = %d, want %d", len(got.ToolCalls), len(tt.want.ToolCalls))
			} else {
				for i := range got.ToolCalls {
					if got.ToolCalls[i].ID != tt.want.ToolCalls[i].ID {
						t.Errorf("ToolCalls[%d].ID = %q, want %q",
							i, got.ToolCalls[i].ID, tt.want.ToolCalls[i].ID)
					}
					if got.ToolCalls[i].Name != tt.want.ToolCalls[i].Name {
						t.Errorf("ToolCalls[%d].Name = %q, want %q",
							i, got.ToolCalls[i].Name, tt.want.ToolCalls[i].Name)
					}
				}
			}
		})
	}
}

func TestNewProvider(t *testing.T) {
	provider := NewProvider("test-key", "https://api.example.com", "")
	if provider == nil {
		t.Fatal("NewProvider() returned nil")
	}
	if provider.apiKey != "test-key" {
		t.Errorf("provider.apiKey = %q, want %q", provider.apiKey, "test-key")
	}
	if provider.apiBase != "https://api.example.com/v1" {
		t.Errorf("provider.apiBase = %q, want %q", provider.apiBase, "https://api.example.com/v1")
	}
}

func TestGetDefaultModel(t *testing.T) {
	provider := NewProvider("test-key", "", "")
	got := provider.GetDefaultModel()
	expected := "claude-sonnet-4.6"
	if got != expected {
		t.Errorf("GetDefaultModel() = %q, want %q", got, expected)
	}

	// New constructor with provider-specific default model
	p2 := NewProviderWithTimeoutAndDefaultModel("test-key", "", "", 0, "MiniMax-M3")
	if got := p2.GetDefaultModel(); got != "MiniMax-M3" {
		t.Errorf("NewProviderWithTimeoutAndDefaultModel GetDefaultModel() = %q, want %q", got, "MiniMax-M3")
	}

	// Empty default model falls back to package default (backward compat)
	p3 := NewProviderWithTimeoutAndDefaultModel("test-key", "", "", 0, "")
	if got := p3.GetDefaultModel(); got != "claude-sonnet-4.6" {
		t.Errorf("empty defaultModel GetDefaultModel() = %q, want %q", got, "claude-sonnet-4.6")
	}
}

// TestBuildRequestBodyEdgeCases tests edge cases for buildRequestBody.
func TestBuildRequestBodyEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
		tools    []ToolDefinition
		model    string
		options  map[string]any
		wantErr  bool
	}{
		{
			name:     "empty message list",
			messages: []Message{},
			model:    "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
		{
			name: "very long system message",
			messages: []Message{
				{Role: "system", Content: strings.Repeat("This is a very long system prompt. ", 1000)},
				{Role: "user", Content: "Hello"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
		{
			name: "multiple consecutive system messages",
			messages: []Message{
				{Role: "system", Content: "First system message"},
				{Role: "system", Content: "Second system message"},
				{Role: "system", Content: "Third system message"},
				{Role: "user", Content: "Hello"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
		{
			name: "tool result without tool call",
			messages: []Message{
				{Role: "user", Content: "Use a tool"},
				{Role: "assistant", Content: "", ToolCalls: []ToolCall{
					{ID: "tool-1", Name: "test_tool", Arguments: map[string]any{"arg": "value"}},
				}},
				{Role: "user", ToolCallID: "tool-1", Content: "Tool result"},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
		{
			name: "skip tool calls with empty names",
			messages: []Message{
				{Role: "assistant", Content: "Calling tool", ToolCalls: []ToolCall{
					{ID: "tool-empty", Name: "", Arguments: map[string]any{"ignored": true}},
					{ID: "tool-valid", Name: "test_tool", Arguments: map[string]any{"arg": "value"}},
				}},
			},
			model: "test-model",
			options: map[string]any{
				"max_tokens": 8192,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildRequestBody(tt.messages, tt.tools, tt.model, tt.options)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildRequestBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			// Verify basic structure
			if got == nil {
				t.Error("buildRequestBody() returned nil")
				return
			}
			if got["model"] != tt.model {
				t.Errorf("model = %v, want %v", got["model"], tt.model)
			}

			if tt.name == "skip tool calls with empty names" {
				messages, ok := got["messages"].([]any)
				if !ok || len(messages) != 1 {
					t.Fatalf("messages = %#v, want single assistant message", got["messages"])
				}

				assistantMsg, ok := messages[0].(map[string]any)
				if !ok {
					t.Fatalf("assistant message = %#v, want map", messages[0])
				}

				content, ok := assistantMsg["content"].([]any)
				if !ok {
					t.Fatalf("assistant content = %#v, want []any", assistantMsg["content"])
				}
				if len(content) != 2 {
					t.Fatalf("assistant content length = %d, want 2", len(content))
				}

				toolUse, ok := content[1].(map[string]any)
				if !ok {
					t.Fatalf("tool_use block = %#v, want map", content[1])
				}
				if gotName := toolUse["name"]; gotName != "test_tool" {
					t.Fatalf("tool_use name = %v, want %q", gotName, "test_tool")
				}
				if gotID := toolUse["id"]; gotID != "tool-valid" {
					t.Fatalf("tool_use id = %v, want %q", gotID, "tool-valid")
				}
			}
		})
	}
}

func TestBuildRequestBody_ConsecutiveToolResultsMerged(t *testing.T) {
	// Consecutive tool results (role "tool") should be merged into a single "user" message
	messages := []Message{
		{Role: "user", Content: "Use tools"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "t1", Name: "tool_a", Arguments: map[string]any{"x": 1}},
			{ID: "t2", Name: "tool_b", Arguments: map[string]any{"y": 2}},
		}},
		{Role: "tool", ToolCallID: "t1", Content: "result1"},
		{Role: "tool", ToolCallID: "t2", Content: "result2"},
	}

	got, err := buildRequestBody(messages, nil, "test-model", map[string]any{"max_tokens": 8192})
	if err != nil {
		t.Fatalf("buildRequestBody() error: %v", err)
	}

	apiMessages, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("messages is not []any")
	}

	// Expect: user, assistant, user (merged tool results)
	if len(apiMessages) != 3 {
		for i, m := range apiMessages {
			t.Logf("message[%d]: %+v", i, m)
		}
		t.Fatalf("expected 3 API messages, got %d", len(apiMessages))
	}

	// The third message should be a user message with 2 tool_result blocks
	toolResultMsg, ok := apiMessages[2].(map[string]any)
	if !ok {
		t.Fatalf("tool result message is not map[string]any")
	}
	if toolResultMsg["role"] != "user" {
		t.Errorf("expected role 'user', got %v", toolResultMsg["role"])
	}
	content, ok := toolResultMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content is not []map[string]any: %T", toolResultMsg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %d", len(content))
	}
	if content[0]["tool_use_id"] != "t1" {
		t.Errorf("first tool_result tool_use_id = %v, want t1", content[0]["tool_use_id"])
	}
	if content[1]["tool_use_id"] != "t2" {
		t.Errorf("second tool_result tool_use_id = %v, want t2", content[1]["tool_use_id"])
	}
}

func TestBuildRequestBody_UserToolResultsMerged(t *testing.T) {
	// Consecutive tool results using role "user" with ToolCallID should also be merged
	messages := []Message{
		{Role: "user", Content: "Use tools"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "t1", Name: "tool_a", Arguments: map[string]any{"x": 1}},
			{ID: "t2", Name: "tool_b", Arguments: map[string]any{"y": 2}},
		}},
		{Role: "user", ToolCallID: "t1", Content: "result1"},
		{Role: "user", ToolCallID: "t2", Content: "result2"},
	}

	got, err := buildRequestBody(messages, nil, "test-model", map[string]any{"max_tokens": 8192})
	if err != nil {
		t.Fatalf("buildRequestBody() error: %v", err)
	}

	apiMessages, ok := got["messages"].([]any)
	if !ok {
		t.Fatalf("messages is not []any")
	}

	// Expect: user, assistant, user (merged tool results)
	if len(apiMessages) != 3 {
		t.Fatalf("expected 3 API messages, got %d", len(apiMessages))
	}

	toolResultMsg := apiMessages[2].(map[string]any)
	content, ok := toolResultMsg["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content is not []map[string]any: %T", toolResultMsg["content"])
	}
	if len(content) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %d", len(content))
	}
}

// TestParseResponseBodyEdgeCases tests edge cases for parseResponseBody.
func TestParseResponseBodyEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		wantErr bool
		check   func(*testing.T, *LLMResponse)
	}{
		{
			name: "empty content blocks",
			body: []byte(`{
				"id": "msg-empty",
				"type": "message",
				"role": "assistant",
				"content": [],
				"stop_reason": "end_turn",
				"model": "test-model",
				"usage": {"input_tokens": 5, "output_tokens": 0}
			}`),
			wantErr: false,
			check: func(t *testing.T, resp *LLMResponse) {
				if resp.Content != "" {
					t.Errorf("Content = %q, want empty string", resp.Content)
				}
				if len(resp.ToolCalls) != 0 {
					t.Errorf("ToolCalls length = %d, want 0", len(resp.ToolCalls))
				}
			},
		},
		{
			name: "multiple tool use blocks",
			body: []byte(`{
				"id": "msg-multi",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "tool_use", "id": "tool-1", "name": "func1", "input": {"arg": "val1"}},
					{"type": "tool_use", "id": "tool-2", "name": "func2", "input": {"arg": "val2"}}
				],
				"stop_reason": "tool_use",
				"model": "test-model",
				"usage": {"input_tokens": 10, "output_tokens": 20}
			}`),
			wantErr: false,
			check: func(t *testing.T, resp *LLMResponse) {
				if len(resp.ToolCalls) != 2 {
					t.Errorf("ToolCalls length = %d, want 2", len(resp.ToolCalls))
				}
			},
		},
		{
			name:    "malformed JSON response",
			body:    []byte(`{invalid json`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResponseBody(tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseResponseBody() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.check != nil && err == nil {
				tt.check(t, got)
			}
		})
	}
}

// TestProviderChatErrors tests error handling in Chat.
// Note: apiBase check removed as it's dead code - normalizeBaseURL() always provides a default.
func TestProviderChatErrors(t *testing.T) {
	tests := []struct {
		name       string
		apiKey     string
		messages   []Message
		wantErrMsg string
	}{
		{
			name:       "missing API key",
			apiKey:     "",
			messages:   []Message{{Role: "user", Content: "Test"}},
			wantErrMsg: "API key not configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create provider using constructor to ensure proper initialization
			provider := NewProvider(tt.apiKey, "https://api.example.com", "")

			_, err := provider.Chat(context.Background(), tt.messages, nil, "test-model", nil)
			if err == nil {
				t.Fatal("Chat() expected error, got nil")
			}
			if err.Error() != tt.wantErrMsg {
				t.Errorf("Chat() error = %q, want %q", err.Error(), tt.wantErrMsg)
			}
		})
	}
}

// TestChatExtraBody verifies the wire contract for SetExtraBody:
// - extra fields are merged into the request body
// - model/max_tokens/messages/system/tools are never overridden
// - SetExtraBody(nil) is a no-op (no panic)
func TestChatExtraBody(t *testing.T) {
	tests := []struct {
		name       string
		extraBody   map[string]any
		wantFields map[string]any
	}{
		{
			name:     "thinking adaptive injected",
			extraBody: map[string]any{"thinking": map[string]any{"type": "adaptive"}},
			wantFields: map[string]any{
				"thinking": map[string]any{"type": "adaptive"},
			},
		},
		{
			name:     "nil extra body is no-op",
			extraBody: nil,
			wantFields: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAPIKey, gotVersion string
			var gotBody map[string]any

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAPIKey = r.Header.Get("X-API-Key")
				gotVersion = r.Header.Get("Anthropic-Version")
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","model":"MiniMax-M3","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			defer srv.Close()

			p := NewProviderWithTimeoutAndDefaultModel("test-key", srv.URL, "test-agent", 0, "MiniMax-M3")
			p.SetExtraBody(tt.extraBody)

			resp, err := p.Chat(context.Background(),
				[]Message{{Role: "user", Content: "hi"}},
				nil, "MiniMax-M3", map[string]any{"max_tokens": 9000})
			if err != nil {
				t.Fatalf("Chat() error = %v", err)
			}
			if resp == nil || resp.Content != "ok" {
				t.Fatalf("Chat() resp = %+v, want content ok", resp)
			}

			// Wire assertions
			if gotPath != "/v1/messages" {
				t.Errorf("request path = %q, want /v1/messages", gotPath)
			}
			if gotAPIKey != "test-key" {
				t.Errorf("X-API-Key = %q, want test-key", gotAPIKey)
			}
			if gotVersion != defaultAPIVersion {
				t.Errorf("Anthropic-Version = %q, want %q", gotVersion, defaultAPIVersion)
			}

			// Core fields must never be overridden by extra body
			if gotBody["model"] != "MiniMax-M3" {
				t.Errorf("body model = %v, want MiniMax-M3 (must not be overridden)", gotBody["model"])
			}
			if gotBody["max_tokens"] != float64(9000) {
				t.Errorf("body max_tokens = %v, want 9000 (must not be overridden)", gotBody["max_tokens"])
			}
			if _, ok := gotBody["messages"]; !ok {
				t.Error("body messages missing (must not be dropped)")
			}

			for wantKey, wantVal := range tt.wantFields {
				gotVal, ok := gotBody[wantKey]
				if !ok {
					t.Errorf("body missing extra key %q", wantKey)
					continue
				}
				if !reflect.DeepEqual(gotVal, wantVal) {
					t.Errorf("body[%q] = %#v, want %#v", wantKey, gotVal, wantVal)
				}
			}
		})
	}
}

// TestChatExtraBodyDoesNotOverrideCoreFields verifies that an extra body key
// colliding with a core field (model) is ignored — the built value wins.
func TestChatExtraBodyDoesNotOverrideCoreFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","model":"MiniMax-M3","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	p := NewProviderWithTimeoutAndDefaultModel("test-key", srv.URL, "test-agent", 0, "MiniMax-M3")
	p.SetExtraBody(map[string]any{"model": "evil-model", "thinking": map[string]any{"type": "adaptive"}})

	if _, err := p.Chat(context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil, "MiniMax-M3", map[string]any{"max_tokens": 9000}); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	if gotBody["model"] != "MiniMax-M3" {
		t.Errorf("body model = %v, want MiniMax-M3 (extra body must not override)", gotBody["model"])
	}
	if _, ok := gotBody["thinking"]; !ok {
		t.Error("body thinking missing (non-colliding extra key must still be injected)")
	}
}

// TestParseResponseBodyThinking verifies thinking block parsing (T4/T5):
// thinking+text → ReasoningContent=thinking, Content=text;
// thinking-only → Content="", ReasoningContent full, FinishReason preserved.
func TestParseResponseBodyThinking(t *testing.T) {
	tests := []struct {
		name             string
		body             string
		wantContent      string
		wantReasoning    string
		wantFinishReason string
		wantToolCalls    int
	}{
		{
			name: "thinking + text blocks",
			body: `{"id":"msg-t","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"Let me reason about this step by step.","signature":"sig-abc"},{"type":"text","text":"The answer is 42."}],"stop_reason":"end_turn","model":"MiniMax-M3","usage":{"input_tokens":10,"output_tokens":20}}`,
			wantContent:      "The answer is 42.",
			wantReasoning:    "Let me reason about this step by step.",
			wantFinishReason: "stop",
			wantToolCalls:    0,
		},
		{
			name: "thinking only (reasoning-only response)",
			body: `{"id":"msg-r","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"I should call set_goal now.","signature":"sig-xyz"}],"stop_reason":"end_turn","model":"MiniMax-M3","usage":{"input_tokens":10,"output_tokens":20}}`,
			wantContent:      "",
			wantReasoning:    "I should call set_goal now.",
			wantFinishReason: "stop",
			wantToolCalls:    0,
		},
		{
			name: "thinking + tool_use (no text)",
			body: `{"id":"msg-tt","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"Tool time.","signature":"sig-1"},{"type":"tool_use","id":"tu-1","name":"set_goal","input":{"objective":"x"}}],"stop_reason":"tool_use","model":"MiniMax-M3","usage":{"input_tokens":10,"output_tokens":20}}`,
			wantContent:      "",
			wantReasoning:    "Tool time.",
			wantFinishReason: "tool_calls",
			wantToolCalls:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := parseResponseBody([]byte(tt.body))
			if err != nil {
				t.Fatalf("parseResponseBody() error = %v", err)
			}
			if resp.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", resp.Content, tt.wantContent)
			}
			if resp.ReasoningContent != tt.wantReasoning {
				t.Errorf("ReasoningContent = %q, want %q", resp.ReasoningContent, tt.wantReasoning)
			}
			if resp.FinishReason != tt.wantFinishReason {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, tt.wantFinishReason)
			}
			if len(resp.ToolCalls) != tt.wantToolCalls {
				t.Errorf("ToolCalls length = %d, want %d", len(resp.ToolCalls), tt.wantToolCalls)
			}
		})
	}
}

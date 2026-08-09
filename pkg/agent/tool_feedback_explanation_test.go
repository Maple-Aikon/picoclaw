// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors
//
// Phase 12.58.3 wire tests: when tool_feedback.separate_messages=false,
// tool feedback messages replace each other in a single tracked chat
// message. An LLM explanation attached to a tool call would therefore be
// lost once the next feedback overwrites the tracked message. The fix
// publishes the explanation as its own durable message (header + blank
// line + body — same shape as the complete_goal explanation message)
// whenever the LLM actually sent content text alongside the tool call.

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

func toolFeedbackExplanationTestConfig(t *testing.T, separate bool) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	cfg.Agents.Defaults.MaxTokens = 4096
	cfg.Agents.Defaults.MaxToolIterations = 10
	cfg.Agents.Defaults.ToolFeedback.Enabled = true
	cfg.Agents.Defaults.ToolFeedback.SeparateMessages = separate
	cfg.Agents.Defaults.ToolFeedback.ExplanationMessages = true
	return cfg
}

func runToolFeedbackExplanationTurn(
	t *testing.T,
	cfg *config.Config,
	provider *textOnlyPublishProvider,
) []bus.OutboundMessage {
	t.Helper()
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	defer al.Close()

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}
	_, err := al.runAgentLoop(context.Background(), defaultAgent, processOptions{
		Dispatch: DispatchRequest{
			SessionKey:  "session-1",
			UserMessage: "continue the goal",
			SessionScope: &session.SessionScope{
				Version:    session.ScopeVersionV1,
				AgentID:    defaultAgent.ID,
				Channel:    "telegram",
				Dimensions: []string{"chat"},
				Values: map[string]string{
					"chat": "direct:chat1",
				},
			},
			InboundContext: &bus.InboundContext{
				Channel:  "telegram",
				ChatID:   "chat1",
				ChatType: "direct",
				SenderID: "user1",
			},
		},
		DefaultResponse: "fallback",
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop() error = %v", err)
	}
	return drainOutbound(msgBus)
}

func outboundKind(msgs []bus.OutboundMessage, kind string) []bus.OutboundMessage {
	var out []bus.OutboundMessage
	for _, m := range msgs {
		if strings.EqualFold(strings.TrimSpace(m.Context.Raw["message_kind"]), kind) {
			out = append(out, m)
		}
	}
	return out
}

func toolFeedbackExplanationProvider() *textOnlyPublishProvider {
	return &textOnlyPublishProvider{responses: []*providers.LLMResponse{
		{
			Content:      "I will read README.md first.",
			ToolCalls:    []providers.ToolCall{textOnlySetGoalToolCall()},
			FinishReason: "tool_calls",
		},
		{
			// No content text — only the per-call continuation hint, which
			// must NOT produce a durable explanation message.
			ToolCalls:    []providers.ToolCall{textOnlyCompleteGoalToolCall("explanation publish done")},
			FinishReason: "tool_calls",
		},
	}}
}

// TestToolFeedbackExplanationPublishedAsSeparateMessageWhenReplacing:
// separate_messages=false — feedback messages replace each other, so the
// LLM explanation attached to a tool call must be published as its own
// durable message (header + blank line + explanation).
func TestToolFeedbackExplanationPublishedAsSeparateMessageWhenReplacing(t *testing.T) {
	cfg := toolFeedbackExplanationTestConfig(t, false)
	msgs := runToolFeedbackExplanationTurn(t, cfg, toolFeedbackExplanationProvider())

	if got := len(outboundKind(msgs, messageKindToolFeedback)); got == 0 {
		t.Fatalf("expected at least one tool_feedback message; outbound = %+v", msgs)
	}
	explanations := outboundKind(msgs, messageKindToolFeedbackExplanation)
	if len(explanations) != 1 {
		t.Fatalf("expected exactly 1 tool_feedback_explanation message, got %d; outbound = %+v",
			len(explanations), msgs)
	}
	body := explanations[0].Content
	if !strings.HasPrefix(body, "💭") {
		t.Fatalf("explanation message must start with the 💭 header; got %q", body)
	}
	if !strings.Contains(body, "\n\nI will read README.md first.") {
		t.Fatalf("explanation message must be header + blank line + explanation; got %q", body)
	}
}

// TestToolFeedbackExplanationSkippedWhenSeparateMessages:
// separate_messages=true — each feedback message is already its own durable
// message including the explanation, so no separate copy.
func TestToolFeedbackExplanationSkippedWhenSeparateMessages(t *testing.T) {
	cfg := toolFeedbackExplanationTestConfig(t, true)
	msgs := runToolFeedbackExplanationTurn(t, cfg, toolFeedbackExplanationProvider())

	if got := len(outboundKind(msgs, messageKindToolFeedbackExplanation)); got != 0 {
		t.Fatalf("separate_messages=true must NOT publish a separate explanation message, got %d; outbound = %+v",
			got, msgs)
	}
}

// TestToolFeedbackExplanationSkippedWhenDisabled:
// explanation_messages=false (the default) — the LLM explanation stays
// inside the tool feedback message only; no separate user-facing message.
func TestToolFeedbackExplanationSkippedWhenDisabled(t *testing.T) {
	cfg := toolFeedbackExplanationTestConfig(t, false)
	cfg.Agents.Defaults.ToolFeedback.ExplanationMessages = false
	msgs := runToolFeedbackExplanationTurn(t, cfg, toolFeedbackExplanationProvider())

	if got := len(outboundKind(msgs, messageKindToolFeedbackExplanation)); got != 0 {
		t.Fatalf("explanation_messages=false must NOT publish a separate explanation message, got %d; outbound = %+v",
			got, msgs)
	}
	if got := len(outboundKind(msgs, messageKindToolFeedback)); got == 0 {
		t.Fatalf("expected tool feedback messages to still carry the explanation; outbound = %+v", msgs)
	}
}

// TestShouldPublishToolFeedbackExplanation matrix: enabled+replacing+flag=true,
// separate=true→false, disabled→false, nil config→false.
func TestShouldPublishToolFeedbackExplanation(t *testing.T) {
	base := toolFeedbackExplanationTestConfig(t, false)
	ts := &turnState{channel: "telegram", chatID: "chat1"}

	cases := []struct {
		name string
		cfg  *config.Config
		ts   *turnState
		want bool
	}{
		{name: "enabled and replacing", cfg: base, ts: ts, want: true},
		{name: "explanation messages disabled", cfg: func() *config.Config {
			c := toolFeedbackExplanationTestConfig(t, false)
			c.Agents.Defaults.ToolFeedback.ExplanationMessages = false
			return c
		}(), ts: ts, want: false},
		{name: "separate messages", cfg: func() *config.Config {
			c := toolFeedbackExplanationTestConfig(t, true)
			return c
		}(), ts: ts, want: false},
		{name: "disabled", cfg: func() *config.Config {
			c := toolFeedbackExplanationTestConfig(t, false)
			c.Agents.Defaults.ToolFeedback.Enabled = false
			return c
		}(), ts: ts, want: false},
		{name: "nil config", cfg: nil, ts: ts, want: false},
		{name: "nil turn state", cfg: base, ts: nil, want: false},
		{name: "no channel", cfg: base, ts: &turnState{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldPublishToolFeedbackExplanation(tc.cfg, tc.ts); got != tc.want {
				t.Fatalf("shouldPublishToolFeedbackExplanation() = %v, want %v", got, tc.want)
			}
		})
	}
}

// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors
//
// Phase 12.56 wire tests: text-only LLM responses (no tool call) carry
// content the LLM intended for the user. When a recovery retry intervenes
// (same-iter at CHECKPOINT/FINAL, next-iter bump at OPEN), that text must
// be published to the user BEFORE the retry — the retry forces the LLM to
// act (call a tool), not to re-say what it already said.

package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// textOnlyPublishProvider scripts a fixed sequence of LLM responses for the
// text-only publish wire tests (task-extraction calls are bypassed).
type textOnlyPublishProvider struct {
	responses []*providers.LLMResponse
	mu        sync.Mutex
	calls     int
}

func (p *textOnlyPublishProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	if isTaskExtractionCall(messages, tools, opts) {
		return &providers.LLMResponse{Content: taskExtractionResponse(messages)}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.calls
	p.calls++
	if idx < len(p.responses) {
		return p.responses[idx], nil
	}
	return &providers.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *textOnlyPublishProvider) GetDefaultModel() string { return "text-only-publish-model" }

func textOnlySetGoalToolCall() providers.ToolCall {
	return providers.ToolCall{
		ID:   "call-set",
		Name: "set_goal",
		Arguments: map[string]any{
			"name":             "text-only-publish-goal",
			"objective":        "Verify text-only publish before recovery retry",
			"success_criteria": []any{"text published"},
		},
	}
}

func textOnlyCompleteGoalToolCall(summary string) providers.ToolCall {
	return providers.ToolCall{
		ID:        "call-complete",
		Name:      "complete_goal",
		Arguments: map[string]any{"summary": summary},
	}
}

// drainOutbound drains the bus outbound channel (PublishToUser →
// PublishResponseIfNeeded → bus.PublishOutbound is synchronous, so every
// message published during the turn is already buffered when the turn
// returns).
func drainOutbound(mb *bus.MessageBus) []bus.OutboundMessage {
	var msgs []bus.OutboundMessage
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case m := <-mb.OutboundChan():
			msgs = append(msgs, m)
		case <-time.After(20 * time.Millisecond):
			return msgs
		}
	}
	return msgs
}

func outboundContains(msgs []bus.OutboundMessage, want string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}

// TestTextOnlyPublishedBeforeOpenNextIterRecovery: at GOAL-OPEN a text-only
// response bumps recovery to the next iteration (RecoveryRetryNextIteration).
// The text must reach the user BEFORE the bump.
func TestTextOnlyPublishedBeforeOpenNextIterRecovery(t *testing.T) {
	provider := &textOnlyPublishProvider{responses: []*providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{textOnlySetGoalToolCall()}, FinishReason: "tool_calls"},
		{Content: "Open words: continuing with tools next.", FinishReason: "stop"},
		{ToolCalls: []providers.ToolCall{textOnlyCompleteGoalToolCall("open-publish done")}, FinishReason: "tool_calls"},
		{Content: "final", FinishReason: "stop"},
	}}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	cfg.Agents.Defaults.MaxTokens = 4096
	cfg.Agents.Defaults.MaxToolIterations = 10

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

	msgs := drainOutbound(msgBus)
	if !outboundContains(msgs, "Open words: continuing with tools next.") {
		t.Fatalf("text-only was NOT published before next-iter recovery; outbound = %+v", msgs)
	}
}

// TestTextOnlyPublishedBeforeCheckpointSameIterRecovery: at GOAL-CHECKPOINT
// a text-only response triggers same-iter retry. The text-only response
// (attempt 0) must reach the user BEFORE the re-prompt.
func TestTextOnlyPublishedBeforeCheckpointSameIterRecovery(t *testing.T) {
	provider := &textOnlyPublishProvider{responses: []*providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{textOnlySetGoalToolCall()}, FinishReason: "tool_calls"},
		{Content: "Open words.", FinishReason: "stop"},
		{Content: "Checkpoint words.", FinishReason: "stop"},
		{ToolCalls: []providers.ToolCall{textOnlyCompleteGoalToolCall("checkpoint done")}, FinishReason: "tool_calls"},
	}}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	cfg.Agents.Defaults.MaxTokens = 4096
	// iterationCap=3 → iter 3 is GOAL-CHECKPOINT.
	cfg.Agents.Defaults.MaxToolIterations = 3

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

	msgs := drainOutbound(msgBus)
	for _, want := range []string{"Open words.", "Checkpoint words."} {
		if !outboundContains(msgs, want) {
			t.Fatalf("text-only %q was NOT published before recovery; outbound = %+v", want, msgs)
		}
	}
}

// TestTextOnlyPublishedOnRetryAttemptInsideHandleGoalRecovery: when the
// same-iter retry loop re-prompts the LLM (attempt > 0) and it answers
// text-only again, that second text must ALSO be published before the next
// re-prompt. Pipeline-level test: seeds a goal, sets ts.al so
// PublishToUser has a back-ref, and drives handleGoalRecovery directly.
func TestTextOnlyPublishedOnRetryAttemptInsideHandleGoalRecovery(t *testing.T) {
	provider := &textOnlyPublishProvider{responses: []*providers.LLMResponse{
		{Content: "Retry words.", FinishReason: "stop"},
		{ToolCalls: []providers.ToolCall{textOnlyCompleteGoalToolCall("retry done")}, FinishReason: "tool_calls"},
	}}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	msgBus, ok := al.bus.(*bus.MessageBus)
	if !ok {
		t.Fatal("expected *bus.MessageBus from newTurnCoordTestLoop")
	}
	pipeline := NewPipeline(al)

	ws := t.TempDir()
	agent.Workspace = ws

	opts := makeTestProcessOpts("session-1")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-1",
		context: newTurnContext(nil, nil, nil),
	})
	ts.al = al // PublishToUser back-ref (normally set in runTurn).
	ts.channel = "telegram"
	ts.chatID = "chat1"
	ts.iterationCap = 3
	ts.iteration = 0
	ts.setIteration(3)

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}

	// Seed the goal AFTER SetupTurn (archive-on-start already ran).
	goalStore := goal.NewStore(ws)
	now := time.Now().UTC()
	if err := goalStore.Write("session-1", &goal.Goal{
		Name: "text-only-publish-goal",
		Description: goal.Description{
			Objective:       "Verify retry-attempt text publish",
			SuccessCriteria: []string{"retry text published"},
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	// Attempt 0 uses the caller's response (text-only).
	exec.response = &providers.LLMResponse{Content: "Checkpoint words.", FinishReason: "stop"}

	ctrl, err := pipeline.handleGoalRecovery(
		context.Background(),
		context.Background(),
		ts,
		exec,
		3,
		RecoveryRetrySameIteration,
		"Your last response was text-only with no tool call.",
	)
	if err != nil {
		t.Fatalf("handleGoalRecovery: %v", err)
	}
	// Phase 12.57: the retry attempt's tool call is now staged and routed
	// to the caller-loop for execution (ControlToolLoop), not dropped.
	if ctrl != ControlToolLoop {
		t.Fatalf("expected ControlToolLoop after retry succeeded with tool call (Phase 12.57), got %v", ctrl)
	}

	msgs := drainOutbound(msgBus)
	if !outboundContains(msgs, "Retry words.") {
		t.Fatalf("retry-attempt text-only %q was NOT published; outbound = %+v", "Retry words.", msgs)
	}
}

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// handledUserMultiCallProvider returns handled_user_tool on call 1, then complete_goal on call 2, then final answer on call 3.
type handledUserMultiCallProvider struct {
	calls int
}

func (m *handledUserMultiCallProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	if isTaskExtractionCall(messages, tools, opts) {
		return &providers.LLMResponse{Content: taskExtractionResponse(messages)}, nil
	}
	m.calls++
	if m.calls == 1 {
		return &providers.LLMResponse{
			Content: "Sending the file now.",
			ToolCalls: []providers.ToolCall{{
				ID:        "call_handled_user_1",
				Type:      "function",
				Name:      "handled_user_tool",
				Arguments: map[string]any{},
			}},
		}, nil
	}
	if m.calls == 2 {
		return &providers.LLMResponse{
			Content: "Completing goal now.",
			ToolCalls: []providers.ToolCall{{
				ID:   "call_complete_goal_2",
				Type: "function",
				Name: "complete_goal",
				Arguments: map[string]any{
					"summary": "Goal finished successfully after sending file.",
				},
			}},
		}, nil
	}
	// Call 3: Post-final 5-part report / final acknowledgment
	return &providers.LLMResponse{
		Content: "All work is done. File was sent and goal archived.",
	}, nil
}

func (m *handledUserMultiCallProvider) GetDefaultModel() string {
	return "handled-user-multi-model"
}

// TestRunAgentLoop_ResponseHandledToolWithActiveGoalDoesNotBreakLoop verifies that
// when an active goal exists on disk, a tool with ResponseHandled=true (such as send_file)
// does NOT terminate the agent loop early (ToolControlBreak). Instead, the loop continues
// to subsequent iterations so the LLM can complete the goal.
func TestRunAgentLoop_ResponseHandledToolWithActiveGoalDoesNotBreakLoop(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = tmpDir
	cfg.Agents.Defaults.ModelName = "test-model"
	cfg.Agents.Defaults.MaxTokens = 4096
	cfg.Agents.Defaults.MaxToolIterations = 10

	msgBus := bus.NewMessageBus()
	provider := &handledUserMultiCallProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	store := media.NewFileMediaStore()
	al.SetMediaStore(store)
	telegramChannel := &fakeMediaChannel{fakeChannel: fakeChannel{id: "rid-telegram"}}
	al.SetChannelManager(newStartedTestChannelManager(t, msgBus, store, "telegram", telegramChannel))
	al.RegisterTool(&handledUserTool{})
	al.SetGoalPhaseForTest(string(GoalPhaseOpen))
	al.SkipGoalArchiveForTest()

	defaultAgent := al.registry.GetDefaultAgent()
	if defaultAgent == nil {
		t.Fatal("expected default agent")
	}

	sessionKey := "session-goal-test-1"

	// Seed an active goal for this session
	goalStore := goal.NewStore(tmpDir)
	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "test-send-file-goal",
		Description: goal.Description{
			Objective:       "send file and continue",
			SuccessCriteria: []string{"loop continues after send_file"},
			Cadence:         "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goalStore.Write(sessionKey, activeGoal); err != nil {
		t.Fatalf("Write goal error: %v", err)
	}

	response, err := al.runAgentLoop(context.Background(), defaultAgent, processOptions{
		Dispatch: DispatchRequest{
			SessionKey:  sessionKey,
			UserMessage: "send the report file and then finalize",
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
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop() error = %v", err)
	}

	// Verify that provider was called at least 2 times (iteration 1: handled_user_tool, iteration 2: complete_goal)
	if provider.calls < 2 {
		t.Fatalf("expected provider.calls >= 2 (agent loop continued after ResponseHandled tool), got %d", provider.calls)
	}

	// Verify that the final response contains either the complete_goal summary or final content
	if response == "" && len(telegramChannel.sentMessages) == 0 {
		t.Fatalf("expected output to user, but got empty response and no sent messages")
	}
}

// Phase 12.61 wire tests: tool-call ID uniqueness tại 2 choke point chính —
// main path (proceedPastLLM) + recovery path (recallAndCheckTool /
// stageRecoveryToolCalls). Assert trên PAYLOAD provider nhận (call sau),
// không chỉ unit-level helper.

package agent

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// payloadCaptureProvider scripts responses + ghi mọi payload (sau
// task-extraction guard) để assert ids sau rewrite.
type payloadCaptureProvider struct {
	responses []*providers.LLMResponse
	payloads  [][]providers.Message
	mu        sync.Mutex
	calls     int
}

func (p *payloadCaptureProvider) Chat(
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
	p.payloads = append(p.payloads, append([]providers.Message(nil), messages...))
	if idx < len(p.responses) {
		return p.responses[idx], nil
	}
	return &providers.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *payloadCaptureProvider) GetDefaultModel() string { return "payload-capture-model" }

func newWireLoop(t *testing.T, provider providers.LLMProvider, maxIters int) (*AgentLoop, *bus.MessageBus) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	cfg.Agents.Defaults.MaxTokens = 4096
	cfg.Agents.Defaults.MaxToolIterations = maxIters
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	t.Cleanup(func() { al.Close() })
	return al, msgBus
}

func runWireTurn(t *testing.T, al *AgentLoop, msgBus *bus.MessageBus) {
	t.Helper()
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
				Values:     map[string]string{"chat": "direct:chat1"},
			},
			InboundContext: &bus.InboundContext{
				Channel:  "telegram",
				ChatID:   "chat1",
				ChatType: "direct",
				SenderID: "user1",
			},
		},
		DefaultResponse: "fallback",
		NoHistory:       true,
	})
	if err != nil {
		t.Fatalf("runAgentLoop error: %v", err)
	}
}

// findAssistantToolCalls — messages chứa assistant tool_calls (bỏ call-set
// của iter 1), trả (assistant msg, các tool results liền sau).
func findAssistantToolCalls(msgs []providers.Message) (providers.Message, []providers.Message, bool) {
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID != "call-set" {
			var results []providers.Message
			for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
				results = append(results, msgs[j])
			}
			return m, results, true
		}
	}
	return providers.Message{}, nil, false
}

func completeGoalTC(id string) providers.ToolCall {
	return providers.ToolCall{
		ID:        id,
		Name:      "complete_goal",
		Arguments: map[string]any{"summary": "wire done"},
	}
}

func setGoalTC() providers.ToolCall {
	return providers.ToolCall{
		ID:   "call-set",
		Name: "set_goal",
		Arguments: map[string]any{
			"name":             "tc-id-goal",
			"objective":        "Verify tool-call id uniqueness",
			"success_criteria": []any{"ids unique"},
		},
	}
}

// T2: main path — response chứa 2 tool calls CÙNG id fix_0 → payload call
// tiếp theo có ids unique + pairing intact.
func TestToolCallIDRewrite_MainPath(t *testing.T) {
	provider := &payloadCaptureProvider{responses: []*providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{setGoalTC()}, FinishReason: "tool_calls"},
		{ToolCalls: []providers.ToolCall{completeGoalTC("fix_0"), completeGoalTC("fix_0")}, FinishReason: "tool_calls"},
		{Content: "final report", FinishReason: "stop"},
	}}
	al, msgBus := newWireLoop(t, provider, 10)
	runWireTurn(t, al, msgBus)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.payloads) < 3 {
		t.Fatalf("expected >= 3 provider calls, got %d", len(provider.payloads))
	}
	asst, results, ok := findAssistantToolCalls(provider.payloads[2])
	if !ok {
		t.Fatalf("expected rewritten assistant tool_calls in payload 3, got %d messages", len(provider.payloads[2]))
	}
	if len(asst.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(asst.ToolCalls))
	}
	if asst.ToolCalls[0].ID == asst.ToolCalls[1].ID {
		t.Fatalf("ids must differ, got %s == %s", asst.ToolCalls[0].ID, asst.ToolCalls[1].ID)
	}
	// conditional: occurrence 1 giữ nguyên fix_0 (unique trong batch), occurrence 2
	// rewrite — nhưng payload không bao giờ có 2 id giống nhau
	fix0Seen := 0
	for _, tc := range asst.ToolCalls {
		if tc.ID == "fix_0" {
			fix0Seen++
		}
	}
	if fix0Seen > 1 {
		t.Fatalf("fix_0 must appear at most once in payload, got %d", fix0Seen)
	}
	for _, tc := range asst.ToolCalls {
		if tc.ID == "fix_0" {
			continue // occurrence 1 giữ nguyên (conditional)
		}
		if !strings.HasPrefix(tc.ID, "call_") {
			t.Fatalf("rewritten id must use call_ prefix, got %s", tc.ID)
		}
		if matched, _ := regexp.MatchString(`^call_san_[0-9]+$`, tc.ID); matched {
			t.Fatalf("layer-1 id %s must not collide with pass-3 namespace", tc.ID)
		}
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 tool results, got %d", len(results))
	}
	for i, tc := range asst.ToolCalls {
		if results[i].ToolCallID != tc.ID {
			t.Fatalf("pairing broken at %d: result %s != call %s", i, results[i].ToolCallID, tc.ID)
		}
	}
	_ = msgBus
}

// T3: recovery path (retryExecuteToolChain → recallAndCheckTool) — response
// recall chứa 2 calls cùng id fix_0 → staged ids unique + pairing.
func TestToolCallIDRewrite_RecoveryPath(t *testing.T) {
	provider := &payloadCaptureProvider{responses: []*providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{setGoalTC()}, FinishReason: "tool_calls"},
		// iter 2 = OPEN: view_goal chạy bình thường
		{ToolCalls: []providers.ToolCall{{ID: "call-vg", Name: "view_goal", Arguments: map[string]any{}}}, FinishReason: "tool_calls"},
		// iter 3 = CHECKPOINT (maxIters=3): view_goal bị gate chặn →
		// retryExecuteToolChain → RecallLLM (call 4)
		{ToolCalls: []providers.ToolCall{{ID: "call-vg2", Name: "view_goal", Arguments: map[string]any{}}}, FinishReason: "tool_calls"},
		// recall response: 2 calls cùng id fix_0 → rewrite tại recallAndCheckTool
		{ToolCalls: []providers.ToolCall{completeGoalTC("fix_0"), completeGoalTC("fix_0")}, FinishReason: "tool_calls"},
		{Content: "final", FinishReason: "stop"},
	}}
	al, _ := newWireLoop(t, provider, 3)
	runWireTurn(t, al, nil)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	// call 5 = final-report iter — payload chứa assistant rewrite ids + results.
	// Lưu ý: payload 4 (recall trước đó) cũng chứa assistant call-vg2 bị gate
	// block (synthetic result) — tìm assistant có ĐÚNG 2 calls (fix_0 pair).
	if len(provider.payloads) < 5 {
		t.Fatalf("expected >= 5 provider calls (incl. recall), got %d", len(provider.payloads))
	}
	var asst providers.Message
	var results []providers.Message
	ok := false
	for _, m := range provider.payloads[4] {
		if m.Role == "assistant" && len(m.ToolCalls) == 2 {
			asst = m
			ok = true
			break
		}
	}
	if !ok {
		t.Fatalf("expected rewritten 2-call assistant msg in payload 5, got %d messages", len(provider.payloads[4]))
	}
	for j := 0; j < len(provider.payloads[4]); j++ {
		if provider.payloads[4][j].Role == "assistant" && len(provider.payloads[4][j].ToolCalls) == 2 {
			for k := j + 1; k < len(provider.payloads[4]) && provider.payloads[4][k].Role == "tool"; k++ {
				results = append(results, provider.payloads[4][k])
			}
			break
		}
	}
	if len(asst.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(asst.ToolCalls))
	}
	if asst.ToolCalls[0].ID == asst.ToolCalls[1].ID {
		t.Fatalf("ids must differ, got %s == %s", asst.ToolCalls[0].ID, asst.ToolCalls[1].ID)
	}
	if asst.ToolCalls[0].ID == "fix_0" && asst.ToolCalls[1].ID == "fix_0" {
		t.Fatalf("dup fix_0 must be rewritten in recovery path")
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 tool results, got %d", len(results))
	}
	for i, tc := range asst.ToolCalls {
		if results[i].ToolCallID != tc.ID {
			t.Fatalf("pairing broken at %d: result %s != call %s", i, results[i].ToolCallID, tc.ID)
		}
	}
}

// T11b: final-report iter strip — payload final-report KHÔNG chứa tool calls
// (strip site vẫn hoạt động, không bị rewrite phá).
func TestToolCallIDRewrite_FinalReportStrip(t *testing.T) {
	provider := &payloadCaptureProvider{responses: []*providers.LLMResponse{
		{ToolCalls: []providers.ToolCall{setGoalTC()}, FinishReason: "tool_calls"},
		// complete_goal tại iter 2 (CHECKPOINT, maxIters=2) → final-report iter
		{ToolCalls: []providers.ToolCall{completeGoalTC("fix_0")}, FinishReason: "tool_calls"},
		// LLM drift: emit tool call ở final-report iter → strip
		{ToolCalls: []providers.ToolCall{completeGoalTC("fix_0")}, FinishReason: "tool_calls"},
		{Content: "final", FinishReason: "stop"},
	}}
	al, _ := newWireLoop(t, provider, 2)
	runWireTurn(t, al, nil)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	// strip hoạt động → call 3 (drift) bị strip → turn end silent (KHÔNG có
	// call 4). Nếu strip vỡ → fix_0 chạy → final-report iter → call 4.
	if len(provider.payloads) != 3 {
		t.Fatalf("strip must end turn after drift call, got %d provider calls", len(provider.payloads))
	}
}

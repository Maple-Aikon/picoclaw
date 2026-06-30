package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestMatchingTurnMessageTail_IgnoresInternalRuntimeFields(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "question"},
		{
			Role: "assistant",
			ToolCalls: []providers.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: &providers.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"/tmp/test"}`,
					},
				},
			},
		},
	}

	persisted := []providers.Message{
		userPromptMessage("question", nil),
		{
			Role: "assistant",
			ToolCalls: []providers.ToolCall{
				{
					ID:               "call_1",
					Type:             "function",
					Name:             "read_file",
					Arguments:        map[string]any{"path": "/tmp/test"},
					ThoughtSignature: "internal-signature",
					Function: &providers.FunctionCall{
						Name:             "read_file",
						Arguments:        `{"path":"/tmp/test"}`,
						ThoughtSignature: "internal-signature",
					},
				},
			},
		},
	}

	if got := matchingTurnMessageTail(history, persisted); got != 2 {
		t.Fatalf("matchingTurnMessageTail() = %d, want 2", got)
	}
}

func TestSplitHistoryForActiveTurn_ProtectsPersistedTail(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "current question"},
		{Role: "tool", Content: "tool output", ToolCallID: "call_1"},
	}

	persisted := []providers.Message{
		userPromptMessage("current question", nil),
		{Role: "tool", Content: "tool output", ToolCallID: "call_1"},
	}

	stable, protected := splitHistoryForActiveTurn(history, persisted)
	if len(stable) != 2 {
		t.Fatalf("stable history len = %d, want 2", len(stable))
	}
	if len(protected) != 2 {
		t.Fatalf("protected tail len = %d, want 2", len(protected))
	}
	if protected[0].Content != "current question" {
		t.Fatalf("protected[0].Content = %q, want current question", protected[0].Content)
	}
}

func TestTrimHistoryToFitContextWindow_WithProtectedTurnTailKeepsActiveTurn(t *testing.T) {
	current := strings.Repeat("current turn ", 80)
	history := []providers.Message{
		{Role: "user", Content: strings.Repeat("old turn ", 60)},
		{Role: "assistant", Content: strings.Repeat("old reply ", 60)},
		{Role: "user", Content: current},
	}

	stable, protected := splitHistoryForActiveTurn(history, []providers.Message{
		userPromptMessage(current, nil),
	})
	trimmedStable, messages, fit := trimHistoryToFitContextWindow(
		stable,
		func(trimmedHistory []providers.Message) []providers.Message {
			return append(append([]providers.Message(nil), trimmedHistory...), protected...)
		},
		120,
		nil,
		0,
	)

	if fit {
		t.Fatal("expected protected active turn alone to remain over budget")
	}
	if len(trimmedStable) != 0 {
		t.Fatalf("trimmed stable history len = %d, want 0", len(trimmedStable))
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1 protected active-turn message", len(messages))
	}
	if messages[0].Content != current {
		t.Fatalf("messages[0].Content = %q, want protected current turn", messages[0].Content)
	}
}

func TestTurnState_RemainingIterations_Basic(t *testing.T) {
	ts := &turnState{
		iteration:    5,
		iterationCap: 20,
		agent:        &AgentInstance{MaxIterations: 20, MaxIterationsCap: 100},
	}
	if got := ts.RemainingIterations(); got != 15 {
		t.Errorf("RemainingIterations() = %d, want 15", got)
	}
}

func TestTurnState_RemainingIterations_Extended(t *testing.T) {
	ts := &turnState{
		iteration:    18,
		iterationCap: 40, // extended from 20 to 40
		agent:        &AgentInstance{MaxIterations: 20, MaxIterationsCap: 100},
	}
	if got := ts.RemainingIterations(); got != 22 {
		t.Errorf("RemainingIterations() = %d, want 22", got)
	}
}

func TestTurnState_RemainingIterations_ClampedToZero(t *testing.T) {
	ts := &turnState{
		iteration:    50,
		iterationCap: 40,
		agent:        &AgentInstance{MaxIterations: 20, MaxIterationsCap: 100},
	}
	if got := ts.RemainingIterations(); got != 0 {
		t.Errorf("RemainingIterations() = %d, want 0 (clamped)", got)
	}
}

func TestTurnState_ExtendIterationCap_Basic(t *testing.T) {
	ts := &turnState{
		iteration:    17,
		iterationCap: 20,
		agent:        &AgentInstance{MaxIterations: 20, MaxIterationsCap: 100},
	}
	newCap, err := ts.ExtendIterationCap(20, "")
	if err != nil {
		t.Fatalf("ExtendIterationCap returned error: %v", err)
	}
	if newCap != 40 {
		t.Errorf("newCap = %d, want 40", newCap)
	}
	if ts.iterationCap != 40 {
		t.Errorf("ts.iterationCap = %d, want 40", ts.iterationCap)
	}
}

func TestTurnState_ExtendIterationCap_RespectsAbsoluteCap(t *testing.T) {
	ts := &turnState{
		iteration:    90,
		iterationCap: 95,
		agent:        &AgentInstance{MaxIterations: 20, MaxIterationsCap: 100},
	}
	_, err := ts.ExtendIterationCap(50, "")
	if err == nil {
		t.Fatal("expected error when extending past MaxIterationsCap, got nil")
	}
	if ts.iterationCap != 95 {
		t.Errorf("ts.iterationCap = %d, want 95 (unchanged on error)", ts.iterationCap)
	}
}

func TestTurnState_ExtendIterationCap_DefaultBudget(t *testing.T) {
	ts := &turnState{
		iteration:    17,
		iterationCap: 20,
		agent:        &AgentInstance{MaxIterations: 20, MaxIterationsCap: 100},
	}
	// n=0 means use agent.MaxIterations as the extension amount
	newCap, err := ts.ExtendIterationCap(0, "")
	if err != nil {
		t.Fatalf("ExtendIterationCap returned error: %v", err)
	}
	if newCap != 40 {
		t.Errorf("newCap = %d, want 40 (MaxIterations=20 applied as extension)", newCap)
	}
}

func TestTurnState_ExtendIterationCap_DisabledWhenCapZero(t *testing.T) {
	ts := &turnState{
		iteration:    17,
		iterationCap: 20,
		agent:        &AgentInstance{MaxIterations: 20, MaxIterationsCap: 0},
	}
	_, err := ts.ExtendIterationCap(20, "")
	if err == nil {
		t.Fatal("expected error when extension disabled (MaxIterationsCap=0), got nil")
	}
}

package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
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

// Phase 10.1: extend_turn_iteration tool was removed in Phase 10, but the
// underlying ExtendIterationCap mechanism was restored so goal_progress can
// self-extend the iteration cap up to agent.MaxIterationsCap. The cap is no
// longer strictly constant per turn.

func TestTurnState_RemainingIterations_Basic(t *testing.T) {
	ts := &turnState{
		iteration:    5,
		iterationCap: 20,
		agent:        &AgentInstance{MaxIterations: 20},
	}
	if got := ts.RemainingIterations(); got != 15 {
		t.Errorf("RemainingIterations() = %d, want 15", got)
	}
}

func TestTurnState_RemainingIterations_ClampedToZero(t *testing.T) {
	ts := &turnState{
		iteration:    50,
		iterationCap: 40,
		agent:        &AgentInstance{MaxIterations: 40},
	}
	if got := ts.RemainingIterations(); got != 0 {
		t.Errorf("RemainingIterations() = %d, want 0 (clamped)", got)
	}
}


// --- Phase 10.1: ExtendIterationCap tests ---

func TestTurnState_ExtendIterationCap_Basic(t *testing.T) {
	ts := &turnState{
		iteration:        20,
		iterationCap:     20,
		maxIterationsCap: 200,
		agent:            &AgentInstance{MaxIterations: 20, MaxIterationsCap: 200},
	}
	newCap, delta := ts.ExtendIterationCap(180, "test basic extend")
	if delta != 200-20 {
		t.Errorf("delta = %d, want %d", delta, 200-20)
	}
	if newCap != 200 {
		t.Errorf("newCap = %d, want 200", newCap)
	}
	if ts.IterationCap() != 200 {
		t.Errorf("IterationCap() after extend = %d, want 200", ts.IterationCap())
	}
}

func TestTurnState_ExtendIterationCap_ClampedAtCeiling(t *testing.T) {
	ts := &turnState{
		iteration:        100,
		iterationCap:     200,
		maxIterationsCap: 200,
		agent:            &AgentInstance{MaxIterations: 200, MaxIterationsCap: 200},
	}
	// Already at ceiling: extend by 50 → clamped, delta=0.
	newCap, delta := ts.ExtendIterationCap(50, "test clamp")
	if delta != 0 {
		t.Errorf("delta = %d, want 0 (already at ceiling)", delta)
	}
	if newCap != 200 {
		t.Errorf("newCap = %d, want 200 (unchanged)", newCap)
	}
}

func TestTurnState_ExtendIterationCap_NegativeIgnored(t *testing.T) {
	ts := &turnState{
		iteration:        5,
		iterationCap:     20,
		maxIterationsCap: 200,
		agent:            &AgentInstance{MaxIterations: 20, MaxIterationsCap: 200},
	}
	newCap, delta := ts.ExtendIterationCap(-10, "test negative")
	if delta != 0 || newCap != 20 {
		t.Errorf("negative extend should be no-op: got (cap=%d, delta=%d), want (20, 0)", newCap, delta)
	}
}

func TestTurnState_ExtendIterationCap_ZeroIsNoop(t *testing.T) {
	ts := &turnState{
		iteration:        5,
		iterationCap:     20,
		maxIterationsCap: 200,
		agent:            &AgentInstance{MaxIterations: 20, MaxIterationsCap: 200},
	}
	newCap, delta := ts.ExtendIterationCap(0, "test zero")
	if delta != 0 {
		t.Errorf("n=0 should return delta=0, got %d", delta)
	}
	if newCap != 20 {
		t.Errorf("n=0 should not modify cap: got %d, want 20", newCap)
	}
	if reason, iter := ts.LastExtensionInfo(); reason != "" || iter != 0 {
		t.Errorf("n=0 should NOT record extension info: got (reason=%q, iter=%d), want zero values", reason, iter)
	}
}

func TestTurnState_ExtendIterationCap_RecordsReason(t *testing.T) {
	ts := &turnState{
		iteration:        5,
		iterationCap:     20,
		maxIterationsCap: 200,
		agent:            &AgentInstance{MaxIterations: 20, MaxIterationsCap: 200},
	}
	_, _ = ts.ExtendIterationCap(30, "my-reason")
	if reason, atIter := ts.LastExtensionInfo(); reason != "my-reason" || atIter != 5 {
		t.Errorf("LastExtensionInfo() = (%q, %d), want (\"my-reason\", 5)", reason, atIter)
	}
}

func TestTurnState_CanExtendIterationCap(t *testing.T) {
	// Case 1: below ceiling → can extend.
	ts1 := &turnState{iterationCap: 50, maxIterationsCap: 200}
	if !ts1.CanExtendIterationCap() {
		t.Error("CanExtendIterationCap() = false below ceiling, want true")
	}
	// Case 2: at ceiling → cannot extend.
	ts2 := &turnState{iterationCap: 200, maxIterationsCap: 200}
	if ts2.CanExtendIterationCap() {
		t.Error("CanExtendIterationCap() = true at ceiling, want false")
	}
	// Case 3: ceiling == 0 (unbounded) → always can extend.
	ts3 := &turnState{iterationCap: 9999, maxIterationsCap: 0}
	if !ts3.CanExtendIterationCap() {
		t.Error("CanExtendIterationCap() = false with unbounded ceiling, want true")
	}
}

func TestTurnState_ExtendIterationCap_UnboundedCeiling(t *testing.T) {
	ts := &turnState{
		iteration:        5,
		iterationCap:     20,
		maxIterationsCap: 0, // 0 = unbounded per design
		agent:            &AgentInstance{MaxIterations: 20},
	}
	newCap, delta := ts.ExtendIterationCap(100, "test unbounded")
	if delta != 100 || newCap != 120 {
		t.Errorf("unbounded ceiling: (cap=%d, delta=%d), want (120, 100)", newCap, delta)
	}
}

func TestTurnState_MaxIterationsCap_Accessor(t *testing.T) {
	ts := &turnState{
		maxIterationsCap: 250,
	}
	if got := ts.MaxIterationsCap(); got != 250 {
		t.Errorf("MaxIterationsCap() = %d, want 250", got)
	}
}

func TestTurnState_AsExtender_InterfaceSatisfied(t *testing.T) {
	ts := &turnState{
		iteration:        5,
		iterationCap:     20,
		maxIterationsCap: 200,
		agent:            &AgentInstance{MaxIterations: 20, MaxIterationsCap: 200},
	}
	var ext goal.IterationExtender = ts.AsExtender()
	if ext == nil {
		t.Fatal("AsExtender() returned nil")
	}
	if got := ext.RemainingIterations(); got != 15 {
		t.Errorf("via extender: RemainingIterations() = %d, want 15", got)
	}
	if !ext.CanExtendIterationCap() {
		t.Error("via extender: CanExtendIterationCap() = false, want true")
	}
}

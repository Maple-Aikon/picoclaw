package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// Test fixture helper: build a turnState + turnExecution with a real
// ContextBuilder (no skills loaded — empty tempDir).
func newPhaseRebuildTestFixture(
	t *testing.T,
	initialLastBuilt string,
	currentPhase GoalPhase,
) (*turnState, *turnExecution, []providers.Message) {
	t.Helper()

	cb := NewContextBuilder(t.TempDir())
	ts := &turnState{
		agent: &AgentInstance{
			ContextBuilder: cb,
		},
		activeSkills:         nil,
		userMessage:          "test",
		media:                nil,
		opts:                 processOptions{Dispatch: DispatchRequest{}},
		lastBuiltPromptPhase: initialLastBuilt,
	}
	ts.opts.Dispatch.SessionKey = "test-session"
	ts.opts.SessionKey = "test-session"

	// Force currentGoalPhase() via PhaseOverrideForTest (test-only escape hatch).
	ts.agent.PhaseOverrideForTest = string(currentPhase)

	exec := &turnExecution{
		history: []providers.Message{},
		summary: "",
	}

	initialMessages := []providers.Message{
		{Role: "system", Content: "ORIGINAL_SYSTEM_PROMPT"},
	}

	return ts, exec, initialMessages
}

// =====================================================================
// T1: First iter with lastBuiltPromptPhase="" triggers rebuild
// =====================================================================

func TestPhaseChangeRebuild_FirstIter_TriggersRebuild(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, "", GoalPhaseSet)

	result := ts.maybeRebuildPromptForPhaseChange(messages, exec, nil, 1)

	if ts.lastBuiltPromptPhase != string(GoalPhaseSet) {
		t.Errorf("expected lastBuiltPromptPhase = %q, got %q", string(GoalPhaseSet), ts.lastBuiltPromptPhase)
	}
	if len(result) < 1 {
		t.Fatalf("expected at least 1 message in result, got %d", len(result))
	}
	if result[0].Content == "ORIGINAL_SYSTEM_PROMPT" {
		t.Errorf("expected messages[0].Content to be replaced, still ORIGINAL_SYSTEM_PROMPT")
	}
}

// =====================================================================
// T2: Same phase → no rebuild
// =====================================================================

func TestPhaseChangeRebuild_SamePhase_NoRebuild(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, string(GoalPhaseOpen), GoalPhaseOpen)

	result := ts.maybeRebuildPromptForPhaseChange(messages, exec, nil, 3)

	if ts.lastBuiltPromptPhase != string(GoalPhaseOpen) {
		t.Errorf("expected lastBuiltPromptPhase unchanged = %q, got %q", string(GoalPhaseOpen), ts.lastBuiltPromptPhase)
	}
	if len(result) != len(messages) {
		t.Errorf("expected result length unchanged (%d), got %d", len(messages), len(result))
	}
	if result[0].Content != "ORIGINAL_SYSTEM_PROMPT" {
		t.Errorf("expected messages[0] unchanged, got %q", result[0].Content)
	}
}

// =====================================================================
// T3: Open → Checkpoint triggers rebuild, preserves messages[1:N]
// =====================================================================

func TestPhaseChangeRebuild_OpenToCheckpoint_TriggersRebuild(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, string(GoalPhaseOpen), GoalPhaseCheckpoint)

	messages = append(messages, providers.Message{Role: "user", Content: "userMsg1"})

	result := ts.maybeRebuildPromptForPhaseChange(messages, exec, nil, 5)

	if ts.lastBuiltPromptPhase != string(GoalPhaseCheckpoint) {
		t.Errorf("expected lastBuiltPromptPhase = %q, got %q", string(GoalPhaseCheckpoint), ts.lastBuiltPromptPhase)
	}
	// Result must be exactly 2 messages: rebuilt system + preserved userMsg1
	// (we take newMessages[0] from BuildMessagesFromPrompt, then append messages[1:])
	if len(result) != 2 {
		t.Fatalf("expected 2 messages (rebuilt system + userMsg1), got %d: %v", len(result), result)
	}
	if result[1].Content != "userMsg1" {
		t.Errorf("expected messages[1] preserved (userMsg1), got %q", result[1].Content)
	}
	if result[0].Content == "ORIGINAL_SYSTEM_PROMPT" {
		t.Errorf("expected messages[0] rebuilt, still ORIGINAL_SYSTEM_PROMPT")
	}
}

// =====================================================================
// T4: Checkpoint → Open triggers rebuild, preserves messages[1:N] (3 elements)
// =====================================================================

func TestPhaseChangeRebuild_CheckpointToOpen_TriggersRebuild(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, string(GoalPhaseCheckpoint), GoalPhaseOpen)

	messages = append(messages,
		providers.Message{Role: "user", Content: "userMsg1"},
		providers.Message{Role: "tool", Content: "toolResult1"},
		providers.Message{Role: "user", Content: "userMsg2"},
	)

	result := ts.maybeRebuildPromptForPhaseChange(messages, exec, nil, 6)

	if ts.lastBuiltPromptPhase != string(GoalPhaseOpen) {
		t.Errorf("expected lastBuiltPromptPhase = %q, got %q", string(GoalPhaseOpen), ts.lastBuiltPromptPhase)
	}
	// All 3 preserved messages must appear at the tail of result (in order).
	expected := []string{"userMsg1", "toolResult1", "userMsg2"}
	if len(result) < len(expected)+1 {
		t.Fatalf("expected at least %d messages, got %d", len(expected)+1, len(result))
	}
	tailStart := len(result) - len(expected)
	for i, exp := range expected {
		if result[tailStart+i].Content != exp {
			t.Errorf("expected result[%d].Content = %q, got %q", tailStart+i, exp, result[tailStart+i].Content)
		}
	}
}

// =====================================================================
// T5: All phase transitions matrix
// =====================================================================

func TestPhaseChangeRebuild_AllPhaseTransitions(t *testing.T) {
	phases := []GoalPhase{GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal}

	for _, lastBuilt := range phases {
		for _, current := range phases {
			name := string(lastBuilt) + "_to_" + string(current)
			t.Run(name, func(t *testing.T) {
				ts, exec, messages := newPhaseRebuildTestFixture(t, string(lastBuilt), current)

				ts.maybeRebuildPromptForPhaseChange(messages, exec, nil, 1)

				if ts.lastBuiltPromptPhase != string(current) {
					t.Errorf("[%s] expected lastBuiltPromptPhase = %q, got %q", name, string(current), ts.lastBuiltPromptPhase)
				}
			})
		}
	}
}

// =====================================================================
// T7 (BONUS): Verify rebuilt messages[0] differs across phase transitions
// =====================================================================

func TestPhaseChangeRebuild_RebuiltPromptHasPhaseSpecificHint(t *testing.T) {
	tsSet, execSet, messagesSet := newPhaseRebuildTestFixture(t, "", GoalPhaseSet)
	resultSet := tsSet.maybeRebuildPromptForPhaseChange(messagesSet, execSet, nil, 1)
	setPrompt := resultSet[0].Content

	tsCp, execCp, messagesCp := newPhaseRebuildTestFixture(t, string(GoalPhaseOpen), GoalPhaseCheckpoint)
	resultCp := tsCp.maybeRebuildPromptForPhaseChange(messagesCp, execCp, nil, 5)
	cpPrompt := resultCp[0].Content

	if strings.TrimSpace(setPrompt) == strings.TrimSpace(cpPrompt) {
		t.Errorf("expected SET and CHECKPOINT prompts to differ")
	}
	t.Logf("SET prompt len=%d, CHECKPOINT prompt len=%d", len(setPrompt), len(cpPrompt))
}

package agent

import (
	"strings"
	"testing"
)

// TestTurnGoalPhaseInjection_UserMessageTail verifies that BuildMessagesFromPrompt
// injects the <goal_phase> XML block into the tail of the active user message,
// leaving the system prompt completely static.
func TestTurnGoalPhaseInjection_UserMessageTail(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md": "# PicoClaw",
	})
	cb := NewContextBuilder(tmpDir)

	testCases := []struct {
		name           string
		phase          GoalPhase
		iter           int
		maxIter        int
		nextCheckpoint int
		expectedTag    string
		expectedText   string
	}{
		{
			name:         "SET Phase",
			phase:        GoalPhaseSet,
			iter:         1,
			maxIter:      250,
			expectedTag:  `<goal_phase phase="SET" iter="1" max="250">`,
			expectedText: "[HARD GUARD] Goal phase: SET.",
		},
		{
			name:           "OPEN Phase",
			phase:          GoalPhaseOpen,
			iter:           3,
			maxIter:        250,
			nextCheckpoint: 25,
			expectedTag:    `<goal_phase phase="OPEN" iter="3" max="250" next_checkpoint="25">`,
			expectedText:   "Goal phase: OPEN (iter 3 / total 250 turn iters).",
		},
		{
			name:         "CHECKPOINT Phase",
			phase:        GoalPhaseCheckpoint,
			iter:         25,
			maxIter:      250,
			expectedTag:  `<goal_phase phase="CHECKPOINT" iter="25" max="250">`,
			expectedText: "[HARD STOP] Goal phase: CHECKPOINT.",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := PromptBuildRequest{
				GoalPhase:        string(tc.phase),
				Iteration:        tc.iter,
				MaxIterationsCap: tc.maxIter,
				IterationCap:     tc.nextCheckpoint,
				CurrentMessage:   "Hello PicoClaw",
			}
			messages := cb.BuildMessagesFromPrompt(req)
			if len(messages) < 2 {
				t.Fatalf("expected at least 2 messages (system + user), got %d", len(messages))
			}

			systemMsg := messages[0]
			userMsg := messages[len(messages)-1]

			// Assert System Prompt does NOT contain the dynamic iteration marker
			if strings.Contains(systemMsg.Content, "<goal_phase") ||
				strings.Contains(systemMsg.Content, "[HARD GUARD]") ||
				strings.Contains(systemMsg.Content, "[HARD STOP]") {
				t.Errorf("System prompt contains dynamic turn-tail banner: %s", systemMsg.Content)
			}

			// Assert User Message DOES contain the dynamic goal phase banner
			if !strings.Contains(userMsg.Content, tc.expectedTag) {
				t.Errorf("User message missing tag %q.\nGot: %s", tc.expectedTag, userMsg.Content)
			}
			if !strings.Contains(userMsg.Content, tc.expectedText) {
				t.Errorf("User message missing text %q.\nGot: %s", tc.expectedText, userMsg.Content)
			}
		})
	}
}

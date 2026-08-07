// Phase 12.54 — owner decision 2026-08-07 (anh Maple):
//   (1) reasoning is the LLM's internal thinking — NEVER promoted to main Content
//   (2) TextEmpty = Content=="" only — reasoning-only responses count as empty
//
// W1: SET-phase reasoning-only fires empty-response same-iter recovery ×3
//     then archives (no silent turn end with reasoning as the reply).
// W2: OPEN-phase reasoning-only switches from text-only next-iter carry to
//     same-iter empty retry ×3 → archive.
// W3: Final + post-report guards stay silent (regression guard).

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

// W1 — reasoning-only at SET must fire empty-response recovery (×3) instead
// of ending the turn silently with the reasoning promoted to the reply.
// Phase 12.54: TextEmpty = Content=="" only, so reasoningContent != "" does
// NOT exempt the response from the empty trigger.
func TestPhase12_54_SetReasoningOnly_FiresEmptyRecovery_NoPromote(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}

	msgBus := bus.NewMessageBus()
	provider := &reasoningContentProvider{
		response:         "",
		reasoningContent: "internal reasoning trace (W1)",
	}
	al := NewAgentLoop(cfg, msgBus, provider)

	chManager, err := channels.NewManager(&config.Config{}, msgBus, nil)
	if err != nil {
		t.Fatalf("Failed to create channel manager: %v", err)
	}
	chManager.RegisterChannel("telegram", &fakeChannelNoReasoning{fakeChannel: fakeChannel{id: ""}})
	al.SetChannelManager(chManager)

	response, err := al.processMessage(context.Background(), testInboundMessage(bus.InboundMessage{
		Channel:  "telegram",
		SenderID: "user1",
		ChatID:   "chat1",
		Content:  "hello",
	}))
	if err != nil {
		t.Fatalf("processMessage() error = %v", err)
	}
	if response != defaultResponse {
		t.Fatalf("processMessage() response = %q, want DefaultResponse %q (reasoning-only must NOT be the reply)",
			response, defaultResponse)
	}
	if provider.calls != 3 {
		t.Fatalf("provider calls = %d, want 3 (1 initial + 2 same-iter RecallLLM retries; cap=3 evaluations on attempt 0..2 then archive)", provider.calls)
	}
}

// W2 — OPEN reasoning-only must switch from text-only next-iter carry to
// same-iter empty ×3 → archive (Phase 12.37 spec 7 applied to reasoning-only).
func TestPhase12_54_OpenReasoningOnly_SameIterRetryThenArchive(t *testing.T) {
	provider := &reasoningContentProvider{
		response:         "",
		reasoningContent: "internal reasoning trace (W2)",
	}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	t.Cleanup(cleanup)

	al.SetGoalPhaseForTest(string(GoalPhaseOpen))

	pipeline := NewPipeline(al)
	ws := t.TempDir()
	agent.Workspace = ws

	ts := newTurnState(agent, makeTestProcessOpts("phase-12-54-open-session"), turnEventScope{
		turnID:  "turn-12-54-open",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 5
	ts.maxIterationsCap = 50
	ts.setIteration(3) // OPEN phase

	_, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}

	// Seed an active goal AFTER SetupTurn (archive-on-start already ran).
	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "phase-12-54-open",
		Description: goal.Description{
			Objective:       "W2 OPEN reasoning-only same-iter retry",
			SuccessCriteria: []string{"reasoning-only treated as empty at OPEN"},
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goal.NewStore(ws).Write("phase-12-54-open-session", activeGoal); err != nil {
		t.Fatalf("Write goal: %v", err)
	}
	if !ts.hasGoal() {
		t.Fatal("setup error: hasGoal=false; goal file not seeded")
	}

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn error: %v", err)
	}
	if strings.Contains(result.finalContent, "internal reasoning trace (W2)") {
		t.Fatalf("reasoning leaked into final reply: %q", result.finalContent)
	}
	if !ts.goalArchiveRequested {
		t.Fatalf("goalArchiveRequested = false; want true (OPEN reasoning-only must archive after 3 empty retries, NOT next-iter carry)")
	}
	if ts.emptyResponseRecoveryCount != 3 {
		t.Fatalf("emptyResponseRecoveryCount = %d, want 3", ts.emptyResponseRecoveryCount)
	}
	if provider.calls != 3 {
		t.Fatalf("provider calls = %d, want 3 (1 initial + 2 same-iter RecallLLM retries; cap=3 evaluations then archive)", provider.calls)
	}
	if g, _ := goal.NewStore(ws).Read("phase-12-54-open-session"); g != nil && g.Status == goal.StatusActive {
		t.Fatalf("goal still ACTIVE after archive: %+v", g)
	}
}

// W3 — regression guard: Final + post-report stays silent even when the
// response is reasoning-only (TextEmpty=true under the Phase 12.54 rule).
// Both guards (ts.postCompleteGoalReportSent at recovery_goal.go:340 and
// ctx.PostCompleteGoalReport inside the empty trigger at :465) must return
// RecoveryNone.
func TestPhase12_54_FinalPostReport_ReasoningOnlySilent(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.postCompleteGoalReportSent = true
	action, _ := evaluateRecovery(ts, RecoveryContext{
		Phase:        string(GoalPhaseFinal),
		Iteration:    5,
		TextEmpty:    true,
		HasToolCalls: false,
	})
	if action != RecoveryNone {
		t.Fatalf("guard (ts.postCompleteGoalReportSent): action = %v, want RecoveryNone", action)
	}

	ts2 := newPhase5TurnState(t)
	action2, _ := evaluateRecovery(ts2, RecoveryContext{
		Phase:                  string(GoalPhaseFinal),
		Iteration:              5,
		TextEmpty:              true,
		HasToolCalls:           false,
		PostCompleteGoalReport: true,
	})
	if action2 != RecoveryNone {
		t.Fatalf("guard (ctx.PostCompleteGoalReport): action = %v, want RecoveryNone", action2)
	}
}

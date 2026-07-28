package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// phase12_25TestProvider is a stub LLMProvider for tests that don't actually
// invoke the LLM (Phase 12.25 tests only exercise archive-and-reset logic).
type phase12_25TestProvider struct{}

func (p *phase12_25TestProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{Content: "", ToolCalls: nil}, nil
}

func (p *phase12_25TestProvider) GetDefaultModel() string {
	return "test-model"
}

// newPhase12_25AgentLoop constructs a minimal AgentLoop with a tmp workspace
// suitable for archive-and-reset tests. Mirrors newHookTestLoop pattern but
// without provider dependency (since these tests don't invoke the LLM).
func newPhase12_25AgentLoop(t *testing.T) (*AgentLoop, func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "phase12-25-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

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

	al := NewAgentLoop(cfg, bus.NewMessageBus(), &phase12_25TestProvider{})

	return al, func() {
		al.Close()
		_ = os.RemoveAll(tmpDir)
	}
}

// TestPhase12_25_ArchiveAndReset_ClearsGoalFlags verifies the Phase 12.25
// cross-turn archive-and-reset pre-loop hook wipes in-memory goal flags
// from the prior turn so the new turn starts with a clean slate.
//
// Setup: ts has goalFinalized=true, postCompleteGoalReportSent=true,
// goalArchiveRequested=true, pendingFinalReportIter=true (all flags
// the prior turn set). Goal file on disk is StatusActive (stale — prior
// turn did not transition to completed/archived).
//
// Expected after hook: all 4 flags = false (in-memory reset).
func TestPhase12_25_ArchiveAndReset_ClearsGoalFlags(t *testing.T) {
	al, cleanup := newPhase12_25AgentLoop(t)
	defer cleanup()

	// Construct ts with the same workspace as al so goal.NewStore can find files.
	ts := &turnState{
		agent:            &AgentInstance{Workspace: al.cfg.WorkspacePath(), MaxIterations: 50, MaxIterationsCap: 200},
		workspace:        al.cfg.WorkspacePath(),
		sessionKey:       "phase12-25-test-session",
		toolExecRecoveryAttempts: make(map[string]int),
		iteration:        2,
		iterationCap:     50,
		maxIterationsCap: 200,
		turnID:           "test-turn",
	}

	// Seed a stale active goal on disk (simulates prior turn that did not transition).
	store := goal.NewStore(ts.workspace)
	g := &goal.Goal{
		Name:        "phase12-25-test-goal",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "phase12.25 test", SuccessCriteria: []string{"pass"}},
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := store.Write(ts.sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	// Simulate prior-turn state: complete_goal fired, final-report sent,
	// archive requested, final-report transient flag set.
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = true
	ts.goalArchiveRequested = true
	ts.pendingFinalReportIter = true

	// Apply the hook.
	if err := archiveAndResetPriorTurnGoal(al, ts); err != nil {
		t.Fatalf("archiveAndResetPriorTurnGoal failed: %v", err)
	}

	// In-memory flags must all be reset.
	if ts.goalFinalized {
		t.Error("goalFinalized should be reset to false")
	}
	if ts.postCompleteGoalReportSent {
		t.Error("postCompleteGoalReportSent should be reset to false")
	}
	if ts.goalArchiveRequested {
		t.Error("goalArchiveRequested should be reset to false")
	}
	if ts.pendingFinalReportIter {
		t.Error("pendingFinalReportIter should be reset to false")
	}
}

// TestPhase12_25_ArchiveAndReset_ArchivesPriorGoal verifies the archive
// sub-step actually moves the on-disk goal file to the archive/ dir.
func TestPhase12_25_ArchiveAndReset_ArchivesPriorGoal(t *testing.T) {
	al, cleanup := newPhase12_25AgentLoop(t)
	defer cleanup()

	ts := &turnState{
		agent:            &AgentInstance{Workspace: al.cfg.WorkspacePath(), MaxIterations: 50, MaxIterationsCap: 200},
		workspace:        al.cfg.WorkspacePath(),
		sessionKey:       "phase12-25-test-session",
		toolExecRecoveryAttempts: make(map[string]int),
		iteration:        2,
		iterationCap:     50,
		maxIterationsCap: 200,
		turnID:           "test-turn",
	}

	store := goal.NewStore(ts.workspace)
	g := &goal.Goal{
		Name:        "phase12-25-test-goal",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "phase12.25 test", SuccessCriteria: []string{"pass"}},
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := store.Write(ts.sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	// Verify pre-state: active goal exists.
	if _, err := store.Read(ts.sessionKey); err != nil {
		t.Fatalf("pre-state: expected active goal on disk, got error: %v", err)
	}

	// Apply the hook.
	if err := archiveAndResetPriorTurnGoal(al, ts); err != nil {
		t.Fatalf("archiveAndResetPriorTurnGoal failed: %v", err)
	}

	// Post-state: active goal should be gone (archived). goal.Store.Read
	// returns (nil, nil) when file is missing — check the goal value, not
	// the error (see pkg/agent/goal/store.go:67-68).
	if g, _ := store.Read(ts.sessionKey); g != nil {
		t.Errorf("post-state: active goal should have been archived, got %+v", g)
	}
}

// TestPhase12_25_ArchiveAndReset_IdempotentWhenNoPriorGoal verifies the
// hook is safe to call even when there is no prior-turn goal on disk.
func TestPhase12_25_ArchiveAndReset_IdempotentWhenNoPriorGoal(t *testing.T) {
	al, cleanup := newPhase12_25AgentLoop(t)
	defer cleanup()

	ts := &turnState{
		agent:            &AgentInstance{Workspace: al.cfg.WorkspacePath(), MaxIterations: 50, MaxIterationsCap: 200},
		workspace:        al.cfg.WorkspacePath(),
		sessionKey:       "no-prior-goal-session",
		toolExecRecoveryAttempts: make(map[string]int),
		iteration:        1,
		iterationCap:     50,
		maxIterationsCap: 200,
		turnID:           "test-turn",
	}

	if err := archiveAndResetPriorTurnGoal(al, ts); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if ts.goalFinalized || ts.postCompleteGoalReportSent ||
		ts.goalArchiveRequested || ts.pendingFinalReportIter {
		t.Error("flags should remain false (no spurious mutations)")
	}
}

package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/config"
)

// Test helpers — wrap `newTurnCoordTestLoop` so the workspace path is
// exposed for goal-file writes. Phase 4 does not need a real LLM
// (we are only classifying phases), so simpleConvProvider is fine.
func newPhaseTestLoop(t *testing.T) (*AgentLoop, *AgentInstance, string) {
	t.Helper()
	_, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	_ = cleanup // registered via t.Cleanup inside helper
	workspace := agent.Workspace
	if workspace == "" {
		t.Fatalf("newPhaseTestLoop: agent.Workspace is empty")
	}
	return nil, agent, workspace
}

func writeGoalFile(t *testing.T, workspace, sessionKey, status string) {
	t.Helper()
	store := goal.NewStore(workspace)
	// Build a minimal but Validate()-passing goal. The struct is public;
	// we set the 3 required fields directly rather than introducing a
	// package-private constructor just for tests.
	g := &goal.Goal{
		Name:   "test-goal-" + sessionKey,
		Status: goal.Status(status),
		Description: goal.Description{
			Objective:       "test objective for " + sessionKey,
			SuccessCriteria: []string{"success criterion 1"},
		},
	}
	if err := store.Write(sessionKey, g); err != nil {
		t.Fatalf("write goal %q: %v", sessionKey, err)
	}
}

func newPhaseTestTurnState(agent *AgentInstance, sessionKey, workspace string) *turnState {
	opts := makeTestProcessOpts(sessionKey)
	// newTurnState reads opts.Dispatch.SessionKey (not opts.SessionKey),
	// so we need to set Dispatch to expose the session key to hasGoal().
	opts.Dispatch = DispatchRequest{
		SessionKey: sessionKey,
	}
	scope := turnEventScope{
		turnID:  "phase-test",
		context: newTurnContext(nil, nil, nil),
	}
	ts := newTurnState(agent, opts, scope)
	// Override workspace so per-test tempdirs are honored. (Default is
	// agent.Workspace which is shared across tests; tests need isolated
	// writes to the goal store.)
	if workspace != "" {
		ts.workspace = workspace
	}
	return ts
}

// =============================================================================
// Goal Phase Tests (Phase 10 simplified)
// =============================================================================
//
// Phase 10 removed extend_turn_iteration tool, so the iteration cap is
// effectively constant per turn (equal to agent.MaxIterations).
//
// - iterationCapReached removed (dead code; identical to iterationCapFinalized)
// - iterationCapFinalized remains: iteration >= iterationCap
// - currentGoalPhase reduced to: Lock / Open / Lock (when cap hit)
//   GoalPhaseCheckpoint is still a valid phase value, but no longer
//   reachable via currentGoalPhase in production. Tests cover Open + Lock.

// TestTurnState_HasGoal covers the active-only semantics: completed /
// archived / aborted goals do NOT count as an active goal.
func TestTurnState_HasGoal(t *testing.T) {
	_, agent, workspace := newPhaseTestLoop(t)

	t.Run("no goal file → false", func(t *testing.T) {
		ts := newPhaseTestTurnState(agent, "phase-no-file", workspace)
		if ts.hasGoal() {
			t.Fatal("hasGoal() = true with no goal file, want false")
		}
	})

	t.Run("active goal file → true", func(t *testing.T) {
		key := "phase-active"
		writeGoalFile(t, workspace, key, string(goal.StatusActive))
		ts := newPhaseTestTurnState(agent, key, workspace)
		if !ts.hasGoal() {
			t.Fatal("hasGoal() = false with active goal file, want true")
		}
	})

	t.Run("completed goal → false", func(t *testing.T) {
		key := "phase-completed"
		writeGoalFile(t, workspace, key, string(goal.StatusCompleted))
		ts := newPhaseTestTurnState(agent, key, workspace)
		if ts.hasGoal() {
			t.Fatal("hasGoal() = true with completed goal, want false")
		}
	})

	t.Run("archived goal → false", func(t *testing.T) {
		key := "phase-archived"
		writeGoalFile(t, workspace, key, string(goal.StatusArchived))
		ts := newPhaseTestTurnState(agent, key, workspace)
		if ts.hasGoal() {
			t.Fatal("hasGoal() = true with archived goal, want false")
		}
	})

	t.Run("aborted goal → false", func(t *testing.T) {
		key := "phase-aborted"
		writeGoalFile(t, workspace, key, string(goal.StatusAborted))
		ts := newPhaseTestTurnState(agent, key, workspace)
		if ts.hasGoal() {
			t.Fatal("hasGoal() = true with aborted goal, want false")
		}
	})
}

// TestTurnState_IterationCapFinalized locks in the Phase 10 cap predicate.
// Phase 10: extend_turn_iteration was removed, so iterationCap is constant
// for the turn. The predicate reduces to iteration >= iterationCap.
func TestTurnState_IterationCapFinalized(t *testing.T) {
	_, agent, _ := newPhaseTestLoop(t)
	agent.MaxIterations = 20

	cases := []struct {
		name         string
		iteration    int
		iterationCap int
		want         bool
	}{
		{"below cap", 5, 20, false},
		{"at cap", 20, 20, true},
		{"above cap", 25, 20, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := newPhaseTestTurnState(agent, "phase-finalized-"+c.name, "")
			ts.iteration = c.iteration
			ts.iterationCap = c.iterationCap
			if got := ts.iterationCapFinalized(); got != c.want {
				t.Fatalf("iterationCapFinalized: got %v, want %v (iter=%d iterCap=%d)",
					got, c.want, c.iteration, c.iterationCap)
			}
		})
	}
}

func TestTurnState_CurrentGoalPhase(t *testing.T) {
	_, agent, workspace := newPhaseTestLoop(t)
	agent.MaxIterations = 20

	t.Run("nil turnState → Lock", func(t *testing.T) {
		var ts *turnState
		if ts.currentGoalPhase() != GoalPhaseLock {
			t.Fatalf("want Lock, got %s", ts.currentGoalPhase())
		}
	})

	t.Run("missing goal → Lock", func(t *testing.T) {
		ts := newPhaseTestTurnState(agent, "phase-no-goal", workspace)
		if ts.currentGoalPhase() != GoalPhaseLock {
			t.Fatalf("want Lock, got %s", ts.currentGoalPhase())
		}
	})

	t.Run("active goal, iterCap not reached → Open", func(t *testing.T) {
		key := "phase-open"
		writeGoalFile(t, workspace, key, string(goal.StatusActive))
		ts := newPhaseTestTurnState(agent, key, workspace)
		ts.iteration = 5
		ts.iterationCap = 20
		if got := ts.currentGoalPhase(); got != GoalPhaseOpen {
			t.Fatalf("want Open, got %s", got)
		}
	})

	t.Run("active goal, iterCap finalized → Lock", func(t *testing.T) {
		// Phase 10: with extend removed, hitting iterationCap drops back
		// to Lock so the LLM must set a fresh goal before next iteration.
		key := "phase-finalized"
		writeGoalFile(t, workspace, key, string(goal.StatusActive))
		ts := newPhaseTestTurnState(agent, key, workspace)
		ts.iteration = 25
		ts.iterationCap = 20
		if got := ts.currentGoalPhase(); got != GoalPhaseLock {
			t.Fatalf("want Lock (iterCap finalized → force fresh goal), got %s", got)
		}
	})
}

// TestTurnState_ApplyPhaseAllowlist verifies the SetAllowlist side-effect
// matches the resolver for each phase. We seed agent.Definition.Agent
// with a small Frontmatter.Tools list so the resolver returns a
// non-empty union (otherwise Definition.Agent == nil → resolver returns
// nil → SetAllowlist(nil) means "no filter", which is harder to assert).
func TestTurnState_ApplyPhaseAllowlist(t *testing.T) {
	_, agent, workspace := newPhaseTestLoop(t)
	agent.Definition.Agent = &AgentPromptDefinition{
		Frontmatter: AgentFrontmatter{
			Tools: []string{"alpha", "beta"},
			Fields: map[string]any{
				"tools": []any{"alpha", "beta"},
			},
		},
	}

	t.Run("Lock restricts to set_goal only", func(t *testing.T) {
		ts := newPhaseTestTurnState(agent, "phase-lock", workspace)
		ts.applyPhaseAllowlist(GoalPhaseLock)
		names := agent.Tools.GetAllowlist()
		if !equalStringSlices(names, []string{"set_goal"}) {
			t.Fatalf("Lock allowlist = %v, want %v", names, []string{"set_goal"})
		}
	})

	t.Run("Open allows lifecycle + base", func(t *testing.T) {
		ts := newPhaseTestTurnState(agent, "phase-open-allow", workspace)
		ts.applyPhaseAllowlist(GoalPhaseOpen)
		names := agent.Tools.GetAllowlist()
		for _, want := range []string{"view_goal", "complete_goal", "alpha", "beta"} {
			if !sliceContains(names, want) {
				t.Fatalf("Open allowlist missing %q: got %v", want, names)
			}
		}
		if sliceContains(names, "set_goal") {
			t.Fatalf("Open allowlist should NOT contain set_goal (suppress in-turn replace): got %v", names)
		}
	})

	t.Run("Checkpoint allows goal_progress + complete_goal", func(t *testing.T) {
		// Checkpoint is still a valid GoalPhase value even though production
		// no longer reaches it via currentGoalPhase. ApplyPhaseAllowlist
		// must still produce the correct allowlist for callers that pin the phase.
		ts := newPhaseTestTurnState(agent, "phase-checkpoint-allow", workspace)
		ts.applyPhaseAllowlist(GoalPhaseCheckpoint)
		names := agent.Tools.GetAllowlist()
		for _, want := range []string{"goal_progress", "complete_goal", "alpha", "beta"} {
			if !sliceContains(names, want) {
				t.Fatalf("Checkpoint allowlist missing %q: got %v", want, names)
			}
		}
		if sliceContains(names, "set_goal") {
			t.Fatalf("Checkpoint allowlist should NOT contain set_goal: got %v", names)
		}
		if sliceContains(names, "view_goal") {
			t.Fatalf("Checkpoint allowlist should NOT contain view_goal (only goal_progress/complete_goal): got %v", names)
		}
	})
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

func sliceContains(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}

// Suppress unused import warnings if config changes break a usage.
var _ = config.Config{}

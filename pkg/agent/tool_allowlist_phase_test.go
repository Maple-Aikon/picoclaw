package agent

import (
	"reflect"
	"testing"
)

// TestResolveAgentToolAllowlistWithPhase_AllPhases walks the 3-phase matrix
// produced by plan §3.5 and verifies that resolveAgentToolAllowlistWithPhase
// returns the exact expected allowlist.
//
// Definitions used:
//   - base = ["read_file", "write_file"] (typical frontmatter frontmatter tools)
//   - phase Lock       → ["set_goal"] (overrides base entirely)
//   - phase Open       → sorted(base ∪ ["view_goal", "complete_goal"])
//   - phase Checkpoint → sorted(base ∪ ["goal_progress", "complete_goal"])
func TestResolveAgentToolAllowlistWithPhase_AllPhases(t *testing.T) {
	def := AgentContextDefinition{
		Agent: &AgentPromptDefinition{
			Frontmatter: AgentFrontmatter{
				Fields: map[string]any{"tools": true}, // mark tools declared
				Tools:  []string{"read_file", "write_file"},
			},
		},
	}

	cases := []struct {
		name  string
		phase GoalPhase
		want  []string
	}{
		{"Lock overrides base", GoalPhaseLock, []string{"set_goal"}},
		{"Open unions view_goal+complete_goal", GoalPhaseOpen,
			[]string{"complete_goal", "read_file", "view_goal", "write_file"}},
		{"Checkpoint unions goal_progress+complete_goal", GoalPhaseCheckpoint,
			[]string{"complete_goal", "goal_progress", "read_file", "write_file"}},
		{"Unknown phase degrades to base only", GoalPhase("gibberish"),
			[]string{"read_file", "write_file"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveAgentToolAllowlistWithPhase(def, c.phase)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("phase=%s:\n got  %v\n want %v", c.phase, got, c.want)
			}
		})
	}
}

// TestResolveAgentToolAllowlistWithPhase_EmptyBase checks the empty-tools
// edge case under each phase:
//   - Lock: still ["set_goal"] (phase wins)
//   - Open / Checkpoint: just the lifecycle union (no base)
//   - Unknown: [] (empty)
func TestResolveAgentToolAllowlistWithPhase_EmptyBase(t *testing.T) {
	def := AgentContextDefinition{
		Agent: &AgentPromptDefinition{
			Frontmatter: AgentFrontmatter{
				Fields: map[string]any{"tools": true},
				Tools:  []string{},
			},
		},
	}

	cases := []struct {
		phase GoalPhase
		want  []string
	}{
		{GoalPhaseLock, []string{"set_goal"}},
		{GoalPhaseOpen, []string{"complete_goal", "view_goal"}},
		{GoalPhaseCheckpoint, []string{"complete_goal", "goal_progress"}},
		{GoalPhase("other"), []string{}},
	}
	for _, c := range cases {
		t.Run(string(c.phase), func(t *testing.T) {
			got := resolveAgentToolAllowlistWithPhase(def, c.phase)
			if !reflect.DeepEqual(got, c.want) && (len(got) != 0 || len(c.want) != 0) {
				t.Fatalf("phase=%s:\n got  %v\n want %v", c.phase, got, c.want)
			}
		})
	}
}

// TestResolveAgentToolAllowlistWithPhase_FrontmatterFailure checks the
// fail-closed behavior: any phase returns empty allowlist when frontmatter
// parse fails. Preserves the pre-Phase-3 invariant regardless of phase.
func TestResolveAgentToolAllowlistWithPhase_FrontmatterFailure(t *testing.T) {
	def := AgentContextDefinition{
		Agent: &AgentPromptDefinition{
			RawFrontmatter: "tools: [",
			FrontmatterErr: "yaml: line 1: did not find expected",
			Frontmatter:    AgentFrontmatter{},
		},
	}
	for _, phase := range []GoalPhase{GoalPhaseLock, GoalPhaseOpen, GoalPhaseCheckpoint} {
		t.Run(string(phase), func(t *testing.T) {
			got := resolveAgentToolAllowlistWithPhase(def, phase)
			if len(got) != 0 {
				t.Fatalf("phase=%s: expected empty allowlist on frontmatter parse failure, got %v", phase, got)
			}
		})
	}
}

// TestResolveAgentToolAllowlistWithPhase_PreservesBackCompat confirms the
// 1-arg wrapper still returns the BASE-ONLY allowlist (pre-Phase-3 semantics),
// not the phase-augmented union. This is what unlocks
// TestResolveAgentToolAllowlistDistinguishesMissingAndEmptyToolsField to
// stay green and is the documented Phase-3 back-compat contract.
func TestResolveAgentToolAllowlistWithPhase_PreservesBackCompat(t *testing.T) {
	def := AgentContextDefinition{
		Agent: &AgentPromptDefinition{
			Frontmatter: AgentFrontmatter{
				Fields: map[string]any{"tools": true},
				Tools:  []string{"read_file", "write_file"},
			},
		},
	}
	got := resolveAgentToolAllowlist(def)
	want := []string{"read_file", "write_file"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("1-arg wrapper should be base-only:\n got  %v\n want %v", got, want)
	}

	// Empty-base: 1-arg wrapper must still return [] (NOT [view_goal, complete_goal]).
	emptyDef := AgentContextDefinition{
		Agent: &AgentPromptDefinition{
			Frontmatter: AgentFrontmatter{
				Fields: map[string]any{"tools": true},
				Tools:  []string{},
			},
		},
	}
	gotEmpty := resolveAgentToolAllowlist(emptyDef)
	if len(gotEmpty) != 0 {
		t.Fatalf("1-arg wrapper on empty base should be empty allowlist, got %v", gotEmpty)
	}
}

// TestUnionAllowlist covers the union helper semantics:
func TestUnionAllowlist(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want []string
	}{
		{"both empty", nil, nil, nil},
		{"a only", []string{"x"}, nil, []string{"x"}},
		{"b only", nil, []string{"y"}, []string{"y"}},
		{"disjoint", []string{"a", "b"}, []string{"c"}, []string{"a", "b", "c"}},
		{"overlap dedup", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{"empty strings dropped", []string{"", "a"}, []string{"b"}, []string{"a", "b"}},
		{"result stable across order",
			[]string{"z", "y", "x"}, []string{"y", "z"},
			[]string{"x", "y", "z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := unionAllowlist(c.a, c.b)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("\n got  %v\n want %v", got, c.want)
			}
		})
	}
}

// TestCurrentGoalPhase_NoActiveGoal covers the GoalPhaseLock fallback when
// no goal is set for the current session. We point currentGoalPhase at a
// fresh workspace with no goal file and verify it returns Lock regardless
// of iteration.
func TestCurrentGoalPhase_NoActiveGoal(t *testing.T) {
	workspace := tempWorkspaceLocal(t)
	defer cleanupWorkspace(t, workspace)

	got := currentGoalPhase(workspace, "session-A", 0, 100)
	if got != GoalPhaseLock {
		t.Fatalf("no-goal workspace should yield Lock, got %s", got)
	}
	got = currentGoalPhase(workspace, "session-A", 99, 100)
	if got != GoalPhaseLock {
		t.Fatalf("no-goal workspace at iteration 99 should still be Lock, got %s", got)
	}
}

// TestCurrentGoalPhase_EmptyArgs covers the fail-closed defaults when
// workspace or sessionKey is empty.
func TestCurrentGoalPhase_EmptyArgs(t *testing.T) {
	if got := currentGoalPhase("", "sess", 0, 0); got != defaultGoalPhase {
		t.Fatalf("empty workspace should return defaultGoalPhase=%s, got %s", defaultGoalPhase, got)
	}
	if got := currentGoalPhase("/any", "", 0, 0); got != defaultGoalPhase {
		t.Fatalf("empty session should return defaultGoalPhase=%s, got %s", defaultGoalPhase, got)
	}
}

// tempWorkspaceLocal generates a unique ephemeral directory for tests that
// require a goal.Store workspace. Mirrors pkg/agent/goal/tools_test.go's
// helper but scoped to this package.
func tempWorkspaceLocal(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// downstream consumers (YAML frontmatter, MCP responses) can rely on it.
func TestGoalPhase_StringValues(t *testing.T) {
	cases := []struct {
		p    GoalPhase
		want string
	}{
		{GoalPhaseLock, "lock"},
		{GoalPhaseOpen, "open"},
		{GoalPhaseCheckpoint, "checkpoint"},
	}
	for _, c := range cases {
		if string(c.p) != c.want {
			t.Errorf("%v: got %q want %q", c.p, string(c.p), c.want)
		}
	}
}

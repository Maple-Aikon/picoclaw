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
		{"Checkpoint is absolute (overrides base)", GoalPhaseCheckpoint,
			[]string{"goal_progress", "complete_goal"}},
		{"Final pins complete_goal only", GoalPhaseFinal,
			[]string{"complete_goal"}},
		{"PostFinal is empty non-nil (no tools at all)", GoalPhasePostFinal,
			[]string{}},
		{"Unknown phase degrades to base only", GoalPhase("gibberish"),
			[]string{"read_file", "write_file"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveAgentToolAllowlistWithPhase(def, c.phase)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("phase=%s:\n got  %v\n want %v", c.phase, got, c.want)
			}
			if c.phase == GoalPhasePostFinal && got == nil {
				t.Fatalf("PostFinal must return non-nil empty slice (R1), got nil")
			}
		})
	}
}

// TestResolveAgentToolAllowlistWithPhase_EmptyBase checks the empty-tools
// edge case under each phase:
//   - Lock: still ["set_goal"] (phase wins)
//   - Open: still returns Open-specific union (base was already empty)
//   - Checkpoint: just the lifecycle tools (phase is absolute — Phase 12.14)
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
		{GoalPhaseCheckpoint, []string{"goal_progress", "complete_goal"}},
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

	got := currentGoalPhase(workspace, "session-A", 0, 100, 200)
	if got != GoalPhaseLock {
		t.Fatalf("no-goal workspace should yield Lock, got %s", got)
	}
	got = currentGoalPhase(workspace, "session-A", 99, 100, 200)
	if got != GoalPhaseLock {
		t.Fatalf("no-goal workspace at iteration 99 should still be Lock, got %s", got)
	}
}

// TestCurrentGoalPhase_EmptyArgs covers the fail-closed defaults when
// workspace or sessionKey is empty.
func TestCurrentGoalPhase_EmptyArgs(t *testing.T) {
	if got := currentGoalPhase("", "sess", 0, 0, 0); got != defaultGoalPhase {
		t.Fatalf("empty workspace should return defaultGoalPhase=%s, got %s", defaultGoalPhase, got)
	}
	if got := currentGoalPhase("/any", "", 0, 0, 0); got != defaultGoalPhase {
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
// TestGoalPhase_StringValues verifies the wire-stable string values of
// each goal phase constant. Downstream consumers (YAML frontmatter, MCP
// responses, allowlist routers) can rely on the value being stable
// across versions.
//
// Phase 11: GoalPhaseLock is now an alias of GoalPhaseSet (both "set").
// New constants GoalPhaseSet + GoalPhaseFinal are added.
func TestGoalPhase_StringValues(t *testing.T) {
	cases := []struct {
		p    GoalPhase
		want string
	}{
		{GoalPhaseSet, "set"},
		{GoalPhaseLock, "set"}, // alias of GoalPhaseSet
		{GoalPhaseOpen, "open"},
		{GoalPhaseCheckpoint, "checkpoint"},
		{GoalPhaseFinal, "final"},
	}
	for _, c := range cases {
		if string(c.p) != c.want {
			t.Errorf("%v: got %q want %q", c.p, string(c.p), c.want)
		}
	}
}

// TestResolveAgentToolAllowlistWithPhase_NoToolsField_PreservesLifecycleOverride
// is a Phase 12.3 + 12.14 regression test.
//
// Bug (Phase 12.3): when an agent's frontmatter omits the `tools:` field
// entirely (the default for most agents — tool lists come from MCP/built-in
// registries), `resolveAgentToolAllowlistWithPhase(def, GoalPhaseSet)`
// returned nil. That nil was passed to SetAllowlist, which cleared the
// registry's allowlist, exposing ALL 84 registered tools to the LLM
// at iter 1 — defeating the Phase 11 "iter 1 = set_goal only" contract.
//
// Bug (Phase 12.14, observed live 2026-07-25 14:54 ICT on goal
// `crg-update-latest`, session `sk_v1_9238bf3573...`): the same nil
// allowlist pattern applied to GoalPhaseCheckpoint, because the
// "missing base tools → return nil" branch sat BELOW the phase
// override switch. When an agent without `tools:` reached iter 25
// (cap-hit), the Checkpoint phase was supposed to expose only
// `[goal_progress, complete_goal]` — but it instead exposed all 85
// registered tools, because nil allowlist means "no filter". The LLM
// kept emitting `exec` tool calls (MiniMax-M3 streaming quirk produced
// empty args, but the parser still counted `HasToolCalls=1` so
// recovery never fired). Turn ended on toolLimitResponse fallback.
//
// Fix: phase override cases (Set, Final, **Checkpoint**) now short-circuit
// BEFORE the "missing tools:" base check, so lifecycle tools always surface
// regardless of base frontmatter. Open still returns nil for missing
// `tools:` because Open legitimately needs all registered tools visible.
func TestResolveAgentToolAllowlistWithPhase_NoToolsField_PreservesLifecycleOverride(t *testing.T) {
	// Agent with no `tools:` field declared — typical MCP-only agent.
	def := AgentContextDefinition{
		Agent: &AgentPromptDefinition{
			Frontmatter: AgentFrontmatter{
				Fields: map[string]any{}, // no `tools:` key
				Tools:  nil,
			},
		},
	}

	t.Run("set phase must surface set_goal even without base", func(t *testing.T) {
		got := resolveAgentToolAllowlistWithPhase(def, GoalPhaseSet)
		want := []string{"set_goal"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GoalPhaseSet: got %v, want %v", got, want)
		}
	})

	t.Run("final phase must surface complete_goal even without base", func(t *testing.T) {
		got := resolveAgentToolAllowlistWithPhase(def, GoalPhaseFinal)
		want := []string{"complete_goal"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GoalPhaseFinal: got %v, want %v", got, want)
		}
	})

	t.Run("open phase returns nil for no-tools (matches base resolver)", func(t *testing.T) {
		// Backward-compat: agents without `tools:` declared have all
		// tools available during Open (nil allowlist = no filter).
		got := resolveAgentToolAllowlistWithPhase(def, GoalPhaseOpen)
		if got != nil {
			t.Errorf("GoalPhaseOpen: got %v, want nil (allow-all)", got)
		}
	})

	t.Run("checkpoint phase returns lifecycle tools even without base", func(t *testing.T) {
		// Phase 12.14 fix: Checkpoint is now an absolute shortcut,
		// matching Set/Final. Iter-cap-hit means the LLM must choose
		// between goal_progress (extend) or complete_goal (finalize).
		// Exposing base tools at this phase leads to the
		// toolLimitResponse fallback observed in turn main-turn-2 on
		// 2026-07-25 12:54 ICT — LLM kept calling exec until the cap.
		got := resolveAgentToolAllowlistWithPhase(def, GoalPhaseCheckpoint)
		want := []string{"goal_progress", "complete_goal"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("GoalPhaseCheckpoint: got %v, want %v", got, want)
		}
	})
}

// --- Phase 12.30 tests ---

// TestResolveGoalPhase_FinalUsesIterIndexNotCap covers the bug fixed in
// Phase 12.30. Previously, the FINAL-phase predicate compared
// `iterationCap >= maxIterationsCap` — but iterationCap is the user-
// config cap (mutable via goal_progress self-extend). When user set
// max_tool_iterations=25 > max_iterations_cap=15, FINAL fired from
// iter 1 because iterationCap (25) already exceeded maxIterationsCap
// (15). The fix compares `iter >= maxIterationsCap` so FINAL only
// fires when the iteration index actually reaches the absolute
// ceiling.
//
// All four scenarios from the original bug report (Horus Protocol
// 2026-07-30):
//   1. iter < iterationCap < maxIterationsCap → GOAL-OPEN
//   2. iter == iterationCap (within maxIterationsCap) → GOAL-CHECKPOINT
//   3. iter == iterationCap == maxIterationsCap → GOAL-CHECKPOINT
//      (NOT FINAL — cap variable equality alone is not enough)
//   4. iter >= maxIterationsCap → GOAL-FINAL
func TestResolveGoalPhase_FinalUsesIterIndexNotCap(t *testing.T) {
	cases := []struct {
		name             string
		hasActiveGoal    bool
		iter             int
		iterationCap     int
		maxIterationsCap int
		goalFinalized    bool
		want             GoalPhase
	}{
		// Pin rule: iter <= 1 always → SET, regardless of cap values.
		{
			name:             "iter=1 pin rule wins over cap mismatch",
			hasActiveGoal:    true,
			iter:             1,
			iterationCap:     25,
			maxIterationsCap: 15,
			goalFinalized:    false,
			want:             GoalPhaseSet,
		},
		// Open-phase: iter well below both caps.
		{
			name:             "iter=3 cap=15 ceiling=15 → OPEN",
			hasActiveGoal:    true,
			iter:             3,
			iterationCap:     15,
			maxIterationsCap: 15,
			goalFinalized:    false,
			want:             GoalPhaseOpen,
		},
		// Checkpoint: iter hits the per-turn cap, but iter is still
		// below the absolute ceiling.
		{
			name:             "iter=5 cap=5 ceiling=15 → CHECKPOINT",
			hasActiveGoal:    true,
			iter:             5,
			iterationCap:     5,
			maxIterationsCap: 15,
			goalFinalized:    false,
			want:             GoalPhaseCheckpoint,
		},
		// Original Horus bug: iterationCap clamped to maxIterationsCap
		// via goal_progress self-extend. Old code returned FINAL here
		// because iterationCap (15) >= maxIterationsCap (15). New code
		// compares iter (10) against ceiling (15) — not yet FINAL, but
		// iter (10) >= iterationCap (10) → CHECKPOINT.
		{
			name:             "iter=10 cap=10 ceiling=15 → CHECKPOINT (Phase 12.30 fix)",
			hasActiveGoal:    true,
			iter:             10,
			iterationCap:     10,
			maxIterationsCap: 15,
			goalFinalized:    false,
			want:             GoalPhaseCheckpoint,
		},
		// Second original bug: user config has
		// max_tool_iterations (25) > max_iterations_cap (15). Old code
		// returned FINAL because iterationCap (25) >= ceiling (15) at
		// iter 1. New code returns CHECKPOINT at iter 24 (still below
		// iter ceiling 15? No, 24 >= 15) → FINAL. The right answer is
		// FINAl because iter (24) >= ceiling (15).
		{
			name:             "iter=24 cap=25 ceiling=15 → FINAL",
			hasActiveGoal:    true,
			iter:             24,
			iterationCap:     25,
			maxIterationsCap: 15,
			goalFinalized:    false,
			want:             GoalPhaseFinal,
		},
		// Iter index at the ceiling.
		{
			name:             "iter=15 cap=15 ceiling=15 → FINAL",
			hasActiveGoal:    true,
			iter:             15,
			iterationCap:     15,
			maxIterationsCap: 15,
			goalFinalized:    false,
			want:             GoalPhaseFinal,
		},
		// goalFinalized flag overrides everything else.
		{
			name:             "goalFinalized=true → FINAL even at iter=1",
			hasActiveGoal:    true,
			iter:             1,
			iterationCap:     25,
			maxIterationsCap: 15,
			goalFinalized:    true,
			want:             GoalPhaseFinal,
		},
		// hasActiveGoal=false pin rule (even at iter=5).
		{
			name:             "no active goal → SET regardless of iter",
			hasActiveGoal:    false,
			iter:             5,
			iterationCap:     5,
			maxIterationsCap: 15,
			goalFinalized:    false,
			want:             GoalPhaseSet,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveGoalPhase(c.hasActiveGoal, c.iter, c.iterationCap, c.maxIterationsCap, c.goalFinalized)
			if got != c.want {
				t.Errorf("ResolveGoalPhase(%v, iter=%d, iterCap=%d, maxCap=%d, finalized=%v) = %s, want %s",
					c.hasActiveGoal, c.iter, c.iterationCap, c.maxIterationsCap, c.goalFinalized, got, c.want)
			}
		})
	}
}

// TestResolveGoalPhase_MaxIterationsCapZero verifies that
// maxIterationsCap=0 disables the FINAL-phase cap. This is the
// "extensions disabled" config — the agent should never hit FINAL via
// the ceiling predicate.
func TestResolveGoalPhase_MaxIterationsCapZero(t *testing.T) {
	// maxIterationsCap=0 → ceiling check is disabled → at iter=100 with
	// iterationCap=100, Open phase should still hold (since iter <
	// iterationCap).
	got := ResolveGoalPhase(true, 100, 100, 0, false)
	if got != GoalPhaseCheckpoint {
		t.Errorf("with ceiling=0: got %s, want %s", got, GoalPhaseCheckpoint)
	}

	// Even at iter=1000, ceiling=0 means no FINAL via cap.
	got = ResolveGoalPhase(true, 1000, 1000, 0, false)
	if got != GoalPhaseCheckpoint {
		t.Errorf("with ceiling=0 at high iter: got %s, want %s", got, GoalPhaseCheckpoint)
	}
}

// TestResolveGoalPhase_PinRuleStrict verifies the iter<=1 pin rule
// from Phase 12.15.7 — even with an active goal file present, iter=1
// always classifies as SET to prevent the LLM from bypassing
// set_goal.
func TestResolveGoalPhase_PinRuleStrict(t *testing.T) {
	// Both hasActiveGoal=true and iter=1 → SET (pin wins).
	if got := ResolveGoalPhase(true, 1, 25, 15, false); got != GoalPhaseSet {
		t.Errorf("hasActiveGoal=true iter=1: got %s, want %s", got, GoalPhaseSet)
	}
	// hasActiveGoal=false also → SET.
	if got := ResolveGoalPhase(false, 1, 25, 15, false); got != GoalPhaseSet {
		t.Errorf("hasActiveGoal=false iter=1: got %s, want %s", got, GoalPhaseSet)
	}
	// iter=2 with active goal → OPEN (pin rule no longer applies).
	if got := ResolveGoalPhase(true, 2, 25, 15, false); got != GoalPhaseOpen {
		t.Errorf("hasActiveGoal=true iter=2: got %s, want %s", got, GoalPhaseOpen)
	}
}

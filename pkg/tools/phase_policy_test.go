// Schema lock test (Plan §4.20 L2): the policy table is the single source of
// truth for per-phase dispatcher behavior in pkg/tools. If any of the 5
// canonical phases is missing a row OR a phase token typo slipped in, this
// test fails — preventing silent drift between plan §3.1 and the actual
// runtime table.
//
// 3-set equality:
//   1. Keys of toolPolicies == phases.AllTokens()
//   2. Each row's LifecycleAllowed map has exactly 4 keys (set_goal, view_goal,
//      goal_progress, complete_goal) — fail-closed missing-key rule
//   3. Each row's Phase field matches the map key (defense against typo where
//      row.Phase="open" but map key="set")
//
// If you intentionally add/remove a phase, update pkg/phases/phases.go first
// (which controls AllTokens), then update toolPolicies here.
package tools

import (
	"sort"
	"testing"

	"github.com/sipeed/picoclaw/pkg/phases"
)

// canonicalLifecycleTools is the 4-tool lifecycle enum — the only tools
// that get the per-phase LifecycleAllowed gate. Kept in sync with
// pkg/tools/lifecycle_tool_gate.go lifecycleToolNames.
var canonicalLifecycleTools = []string{
	"set_goal",
	"view_goal",
	"goal_progress",
	"complete_goal",
}

func TestPhasePolicy_SchemaLock_KeySetMatchesPhases(t *testing.T) {
	want := phases.AllTokens()
	got := make([]string, 0, len(toolPolicies))
	for k := range toolPolicies {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("policy table has %d rows, want %d (%v)", len(got), len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("policy[%d]=%q want %q (table drift)", i, got[i], want[i])
		}
	}
}

func TestPhasePolicy_SchemaLock_EveryRowPhaseMatchesKey(t *testing.T) {
	for key, row := range toolPolicies {
		if row == nil {
			t.Fatalf("toolPolicies[%q] is nil", key)
		}
		if row.Phase != key {
			t.Fatalf("toolPolicies[%q].Phase=%q mismatch (typo in row)", key, row.Phase)
		}
	}
}

func TestPhasePolicy_SchemaLock_LifecycleAllowedHasFourKeys(t *testing.T) {
	want := append([]string{}, canonicalLifecycleTools...)
	sort.Strings(want)
	for key, row := range toolPolicies {
		if row.LifecycleAllowed == nil {
			t.Fatalf("toolPolicies[%q].LifecycleAllowed is nil (must be 4-key map)", key)
		}
		got := make([]string, 0, len(row.LifecycleAllowed))
		for k := range row.LifecycleAllowed {
			got = append(got, k)
		}
		sort.Strings(got)
		if len(got) != len(want) {
			t.Fatalf("toolPolicies[%q].LifecycleAllowed has %d keys, want %d (%v got %v)",
				key, len(got), len(want), want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("toolPolicies[%q].LifecycleAllowed key[%d]=%q want %q",
					key, i, got[i], want[i])
			}
		}
	}
}

func TestToolPolicyForPhase_KnownReturnsPolicy(t *testing.T) {
	for _, tok := range phases.AllTokens() {
		p := ToolPolicyForPhase(tok)
		if p == nil {
			t.Errorf("ToolPolicyForPhase(%q) = nil, want policy", tok)
			continue
		}
		if p.Phase != tok {
			t.Errorf("ToolPolicyForPhase(%q).Phase=%q", tok, p.Phase)
		}
	}
}

func TestToolPolicyForPhase_UnknownReturnsNil(t *testing.T) {
	// Plan §4.20 L3: unknown NON-EMPTY trimmed phase → nil (fail-closed at
	// call sites that read this). Empty string is "no phase set" — also nil
	// for safety. Lookup is intentionally case-insensitive (Phase 12.1 fix
	// regression guard) so "Open", "OPEN", " set" all resolve.
	for _, s := range []string{"", "unknown", "gibberish"} {
		if p := ToolPolicyForPhase(s); p != nil {
			t.Errorf("ToolPolicyForPhase(%q) = %v, want nil (unknown)", s, p)
		}
	}
}

func TestLifecycleAllowedAt_UnknownPhaseReturnsFalse(t *testing.T) {
	// fail-CLOSED: any unknown phase must report false for ALL 4 lifecycle
	// tools. This is the 2nd half of R6-F1 — site 3 (toolAllowedLocked) +
	// site 2 (IsLifecycleToolAllowed) both rely on this gate.
	for _, unknown := range []string{"", "gibberish", "x"} {
		for _, tool := range canonicalLifecycleTools {
			if LifecycleAllowedAt(unknown, tool) {
				t.Errorf("LifecycleAllowedAt(%q, %q) = true, want false (fail-closed)",
					unknown, tool)
			}
		}
	}
}

func TestLifecycleAllowedAt_KnownPhaseReturnsRowValue(t *testing.T) {
	// Spot-check: set_goal is allowed at SET only; goal_progress at CHECKPOINT
	// only; complete_goal allowed at all non-EMPTY non-POST_FINAL phases.
	cases := []struct {
		phase, tool string
		want        bool
	}{
		{phases.PhaseSet, "set_goal", true},
		{phases.PhaseSet, "goal_progress", false},
		{phases.PhaseOpen, "view_goal", true},
		{phases.PhaseOpen, "goal_progress", false},
		{phases.PhaseCheckpoint, "goal_progress", true},
		{phases.PhaseCheckpoint, "set_goal", false},
		{phases.PhaseFinal, "complete_goal", true},
		{phases.PhaseFinal, "set_goal", false},
		{phases.PhasePostFinal, "complete_goal", false},
		{phases.PhasePostFinal, "view_goal", false},
	}
	for _, c := range cases {
		got := LifecycleAllowedAt(c.phase, c.tool)
		if got != c.want {
			t.Errorf("LifecycleAllowedAt(%q, %q) = %v, want %v", c.phase, c.tool, got, c.want)
		}
	}
}

package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/phases"
	"github.com/sipeed/picoclaw/pkg/tools"
)

var _ = os.Getenv

// TestResolveAllowlist_MatchesPolicyTable — T1 schema test. The resolver
// (site 1) MUST return exactly the ToolVisibilityPolicy.Allowlist for
// ABSOLUTE phases and exactly ToolPolicyForPhase.Allowlist for non-base
// RELATIVE phases. This is the only spot where pkg/agent binds to the
// pkg/tools policy row.
//
// Plan §3.1 + §3.3 site 1 contract:
//   - ABSOLUTE row (Set/Final/Checkpoint/PostFinal): return Allowlist as-is.
//   - RELATIVE row (Open): return base ∪ BaseAdds — but the union must
//     dedupe + ToLower+TrimSpace.
func TestResolveAllowlist_MatchesPolicyTable(t *testing.T) {
	// Plan §3.3 site 1 contract: ABSOLUTE phases override base config;
	// RELATIVE phases (Open) return base ∪ BaseAdds ONLY when base is
	// declared. With nil Agent / no `tools:` field, Open returns nil
	// (backward compat: agents rely on registry-wide visibility).
	type tc struct {
		phase    GoalPhase
		wantCols []string // expected allowlist (set equality, order-independent)
	}
	cases := []tc{
		{GoalPhaseSet, []string{"set_goal"}},
		{GoalPhaseFinal, []string{"complete_goal"}},
		{GoalPhaseCheckpoint, []string{"goal_progress", "complete_goal"}},
		{GoalPhasePostFinal, []string{}},
	}

	for _, c := range cases {
		// Construct a minimal AgentContextDefinition with no `tools:` field
		// so base allowlist is "nil = allow-all from registry". This is the
		// scenario that triggered Phase 12.3 wire bug: ABSOLUTE phase must
		// override the nil base.
		def := AgentContextDefinition{
			// nil Agent means "no frontmatter declared" → base is empty.
		}
		got := resolveAgentToolAllowlistWithPhase(def, c.phase)
		if !stringSliceEqCI(got, c.wantCols) {
			t.Errorf("phase=%q: got %v, want %v", c.phase, got, c.wantCols)
		}
		// Cross-table equality: the resolver result must match the
		// policy table row (single source of truth, Plan §4.20 L1).
		p := tools.ToolPolicyForPhase(string(c.phase))
		if p == nil {
			t.Errorf("phase=%q: policy row is nil", c.phase)
			continue
		}
		// For ABSOLUTE phases, resolver == policy row.Allowlist.
		if c.phase != GoalPhaseOpen {
			if !stringSliceEqCI(got, p.Allowlist) {
				t.Errorf("phase=%q: resolver %v != policy Allowlist %v", c.phase, got, p.Allowlist)
			}
		}
	}
}

// TestResolveAllowlist_OpenRelative_AddsLifecycle — OPEN phase is RELATIVE.
// With nil Agent (no frontmatter), resolver returns nil (backward compat:
// agents without `tools:` field rely on registry-wide visibility). With
// `tools:` declared, resolver returns base ∪ BaseAdds.
func TestResolveAllowlist_OpenRelative_AddsLifecycle(t *testing.T) {
	// nil Agent → nil allowlist (allow-all).
	def := AgentContextDefinition{}
	got := resolveAgentToolAllowlistWithPhase(def, GoalPhaseOpen)
	if got != nil {
		t.Errorf("OPEN with nil Agent: got %v, want nil", got)
	}

	// With base declared → base ∪ BaseAdds.
def2 := AgentContextDefinition{
		Agent: &AgentPromptDefinition{
			Frontmatter: AgentFrontmatter{
				Fields: map[string]any{"tools": true},
				Tools:  []string{"read_file", "exec"},
			},
		},
	}
	got2 := resolveAgentToolAllowlistWithPhase(def2, GoalPhaseOpen)
	want := map[string]bool{
		"read_file": true, "exec": true,
		"view_goal": true, "complete_goal": true,
	}
	gotMap := make(map[string]bool, len(got2))
	for _, s := range got2 {
		gotMap[strings.ToLower(s)] = true
	}
	for k := range want {
		if !gotMap[k] {
			t.Errorf("OPEN with base: missing %q in result %v", k, got2)
		}
	}
	if len(gotMap) != len(want) {
		t.Errorf("OPEN with base: got %v, want 4 items", got2)
	}
}

// TestCrossTable_PhaseTokensAgree — pkg/agent (GoalPhase-keyed) and pkg/tools
// (string-keyed) MUST agree on the exact set of phase tokens (P2-F1, R3-F6).
// Catches any drift between the two generator outputs.
func TestCrossTable_PhaseTokensAgree(t *testing.T) {
	pkgAgentTokens := []string{
		string(GoalPhaseSet),
		string(GoalPhaseOpen),
		string(GoalPhaseCheckpoint),
		string(GoalPhaseFinal),
		string(GoalPhasePostFinal),
	}
	// pkg/tools tokens sorted alphabetically.
	pkgToolsTokens := tools.SortedToolPhases()
	pkgPhasesTokens := phases.AllTokens()

	// pkg/agent and pkg/tools must have the SAME set.
	if !stringSliceEqCI(pkgAgentTokens, pkgToolsTokens) {
		t.Errorf("agent tokens %v != tools tokens %v", pkgAgentTokens, pkgToolsTokens)
	}

	// pkg/phases (canonical source) must match BOTH.
	wantSet := make(map[string]bool)
	for _, tok := range pkgPhasesTokens {
		wantSet[tok] = true
	}
	if len(wantSet) != len(pkgPhasesTokens) {
		t.Errorf("pkg/phases has duplicates: %v", pkgPhasesTokens)
	}
	for _, tok := range pkgAgentTokens {
		if !wantSet[tok] {
			t.Errorf("agent token %q not in pkg/phases canonical set", tok)
		}
	}
	for _, tok := range pkgToolsTokens {
		if !wantSet[tok] {
			t.Errorf("tools token %q not in pkg/phases canonical set", tok)
		}
	}
}

func stringSliceEqCI(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	am := make(map[string]int, len(a))
	bm := make(map[string]int, len(b))
	for _, s := range a {
		am[strings.ToLower(s)]++
	}
	for _, s := range b {
		bm[strings.ToLower(s)]++
	}
	if len(am) != len(bm) {
		return false
	}
	for k, v := range am {
		if bm[k] != v {
			return false
		}
	}
	return true
}

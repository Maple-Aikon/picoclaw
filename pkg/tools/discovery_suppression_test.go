package tools

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/phases"
)

// TestDiscoverySuppression_PerPhase — T3 regression-lock. Plan §3.1 + Phase 12.5 +
// Phase 12.18 + Phase 12.47. Expected matrix:
//
//	phase:        bm25 visible?   regex visible?   exec visible?
//	set:          false           false            false
//	open:         true            true             true
//	checkpoint:   false           false            false
//	final:        false           false            false
//	post_final:   false           false            false
//
// "exec" is the canary non-discovery tool — it must be allowed at all
// phases EXCEPT post_final (where hard guard blocks everything).
func TestDiscoverySuppression_PerPhase(t *testing.T) {
	cases := []struct {
		phase string
		want  map[string]bool // tool → visible
	}{
		{"set", map[string]bool{
			"tool_search_tool_bm25":   false,
			"tool_search_tool_regex":  false,
			"exec":                   false, // blocked at SET (allowlist = [set_goal])
		}},
		{"open", map[string]bool{
			"tool_search_tool_bm25":   true,
			"tool_search_tool_regex":  true,
			"exec":                   true,
		}},
		{"checkpoint", map[string]bool{
			"tool_search_tool_bm25":   false,
			"tool_search_tool_regex":  false,
			"exec":                   false, // ABSOLUTE allowlist = [goal_progress, complete_goal]
		}},
		{"final", map[string]bool{
			"tool_search_tool_bm25":   false,
			"tool_search_tool_regex":  false,
			"exec":                   false, // ABSOLUTE allowlist = [complete_goal]
		}},
		{"post_final", map[string]bool{
			"tool_search_tool_bm25":   false,
			"tool_search_tool_regex":  false,
			"exec":                   false, // hard guard blocks all
		}},
	}

	for _, c := range cases {
		r := NewToolRegistry()
		r.SetPhase(c.phase)
		// Use a permissive allowlist so we exercise the per-phase gate, not allowlist membership.
		// ABSOLUTE phases (set/checkpoint/final) need their Allowlist set; otherwise allowlist
		// overrides our test expectations. Open and post_final have explicit allowlists too.
		var allowlist []string
		switch c.phase {
		case "set":
			allowlist = []string{"set_goal"}
		case "open":
			// Relative: nil allowlist (allow-all) so only gate matters.
			allowlist = nil
		case "checkpoint":
			allowlist = []string{"goal_progress", "complete_goal"}
		case "final":
			allowlist = []string{"complete_goal"}
		case "post_final":
			allowlist = []string{}
		}
		r.SetAllowlist(allowlist)
		// CRITICAL: SetAllowlist resets r.phase="" (Phase 12.5 contract) — must
		// SetPhase AFTER SetAllowlist for the per-phase policy to apply.
		r.SetPhase(c.phase)
		// Register the BM25/Regex tools so IsAllowed can identify them as
		// discovery (else the exemption path is skipped).
		r.Register(newMockTool("tool_search_tool_bm25", "discover hidden"))
		r.Register(newMockTool("tool_search_tool_regex", "discover hidden"))
		r.Register(newMockTool("exec", "exec shell"))

		for tool, want := range c.want {
			got := r.IsAllowed(tool)
			if got != want {
				p := ToolPolicyForPhase(c.phase)
				isDisc := isToolDiscoveryToolName(tool)
				t.Errorf("phase=%q tool=%q (disc=%v): IsAllowed=%v, want %v — policy=%+v allowlist=%+v r.phase=%q", c.phase, tool, isDisc, got, want, p, r.GetAllowlist(), c.phase)
			}
		}
	}
}

// TestLifecycleKeys_SchemaLock — T3c regression-lock. ALL 5 phase rows MUST
// have the canonical 4 lifecycle keys: set_goal/goal_progress/view_goal/complete_goal.
// Missing key = BLOCKED silently = bug. (Plan §4.20 L3.)
func TestLifecycleKeys_SchemaLock(t *testing.T) {
	for _, phase := range phases.AllTokens() {
		p := ToolPolicyForPhase(phase)
		if p == nil {
			t.Errorf("phase %q policy is nil", phase)
			continue
		}
		err := MustHaveCanonicalLifecycleKeys(p)
		if err != nil {
			t.Errorf("phase %q: %v", phase, err)
		}
	}
}

// TestPostFinal_RowIsSchemaContract — T3c schema contract. The post_final row
// has lifecycle keys all-false AND Allowlist non-nil len==0 — schema-contract
// ONLY. The runtime hard guard at top of toolAllowedLocked enforces block-all
// in code (R4-F2). Removing the hard guard would still leave post_final blocked
// by policy data alone (defense-in-depth).
func TestPostFinal_RowIsSchemaContract(t *testing.T) {
	p := ToolPolicyForPhase("post_final")
	if p == nil {
		t.Fatal("post_final policy is nil")
	}
	if p.Allowlist == nil {
		t.Error("post_final Allowlist is nil (should be non-nil empty)")
	}
	if len(p.Allowlist) != 0 {
		t.Errorf("post_final Allowlist len = %d, want 0", len(p.Allowlist))
	}
	for tool, v := range p.LifecycleAllowed {
		if v {
			t.Errorf("post_final LifecycleAllowed[%q] = true, want false", tool)
		}
	}
	if p.DiscoveryVisible {
		t.Error("post_final DiscoveryVisible = true, want false")
	}
}

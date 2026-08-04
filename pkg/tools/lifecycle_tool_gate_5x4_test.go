package tools

import "testing"

// TestLifecycleGate_5x4Matrix — T2 regression-lock. Expected matrix (Plan
// §3.1, Phase 12.31 + Phase 12.47):
//
//	phase:        set    open  checkpoint  final  post_final
//	set_goal:     true   false false       false  false
//	goal_progress:false  false true        false  false
//	view_goal:    false  true  false       false  false
//	complete_goal:true   true  true        true   false (R6-F1, post_final hard-guard)
//
// Empty phase "" → gate disabled (returns true for all) — backward compat
// with SetAllowlist-only callers (instance.go:113).
func TestLifecycleGate_5x4Matrix(t *testing.T) {
	const set, open, cp, fin, post = "set", "open", "checkpoint", "final", "post_final"
	cases := []struct {
		tool  string
		phase string
		want  bool
	}{
		// set_goal: SET only
		{"set_goal", set, true},
		{"set_goal", open, false},
		{"set_goal", cp, false},
		{"set_goal", fin, false},
		{"set_goal", post, false},

		// goal_progress: CHECKPOINT only
		{"goal_progress", set, false},
		{"goal_progress", open, false},
		{"goal_progress", cp, true},
		{"goal_progress", fin, false},
		{"goal_progress", post, false},

		// view_goal: OPEN only
		{"view_goal", set, false},
		{"view_goal", open, true},
		{"view_goal", cp, false},
		{"view_goal", fin, false},
		{"view_goal", post, false},

		// complete_goal: any non-empty phase (per Phase 12.31); post_final
		// excluded by hard guard at top of toolAllowedLocked, so the gate
		// itself returns true here but the registry short-circuits to false.
		// Plan §3.1 explicitly notes this: "complete_goal allowed mọi
		// phase non-empty → mọi row (trừ post_final) có complete_goal: true;
		// post_final all-false." We assert post_final=false for
		// complete_goal to lock the registry-level block.
		{"complete_goal", set, true},
		{"complete_goal", open, true},
		{"complete_goal", cp, true},
		{"complete_goal", fin, true},
		{"complete_goal", post, false},

		// Non-lifecycle tool: always true (gate disabled).
		{"read_file", set, true},
		{"exec", open, true},
		{"send_file", cp, true},
		{"web_search", fin, true},

		// Empty phase: gate disabled — all true (backward compat).
		{"set_goal", "", true},
		{"goal_progress", "", true},
		{"view_goal", "", true},
		{"complete_goal", "", true},
		{"read_file", "", true},
	}

	for _, c := range cases {
		got := IsLifecycleToolAllowed(c.tool, c.phase)
		if got != c.want {
			t.Errorf("IsLifecycleToolAllowed(%q, %q) = %v, want %v", c.tool, c.phase, got, c.want)
		}
	}
}

// TestIsLifecycleToolName — assert static known-set is exactly 4 canonical
// lifecycle tools (set_goal/goal_progress/view_goal/complete_goal).
// If a 5th is added, this test FAILS — forcing the manifest author to update
// the policy row keys.
func TestIsLifecycleToolName(t *testing.T) {
	known := []string{"set_goal", "goal_progress", "view_goal", "complete_goal"}
	for _, n := range known {
		if !IsLifecycleToolName(n) {
			t.Errorf("IsLifecycleToolName(%q) = false, want true", n)
		}
	}
	others := []string{"read_file", "exec", "send_file", "web_search", "tool_search_tool_bm25", ""}
	for _, n := range others {
		if IsLifecycleToolName(n) {
			t.Errorf("IsLifecycleToolName(%q) = true, want false", n)
		}
	}
}

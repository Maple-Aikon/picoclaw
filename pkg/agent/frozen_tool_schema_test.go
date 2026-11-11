package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/tools"
	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// mockTool implements tools.Tool for testing
type mockTool struct {
	name string
}

func (m *mockTool) Name() string                     { return m.name }
func (m *mockTool) Description() string              { return "mock tool for testing" }
func (m *mockTool) Parameters() map[string]any       { return map[string]any{"type": "object"} }
func (m *mockTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	return toolshared.NewToolResult("ok")
}

// TestFrozenToolSchema_InvariantAcrossGoalPhases verifies the tool-schema
// projection invariants that hold when ToolProjectionMode is frozen.
//
// CONTRACT CHANGE (P1 — plan checkpoint-misfire-fix, contract C8):
//
// This test USED TO assert that ToProviderDefs() returns the SAME 5 schemas
// in EVERY goal phase (SET, OPEN, CHECKPOINT, FINAL). P1 deliberately breaks
// that invariant at the NARROW phases, because the whole point of P1 is to
// stop showing the LLM the ~76 schemas it is not allowed to call (the
// model picks one, the runtime gate rejects it, the turn burns an
// iteration on a misfire).
//
// LEVER (b) (plan §20, anh chốt 2026-11-11 07:58 ICT) then REVERSED that
// for CHECKPOINT ONLY: the CHECKPOINT wire is the full registry again, so
// the OPEN→CHECKPOINT transition does not move the prompt-cache prefix.
// The RUNTIME GATE at CHECKPOINT is still {goal_progress, complete_goal}.
//
// Per C8 the test is UPDATED, NOT DELETED. What it keeps:
//
//	(a) the RUNTIME GATE assertions — the gate is the security boundary and
//	    P1 must not weaken it;
//	(b) the OPEN-phase consistency assertion — OPEN keeps the FULL frozen
//	    projection so the prompt-cache prefix does not move.
//
// What it now asserts instead of "identical in every phase":
//
//	- OPEN (and the unset-phase backward-compat path) project ALL 5 tools,
//	  byte-identical to the pre-P1 behavior;
//	- CHECKPOINT projects ALL 5 tools too (lever (b)) — the wire matches
//	  OPEN so the prompt-cache prefix does not move on the transition;
//	- the remaining NARROW phases project exactly the tools their phase
//	  allowlist permits — SET -> 1, FINAL -> 1.
func TestFrozenToolSchema_InvariantAcrossGoalPhases(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.SetProjectionFrozen(true)

	// Register lifecycle tools and general execution tools
	reg.Register(&mockTool{name: "set_goal"})
	reg.Register(&mockTool{name: "goal_progress"})
	reg.Register(&mockTool{name: "complete_goal"})
	reg.Register(&mockTool{name: "read_file"})
	reg.Register(&mockTool{name: "exec"})

	// --- (b) OPEN keeps the FULL frozen projection (cache stability) -------
	//
	// The OPEN prompt-cache prefix is the hot path: it is reused across
	// turns. If the OPEN projection shrank, every OPEN turn would pay full
	// uncached input. So OPEN must project ALL registered tools, exactly
	// as it did before P1.
	reg.SetAllowlist(nil)
	reg.SetPhase("OPEN")

	defsOpen := reg.ToProviderDefs()
	if len(defsOpen) != 5 {
		t.Fatalf("OPEN phase must keep the full frozen projection (5 tools), got %d", len(defsOpen))
	}
	openNames := map[string]bool{}
	for _, d := range defsOpen {
		openNames[d.Function.Name] = true
	}
	for _, want := range []string{"set_goal", "goal_progress", "complete_goal", "read_file", "exec"} {
		if !openNames[want] {
			t.Errorf("OPEN projection lost %q — the prompt-cache prefix would move", want)
		}
	}

	// --- The NARROW phases project only their allowlist -------------------
	//
	// These are the ABSOLUTE phase rows from pkg/tools/phase_policy_table.go.
	// Each phase's allowlist is pinned by the policy table, so the expected
	// projected set is exact.
	//
	// LEVER (b), plan §20 (anh chốt 2026-11-11 07:58 ICT): CHECKPOINT is
	// NO LONGER narrow — its projection is the whole registry, identical to
	// OPEN, so the OPEN→CHECKPOINT transition does not move the
	// prompt-cache prefix. Its executable set is unchanged (still the 2
	// lifecycle tools); only the advertised schema set widened.
	narrowCases := []struct {
		phase     string
		allowlist []string
		want      []string
	}{
		{"SET", []string{"set_goal"}, []string{"set_goal"}},
		{"FINAL", []string{"complete_goal"}, []string{"complete_goal"}},
	}

	for _, tc := range narrowCases {
		t.Run(tc.phase, func(t *testing.T) {
			reg.SetAllowlist(tc.allowlist)
			reg.SetPhase(tc.phase)

			defs := reg.ToProviderDefs()
			if len(defs) != len(tc.want) {
				t.Fatalf("%s phase must project exactly %v (%d tools), got %d",
					tc.phase, tc.want, len(tc.want), len(defs))
			}
			got := map[string]bool{}
			for _, d := range defs {
				got[d.Function.Name] = true
			}
			for _, want := range tc.want {
				if !got[want] {
					t.Errorf("%s phase did not project %q (got %v)", tc.phase, want, got)
				}
			}
		})
	}

	// --- LEVER (b): CHECKPOINT projects the whole registry ---------------
	//
	// Identical to the OPEN projection — that is the whole point: the
	// prompt-cache prefix must not move when the turn crosses the
	// OPEN→CHECKPOINT boundary.
	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("CHECKPOINT")
	defsCheckpoint := reg.ToProviderDefs()
	if len(defsCheckpoint) != 5 {
		t.Fatalf("CHECKPOINT phase must project the whole registry (5 tools) under lever (b), got %d", len(defsCheckpoint))
	}
	ckptNames := map[string]bool{}
	for _, d := range defsCheckpoint {
		ckptNames[d.Function.Name] = true
	}
	for _, want := range []string{"set_goal", "goal_progress", "complete_goal", "read_file", "exec"} {
		if !ckptNames[want] {
			t.Errorf("CHECKPOINT projection lost %q — the wire must match OPEN", want)
		}
	}

	// --- (a) Runtime Gate must still enforce phase restrictions! ----------
	//
	// P1 changes what the LLM SEES. It must NOT change what the LLM is
	// allowed to RUN. At SET phase, 'read_file' must be blocked at
	// execution time even though (in the frozen legacy projection) its
	// schema was visible.
	//
	// LEVER (b) widens the CHECKPOINT wire — the gate must NOT follow.
	// This is the safety half of (b): at CHECKPOINT the schema set is the
	// whole registry, but the executable set is still {goal_progress,
	// complete_goal}.
	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("CHECKPOINT")
	if reg.IsAllowed("exec") {
		t.Errorf("runtime execution gate failed: 'exec' must not be accessible at CHECKPOINT even though its schema is projected")
	}
	if reg.IsAllowed("read_file") {
		t.Errorf("runtime execution gate failed: 'read_file' must not be accessible at CHECKPOINT even though its schema is projected")
	}
	if !reg.IsAllowed("goal_progress") {
		t.Errorf("runtime execution gate failed: 'goal_progress' must be accessible at CHECKPOINT")
	}
	if !reg.IsAllowed("complete_goal") {
		t.Errorf("runtime execution gate failed: 'complete_goal' must be accessible at CHECKPOINT")
	}

	reg.SetAllowlist([]string{"set_goal"})
	reg.SetPhase("SET")
	if reg.IsAllowed("read_file") {
		t.Errorf("runtime execution gate failed: 'read_file' should not be accessible at SET phase")
	}
	if reg.IsAllowed("exec") {
		t.Errorf("runtime execution gate failed: 'exec' should not be accessible at SET phase")
	}
	if !reg.IsAllowed("set_goal") {
		t.Errorf("runtime execution gate failed: 'set_goal' should be accessible at SET phase")
	}
}

package tools

// LEVER (b) — CHECKPOINT projection parity with OPEN (plan
// checkpoint-misfire-fix §20, anh chốt 2026-11-11 07:58 ICT).
//
// The projection lives here (pkg/tools/registry.go: projectToolLocked).
// This file pins the contract at the layer that actually owns the
// behavior, so a future refactor of pkg/agent cannot silently widen or
// narrow the CHECKPOINT wire.
//
// CONTRACT:
//
//	L1  CHECKPOINT projects the FULL registry — identical to OPEN.
//	L2  SET projects exactly {set_goal}.
//	L3  FINAL projects exactly {complete_goal}.
//	L4  POST-FINAL projects NOTHING (the hard guard at the top of
//	    toolAllowedLocked must stay first).
//	L5  The runtime gate at CHECKPOINT still refuses everything outside
//	    {goal_progress, complete_goal}.
//	L6  An unknown phase still fails CLOSED.
//
// L1 and L5 together are the whole safety argument: the wire is wide,
// the gate is narrow.

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

type lbToolsMockTool struct {
	name string
}

func (m *lbToolsMockTool) Name() string               { return m.name }
func (m *lbToolsMockTool) Description() string        { return "mock tool for testing" }
func (m *lbToolsMockTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (m *lbToolsMockTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	return toolshared.NewToolResult("ok")
}

func lbToolsRegistry() *ToolRegistry {
	reg := NewToolRegistry()
	reg.SetProjectionFrozen(true)
	for _, name := range []string{"set_goal", "view_goal", "goal_progress", "complete_goal", "read_file", "exec"} {
		reg.Register(&lbToolsMockTool{name: name})
	}
	return reg
}

func lbToolsNames(defs []providers.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Function.Name)
	}
	return out
}

// --- L1: CHECKPOINT projection == OPEN projection -------------------------

func TestLeverB_ToolsCheckpointProjectsFullRegistry(t *testing.T) {
	reg := lbToolsRegistry()
	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("checkpoint")

	names := lbToolsNames(reg.ToProviderDefs())
	if len(names) != 6 {
		t.Fatalf("L1 violated: CHECKPOINT projected %v (%d), want the full registry (6)", names, len(names))
	}
	for _, want := range []string{"exec", "read_file", "goal_progress", "complete_goal"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("L1 violated: CHECKPOINT projection %v is missing %q", names, want)
		}
	}
}

// --- L2/L3/L4: the other phases are UNCHANGED ----------------------------

func TestLeverB_ToolsOtherPhasesUnchanged(t *testing.T) {
	cases := []struct {
		phase     string
		allowlist []string
		wantCount int
	}{
		{"set", []string{"set_goal"}, 1},
		{"final", []string{"complete_goal"}, 1},
		{"post_final", []string{}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			reg := lbToolsRegistry()
			reg.SetAllowlist(tc.allowlist)
			reg.SetPhase(tc.phase)

			defs := reg.ToProviderDefs()
			if len(defs) != tc.wantCount {
				t.Fatalf("phase=%s projected %v (%d), want %d", tc.phase, lbToolsNames(defs), len(defs), tc.wantCount)
			}
		})
	}
}

// --- L5: the runtime gate at CHECKPOINT stays narrow ----------------------

func TestLeverB_ToolsRuntimeGateNarrowAtCheckpoint(t *testing.T) {
	reg := lbToolsRegistry()
	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("checkpoint")

	if reg.IsAllowed("exec") {
		t.Fatal("L5 violated: exec must be blocked at execution time at CHECKPOINT")
	}
	if reg.IsAllowed("read_file") {
		t.Fatal("L5 violated: read_file must be blocked at execution time at CHECKPOINT")
	}
	if !reg.IsAllowed("goal_progress") || !reg.IsAllowed("complete_goal") {
		t.Fatal("L5 violated: goal_progress + complete_goal must remain executable at CHECKPOINT")
	}
}

// --- L6: unknown phase fails CLOSED ---------------------------------------

func TestLeverB_ToolsUnknownPhaseFailsClosed(t *testing.T) {
	reg := lbToolsRegistry()
	reg.SetAllowlist([]string{"set_goal"})
	reg.SetPhase("not_a_real_phase")

	names := lbToolsNames(reg.ToProviderDefs())
	for _, n := range names {
		if n == "exec" || n == "read_file" {
			t.Fatalf("L6 violated: unknown phase projected %v — must fail closed", names)
		}
		if !reg.IsAllowed(n) {
			t.Fatalf("L6 violated: unknown phase advertised %q but the gate blocks it", n)
		}
	}
}

// --- The POST-FINAL hard guard stays FIRST --------------------------------
//
// Plan §20.5 guard #7: after (b) the CHECKPOINT projection is the full
// registry, so anyone who merges the projection branches could
// accidentally reopen POST-FINAL. The hard guard must stay an
// early-return at the very top of toolAllowedLocked — i.e. it must
// win over ANY allowlist state, including a nil (allow-all) allowlist.

func TestLeverB_ToolsPostFinalHardGuardBeatsNilAllowlist(t *testing.T) {
	reg := lbToolsRegistry()
	reg.SetAllowlist(nil) // allow-all
	reg.SetPhase("post_final")

	if reg.IsAllowed("exec") {
		t.Fatal("POST-FINAL hard guard violated: exec must be blocked even with a nil (allow-all) allowlist")
	}
	if reg.IsAllowed("complete_goal") {
		t.Fatal("POST-FINAL hard guard violated: complete_goal must be blocked even with a nil (allow-all) allowlist")
	}
	if defs := reg.ToProviderDefs(); len(defs) != 0 {
		t.Fatalf("POST-FINAL must project 0 tools, got %d", len(defs))
	}
}

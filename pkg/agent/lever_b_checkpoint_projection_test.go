package agent

// LEVER (b) — CHECKPOINT projection parity with OPEN (plan
// checkpoint-misfire-fix §20, anh chốt 2026-11-11 07:58 ICT).
//
// WHY THIS FILE EXISTS
// -------------------
// P1 (Phase 12.72 / plan §17) made the tool-schema PROJECTION
// phase-scoped: at the narrow phases (set / checkpoint / final /
// post_final) only the phase allowlist is projected, so the LLM stops
// seeing the ~76 schemas it may not call.
//
// Lever (b) reverses that for CHECKPOINT ONLY:
//
//	set         -> 1 schema   (set_goal)          — UNCHANGED
//	open        -> 76 schemas (full registry)      — UNCHANGED
//	checkpoint  -> 76 schemas (full registry)      — CHANGED (was 2)
//	post_final  -> 0 schemas                       — UNCHANGED
//
// Rationale (plan §20.7): (b) is a CACHE lever, not a misfire fix. The
// OPEN→CHECKPOINT transition rewrites the tool-schema prefix, so every
// transition pays full uncached input. Keeping the wire identical at
// OPEN and CHECKPOINT makes the prefix byte-stable across the
// transition.
//
// The RUNTIME GATE IS UNTOUCHED. A tool that is advertised at
// CHECKPOINT is still refused at execution time by toolAllowedLocked +
// gatePreCheckDeny. Projection is model-visibility; the gate is the
// execution boundary.
//
// CONTRACT (constraint-driven — stated before the code, per plan §20.5):
//
//	B1  CHECKPOINT projection == OPEN projection (same tool set).
//	B2  SET projection is still exactly {set_goal}.
//	B3  POST-FINAL projection is still empty (hard guard).
//	B4  FINAL projection is still exactly {complete_goal}.
//	B5  The runtime gate is NOT weakened: exec / read_file are still
//	    BLOCKED at execution time at CHECKPOINT.
//	B6  Unknown / unrecognised phase still fails CLOSED (never widens).
//	B7  The CHECKPOINT hint must not claim "Only N tools are available"
//	    with N != the projected schema count (plan §20.3) — the hint
//	    may talk about EXECUTABLE tools, not about schemas on the wire.
//
// B5 and B7 together are the guard against the two failure modes of
// (b): (i) leaking executable tools that the gate refuses, and (ii)
// telling the model a schema count that does not match the wire.

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// lbMockTool implements tools.Tool for the lever-(b) tests.
type lbMockTool struct {
	name string
}

func (m *lbMockTool) Name() string               { return m.name }
func (m *lbMockTool) Description() string        { return "mock tool for testing" }
func (m *lbMockTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (m *lbMockTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	return toolshared.NewToolResult("ok")
}

// lbRegistry builds a registry with the 4 lifecycle tools + 2 domain
// tools, projection frozen (the production configuration after
// Phase 12.72).
func lbRegistry() *tools.ToolRegistry {
	reg := tools.NewToolRegistry()
	reg.SetProjectionFrozen(true)
	for _, name := range []string{"set_goal", "view_goal", "goal_progress", "complete_goal", "read_file", "exec"} {
		reg.Register(&lbMockTool{name: name})
	}
	return reg
}

func lbNames(defs []providers.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Function.Name)
	}
	return out
}

func lbContains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// --- B1: CHECKPOINT projects the same set as OPEN -------------------------
//
// This is the core of lever (b). The registry is the mock 6-tool
// registry, so "the same set as OPEN" == "all 6 registered tools".

func TestLeverB_CheckpointProjectionMatchesOpen(t *testing.T) {
	openReg := lbRegistry()
	openReg.SetAllowlist(nil)
	openReg.SetPhase("open")
	openNames := lbNames(openReg.ToProviderDefs())

	ckptReg := lbRegistry()
	ckptReg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	ckptReg.SetPhase("checkpoint")
	ckptNames := lbNames(ckptReg.ToProviderDefs())

	if len(ckptNames) != len(openNames) {
		t.Fatalf("B1 violated: CHECKPOINT projected %v (%d), OPEN projected %v (%d) — "+
			"lever (b) requires the CHECKPOINT wire to match the OPEN wire",
			ckptNames, len(ckptNames), openNames, len(openNames))
	}
	for _, want := range openNames {
		if !lbContains(ckptNames, want) {
			t.Fatalf("B1 violated: CHECKPOINT projection %v is missing %q (OPEN has it)", ckptNames, want)
		}
	}
	// The domain tools must be VISIBLE at CHECKPOINT now — that is the
	// whole point of (b): the prefix must not move on the transition.
	for _, want := range []string{"exec", "read_file"} {
		if !lbContains(ckptNames, want) {
			t.Fatalf("B1 violated: CHECKPOINT projection %v lost %q", ckptNames, want)
		}
	}
}

// --- B2/B3/B4: the other phases are UNCHANGED ----------------------------

func TestLeverB_OtherPhasesUnchanged(t *testing.T) {
	cases := []struct {
		phase     string
		allowlist []string
		want      []string
	}{
		// B2 — SET is still exactly {set_goal}.
		{"set", []string{"set_goal"}, []string{"set_goal"}},
		// B4 — FINAL is still exactly {complete_goal}.
		{"final", []string{"complete_goal"}, []string{"complete_goal"}},
		// B3 — POST-FINAL is still empty (the hard guard).
		{"post_final", []string{}, []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			reg := lbRegistry()
			reg.SetAllowlist(tc.allowlist)
			reg.SetPhase(tc.phase)

			names := lbNames(reg.ToProviderDefs())
			if len(names) != len(tc.want) {
				t.Fatalf("phase=%s projected %v (%d), want exactly %v", tc.phase, names, len(names), tc.want)
			}
			for _, w := range tc.want {
				if !lbContains(names, w) {
					t.Fatalf("phase=%s projected %v, missing %q", tc.phase, names, w)
				}
			}
		})
	}
}

// --- B5: the runtime gate is NOT weakened ---------------------------------
//
// The whole safety argument of (b): advertising a schema does not grant
// the ability to execute it. At CHECKPOINT the phase allowlist is
// {goal_progress, complete_goal} — everything else stays blocked.

func TestLeverB_RuntimeGateStillBlocksAtCheckpoint(t *testing.T) {
	reg := lbRegistry()
	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("checkpoint")

	if reg.IsAllowed("exec") {
		t.Fatal("B5 violated: exec must still be blocked at execution time at CHECKPOINT")
	}
	if reg.IsAllowed("read_file") {
		t.Fatal("B5 violated: read_file must still be blocked at execution time at CHECKPOINT")
	}
	if !reg.IsAllowed("goal_progress") {
		t.Fatal("B5 violated: goal_progress must remain executable at CHECKPOINT")
	}
	if !reg.IsAllowed("complete_goal") {
		t.Fatal("B5 violated: complete_goal must remain executable at CHECKPOINT")
	}

	// The projection is the FULL registry, but the executable set is the
	// narrow allowlist. The two are DIFFERENT sets — that is by design.
	defs := reg.ToProviderDefs()
	executable := 0
	for _, n := range lbNames(defs) {
		if reg.IsAllowed(n) {
			executable++
		}
	}
	if executable != 2 {
		t.Fatalf("B5 violated: expected exactly 2 executable tools at CHECKPOINT, got %d", executable)
	}
}

// --- B6: an unknown phase still fails CLOSED ------------------------------
//
// (b) must not become a hole: an unrecognised phase must never WIDEN
// the visible set. The unknown phase keeps the pre-(b) behaviour —
// project only what the allowlist permits.

func TestLeverB_UnknownPhaseStillFailsClosed(t *testing.T) {
	reg := lbRegistry()
	reg.SetAllowlist([]string{"set_goal"})
	reg.SetPhase("not_a_real_phase")

	names := lbNames(reg.ToProviderDefs())
	if lbContains(names, "exec") || lbContains(names, "read_file") {
		t.Fatalf("B6 violated: unknown phase projected %v — must fail closed, never widen the visible set", names)
	}
	for _, n := range names {
		if !reg.IsAllowed(n) {
			t.Fatalf("B6 violated: unknown phase advertised %q but the runtime gate blocks it", n)
		}
	}
}

// --- B7: the CHECKPOINT hint must not contradict the wire -----------------
//
// After (b) the wire carries ~76 schemas at CHECKPOINT. A hint that says
// "Only 2 tools are now available" contradicts the wire: the model
// sees 76 schemas and is told 2 exist. Per plan §20.3 the hint may talk
// about EXECUTABLE tools, but must never claim a schema count.
//
// The assertion is about the ABSENCE of a contradicting schema-count
// claim, plus the PRESENCE of the executable-tool wording.

func TestLeverB_CheckpointHintDoesNotContradictWire(t *testing.T) {
	// Both production paths of the hint contributor: the compass path
	// (MaxIterationsCap > 0) and the legacy fallback (cap == 0).
	for _, req := range []PromptBuildRequest{
		{GoalPhase: string(GoalPhaseCheckpoint), MaxIterationsCap: 250, Iteration: 25},
		{GoalPhase: string(GoalPhaseCheckpoint), Iteration: 25},
	} {
		got := goalPhaseCheckpointHintContributor(req)
		if got == nil {
			t.Fatal("expected non-nil CHECKPOINT hint")
		}
		content := got.Content

		// (a) No schema-count claim that contradicts the wire.
		for _, forbidden := range []string{
			"Only 2 tools are now available",
			"Only 2 tools are available",
			"2 tools are now available",
		} {
			if strings.Contains(content, forbidden) {
				t.Errorf("B7 violated: CHECKPOINT hint claims %q — after lever (b) the wire carries the full registry, "+
					"so the hint may only talk about EXECUTABLE tools: %s", forbidden, content)
			}
		}

		// (b) The executable-tool wording must be present, so the model
		// still knows what it may run.
		if !strings.Contains(content, "lifecycle tools can actually be executed") {
			t.Errorf("B7 violated: CHECKPOINT hint must state which tools can actually be executed: %s", content)
		}
	}
}

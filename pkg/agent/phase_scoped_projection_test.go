package agent

// Phase-scoped tool-schema projection (plan checkpoint-misfire-fix, P1).
//
// WHY THIS FILE EXISTS
// -------------------
// The tool SCHEMA projection used to be frozen at every goal phase
// (agent_init.go: SetProjectionFrozen(true) → ToProviderDefs() emits the
// WHOLE registry). That keeps the OPEN-phase prompt-cache prefix byte-stable,
// but it also means the LLM at CHECKPOINT / FINAL / SET still SEES ~76 tool
// schemas it is not allowed to call — the model picks one, the runtime gate
// blocks it, and the turn burns an iteration on a misfire.
//
// P1 narrows the PROJECTION (what the LLM sees) at the narrow phases while
// leaving the RUNTIME GATE (what may execute) untouched. The OPEN phase
// keeps the full frozen projection so the cache-critical prefix does not move.
//
// CONTRACT (constraint-driven — stated before the code, per plan §9.6):
//
//	C1  OPEN        -> schema set IDENTICAL to today's frozen projection
//	                   (prompt-cache prefix stability preserved).
//	C2  CHECKPOINT  -> schema set == exactly {goal_progress, complete_goal}.
//	C3  SET         -> schema set == exactly {set_goal}.
//	C4  FINAL       -> schema set == exactly {complete_goal}.
//	C5  POST-FINAL  -> schema set is empty.
//	C6  The runtime execution gate is NOT weakened: a tool absent from the
//	    projected schema is still blocked at execution time.
//	C7  Unknown / empty phase -> fail CLOSED onto the phase allowlist.
//	    The visible set must never be WIDENED by an unrecognised phase.
//
// C8 (plan §9.6) — pkg/agent/frozen_tool_schema_test.go asserted that
// ToProviderDefs() returns the SAME schemas in every phase. P1 deliberately
// breaks that invariant at the NARROW phases, so that test is updated (not
// deleted) in the same change: it keeps the runtime-gate assertions and the
// OPEN-phase identity assertion.
//
// C9 (plan §9.6) — the canary measures the EXECUTABLE set
// (ToolRegistry.EffectiveToolCount), not the projected schema count, so
// narrowing the projection does not perturb the canary baselines.
//
// NOTE ON THE OPEN-PHASE CACHE CLAIM: the plan asserts that filtering
// tools at narrow phases cannot break the OPEN cache because
// isCacheableGoalPhase(phase) == (phase == OPEN || phase == "").
// That is verified against source. The byte-identity of the SYSTEM PROMPT
// across the change is asserted separately by
// TestP1_SystemPromptUnchangedByProjection.

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// p1MockTool implements tools.Tool for the projection tests.
type p1MockTool struct {
	name string
}

func (m *p1MockTool) Name() string                     { return m.name }
func (m *p1MockTool) Description() string              { return "mock tool for testing" }
func (m *p1MockTool) Parameters() map[string]any       { return map[string]any{"type": "object"} }
func (m *p1MockTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	return toolshared.NewToolResult("ok")
}

// p1Registry builds a registry with the 4 lifecycle tools + 2 domain tools,
// projection frozen (the production configuration after Phase 12.72).
func p1Registry() *tools.ToolRegistry {
	reg := tools.NewToolRegistry()
	reg.SetProjectionFrozen(true)
	for _, name := range []string{"set_goal", "view_goal", "goal_progress", "complete_goal", "read_file", "exec"} {
		reg.Register(&p1MockTool{name: name})
	}
	return reg
}

// p1Names extracts the projected tool names in projection order.
func p1Names(defs []providers.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Function.Name)
	}
	return out
}

func p1Contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// --- C1: OPEN keeps the full frozen projection (cache stability) -----------

func TestP1_OpenPhaseKeepsFullFrozenProjection(t *testing.T) {
	reg := p1Registry()

	// OPEN is a RELATIVE phase: the base allowlist is everything the agent
	// declared (no frontmatter -> allow-all), plus the lifecycle BaseAdds.
	reg.SetAllowlist(nil)
	reg.SetPhase("open")

	names := p1Names(reg.ToProviderDefs())

	// The domain tools must remain visible at OPEN. If they vanished, the
	// OPEN cache prefix would change and every OPEN turn would pay full
	// uncached input — P1 would be a net regression.
	for _, want := range []string{"exec", "read_file", "view_goal", "complete_goal"} {
		if !p1Contains(names, want) {
			t.Fatalf("C1 violated: OPEN phase projected %v — %q must remain visible at OPEN", names, want)
		}
	}
	if len(names) != 6 {
		t.Fatalf("C1 violated: OPEN phase projected %d tools (%v), want all 6 registered", len(names), names)
	}
}

// --- C1b: empty phase also keeps the full projection ----------------------

func TestP1_EmptyPhaseKeepsFullProjection(t *testing.T) {
	reg := p1Registry()
	reg.SetAllowlist(nil)
	reg.SetPhase("")

	names := p1Names(reg.ToProviderDefs())
	if len(names) != 6 {
		t.Fatalf("C1b violated: empty phase projected %d tools (%v), want all 6", len(names), names)
	}
}

// --- C2/C3/C4/C5: narrow phases expose ONLY their allowlist ---------------
//
// LEVER (b), plan §20 (anh chốt 2026-11-11 07:58 ICT): CHECKPOINT is no
// longer a NARROW phase. Its projection is the WHOLE registry — identical
// to OPEN — so the OPEN→CHECKPOINT transition does not move the
// prompt-cache prefix (that transition used to cost full uncached input).
//
// The contract below is therefore:
//   - SET       -> exactly {set_goal}          (unchanged)
//   - CHECKPOINT -> the whole registry          (CHANGED by lever (b))
//   - FINAL     -> exactly {complete_goal}      (unchanged)
//   - POST-FINAL -> empty                      (unchanged)
//
// The CHECKPOINT row keeps an assertion of its own: the projection must
// CONTAIN the executable set (so the model can still call the 2 lifecycle
// tools it is allowed to run).

func TestP1_NarrowPhasesProjectOnlyTheirAllowlist(t *testing.T) {
	cases := []struct {
		phase     string
		allowlist []string
		want      []string
	}{
		{"set", []string{"set_goal"}, []string{"set_goal"}},
		{"final", []string{"complete_goal"}, []string{"complete_goal"}},
		{"post_final", []string{}, []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			reg := p1Registry()
			reg.SetAllowlist(tc.allowlist)
			reg.SetPhase(tc.phase)

			names := p1Names(reg.ToProviderDefs())
			if len(names) != len(tc.want) {
				t.Fatalf("C2-C5 violated: phase=%s projected %v (%d), want exactly %v", tc.phase, names, len(names), tc.want)
			}
			for _, w := range tc.want {
				if !p1Contains(names, w) {
					t.Fatalf("C2-C5 violated: phase=%s projected %v, missing %q", tc.phase, names, w)
				}
			}
		})
	}
}

// TestP1_CheckpointProjectsWholeRegistry — LEVER (b), plan §20.
//
// CHECKPOINT projects the whole registry, exactly like OPEN. The
// executable set stays {goal_progress, complete_goal} — the projection
// and the gate are two different sets.
func TestP1_CheckpointProjectsWholeRegistry(t *testing.T) {
	openReg := p1Registry()
	openReg.SetAllowlist(nil)
	openReg.SetPhase("open")
	openNames := p1Names(openReg.ToProviderDefs())

	reg := p1Registry()
	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("checkpoint")
	names := p1Names(reg.ToProviderDefs())

	if len(names) != len(openNames) {
		t.Fatalf("lever (b) violated: CHECKPOINT projected %v (%d), OPEN projected %v (%d) — "+
			"the CHECKPOINT wire must match the OPEN wire", names, len(names), openNames, len(openNames))
	}
	for _, want := range openNames {
		if !p1Contains(names, want) {
			t.Fatalf("lever (b) violated: CHECKPOINT projection %v is missing %q (OPEN has it)", names, want)
		}
	}
	// The 2 executable lifecycle tools must still be advertised — the
	// model must be able to call them.
	for _, want := range []string{"goal_progress", "complete_goal"} {
		if !p1Contains(names, want) {
			t.Fatalf("lever (b) violated: CHECKPOINT projection %v lost the executable tool %q", names, want)
		}
	}
}

// --- C6: the runtime execution gate is NOT weakened by P1 -----------------

func TestP1_RuntimeGateStillBlocksHiddenTools(t *testing.T) {
	reg := p1Registry()

	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("checkpoint")

	// exec is hidden from the projection at CHECKPOINT — it must still be
	// blocked at EXECUTION time. The projection is a UX/cache measure, the
	// gate is the security boundary.
	if reg.IsAllowed("exec") {
		t.Fatal("C6 violated: exec must still be blocked at CHECKPOINT even though the projection changed")
	}
	if reg.IsAllowed("read_file") {
		t.Fatal("C6 violated: read_file must still be blocked at CHECKPOINT")
	}
	if !reg.IsAllowed("goal_progress") {
		t.Fatal("C6 violated: goal_progress must remain allowed at CHECKPOINT")
	}
	if !reg.IsAllowed("complete_goal") {
		t.Fatal("C6 violated: complete_goal must remain allowed at CHECKPOINT")
	}
}

// --- C7: an unknown phase fails CLOSED ------------------------------------
//
// NOTE (2026-11-03, measured — not assumed):
//
// The original wording of this test demanded that an unknown phase project
// EXACTLY the allowlist ([set_goal]). That expectation is WRONG, and the
// measurement says why:
//
//	IsLifecycleToolAllowed() (pkg/tools/lifecycle_tool_gate.go:73-79) is
//	fail-CLOSED: when ToolPolicyForPhase(phase) returns nil (unknown
//	phase), it returns false for EVERY lifecycle tool — set_goal included.
//	toolAllowedLocked() runs that gate BEFORE the allowlist check, so at
//	an unknown phase NOTHING in the allowlist is executable.
//
//	Measured with pkg/tools/zz_c7_probe_test.go:
//
//	  phase="not_a_real_phase", allowlist=[set_goal]
//	    executable = []   (count=0)   projected = []   (count=0)
//	  phase="not_a_real_phase", allowlist=nil
//	    executable = [exec read_file]  projected = [exec read_file]
//
// So at an unknown phase the EXECUTABLE set is either empty (allowlist
// given) or exactly the non-lifecycle tools (allow-all). Projecting the
// allowlist there would advertise tools the runtime gate refuses to run —
// which is the very misfire P1 exists to remove.
//
// C7's real requirement — "an unrecognised phase must never WIDEN the
// visible set" — is therefore asserted as:
//
//	projected ⊆ executable   (never advertise what the gate will refuse)
//
// which holds at every phase, and is strictly stronger than "the visible
// set does not grow".

func TestP1_UnknownPhaseFailsClosed(t *testing.T) {
	reg := p1Registry()
	reg.SetAllowlist([]string{"set_goal"})
	reg.SetPhase("not_a_real_phase")

	names := p1Names(reg.ToProviderDefs())

	// The lifecycle gate is fail-closed at an unknown phase, so set_goal
	// is NOT executable there — and must not be advertised either.
	if p1Contains(names, "set_goal") {
		t.Fatalf("C7 violated: unknown phase projected set_goal, but the lifecycle gate refuses it — "+
			"projected %v (executable at an unknown phase is empty)", names)
	}
	if p1Contains(names, "exec") || p1Contains(names, "read_file") {
		t.Fatalf("C7 violated: unknown phase projected %v — must fail closed, never widen the visible set", names)
	}

	// The fail-closed invariant: nothing may be advertised that the gate
	// would refuse at execution time.
	for _, n := range names {
		if !reg.IsAllowed(n) {
			t.Fatalf("C7 violated: unknown phase advertised %q but the runtime gate blocks it — projected %v", n, names)
		}
	}
}

// --- The projection never hides an executable tool, and never WIDENS the
//     executable set at a narrow phase ------------------------------------
//
// Two distinct invariants, both measured (pkg/tools/zz_c7_probe_test.go):
//
//	I1  projected ⊇ executable — a tool the gate allows must be advertised,
//	    or the LLM cannot call it at all.
//
//	I2  At a NARROW phase (set / final / post_final) and at an
//	    UNKNOWN phase, projected == executable EXACTLY. This is the
//	    anti-misfire property of P1: the LLM must never be offered a tool
//	    the runtime will refuse.
//
//	    LEVER (b), plan §20: CHECKPOINT is no longer narrow — its
//	    projection is the whole registry (like OPEN) so the
//	    OPEN→CHECKPOINT transition does not move the prompt-cache
//	    prefix. At CHECKPOINT the projection is therefore a SUPERSET of
//	    the executable set, exactly like OPEN. The gate still refuses
//	    everything outside {goal_progress, complete_goal}; only the
//	    ADVERTISED set widened.
//
//	At OPEN the projection is deliberately a SUPERSET (the whole registry,
//	    byte-identical to the pre-P1 frozen projection so the prompt-cache
//	    prefix does not move). Measured at OPEN: executable = 4
//	    (view_goal, complete_goal, read_file, exec), projected = 6 — the
//	    extra two (set_goal, goal_progress) are the pre-P1 behaviour,
//	    unchanged, and the plan measured OPEN misfire at 0.29%.
//	    Narrowing OPEN is P9's job, not P1's.

func TestP1_ProjectionInvariants(t *testing.T) {
	allTools := []string{"set_goal", "view_goal", "goal_progress", "complete_goal", "read_file", "exec"}

	for _, tc := range []struct {
		phase string
		allow []string
		// exactSubset = projected must EQUAL the executable set.
		exactSubset bool
	}{
		{"", nil, true},
		{"open", nil, false}, // superset by design (cache stability)
		{"set", []string{"set_goal"}, true},
		{"checkpoint", []string{"goal_progress", "complete_goal"}, false},
		{"final", []string{"complete_goal"}, true},
		{"post_final", []string{}, true},
		{"not_a_real_phase", []string{"set_goal"}, true},
	} {
		t.Run("phase="+tc.phase, func(t *testing.T) {
			reg := p1Registry()
			reg.SetAllowlist(tc.allow)
			reg.SetPhase(tc.phase)

			names := p1Names(reg.ToProviderDefs())

			executable := []string{}
			for _, n := range allTools {
				if reg.IsAllowed(n) {
					executable = append(executable, n)
				}
			}

			// I2 (exact phases): projected == executable.
			if tc.exactSubset {
				if len(names) != len(executable) {
					t.Fatalf("I2 violated: phase=%q projected %v (%d) but executable is %v (%d) — "+
						"the projection must match the executable set exactly at a narrow/unknown phase",
						tc.phase, names, len(names), executable, len(executable))
				}
				for _, n := range executable {
					if !p1Contains(names, n) {
						t.Fatalf("I2 violated: phase=%q projected %v but executable includes %q",
							tc.phase, names, n)
					}
				}
				return
			}

			// I1 (all phases): projected ⊇ executable.
			for _, n := range executable {
				if !p1Contains(names, n) {
					t.Fatalf("I1 violated: phase=%q projection %v HID %q even though the runtime gate allows it",
						tc.phase, names, n)
				}
			}
		})
	}
}

// --- The projection never advertises a tool the gate would refuse ---------
//
// The anti-misfire property, asserted on the NARROW + UNKNOWN phases, where
// the projection is exact: everything advertised must be executable. (At
// OPEN the projection is deliberately wider — see the note above.)
//
// LEVER (b), plan §20: CHECKPOINT joins OPEN as a deliberately-WIDE
// projection (whole registry). The "never advertise a blocked tool"
// property therefore does NOT hold at CHECKPOINT any more — by design.
// What still holds there is the complementary invariant: the EXECUTABLE
// set is unchanged ({goal_progress, complete_goal}), so widening the
// wire does not widen what may run.

func TestP1_NarrowPhasesNeverAdvertiseBlockedTools(t *testing.T) {
	for _, tc := range []struct {
		phase string
		allow []string
	}{
		{"set", []string{"set_goal"}},
		{"final", []string{"complete_goal"}},
		{"post_final", []string{}},
		{"not_a_real_phase", []string{"set_goal"}},
	} {
		t.Run("phase="+tc.phase, func(t *testing.T) {
			reg := p1Registry()
			reg.SetAllowlist(tc.allow)
			reg.SetPhase(tc.phase)

			for _, n := range p1Names(reg.ToProviderDefs()) {
				if !reg.IsAllowed(n) {
					t.Fatalf("phase=%q advertised %q but the runtime gate blocks it — "+
						"that is exactly the misfire P1 removes", tc.phase, n)
				}
			}
		})
	}
}

// TestLeverB_CheckpointWireIsWideButGateStaysNarrow — LEVER (b), plan §20.
//
// The CHECKPOINT projection is the whole registry (wide), but the
// EXECUTABLE set is still exactly {goal_progress, complete_goal} (narrow).
// This test pins both halves at once, because lever (b) is only safe if
// the wire widens WITHOUT the gate widening.
func TestLeverB_CheckpointWireIsWideButGateStaysNarrow(t *testing.T) {
	reg := p1Registry()
	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("checkpoint")

	projected := p1Names(reg.ToProviderDefs())
	for _, want := range []string{"exec", "read_file"} {
		if !p1Contains(projected, want) {
			t.Fatalf("lever (b) violated: CHECKPOINT projection %v lost %q — the wire must be wide", projected, want)
		}
	}

	executable := []string{}
	for _, n := range []string{"set_goal", "view_goal", "goal_progress", "complete_goal", "read_file", "exec"} {
		if reg.IsAllowed(n) {
			executable = append(executable, n)
		}
	}
	if len(executable) != 2 {
		t.Fatalf("lever (b) violated: executable set at CHECKPOINT = %v (%d), want exactly 2 "+
			"{goal_progress, complete_goal} — the gate must NOT widen with the wire", executable, len(executable))
	}
}

// --- The projection is phase-scoped, not global ---------------------------
//
// Regression guard for the temptation to implement P1 by turning the
// projection freeze OFF wholesale: that would shrink OPEN too and break the
// prompt-cache prefix. This test pins both halves at once.
//
// LEVER (b), plan §20: CHECKPOINT now shares OPEN's whole-registry
// projection, so the "strictly fewer" comparison is no longer the right
// invariant for that phase. The invariant that IS still pinned:
//   - the NARROW phases (set / final / post_final) project strictly
//     fewer tools than OPEN (the freeze is not off wholesale), and
//   - CHECKPOINT projects EXACTLY the same set as OPEN (the cache prefix
//     must not move on the OPEN→CHECKPOINT transition).

func TestP1_ProjectionIsPhaseScopedNotGlobal(t *testing.T) {
	reg := p1Registry()
	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("checkpoint")
	checkpointNames := p1Names(reg.ToProviderDefs())

	reg.SetAllowlist(nil)
	reg.SetPhase("open")
	openNames := p1Names(reg.ToProviderDefs())

	// LEVER (b): CHECKPOINT == OPEN, exactly.
	if len(checkpointNames) != len(openNames) {
		t.Fatalf("lever (b) violated: checkpoint=%v (%d), open=%v (%d) — "+
			"the CHECKPOINT wire must match the OPEN wire so the prompt-cache prefix does not move",
			checkpointNames, len(checkpointNames), openNames, len(openNames))
	}
	for _, want := range openNames {
		if !p1Contains(checkpointNames, want) {
			t.Fatalf("lever (b) violated: checkpoint=%v missing %q (open has it)", checkpointNames, want)
		}
	}
	if !p1Contains(openNames, "exec") {
		t.Fatalf("OPEN projection lost exec: %v", openNames)
	}

	// The freeze is NOT off wholesale: a narrow phase still projects
	// strictly fewer tools than OPEN.
	reg.SetAllowlist([]string{"set_goal"})
	reg.SetPhase("set")
	setNames := p1Names(reg.ToProviderDefs())
	if len(setNames) >= len(openNames) {
		t.Fatalf("projection freeze looks off: set=%v (%d), open=%v (%d) — a narrow phase must project strictly fewer tools",
			setNames, len(setNames), openNames, len(openNames))
	}
}

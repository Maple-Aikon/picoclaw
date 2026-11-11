package tools

import (
	"context"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// Phase 12.80 — EffectiveToolCount: the number of tools that would
// actually EXECUTE right now.
//
// This is deliberately different from len(ToProviderDefs()):
//   - ToProviderDefs() returns the PROJECTED schema set. When the
//     projection is frozen (agent_init.go: SetProjectionFrozen(true)) that
//     is the WHOLE registry at every phase — a prompt-cache artifact, not
//     a policy signal.
//   - EffectiveToolCount() returns the EXECUTABLE set: registered, not
//     TTL-expired, and passing the runtime allowlist + lifecycle gate.
//
// The phase canary drift check (pkg/agent/canary_drift.go) compares the
// executable count against the phase policy's expectation. Before this
// method existed it compared the frozen projection against a per-phase
// constant, which fired on every call at every phase.

// stubTool is a minimal Tool implementation for registry tests.
type stubTool struct{ name string }

func (s *stubTool) Name() string               { return s.name }
func (s *stubTool) Description() string        { return "stub" }
func (s *stubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s *stubTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	return toolshared.NewToolResult("ok")
}

// TestEffectiveToolCount_AllowAll — no allowlist → every registered,
// non-expired tool is executable.
func TestEffectiveToolCount_AllowAll(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "alpha"})
	reg.Register(&stubTool{name: "beta"})
	reg.Register(&stubTool{name: "gamma"})

	if got := reg.EffectiveToolCount(); got != 3 {
		t.Errorf("EffectiveToolCount() = %d, want 3 (allow-all)", got)
	}
}

// TestEffectiveToolCount_AllowlistFilters — a non-nil allowlist restricts
// the executable set to its members.
func TestEffectiveToolCount_AllowlistFilters(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "alpha"})
	reg.Register(&stubTool{name: "beta"})
	reg.Register(&stubTool{name: "gamma"})

	reg.SetAllowlist([]string{"alpha", "gamma"})

	if got := reg.EffectiveToolCount(); got != 2 {
		t.Errorf("EffectiveToolCount() = %d, want 2 (allowlist alpha+gamma)", got)
	}
}

// TestEffectiveToolCount_CheckpointPhase pins the production-relevant
// case: at the CHECKPOINT phase the policy pins the allowlist to
// {goal_progress, complete_goal}, so exactly 2 tools are executable even
// though the registry holds many more.
func TestEffectiveToolCount_CheckpointPhase(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "alpha"})
	reg.Register(&stubTool{name: "beta"})
	reg.Register(&stubTool{name: "goal_progress"})
	reg.Register(&stubTool{name: "complete_goal"})

	reg.SetAllowlist([]string{"goal_progress", "complete_goal"})
	reg.SetPhase("checkpoint")

	if got := reg.EffectiveToolCount(); got != 2 {
		t.Errorf("EffectiveToolCount() at CHECKPOINT = %d, want 2", got)
	}

	// P1 (plan checkpoint-misfire-fix): the FROZEN projection used to be the
	// WHOLE registry at every phase — that phase-INDEPENDENCE was the
	// artifact that made the old canary fire on every call (it compared the
	// projected count against a per-phase baseline).
	//
	// P1 makes the projection PHASE-SCOPED: at a NARROW phase the frozen
	// projection is narrowed to the phase allowlist.
	//
	// LEVER (b), plan §20 (anh chốt 2026-11-11 07:58 ICT): CHECKPOINT
	// joins OPEN — the frozen projection there is the WHOLE registry again,
	// so the OPEN→CHECKPOINT transition no longer moves the prompt-cache
	// prefix. The EXECUTABLE set is unchanged: still exactly 2.
	//
	// The canary itself is unaffected: canary_drift.go measures
	// EffectiveToolCount() (the EXECUTABLE set), never the projected count.
	reg.SetProjectionFrozen(true)
	if got := len(reg.ToProviderDefs()); got != 4 {
		t.Errorf("frozen projection at CHECKPOINT = %d tools, want 4 (whole registry, lever (b))", got)
	}
	if got := reg.EffectiveToolCount(); got != 2 {
		t.Errorf("EffectiveToolCount() at CHECKPOINT (frozen) = %d, want 2", got)
	}

	// The OPEN phase keeps the FULL frozen projection — the prompt-cache
	// prefix must not move, or every OPEN turn pays full uncached input.
	reg.SetAllowlist(nil)
	reg.SetPhase("open")
	if got := len(reg.ToProviderDefs()); got != 4 {
		t.Errorf("frozen projection at OPEN = %d tools, want 4 (whole registry)", got)
	}
}

// TestEffectiveToolCount_LifecycleGateBlocks — the lifecycle gate is
// enforced inside toolAllowedLocked, so a lifecycle tool that is in the
// allowlist but not allowed at the phase is NOT executable.
func TestEffectiveToolCount_LifecycleGateBlocks(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "alpha"})
	reg.Register(&stubTool{name: "set_goal"})

	// set_goal is in the allowlist but the lifecycle gate blocks it at
	// CHECKPOINT (LifecycleAllowed: set_goal=false).
	reg.SetAllowlist([]string{"alpha", "set_goal"})
	reg.SetPhase("checkpoint")

	if got := reg.EffectiveToolCount(); got != 1 {
		t.Errorf("EffectiveToolCount() = %d, want 1 (set_goal blocked by lifecycle gate)", got)
	}
}

// TestEffectiveToolCount_PostFinalBlocksEverything — POST-FINAL is the
// hard guard at the top of toolAllowedLocked: no tool is executable.
func TestEffectiveToolCount_PostFinalBlocksEverything(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "alpha"})
	reg.Register(&stubTool{name: "beta"})

	reg.SetAllowlist([]string{"alpha", "beta"})
	reg.SetPhase("post_final")

	if got := reg.EffectiveToolCount(); got != 0 {
		t.Errorf("EffectiveToolCount() at POST_FINAL = %d, want 0", got)
	}
}

// TestEffectiveToolCount_EmptyAllowlistBlocksEverything — an empty
// (non-nil) allowlist means "allow none".
func TestEffectiveToolCount_EmptyAllowlistBlocksEverything(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&stubTool{name: "alpha"})

	reg.SetAllowlist([]string{})

	if got := reg.EffectiveToolCount(); got != 0 {
		t.Errorf("EffectiveToolCount() with empty allowlist = %d, want 0", got)
	}
}

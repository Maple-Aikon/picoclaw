// cache-utilization-v2 Phase 12.72 — T1.1 RED test.
//
// Fix #1 wires SetProjectionFrozen(true) at the end of registerSharedTools so
// every agent's ToolRegistry projects its full tool list to the LLM on every
// turn. The MiniMax-M3 cache model is prefix-identity — a tool that drops in
// or out of the projection changes the cached prefix and invalidates the
// slot. The runtime allowlist (Phase 12.3) still enforces per-call
// IsAllowed() correctness, so the projection freeze is purely a prompt-cache
// optimization.
//
// T1.1 (RED on HEAD): assert every agent's Tools registry has
// ProjectionFrozen()==true after NewAgentLoop constructs the loop. RED because
// no caller wires SetProjectionFrozen(true). GREEN after the wire is in place
// at agent_init.go:442.
package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// TestAgentInit_FreezesAllAgentToolProjection verifies that after
// NewAgentLoop(...) finishes, every agent's ToolRegistry has projection
// frozen — i.e. SetProjectionFrozen(true) was called inside
// registerSharedTools for each agent ID in the registry.
//
// Iterates al.registry.ListAgentIDs() (mirroring the loop in
// registerSharedTools) and asserts ProjectionFrozen()==true for every
// agent. Defensive zero-agent guard fails the test loudly instead of
// silently passing if the default config produced no agents.
func TestAgentInit_FreezesAllAgentToolProjection(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()

	al := NewAgentLoop(cfg, bus.NewMessageBus(), &mockProvider{})
	if al == nil {
		t.Fatal("NewAgentLoop returned nil")
	}

	agentIDs := al.registry.ListAgentIDs()
	if len(agentIDs) == 0 {
		t.Fatal("registry has zero agents; cannot verify freeze coverage")
	}

	for _, agentID := range agentIDs {
		gotAgent, ok := al.registry.GetAgent(agentID)
		if !ok || gotAgent == nil {
			t.Fatalf("agent %q not found in registry", agentID)
		}
		if gotAgent.Tools == nil {
			t.Fatalf("agent %q: Tools registry is nil", agentID)
		}
		if !gotAgent.Tools.ProjectionFrozen() {
			t.Errorf(
				"agent %q: Tools.ProjectionFrozen()=false, want true (SetProjectionFrozen not wired in registerSharedTools)",
				agentID,
			)
		}
	}
}
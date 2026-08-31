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

// TestFrozenToolSchema_InvariantAcrossGoalPhases verifies that when
// ToolProjectionMode is frozen, ToProviderDefs() returns identical schemas
// regardless of the active goal phase (SET, OPEN, CHECKPOINT, FINAL).
func TestFrozenToolSchema_InvariantAcrossGoalPhases(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.SetProjectionFrozen(true)

	// Register lifecycle tools and general execution tools
	reg.Register(&mockTool{name: "set_goal"})
	reg.Register(&mockTool{name: "goal_progress"})
	reg.Register(&mockTool{name: "complete_goal"})
	reg.Register(&mockTool{name: "read_file"})
	reg.Register(&mockTool{name: "exec"})

	// Phase SET
	reg.SetPhase("SET")
	defsSet := reg.ToProviderDefs()

	// Phase OPEN
	reg.SetPhase("OPEN")
	defsOpen := reg.ToProviderDefs()

	// Phase CHECKPOINT
	reg.SetPhase("CHECKPOINT")
	defsCheckpoint := reg.ToProviderDefs()

	// Phase FINAL
	reg.SetPhase("FINAL")
	defsFinal := reg.ToProviderDefs()

	if len(defsSet) != 5 || len(defsOpen) != 5 || len(defsCheckpoint) != 5 || len(defsFinal) != 5 {
		t.Fatalf("expected 5 tools projected in all phases, got Set=%d, Open=%d, Checkpoint=%d, Final=%d",
			len(defsSet), len(defsOpen), len(defsCheckpoint), len(defsFinal))
	}

	// Verify names are identical and in same sorted order
	for i := 0; i < len(defsSet); i++ {
		if defsSet[i].Function.Name != defsOpen[i].Function.Name ||
			defsSet[i].Function.Name != defsCheckpoint[i].Function.Name ||
			defsSet[i].Function.Name != defsFinal[i].Function.Name {
			t.Errorf("mismatch at index %d across phases: set=%s, open=%s, checkpoint=%s, final=%s",
				i, defsSet[i].Function.Name, defsOpen[i].Function.Name,
				defsCheckpoint[i].Function.Name, defsFinal[i].Function.Name)
		}
	}

	// Runtime Gate must still enforce phase restrictions!
	// At SET phase, 'read_file' must be blocked at execution time
	reg.SetAllowlist([]string{"set_goal"})
	reg.SetPhase("SET")
	if reg.IsAllowed("read_file") {
		t.Errorf("runtime execution gate failed: 'read_file' should not be accessible at SET phase")
	}
	if !reg.IsAllowed("set_goal") {
		t.Errorf("runtime execution gate failed: 'set_goal' should be accessible at SET phase")
	}
}

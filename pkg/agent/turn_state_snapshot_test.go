package agent

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// newSnapshotConfig returns a minimal Config whose WorkspacePath() resolves
// to ws. We avoid depending on Config defaults by constructing the minimum
// field path needed by turn_state_snapshot.go.
func newSnapshotConfig(ws string) *config.Config {
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{Workspace: ws},
		},
	}
}

// ---------------------------------------------------------------------------
// Phase 8.3 — single-path implementation (goal store as sole source of truth)
// ---------------------------------------------------------------------------

// TestLoadTaskSummary_MissingKey_ReturnsEmpty verifies the no-op contract
// when no goal has been written for the session key.
func TestLoadTaskSummary_MissingKey_ReturnsEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{cfg: newSnapshotConfig(tmpDir)}
	if got := al.loadTaskSummary("never-stored"); got != "" {
		t.Fatalf("missing key = %q, want empty", got)
	}
}

// TestLoadTaskSummary_NoCfg_ReturnsEmpty verifies graceful nil handling.
func TestLoadTaskSummary_NoCfg_ReturnsEmpty(t *testing.T) {
	al := &AgentLoop{cfg: nil}
	if got := al.loadTaskSummary("anything"); got != "" {
		t.Fatalf("nil cfg: got %q, want empty", got)
	}
}

// TestLoadTaskSummary_EmptySessionKey_ReturnsEmpty verifies that an empty
// session key never reaches the goal store.
func TestLoadTaskSummary_EmptySessionKey_ReturnsEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{cfg: newSnapshotConfig(tmpDir)}
	if got := al.loadTaskSummary(""); got != "" {
		t.Fatalf("empty session key: got %q, want empty", got)
	}
}

// TestStoreTaskSummary_EmptySummary_NoOp verifies that an empty summary
// must never get written to the goal store.
func TestStoreTaskSummary_EmptySummary_NoOp(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{cfg: newSnapshotConfig(tmpDir)}
	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-empty", &goal.Goal{
		Name:        "phase83-empty",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "t", SuccessCriteria: []string{"d"}},
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	al.storeTaskSummary("sess-empty", "") // must not panic and must not write
	snap, _ := store.LoadStatusSnapshot("sess-empty")
	if snap != "" {
		t.Fatalf("empty summary was stored: %q", snap)
	}
}

// TestStoreTaskSummary_WritesToGoalStore verifies Phase 8.3 single-path:
// the summary is written into goal.StatusSnapshot (no other side effects).
func TestStoreTaskSummary_WritesToGoalStore(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{cfg: newSnapshotConfig(tmpDir)}
	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-goal", &goal.Goal{
		Name:        "phase83-store",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "test", SuccessCriteria: []string{"done"}},
	}); err != nil {
		t.Fatalf("setup goal: %v", err)
	}

	al.storeTaskSummary("sess-goal", "phase 8.3 in flight")

	snap, err := store.LoadStatusSnapshot("sess-goal")
	if err != nil {
		t.Fatalf("LoadStatusSnapshot: %v", err)
	}
	if snap != "phase 8.3 in flight" {
		t.Fatalf("goal.StatusSnapshot = %q, want %q", snap, "phase 8.3 in flight")
	}
}

// TestLoadTaskSummary_ReadsFromGoalStore verifies Phase 8.3 single-path:
// load reads from goal.StatusSnapshot.
func TestLoadTaskSummary_ReadsFromGoalStore(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{cfg: newSnapshotConfig(tmpDir)}
	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-load-goal", &goal.Goal{
		Name:        "phase83-load",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "t", SuccessCriteria: []string{"d"}},
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := store.UpdateStatusSnapshot("sess-load-goal", "snap from disk"); err != nil {
		t.Fatalf("UpdateStatusSnapshot: %v", err)
	}

	if got := al.loadTaskSummary("sess-load-goal"); got != "snap from disk" {
		t.Fatalf("load from goal store = %q, want %q", got, "snap from disk")
	}
}

// TestLoadTaskSummary_NoActiveGoal_ReturnsEmpty verifies the no-op contract
// when set_goal hasn't been called for the session.
func TestLoadTaskSummary_NoActiveGoal_ReturnsEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{cfg: newSnapshotConfig(tmpDir)}
	if got := al.loadTaskSummary("nonexistent-session"); got != "" {
		t.Fatalf("no active goal: got %q, want empty", got)
	}
}

// TestStoreTaskSummary_NoActiveGoal_NoOp verifies Phase 8.3 — when no goal
// is set, storeTaskSummary is a silent no-op. (Phase 7 used to fall back to
// legacy map; Phase 8.3 removed that path. Cross-turn context recovery is
// only available once set_goal has been called.)
func TestStoreTaskSummary_NoActiveGoal_NoOp(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{cfg: newSnapshotConfig(tmpDir)}
	// No goal has been written yet — must not panic and must not error.
	al.storeTaskSummary("nonexistent-session", "no goal here")
	// Verify no file was created on disk.
	store := goal.NewStore(tmpDir)
	snap, _ := store.LoadStatusSnapshot("nonexistent-session")
	if snap != "" {
		t.Fatalf("no-goal store leaked to disk: %q", snap)
	}
}

// TestDeleteTaskSummary_PreservesGoalFile verifies the Phase 7+ contract
// (preserved in 8.3): /clear does NOT clear the goal file's StatusSnapshot
// because the goal store is the system of record.
func TestDeleteTaskSummary_PreservesGoalFile(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{cfg: newSnapshotConfig(tmpDir)}
	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-clear", &goal.Goal{
		Name:        "phase83-clear",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "t", SuccessCriteria: []string{"d"}},
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := store.UpdateStatusSnapshot("sess-clear", "important context"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	al.deleteTaskSummary("sess-clear")

	if snap, _ := store.LoadStatusSnapshot("sess-clear"); snap != "important context" {
		t.Fatalf("after delete: snapshot = %q, want preserved %q", snap, "important context")
	}
}

// TestDeleteTaskSummary_IdempotentNoCfg verifies the no-op contract holds
// even with no config wired.
func TestDeleteTaskSummary_IdempotentNoCfg(t *testing.T) {
	al := &AgentLoop{cfg: nil}
	al.deleteTaskSummary("anything") // must not panic
	al.deleteTaskSummary("")         // empty key must also not panic
}

// TestCrossTurnContextRecovery_GoalModeOnly is the Phase 8.3 success
// criterion: turn 1 stores a summary, turn 2 reads it via a fresh agent
// loop instance — demonstrating the goal file is the single source of
// truth for cross-turn context.
func TestCrossTurnContextRecovery_GoalModeOnly(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := newSnapshotConfig(tmpDir)
	alTurn1 := &AgentLoop{cfg: cfg}
	alTurn2 := &AgentLoop{cfg: cfg}

	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-cross-turn", &goal.Goal{
		Name:        "phase83-cross",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "multi-turn", SuccessCriteria: []string{"recovered"}},
	}); err != nil {
		t.Fatalf("setup goal: %v", err)
	}

	alTurn1.storeTaskSummary("sess-cross-turn", "step 1 completed, ready for step 2")

	// Fresh process: turn 2 reads from disk only.
	got := alTurn2.loadTaskSummary("sess-cross-turn")
	if got != "step 1 completed, ready for step 2" {
		t.Fatalf("turn 2 retrieval = %q, want %q", got, "step 1 completed, ready for step 2")
	}
	if snap, _ := store.LoadStatusSnapshot("sess-cross-turn"); snap != got {
		t.Fatalf("disk read and loadTaskSummary diverge: %q vs %q", snap, got)
	}
}

// TestGoalWorkspace_NoCfg_ReturnsEmpty verifies graceful nil handling.
func TestGoalWorkspace_NoCfg_ReturnsEmpty(t *testing.T) {
	al := &AgentLoop{cfg: nil}
	if got := al.goalWorkspace(); got != "" {
		t.Fatalf("nil cfg: got %q, want empty", got)
	}
}

// TestGoalWorkspace_EmptyConfig_ReturnsEmpty verifies that even when cfg is
// non-nil, an unset Workspace field resolves to "".
func TestGoalWorkspace_EmptyConfig_ReturnsEmpty(t *testing.T) {
	al := &AgentLoop{cfg: newSnapshotConfig("")}
	if got := al.goalWorkspace(); got != "" {
		t.Fatalf("empty workspace: got %q, want empty", got)
	}
}

// Suppress unused imports when test helpers are unused in some configs.
var _ = atomic.LoadInt64

// ---------------------------------------------------------------------------
// Phase 8.2 — read-side fallback chain (plan §3.3, §3.4)
// ---------------------------------------------------------------------------

// TestBuildRawTextReminder_Simple verifies the helper concatenates user
// message and last assistant tail verbatim, with the tail capped at 200.
func TestBuildRawTextReminder_Simple(t *testing.T) {
	got := buildRawTextReminder("user says X", "assistant says Y")
	want := "user says X | assistant says Y"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildRawTextReminder_TailCap verifies the 200-char cap on the
// assistant tail. We construct a 500-char tail and expect only the last
// 200 chars to appear in the reminder.
func TestBuildRawTextReminder_TailCap(t *testing.T) {
	tail := strings.Repeat("Z", 500)
	got := buildRawTextReminder("hi", tail)
	want := "hi | " + strings.Repeat("Z", 200)
	if got != want {
		t.Errorf("got len=%d, want len=%d\ngot=%q\nwant=%q", len(got), len(want), got, want)
	}
	if !strings.HasSuffix(got, strings.Repeat("Z", 200)) {
		t.Errorf("tail was not capped to 200 chars: %q", got)
	}
}

// TestBuildRawTextReminder_EmptyTail verifies the helper still returns
// the user message even when the assistant tail is empty.
func TestBuildRawTextReminder_EmptyTail(t *testing.T) {
	got := buildRawTextReminder("user only", "")
	if got != "user only" {
		t.Errorf("got %q, want %q", got, "user only")
	}
}

// TestBuildRawTextReminder_EmptyUser verifies the helper returns just
// the trimmed tail when the user message is empty.
func TestBuildRawTextReminder_EmptyUser(t *testing.T) {
	got := buildRawTextReminder("", "tail only")
	if got != "tail only" {
		t.Errorf("got %q, want %q", got, "tail only")
	}
}

// TestBuildRawTextReminder_TotalCap verifies the overall 280-char cap on
// the rendered reminder. A very long user message + tail must be trimmed
// to keep within the budget.
func TestBuildRawTextReminder_TotalCap(t *testing.T) {
	userLong := strings.Repeat("u", 200)
	tailLong := strings.Repeat("t", 200)
	got := buildRawTextReminder(userLong, tailLong)
	if len(got) > 280 {
		t.Errorf("reminder len=%d exceeds 280 cap: %q", len(got), got)
	}
}

// TestLastAssistantContent_Found verifies the helper walks the history in
// reverse and returns the most recent assistant message's Content.
func TestLastAssistantContent_Found(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "first a"},
		{Role: "tool", Content: "tool result"},
		{Role: "assistant", Content: "second a"},
	}
	if got := lastAssistantContent(history); got != "second a" {
		t.Errorf("got %q, want %q", got, "second a")
	}
}

// TestLastAssistantContent_None verifies the helper returns "" when no
// assistant message is present.
func TestLastAssistantContent_None(t *testing.T) {
	history := []providers.Message{
		{Role: "user", Content: "u1"},
		{Role: "tool", Content: "t1"},
	}
	if got := lastAssistantContent(history); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
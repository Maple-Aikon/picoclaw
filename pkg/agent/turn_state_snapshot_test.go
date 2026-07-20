package agent

import (
	"sync/atomic"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/config"
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

// TestLoadTaskSummary_LegacyMode_NoFlag verifies useGoalProgress=false
// (Phase 1-5 default) continues to use the in-memory sync.Map. The flag
// stays off through steps 2-3 so existing behavior is preserved unchanged.
func TestLoadTaskSummary_LegacyMode_NoFlag(t *testing.T) {
	al := &AgentLoop{useGoalProgress: false}
	al.legacyTaskSummary.Store("sess-legacy", "alpha legacy")

	got := al.loadTaskSummary("sess-legacy")
	if got != "alpha legacy" {
		t.Fatalf("loadTaskSummary legacy = %q, want %q", got, "alpha legacy")
	}
}

// TestStoreTaskSummary_LegacyMode_NoFlag verifies the legacy store path
// keeps working when useGoalProgress=false.
func TestStoreTaskSummary_LegacyMode_NoFlag(t *testing.T) {
	al := &AgentLoop{useGoalProgress: false}
	al.storeTaskSummary("sess-legacy", "storing now")
	got := al.loadTaskSummary("sess-legacy")
	if got != "storing now" {
		t.Fatalf("after store legacy = %q, want %q", got, "storing now")
	}
}

// TestStoreTaskSummary_EmptySummary_NoOp verifies that an empty summary
// must never get written to either store.
func TestStoreTaskSummary_EmptySummary_NoOp(t *testing.T) {
	al := &AgentLoop{useGoalProgress: false}
	al.storeTaskSummary("sess-empty", "") // must not panic
	if val, ok := al.legacyTaskSummary.Load("sess-empty"); ok {
		t.Fatalf("empty summary was stored: %v", val)
	}
}

// TestDeleteTaskSummary_LegacyMode_NoFlag verifies the legacy delete path.
func TestDeleteTaskSummary_LegacyMode_NoFlag(t *testing.T) {
	al := &AgentLoop{useGoalProgress: false}
	al.legacyTaskSummary.Store("sess-del", "before")
	al.deleteTaskSummary("sess-del")
	if _, ok := al.legacyTaskSummary.Load("sess-del"); ok {
		t.Fatalf("delete did not remove entry")
	}
	// Idempotent.
	al.deleteTaskSummary("sess-del")
}

// TestLoadTaskSummary_LegacyMode_MissingKey_ReturnsEmpty verifies the
// legacy map miss returns "" without an error.
func TestLoadTaskSummary_LegacyMode_MissingKey_ReturnsEmpty(t *testing.T) {
	al := &AgentLoop{useGoalProgress: false}
	if got := al.loadTaskSummary("never-stored"); got != "" {
		t.Fatalf("missing key = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// useGoalProgress=true path (Phase 7 step 4 — goal store as source of truth)
// ---------------------------------------------------------------------------

// TestStoreTaskSummary_GoalMode_WritesToGoalStore verifies useGoalProgress=true
// writes the summary into goal.StatusSnapshot (not the legacy map).
func TestStoreTaskSummary_GoalMode_WritesToGoalStore(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{
		useGoalProgress: true,
		cfg:            newSnapshotConfig(tmpDir),
	}
	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-goal", &goal.Goal{
		Name:        "phase7-test",
		Status:      goal.StatusActive,
		Description: goal.Description{Objective: "test", SuccessCriteria: []string{"done"}},
	}); err != nil {
		t.Fatalf("setup goal: %v", err)
	}

	al.storeTaskSummary("sess-goal", "phase 7 in flight")

	snap, err := store.LoadStatusSnapshot("sess-goal")
	if err != nil {
		t.Fatalf("LoadStatusSnapshot: %v", err)
	}
	if snap != "phase 7 in flight" {
		t.Fatalf("goal.StatusSnapshot = %q, want %q", snap, "phase 7 in flight")
	}
}

// TestLoadTaskSummary_GoalMode_ReadsFromGoalStore verifies useGoalProgress=true
// reads the summary from the goal store, not the legacy sync.Map.
func TestLoadTaskSummary_GoalMode_ReadsFromGoalStore(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{
		useGoalProgress: true,
		cfg:            newSnapshotConfig(tmpDir),
	}
	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-load-goal", &goal.Goal{
		Name:        "phase7-load-test",
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

	// Legacy map must remain ignored: even if it has a stale value, the
	// goal mode reads from disk only.
	al.legacyTaskSummary.Store("sess-load-goal", "stale map value")
	if got := al.loadTaskSummary("sess-load-goal"); got != "snap from disk" {
		t.Fatalf("goal mode leaked from legacy map: got %q, want %q", got, "snap from disk")
	}
}

// TestLoadTaskSummary_GoalMode_NoActiveGoal_ReturnsEmpty verifies the no-op
// contract when set_goal hasn't been called.
func TestLoadTaskSummary_GoalMode_NoActiveGoal_ReturnsEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{
		useGoalProgress: true,
		cfg:            newSnapshotConfig(tmpDir),
	}
	if got := al.loadTaskSummary("nonexistent-session"); got != "" {
		t.Fatalf("no active goal: got %q, want empty", got)
	}
}

// TestStoreTaskSummary_GoalMode_NoActiveGoal_StillPersistsLegacy verifies
// that when the goal store has no goal for this session (pre-set_goal),
// storeTaskSummary still preserves the cross-turn context by writing to
// the legacy map. This is the Phase 7 graceful-degradation contract:
// set_goal is not a precondition for cross-turn recovery.
func TestStoreTaskSummary_GoalMode_NoActiveGoal_StillPersistsLegacy(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{
		useGoalProgress: true,
		cfg:            newSnapshotConfig(tmpDir),
	}
	// No goal has been written yet for this session.
	al.storeTaskSummary("nonexistent-session", "still need to track this")

	// Verify the legacy map absorbed the summary (fallback path).
	if val, ok := al.legacyTaskSummary.Load("nonexistent-session"); !ok {
		t.Fatalf("legacy fallback did not happen")
	} else if val != "still need to track this" {
		t.Fatalf("legacy fallback = %q, want %q", val, "still need to track this")
	}
}

// TestDeleteTaskSummary_GoalMode_DoesNotClearGoalFile verifies that even in
// goal mode, /clear does NOT clear the goal file (Phase 4 established the
// goal file as the system of record).
func TestDeleteTaskSummary_GoalMode_DoesNotClearGoalFile(t *testing.T) {
	tmpDir := t.TempDir()
	al := &AgentLoop{
		useGoalProgress: true,
		cfg:            newSnapshotConfig(tmpDir),
	}
	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-clear", &goal.Goal{
		Name:        "phase7-clear-test",
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

// TestPhase7_CrossTurnContextRecovery_GoalMode is the Phase 7 (plan §3.7)
// success criterion: turn 1 stores a summary, turn 2 reads it via a fresh
// agent loop instance — demonstrating the goal file is the single source
// of truth for cross-turn context.
func TestPhase7_CrossTurnContextRecovery_GoalMode(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := newSnapshotConfig(tmpDir)
	alTurn1 := &AgentLoop{useGoalProgress: true, cfg: cfg}
	alTurn2 := &AgentLoop{useGoalProgress: true, cfg: cfg}

	store := goal.NewStore(tmpDir)
	if err := store.Write("sess-cross-turn", &goal.Goal{
		Name:        "phase7-cross",
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
	al := &AgentLoop{useGoalProgress: true, cfg: nil}
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

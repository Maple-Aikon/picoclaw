package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// makeActiveGoalInWorkspace creates a workspace directory and writes an
// active goal file there so finalizeGoalOnTurnEnd has something to archive.
// Returns the workspace path.
func makeActiveGoalInWorkspace(t *testing.T, sessionKey string) string {
	t.Helper()
	ws := t.TempDir()
	store := goal.NewStore(ws)
	now := time.Now().UTC()
	g := &goal.Goal{
		Name: "phase6-finalize-test",
		Description: goal.Description{
			Objective:        "test objective",
			SuccessCriteria:  []string{"pass all phase 6 tests"},
			InScope:          []string{"hook 1-4"},
			OutOfScope:       []string{"phase 7"},
			Cadence:          "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Write(sessionKey, g); err != nil {
		t.Fatalf("setup: Write goal: %v", err)
	}
	return ws
}

// TestFinalizeGoalOnTurnEnd_ActiveGoal_Archives verifies Hook 1 happy path:
// an active goal is transitioned to StatusAborted with AbortedAt +
// AbortReason populated and written back to disk.
func TestFinalizeGoalOnTurnEnd_ActiveGoal_Archives(t *testing.T) {
	sessionKey := "telegram-5680819959-test"
	ws := makeActiveGoalInWorkspace(t, sessionKey)

	ts := &turnState{
		agent: &AgentInstance{
			Workspace: ws,
		},
		sessionKey: sessionKey,
	}

	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonToolPanic); err != nil {
		t.Fatalf("finalizeGoalOnTurnEnd: %v", err)
	}

	// Re-read goal from disk and verify status/fields.
	store := goal.NewStore(ws)
	g, err := store.Read(sessionKey)
	if err != nil {
		t.Fatalf("re-read after finalize: %v", err)
	}
	if g.Status != goal.StatusAborted {
		t.Fatalf("status = %q, want %q", g.Status, goal.StatusAborted)
	}
	if g.AbortedAt == nil {
		t.Fatalf("AbortedAt = nil, want set")
	}
	if time.Since(*g.AbortedAt) > 5*time.Second {
		t.Fatalf("AbortedAt = %v, want recent", *g.AbortedAt)
	}
	if g.AbortReason != GoalAbortReasonToolPanic {
		t.Fatalf("AbortReason = %q, want %q", g.AbortReason, GoalAbortReasonToolPanic)
	}
	if g.Name != "phase6-finalize-test" {
		t.Fatalf("name = %q, want phase6-finalize-test", g.Name)
	}
}

// TestFinalizeGoalOnTurnEnd_AlreadyArchived_Idempotent verifies the
// no-op contract when the goal is already non-active (idempotent call).
func TestFinalizeGoalOnTurnEnd_AlreadyArchived_Idempotent(t *testing.T) {
	sessionKey := "telegram-5680819959-test"
	ws := makeActiveGoalInWorkspace(t, sessionKey)

	// First call: archive.
	ts := &turnState{agent: &AgentInstance{Workspace: ws}, sessionKey: sessionKey}
	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonToolPanic); err != nil {
		t.Fatalf("first finalize: %v", err)
	}

	// Capture the first AbortedAt for comparison.
	store := goal.NewStore(ws)
	g1, _ := store.Read(sessionKey)
	if g1.AbortedAt == nil {
		t.Fatalf("expected AbortedAt set after first call")
	}
	firstAbortedAt := *g1.AbortedAt
	firstUpdatedAt := g1.UpdatedAt

	// Sleep so UpdatedAt differs if hook were non-idempotent.
	time.Sleep(20 * time.Millisecond)

	// Second call: should be a no-op (idempotent).
	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonRunTurnPanic); err != nil {
		t.Fatalf("second finalize: %v", err)
	}

	g2, _ := store.Read(sessionKey)
	if !g2.AbortedAt.Equal(firstAbortedAt) {
		t.Fatalf("AbortedAt changed across idempotent calls: %v → %v", firstAbortedAt, *g2.AbortedAt)
	}
	if !g2.UpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("UpdatedAt changed across idempotent calls: %v → %v", firstUpdatedAt, g2.UpdatedAt)
	}
	if g2.AbortReason != GoalAbortReasonToolPanic {
		t.Fatalf("AbortReason = %q, want original %q (idempotent must preserve)",
			g2.AbortReason, GoalAbortReasonToolPanic)
	}
}

// TestFinalizeGoalOnTurnEnd_NoWorkspace_NoOp verifies the no-op contract
// when the agent has no Workspace (transient / test mode).
func TestFinalizeGoalOnTurnEnd_NoWorkspace_NoOp(t *testing.T) {
	ts := &turnState{agent: &AgentInstance{Workspace: ""}, sessionKey: "any"}
	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonRunTurnPanic); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
}

// TestFinalizeGoalOnTurnEnd_NilAgent_NoOp verifies the no-op contract for
// a nil-agent turnState (defensive guard).
func TestFinalizeGoalOnTurnEnd_NilAgent_NoOp(t *testing.T) {
	var ts *turnState
	if err := ts.finalizeGoalOnTurnEnd("anything"); err != nil {
		t.Fatalf("expected no-op on nil ts, got error: %v", err)
	}
}

// TestFinalizeGoalOnTurnEnd_NoGoalFile_NoOp verifies the no-op contract
// when the session has no goal file at all (e.g. transient commands).
func TestFinalizeGoalOnTurnEnd_NoGoalFile_NoOp(t *testing.T) {
	ws := t.TempDir()
	ts := &turnState{
		agent:      &AgentInstance{Workspace: ws},
		sessionKey: "no-such-session",
	}
	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonToolPanic); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
}

// TestFinalizeGoalOnTurnEnd_EmptySessionKey_NoOp verifies the safety guard
// when sessionKey is empty (should never happen in production but guards
// against partial turn state during init).
func TestFinalizeGoalOnTurnEnd_EmptySessionKey_NoOp(t *testing.T) {
	ws := t.TempDir()
	ts := &turnState{agent: &AgentInstance{Workspace: ws}, sessionKey: ""}
	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonToolPanic); err != nil {
		t.Fatalf("expected no-op on empty sessionKey, got error: %v", err)
	}
}

// TestFinalizeGoalOnTurnEnd_CompletedGoal_NotReArchived verifies that a
// goal in StatusCompleted is NOT demoted to StatusAborted (only active
// goals get force-archived).
func TestFinalizeGoalOnTurnEnd_CompletedGoal_NotReArchived(t *testing.T) {
	sessionKey := "telegram-5680819959-test"
	ws := t.TempDir()
	store := goal.NewStore(ws)
	now := time.Now().UTC()
	g := &goal.Goal{
		Name: "completed-test",
		Description: goal.Description{
			Objective:       "done",
			SuccessCriteria: []string{"marker"},
		},
		Status:    goal.StatusCompleted,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.Write(sessionKey, g); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ts := &turnState{agent: &AgentInstance{Workspace: ws}, sessionKey: sessionKey}
	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonToolPanic); err != nil {
		t.Fatalf("finalize on completed goal: %v", err)
	}

	g2, _ := store.Read(sessionKey)
	if g2.Status != goal.StatusCompleted {
		t.Fatalf("status = %q, want %q (completed must not demote to aborted)",
			g2.Status, goal.StatusCompleted)
	}
	if g2.AbortReason != "" {
		t.Fatalf("AbortReason = %q, want empty (completed goal untouched)", g2.AbortReason)
	}
}

// TestFinalizeGoalOnTurnEnd_AbortedGoal_NotModified verifies that a goal
// already in StatusAborted is not modified by a subsequent call (idempotent
// preserves original AbortedAt + AbortReason even when caller passes a
// different reason).
func TestFinalizeGoalOnTurnEnd_AbortedGoal_NotModified(t *testing.T) {
	sessionKey := "telegram-5680819959-test"
	ws := t.TempDir()
	store := goal.NewStore(ws)
	now := time.Now().UTC()
	g := &goal.Goal{
		Name: "already-aborted",
		Description: goal.Description{
			Objective:       "aborted earlier",
			SuccessCriteria: []string{"marker"},
		},
		Status:      goal.StatusAborted,
		CreatedAt:   now.Add(-time.Hour),
		UpdatedAt:   now.Add(-time.Minute),
		AbortedAt:   &now,
		AbortReason: GoalAbortReasonRunTurnPanic,
	}
	if err := store.Write(sessionKey, g); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ts := &turnState{agent: &AgentInstance{Workspace: ws}, sessionKey: sessionKey}
	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonToolPanic); err != nil {
		t.Fatalf("finalize on already-aborted: %v", err)
	}

	g2, _ := store.Read(sessionKey)
	if g2.AbortReason != GoalAbortReasonRunTurnPanic {
		t.Fatalf("AbortReason = %q, want %q (original preserved)",
			g2.AbortReason, GoalAbortReasonRunTurnPanic)
	}
	if g2.AbortedAt == nil || !g2.AbortedAt.Equal(now) {
		t.Fatalf("AbortedAt = %v, want original %v", g2.AbortedAt, now)
	}
}

// TestGoalArchiveRequestedFromState_NoFlag verifies the helper returns ""
// when the turn has not requested archive.
func TestGoalArchiveRequestedFromState_NoFlag(t *testing.T) {
	ts := &turnState{}
	if reason := ts.goalArchiveRequestedFromState(); reason != "" {
		t.Fatalf("expected empty reason, got %q", reason)
	}
}

// TestGoalArchiveRequestedFromState_NilSafe verifies nil-receiver guard.
func TestGoalArchiveRequestedFromState_NilSafe(t *testing.T) {
	var ts *turnState
	if reason := ts.goalArchiveRequestedFromState(); reason != "" {
		t.Fatalf("expected empty reason on nil, got %q", reason)
	}
}

// TestGoalArchiveRequestedFromState_FlagSet verifies the helper returns
// the bexhausted reason when the trigger flag is set.
func TestGoalArchiveRequestedFromState_FlagSet(t *testing.T) {
	ts := &turnState{goalArchiveRequested: true}
	reason := ts.goalArchiveRequestedFromState()
	if !strings.HasPrefix(reason, GoalAbortReasonBexhausted) {
		t.Fatalf("expected bexhausted prefix, got %q", reason)
	}
}

// TestFinalizeGoalOnTurnEnd_DiskFileContainsAbortMetadata verifies the
// on-disk YAML file actually contains the abort metadata so it can be
// inspected by an admin tool or the next view_goal call.
func TestFinalizeGoalOnTurnEnd_DiskFileContainsAbortMetadata(t *testing.T) {
	sessionKey := "telegram-5680819959-test"
	ws := makeActiveGoalInWorkspace(t, sessionKey)
	ts := &turnState{agent: &AgentInstance{Workspace: ws}, sessionKey: sessionKey}

	if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonBexhausted + ":hook_replay"); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// Locate the goal file in workspace/memory/goal/<slug>.md and read it.
	matches, err := filepath.Glob(filepath.Join(ws, "memory", "goal", "*.md"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 goal file, got %d", len(matches))
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read goal file: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "status: aborted") {
		t.Fatalf("goal file missing status: aborted\n---\n%s", text)
	}
	if !strings.Contains(text, "abort_reason:") {
		t.Fatalf("goal file missing abort_reason\n---\n%s", text)
	}
	if !strings.Contains(text, "aborted_at:") {
		t.Fatalf("goal file missing aborted_at\n---\n%s", text)
	}
	if !strings.Contains(text, "Aborted at") {
		t.Fatalf("goal file body missing 'Aborted at' line\n---\n%s", text)
	}
	if !strings.Contains(text, "Abort reason") {
		t.Fatalf("goal file body missing 'Abort reason' line\n---\n%s", text)
	}
}
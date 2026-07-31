package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// makeSeedGoalForSnapshot writes a fresh active goal file under
// <workspace>/memory/goal/<sessionKey>.md and returns the workspace path.
func makeSeedGoalForSnapshot(t *testing.T, sessionKey string) string {
	t.Helper()
	ws := t.TempDir()
	store := goal.NewStore(ws)
	g := &goal.Goal{
		Name:      "snapshot-test",
		Status:    goal.StatusActive,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Description: goal.Description{
			Objective:        "verify multi-turn test",
			SuccessCriteria:  []string{"step 1 done", "step 2 done"},
			InScope:          []string{"file editing"},
			Cadence:          "",
		},
	}
	if err := store.Write(sessionKey, g); err != nil {
		t.Fatalf("seed write failed: %v", err)
	}
	return ws
}

// TestLoadGoalSnapshotForHint_ReturnsRenderedHeader verifies the happy
// path: workspace + active goal → RenderHeader() output.
func TestLoadGoalSnapshotForHint_ReturnsRenderedHeader(t *testing.T) {
	ws := makeSeedGoalForSnapshot(t, "sk_v1_aaa")
	got := loadGoalSnapshotForHint(ws, "sk_v1_aaa")
	if got == "" {
		t.Fatal("expected non-empty snapshot for active goal")
	}
	// RenderHeader content includes the objective and success criteria.
	mustContain := []string{
		"## Goal: snapshot-test",
		"**Objective:** verify multi-turn test",
		"step 1 done",
		"step 2 done",
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("snapshot missing %q\n--- snapshot ---\n%s", s, got)
		}
	}
}

// TestLoadGoalSnapshotForHint_EmptyInputsNoPanic verifies the empty-input
// fail-closed path. workspace="" OR sessionKey="" → "" (no panic, no
// false-positive on a missing goal).
func TestLoadGoalSnapshotForHint_EmptyInputsNoPanic(t *testing.T) {
	tests := []struct {
		name       string
		workspace  string
		sessionKey string
	}{
		{"empty_workspace", "", "sk_v1_x"},
		{"empty_session", "/tmp/whatever", ""},
		{"both_empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := loadGoalSnapshotForHint(tt.workspace, tt.sessionKey)
			if got != "" {
				t.Errorf("expected empty for %s; got %q", tt.name, got)
			}
		})
	}
}

// TestLoadGoalSnapshotForHint_NoGoalFile verifies missing goal file → "".
// Per fail-closed semantics.
func TestLoadGoalSnapshotForHint_NoGoalFile(t *testing.T) {
	ws := t.TempDir()
	got := loadGoalSnapshotForHint(ws, "sk_v1_nonexistent")
	if got != "" {
		t.Errorf("expected empty for missing goal; got %q", got)
	}
}

// TestLoadGoalSnapshotForHint_CompletedGoalSkipped verifies terminal-state
// goals (completed/archived) return "" — no point injecting goal context
// for a finalized goal.
func TestLoadGoalSnapshotForHint_CompletedGoalSkipped(t *testing.T) {
	ws := t.TempDir()
	store := goal.NewStore(ws)
	g := &goal.Goal{
		Name:      "done-goal",
		Status:    goal.StatusCompleted,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Description: goal.Description{
			Objective:       "old goal",
			SuccessCriteria: []string{"x"},
		},
	}
	if err := store.Write("sk_v1_done", g); err != nil {
		t.Fatalf("seed write failed: %v", err)
	}
	got := loadGoalSnapshotForHint(ws, "sk_v1_done")
	if got != "" {
		t.Errorf("expected empty for completed goal; got %q", got)
	}
}

package agent

// Phase 11: tests for the per-turn goal workspace wiring (the cross-turn
// StatusSnapshot mechanism that this file covered through Phase 10.1.1
// has been removed — see pkg/agent/turn_state_snapshot.go for the new
// thin shell + pkg/agent/turn_state_finalize.go for the stale-goal
// recovery hook).

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/config"
)

// TestGoalWorkspace_EmptyCfg verifies graceful nil handling when no
// AgentLoop config is wired. Matches the pre-Phase-11 contract.
func TestGoalWorkspace_EmptyCfg(t *testing.T) {
	al := &AgentLoop{}
	if got := al.goalWorkspace(); got != "" {
		t.Fatalf("expected empty workspace, got %q", got)
	}
	if got := al.goalStore(); got != nil {
		t.Fatalf("expected nil store, got %v", got)
	}
}

// TestGoalStore_BackendPath verifies goalStore() returns a non-nil
// Store rooted at the configured workspace. Phase 11: the per-turn
// stale recovery + view_goal injection paths need a usable store.
func TestGoalStore_BackendPath(t *testing.T) {
	tmp := t.TempDir()
	al := &AgentLoop{
		cfg: &config.Config{
			Agents: config.AgentsConfig{
				Defaults: config.AgentDefaults{
					Workspace: tmp,
				},
			},
		},
	}
	store := al.goalStore()
	if store == nil {
		t.Fatalf("expected non-nil store")
	}
	// Verify the store writes goals under <workspace>/memory/goal/.
	target := filepath.Join(tmp, "memory", "goal", "test-session.md")
	if err := store.Write("test-session", stubGoalCompleted("g1")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("expected goal file at %q: %v", target, err)
	}
}

// TestArchiveStaleGoalOnTurnStart verifies the Phase 11 stale recovery
// hook: if a previous turn left a StatusActive goal on disk, the next
// turn's SetupTurn path must archive it with StatusAborted +
// AbortReason="stale_turn_boundary" so the LLM is not confused by a
// stale cross-turn goal.
func TestArchiveStaleGoalOnTurnStart(t *testing.T) {
	tmp := t.TempDir()
	al := &AgentLoop{
		cfg: &config.Config{
			Agents: config.AgentsConfig{
				Defaults: config.AgentDefaults{
					Workspace: tmp,
				},
			},
		},
	}
	store := al.goalStore()
	if err := store.Write("stale-session", stubGoalActive("stale-1")); err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	// Pre-condition: file present, status=active.
	pre, err := store.Read("stale-session")
	if err != nil {
		t.Fatalf("pre Read: %v", err)
	}
	if pre.Status != "active" {
		t.Fatalf("pre Status=%q, want active", pre.Status)
	}

	if err := archiveStaleGoalOnTurnStart(al, "stale-session"); err != nil {
		t.Fatalf("archiveStaleGoalOnTurnStart: %v", err)
	}

	// Post-condition: file moved to archive/. Active file should not
	// exist (Read returns nil goal, not error).
	postActive, err := store.Read("stale-session")
	if err != nil {
		t.Fatalf("post Read: %v", err)
	}
	if postActive != nil {
		t.Fatalf("expected active file to be moved to archive/, got %+v", postActive)
	}
	post, err := store.ReadAny("stale-session")
	if err != nil {
		t.Fatalf("post ReadAny: %v", err)
	}
	if post == nil {
		t.Fatalf("expected archive to be readable via ReadAny, got nil")
	}
	if post.Status != "aborted" {
		t.Fatalf("post Status=%q, want aborted", post.Status)
	}
	if post.AbortReason != "stale_turn_boundary" {
		t.Fatalf("AbortReason=%q, want stale_turn_boundary", post.AbortReason)
	}
	if post.AbortedAt == nil {
		t.Fatalf("AbortedAt should be non-nil")
	}
}

// TestArchiveStaleGoalOnTurnStart_NoActiveGoal verifies the hook is
// a no-op when no goal exists for the session (defense-in-depth: must
// not error or create spurious archive files).
func TestArchiveStaleGoalOnTurnStart_NoActiveGoal(t *testing.T) {
	tmp := t.TempDir()
	al := &AgentLoop{
		cfg: &config.Config{
			Agents: config.AgentsConfig{
				Defaults: config.AgentDefaults{
					Workspace: tmp,
				},
			},
		},
	}
	if err := archiveStaleGoalOnTurnStart(al, "no-such-session"); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
	// Verify no archive file was created.
	matches, err := filepath.Glob(filepath.Join(tmp, "memory", "goal", "archive", "*"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected empty archive dir, got %v", matches)
	}
}

// stubGoalCompleted returns a minimal completed goal for tests that
// just need a file on disk.
func stubGoalCompleted(name string) *goal.Goal {
	now := time.Now().UTC()
	return &goal.Goal{
		Name: name,
		Description: goal.Description{
			Objective:        "test objective",
			SuccessCriteria: []string{"test criterion"},
		},
		CreatedAt: now,
		UpdatedAt: now,
		Status:    goal.StatusCompleted,
	}
}

// stubGoalActive returns a minimal ACTIVE goal — used to simulate a
// crash-leaked goal that archiveStaleGoalOnTurnStart should sweep.
func stubGoalActive(name string) *goal.Goal {
	now := time.Now().UTC()
	return &goal.Goal{
		Name: name,
		Description: goal.Description{
			Objective:        "test objective",
			SuccessCriteria: []string{"test criterion"},
		},
		CreatedAt: now,
		UpdatedAt: now,
		Status:    goal.StatusActive,
	}
}

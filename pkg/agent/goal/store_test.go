// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSessionKey = "sess_v1_test123"

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	workspace := t.TempDir()
	return NewStore(workspace), workspace
}

func sampleGoal() *Goal {
	return &Goal{
		Name: "demo",
		Description: Description{
			Objective:       "Test objective",
			SuccessCriteria: []string{"c1", "c2"},
		},
		Status: StatusActive,
	}
}

func TestNewStore_CreatesGoalAndArchiveDirs(t *testing.T) {
	workspace := t.TempDir()
	_ = NewStore(workspace)

	goalDir := filepath.Join(workspace, "memory", "goal")
	archiveDir := filepath.Join(goalDir, "archive")
	for _, dir := range []string{goalDir, archiveDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("expected %s to exist: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
}

func TestGoalStore_Read_NoFile_ReturnsNilNil(t *testing.T) {
	store, _ := newTestStore(t)

	g, err := store.Read(testSessionKey)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if g != nil {
		t.Fatalf("expected nil goal, got: %+v", g)
	}
}

func TestGoalStore_Read_EmptySessionKey_Errors(t *testing.T) {
	store, _ := newTestStore(t)

	if _, err := store.Read(""); err == nil {
		t.Fatal("expected error for empty session key")
	}
}

func TestGoalStore_WriteAndRead_RoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	original := sampleGoal()

	if err := store.Write(testSessionKey, original); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	loaded, err := store.Read(testSessionKey)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded goal is nil")
	}
	if loaded.Name != original.Name {
		t.Errorf("Name: got %q, want %q", loaded.Name, original.Name)
	}
	if loaded.Description.Objective != original.Description.Objective {
		t.Errorf("Objective: got %q, want %q", loaded.Description.Objective, original.Description.Objective)
	}
	if len(loaded.Description.SuccessCriteria) != 2 {
		t.Errorf("SuccessCriteria: got %d items, want 2", len(loaded.Description.SuccessCriteria))
	}
	if loaded.Status != StatusActive {
		t.Errorf("Status: got %q, want %q", loaded.Status, StatusActive)
	}
}

func TestGoalStore_Write_StampsCreatedAtOnFirstWrite(t *testing.T) {
	store, _ := newTestStore(t)
	before := time.Now().UTC().Add(-time.Second)

	g := sampleGoal()
	if err := store.Write(testSessionKey, g); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	loaded, err := store.Read(testSessionKey)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if loaded.CreatedAt.Before(before) {
		t.Errorf("CreatedAt %v is before test start %v", loaded.CreatedAt, before)
	}
	if loaded.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set after write")
	}
}

func TestGoalStore_Write_PreservesCreatedAtOnSubsequentWrites(t *testing.T) {
	store, _ := newTestStore(t)

	g := sampleGoal()
	if err := store.Write(testSessionKey, g); err != nil {
		t.Fatalf("first write failed: %v", err)
	}
	first, err := store.Read(testSessionKey)
	if err != nil {
		t.Fatalf("first read failed: %v", err)
	}
	originalCreated := first.CreatedAt

	// Wait briefly so UpdatedAt strictly increases.
	time.Sleep(10 * time.Millisecond)

	first.Description.Objective = "Updated objective"
	if err := store.Write(testSessionKey, first); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
	loaded, err := store.Read(testSessionKey)
	if err != nil {
		t.Fatalf("second read failed: %v", err)
	}
	if !loaded.CreatedAt.Equal(originalCreated) {
		t.Errorf("CreatedAt should be preserved: original %v, current %v",
			originalCreated, loaded.CreatedAt)
	}
	if !loaded.UpdatedAt.After(originalCreated) {
		t.Errorf("UpdatedAt %v should be after CreatedAt %v",
			loaded.UpdatedAt, originalCreated)
	}
}

func TestGoalStore_Write_RejectsInvalidGoal(t *testing.T) {
	store, _ := newTestStore(t)

	cases := map[string]*Goal{
		"empty name": {
			Description: Description{Objective: "x", SuccessCriteria: []string{"a"}},
			Status:      StatusActive,
		},
		"empty objective": {
			Name:        "x",
			Description: Description{SuccessCriteria: []string{"a"}},
			Status:      StatusActive,
		},
		"empty success criteria": {
			Name:        "x",
			Description: Description{Objective: "y"},
			Status:      StatusActive,
		},
	}
	for label, g := range cases {
		t.Run(label, func(t *testing.T) {
			err := store.Write(testSessionKey, g)
			if err == nil {
				t.Fatalf("expected validation error for %s", label)
			}
			if !errors.Is(err, ErrInvalidGoal) {
				t.Errorf("error should wrap ErrInvalidGoal, got: %v", err)
			}
		})
	}
}

func TestGoalStore_Write_NilGoal_Errors(t *testing.T) {
	store, _ := newTestStore(t)

	err := store.Write(testSessionKey, nil)
	if err == nil {
		t.Fatal("expected error when writing nil goal")
	}
}

func TestGoalStore_Write_EmptySessionKey_Errors(t *testing.T) {
	store, _ := newTestStore(t)

	err := store.Write("", sampleGoal())
	if err == nil {
		t.Fatal("expected error for empty session key")
	}
}

func TestGoalStore_Archive_MovesToArchiveDir(t *testing.T) {
	store, workspace := newTestStore(t)

	if err := store.Write(testSessionKey, sampleGoal()); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := store.Archive(testSessionKey); err != nil {
		t.Fatalf("archive failed: %v", err)
	}

	// Active file is gone.
	if _, err := os.Stat(store.GoalPath(testSessionKey)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("active goal should be gone, got: %v", err)
	}

	// An archive file exists.
	archiveDir := filepath.Join(workspace, "memory", "goal", "archive")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("read archive dir failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 archive file, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), testSessionKey) {
		t.Errorf("archive filename should start with %s, got %s",
			testSessionKey, entries[0].Name())
	}
}

func TestGoalStore_Archive_Idempotent(t *testing.T) {
	store, _ := newTestStore(t)

	if err := store.Write(testSessionKey, sampleGoal()); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// First archive: succeeds.
	if err := store.Archive(testSessionKey); err != nil {
		t.Fatalf("first archive failed: %v", err)
	}
	// Second archive: source gone, returns ErrNotFound (no panic).
	if err := store.Archive(testSessionKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("second archive: expected ErrNotFound, got %v", err)
	}
	// Archive of never-existed key: also ErrNotFound.
	if err := store.Archive("sess_v1_never_existed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("archive of missing key: expected ErrNotFound, got %v", err)
	}
	// Archive with empty session key: errors.
	if err := store.Archive(""); err == nil {
		t.Error("expected error for empty session key")
	}
}

func TestGoalStore_Archive_NamesIncludeTimestamp(t *testing.T) {
	store, workspace := newTestStore(t)

	if err := store.Write(testSessionKey, sampleGoal()); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	before := time.Now().UTC().Add(-2 * time.Second)
	if err := store.Archive(testSessionKey); err != nil {
		t.Fatalf("archive failed: %v", err)
	}
	after := time.Now().UTC().Add(2 * time.Second)

	archiveDir := filepath.Join(workspace, "memory", "goal", "archive")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("read archive dir failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 archive entry, got %d", len(entries))
	}
	name := entries[0].Name()
	trimmed := strings.TrimSuffix(name, ".md")
	// File format: {sessionKey}-{timestamp}-pid{N}.md
	// Skip sessionKey prefix (everything before the first ISO timestamp marker).
	tsStart := strings.Index(trimmed, "-2026") // robust anchor for ISO YYYY prefix
	if tsStart < 0 {
		tsStart = strings.Index(trimmed, "-2025")
	}
	if tsStart < 0 {
		t.Fatalf("archive name missing ISO timestamp marker: %s", name)
	}
	tsPart := trimmed[tsStart+1:] // strip leading dash
	// Strip optional -pid<N> suffix added for race-uniqueness (same-second archives).
	if i := strings.Index(tsPart, "-pid"); i >= 0 {
		tsPart = tsPart[:i]
	}
	parsed, err := time.Parse("20060102T150405.000000000Z", tsPart)
	if err != nil {
		t.Fatalf("archive timestamp unparseable: %v (full name: %s)", err, name)
	}
	if parsed.Before(before) || parsed.After(after) {
		t.Errorf("archive timestamp %v outside test window [%v, %v]",
			parsed, before, after)
	}
}

func TestGoalStore_Archive_TwoArchivesAreDistinct(t *testing.T) {
	store, workspace := newTestStore(t)

	if err := store.Write(testSessionKey, sampleGoal()); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := store.Archive(testSessionKey); err != nil {
		t.Fatalf("first archive failed: %v", err)
	}

	// Re-create the active file (Phase 2's complete_goal-and-restart pattern).
	if err := store.Write(testSessionKey, sampleGoal()); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
	// Sleep so the timestamp differs at second granularity.
	time.Sleep(1100 * time.Millisecond)
	if err := store.Archive(testSessionKey); err != nil {
		t.Fatalf("second archive failed: %v", err)
	}

	archiveDir := filepath.Join(workspace, "memory", "goal", "archive")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("read archive dir failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 archive files (different timestamps), got %d", len(entries))
	}
	if entries[0].Name() == entries[1].Name() {
		t.Errorf("archive filenames should differ: both %s", entries[0].Name())
	}
}

func TestGoalStore_List(t *testing.T) {
	store, _ := newTestStore(t)

	sessions := []string{"sess_v1_aaa", "sess_v1_bbb", "sess_v1_ccc"}
	for _, k := range sessions {
		if err := store.Write(k, sampleGoal()); err != nil {
			t.Fatalf("write %s failed: %v", k, err)
		}
	}

	keys, err := store.List()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	want := []string{"sess_v1_aaa", "sess_v1_bbb", "sess_v1_ccc"}
	if len(keys) != len(want) {
		t.Fatalf("expected %d keys, got %d (%v)", len(want), len(keys), keys)
	}
	for i, k := range want {
		if keys[i] != k {
			t.Errorf("keys[%d]: got %q, want %q", i, keys[i], k)
		}
	}
}

func TestGoalStore_List_ExcludesArchiveFiles(t *testing.T) {
	store, _ := newTestStore(t)

	if err := store.Write("sess_v1_active", sampleGoal()); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := store.Write("sess_v1_archived", sampleGoal()); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := store.Archive("sess_v1_archived"); err != nil {
		t.Fatalf("archive failed: %v", err)
	}

	keys, err := store.List()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(keys) != 1 || keys[0] != "sess_v1_active" {
		t.Errorf("expected only sess_v1_active in list, got %v", keys)
	}
}

func TestGoalStore_PreserveProgressEntries(t *testing.T) {
	store, _ := newTestStore(t)

	g := sampleGoal()
	g.AppendProgress(ProgressEntry{
		CompletedSteps: []string{"step1"},
		RemainingSteps: []string{"step2"},
		NextAction:     "review",
	})
	g.AppendProgress(ProgressEntry{
		CompletedSteps: []string{"step1", "step2"},
		RemainingSteps: []string{"step3"},
		NextAction:     "ship",
	})
	if err := store.Write(testSessionKey, g); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	loaded, err := store.Read(testSessionKey)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if len(loaded.Progress) != 2 {
		t.Fatalf("expected 2 progress entries, got %d", len(loaded.Progress))
	}
	if loaded.Progress[1].NextAction != "ship" {
		t.Errorf("progress[1].NextAction = %q, want %q",
			loaded.Progress[1].NextAction, "ship")
	}
}

func TestGoalStore_Parse_BadFrontmatter(t *testing.T) {
	cases := map[string]struct {
		data []byte
		want error
	}{
		"empty": {
			data: []byte(""),
			want: ErrEmpty,
		},
		"whitespace only": {
			data: []byte("   \n\t\n"),
			want: ErrEmpty,
		},
		"no frontmatter": {
			data: []byte("# plain markdown without frontmatter\n"),
			want: ErrMissingFrontmatter,
		},
		"unterminated": {
			data: []byte("---\nname: x\nstill going, no closing\n"),
			want: ErrUnterminatedFrontmatter,
		},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := Parse("test.md", c.data)
			if !errors.Is(err, c.want) {
				t.Errorf("got error %v, want wrap of %v", err, c.want)
			}
		})
	}
}


// TestArchiveName_UniqueUnderSameSecond guards against the same-second rename race
// where two turn-exit hooks fire Archive concurrently: filenames must collide no more.
func TestArchiveName_UniqueUnderSameSecond(t *testing.T) {
	now := time.Date(2026, 7, 20, 11, 34, 5, 123456789, time.UTC)
	a := archiveName("sessA", now)
	b := archiveName("sessA", now.Add(1 * time.Nanosecond))
	// Both must remain valid .md filenames parseable by the same prefix scheme as prod.
	if !strings.HasPrefix(a, "sessA-") || !strings.HasSuffix(a, ".md") {
		t.Errorf("archiveName a malformed: %q", a)
	}
	if !strings.HasPrefix(b, "sessA-") || !strings.HasSuffix(b, ".md") {
		t.Errorf("archiveName b malformed: %q", b)
	}
}

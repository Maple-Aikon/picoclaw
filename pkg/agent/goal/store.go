// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

const extGoal = ".md"

// Store persists Goals as Markdown files under <workspace>/memory/goal/.
//
// One file per session key. The session key is opaque (e.g. sess_v1_<sha256>
// from pkg/session/key) so filenames are filesystem-safe. The human-readable
// goal name lives inside the file as the Name field.
type Store struct {
	workspace  string
	goalDir    string // <workspace>/memory/goal
	archiveDir string // <workspace>/memory/goal/archive
}

// NewStore returns a Store rooted at the given workspace. It creates the goal
// directory and the archive sub-directory if they do not already exist.
func NewStore(workspace string) *Store {
	goalDir := filepath.Join(workspace, "memory", "goal")
	archiveDir := filepath.Join(goalDir, "archive")
	_ = os.MkdirAll(archiveDir, 0o755)
	return &Store{
		workspace:  workspace,
		goalDir:    goalDir,
		archiveDir: archiveDir,
	}
}

// GoalPath returns the on-disk path for a given session's active goal file.
func (s *Store) GoalPath(sessionKey string) string {
	return filepath.Join(s.goalDir, sessionKey+extGoal)
}

// ErrNotFound is returned when no active goal exists for the session.
// It is also returned by Archive when the file is already gone.
var ErrNotFound = errors.New("goal not found")

// Read loads the active goal for a session. Returns (nil, nil) when no goal
// exists for the session — absence is not an error, only corruption is.
func (s *Store) Read(sessionKey string) (*Goal, error) {
	if sessionKey == "" {
		return nil, fmt.Errorf("goal store: empty session key")
	}
	path := s.GoalPath(sessionKey)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read goal %s: %w", path, err)
	}
	g, err := Parse(path, data)
	if err != nil {
		return nil, fmt.Errorf("parse goal %s: %w", path, err)
	}
	return g, nil
}

// Write persists the goal atomically. It sets CreatedAt on the first write
// and updates UpdatedAt on every call. Status defaults to active on the
// first write when unset (Phase 2's set_goal always sets it explicitly;
// this is a safety net for direct programmatic callers).
func (s *Store) Write(sessionKey string, g *Goal) error {
	if sessionKey == "" {
		return fmt.Errorf("goal store: empty session key")
	}
	if g == nil {
		return fmt.Errorf("goal store: nil goal")
	}
	if err := g.Validate(); err != nil {
		return fmt.Errorf("write goal %s: %w", sessionKey, err)
	}

	now := time.Now()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	g.UpdatedAt = now

	if g.Status == "" {
		g.Status = StatusActive
	}

	data, err := Serialize(g)
	if err != nil {
		return fmt.Errorf("serialize goal %s: %w", sessionKey, err)
	}
	return fileutil.WriteFileAtomic(s.GoalPath(sessionKey), data, 0o600)
}

// Archive moves the active goal file to the archive directory. The archive
// filename includes an ISO-8601 timestamp to avoid clobbering prior archives.
//
// Idempotent at the file-system level: if the source has already been moved
// (e.g. by a parallel Archive call from a different turn-exit hook — see
// Section 8.6 of the plan), this returns ErrNotFound without touching the
// existing archive.
func (s *Store) Archive(sessionKey string) error {
	if sessionKey == "" {
		return fmt.Errorf("goal store: empty session key")
	}
	src := s.GoalPath(sessionKey)
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("stat goal %s: %w", src, err)
	}
	dst := filepath.Join(s.archiveDir, archiveName(sessionKey, time.Now()))
	if err := os.Rename(src, dst); err != nil {
		// Race: another hook moved the file between Stat and Rename.
		// Treat as success (idempotent archive).
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("archive %s -> %s: %w", src, dst, err)
	}
	return nil
}

func archiveName(sessionKey string, at time.Time) string {
	ts := at.UTC().Format("20060102T150405.000000000Z")
	return sessionKey + "-" + ts + "-pid" + strconv.Itoa(os.Getpid()) + extGoal
}

// ReadAny loads the most-recent goal for a session, preferring the active
// file and falling back to the most-recent archive when none is active.
//
// Returns (nil, nil) when no goal exists in either location. Used by
// complete_goal to detect the "already-completed" replay state — once the
// active file has been moved to archive/, Read alone would report absence.
func (s *Store) ReadAny(sessionKey string) (*Goal, error) {
	if g, err := s.Read(sessionKey); err != nil {
		return nil, err
	} else if g != nil {
		return g, nil
	}
	return s.readNewestArchive(sessionKey)
}

// readNewestArchive returns the most-recent archived goal for the session
// key, or (nil, nil) if no archive exists.
func (s *Store) readNewestArchive(sessionKey string) (*Goal, error) {
	entries, err := os.ReadDir(s.archiveDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read archive dir %s: %w", s.archiveDir, err)
	}
	prefix := sessionKey + "-"
	var newest os.FileInfo
	var newestName string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, extGoal) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if newest == nil || fi.ModTime().After(newest.ModTime()) {
			newest = fi
			newestName = name
		}
	}
	if newestName == "" {
		return nil, nil
	}
	path := filepath.Join(s.archiveDir, newestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read archive %s: %w", path, err)
	}
	g, err := Parse(path, data)
	if err != nil {
		return nil, fmt.Errorf("parse archive %s: %w", path, err)
	}
	return g, nil
}

// List returns all session keys with an active goal, sorted alphabetically.
// Archived keys (those in archive/ only) are not returned.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.goalDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read goal dir %s: %w", s.goalDir, err)
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, extGoal) {
			continue
		}
		keys = append(keys, strings.TrimSuffix(name, extGoal))
	}
	sort.Strings(keys)
	return keys, nil
}

// ErrNoActiveGoal is returned by goal store operations when the session has
// no active goal file. Callers can use errors.Is to distinguish this case and
// fall back to legacy storage (Phase 7 plan §3.7 graceful degradation).
var ErrNoActiveGoal = errors.New("no active goal for session")

// UpdateStatusSnapshot updates the active goal's StatusSnapshot field. It is
// the Phase 7 (plan §3.7) replacement for the legacy sessionTaskSummary
// in-memory sync.Map — the goal store becomes the single source of truth for
// cross-turn task context. When the session has no active goal, returns
// ErrNoActiveGoal so callers can fall back to legacy storage. Idempotent:
// calling it twice with the same snapshot is safe.
func (s *Store) UpdateStatusSnapshot(sessionKey, snapshot string) error {
	if sessionKey == "" {
		return fmt.Errorf("empty session key")
	}
	g, err := s.Read(sessionKey)
	if err != nil {
		return err
	}
	if g == nil {
		// No active goal — caller decides whether to fall back to legacy.
		return ErrNoActiveGoal
	}
	if g.StatusSnapshot == snapshot {
		return nil
	}
	g.StatusSnapshot = snapshot
	g.UpdatedAt = time.Now().UTC()
	return s.Write(sessionKey, g)
}

// LoadStatusSnapshot returns the active goal's StatusSnapshot, or "" when
// no active goal exists. Callers use this instead of
// al.sessionTaskSummary.Load() when the Phase 7 migration flag was enabled.
	// Phase 8.3: legacy map removed — this is the sole read path.
func (s *Store) LoadStatusSnapshot(sessionKey string) (string, error) {
	if sessionKey == "" {
		return "", fmt.Errorf("empty session key")
	}
	g, err := s.Read(sessionKey)
	if err != nil {
		return "", err
	}
	if g == nil {
		return "", nil
	}
	return g.StatusSnapshot, nil
}

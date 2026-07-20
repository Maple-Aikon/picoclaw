// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status tracks the goal's lifecycle stage.
type Status string

const (
	StatusActive    Status = "active"
	StatusCompleted Status = "completed"
	StatusArchived  Status = "archived"
	// StatusAborted is set when a goal is force-archived because the agent
	// loop could not recover (Phase 6 Hook 1 — finalizeGoalOnTurnEnd). An
	// aborted goal is written back to disk with StatusAborted + AbortedAt +
	// AbortReason so the next session can inspect why the prior attempt ended
	// without explicit user-driven completion.
	StatusAborted Status = "aborted"
)

// Description is the structured objective block set by set_goal (Phase 2).
// Free-form text is rejected at the tool layer; the schema enforces that
// Objective and SuccessCriteria are non-empty.
type Description struct {
	Objective       string   `yaml:"objective"`
	SuccessCriteria []string `yaml:"success_criteria"`
	InScope         []string `yaml:"in_scope,omitempty"`
	OutOfScope      []string `yaml:"out_of_scope,omitempty"`
	Cadence         string   `yaml:"cadence,omitempty"`
}

// ProgressEntry is one checkpoint added by goal_progress().
type ProgressEntry struct {
	Timestamp      time.Time `yaml:"timestamp"`
	CompletedSteps []string  `yaml:"completed_steps"`
	Blockers       []string  `yaml:"blockers,omitempty"`
	RemainingSteps []string  `yaml:"remaining_steps"`
	DriftDetected  bool      `yaml:"drift_detected,omitempty"`
	NextAction     string    `yaml:"next_action"`
}

// Goal is one persisted long-running task.
type Goal struct {
	Name        string          `yaml:"name"`
	Description Description     `yaml:"description"`
	Progress    []ProgressEntry `yaml:"progress,omitempty"`
	CreatedAt   time.Time       `yaml:"created_at"`
	UpdatedAt   time.Time       `yaml:"updated_at"`
	Status      Status          `yaml:"status"`

	// AbortedAt is set when Status transitions to StatusAborted (Phase 6
	// Hook 1 — finalizeGoalOnTurnEnd). Nil when the goal completed normally
	// or is still active.
	AbortedAt *time.Time `yaml:"aborted_at,omitempty"`
	// AbortReason captures the trigger source for an aborted goal — one of:
	//   "runTurn_panic", "tool_panic", "bexhausted:<loop-name>", "user_abort"
	// Empty string when status != StatusAborted.
	AbortReason string `yaml:"abort_reason,omitempty"`
}

// ErrInvalidGoal is returned by Validate when required fields are missing.
var ErrInvalidGoal = errors.New("invalid goal")

// Validate enforces the schema required by Phase 2's set_goal tool:
// non-empty Name, Description.Objective, and at least one SuccessCriterion.
func (g *Goal) Validate() error {
	var problems []string
	if strings.TrimSpace(g.Name) == "" {
		problems = append(problems, "name is required")
	}
	if strings.TrimSpace(g.Description.Objective) == "" {
		problems = append(problems, "description.objective is required")
	}
	if len(g.Description.SuccessCriteria) == 0 {
		problems = append(problems, "description.success_criteria must contain at least one item")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidGoal, strings.Join(problems, "; "))
	}
	return nil
}

// AppendProgress adds one progress entry and bumps UpdatedAt.
func (g *Goal) AppendProgress(p ProgressEntry) {
	if p.Timestamp.IsZero() {
		p.Timestamp = time.Now()
	}
	g.Progress = append(g.Progress, p)
	g.UpdatedAt = time.Now()
}

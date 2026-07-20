// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

// Package goal implements the long-running task lifecycle for PicoClaw.
//
// A Goal is a structured plan persisted as a Markdown file with YAML
// frontmatter, located at <workspace>/memory/goal/<session>.md. When a goal
// is completed (via complete_goal) or the turn exits, the file is moved to
// <workspace>/memory/goal/archive/<session>-<timestamp>.md.
//
// Phase 1 of the goal-lifecycle plan ships the persistence layer and types.
// Phase 2 ships the four tool-facing operations (set_goal, view_goal,
// goal_progress, complete_goal). Phase 3 ties them to dynamic tool
// allowlists driven by the goal's current phase (Lock / Open / Checkpoint).
//
// Plan: memory/plan/picoclaw-goal-lifecycle-long-running-task-vói-setviewcomplete-goal-goal-phase-tool-allowlist-20260719.md
package goal

import (
	"errors"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// invalidInputForLLM returns a ToolResult marked as invalid_input. The LLM is
// expected to inspect the message, adjust its arguments, and retry — never to
// call again with the same inputs. The system policy in result.go will append
// the standard "DO NOT RETRY this exact call" note.
func invalidInputForLLM(msg string) *toolshared.ToolResult {
	return &toolshared.ToolResult{
		ForLLM:  msg,
		IsError: true,
		ErrKind: toolshared.ErrInvalidInput,
	}
}

// mapStoreError translates a goal.Store error into a ToolResult. Validation
// failures are reported as invalid_input so the LLM retries with corrected
// arguments; I/O and unexpected errors map to transient so the system can
// retry once before surfacing the failure.
func mapStoreError(err error) *toolshared.ToolResult {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrInvalidGoal) {
		return invalidInputForLLM(err.Error())
	}
	if errors.Is(err, ErrNotFound) {
		// ErrNotFound from Archive is unusual at the tool layer; surface
		// as invalid_input so the LLM is told the goal is missing rather
		// than seeing a transient error.
		return invalidInputForLLM(err.Error())
	}
	return &toolshared.ToolResult{
		ForLLM:  "internal goal-store error: " + err.Error(),
		IsError: true,
		ErrKind: toolshared.ErrTransient,
	}
}

// shortSessionKey is a printable summary of a session key for user-facing
// messages. Long opaque keys are truncated to keep tool output compact.
func shortSessionKey(key string) string {
	const keep = 16
	if len(key) <= keep {
		return key
	}
	return key[:keep] + "\u2026"
}

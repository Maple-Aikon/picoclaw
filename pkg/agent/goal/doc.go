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
// Phase 1 of the goal-lifecycle plan ships the persistence layer and types
// only. Phase 2 wires the four tool-facing operations (set_goal, view_goal,
// goal_progress, complete_goal), and Phase 3+ tie them to dynamic tool
// allowlists driven by the goal's current phase (Lock / Open / Checkpoint).
//
// Plan: memory/plan/picoclaw-goal-lifecycle-long-running-task-voi-setviewcomplete-goal-goal-phase-tool-allowlist-20260719.md
package goal

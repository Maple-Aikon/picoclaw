// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// goalNameRe enforces the schema-declared ASCII / hyphen / underscore charset.
// The pattern also caps length to 64 chars to keep archive filenames predictable.
var goalNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// shared storeRefs are read from the per-turn context by every goal tool.
// We pull them in once per Execute rather than at construction time because
// the same Tool instance is shared across turns, agents and sessions — only
// the context-bound channel/chatID/sessionKey change per request.

func sessionKeyFromCtx(ctx context.Context) (sessionKey, agentID string) {
	return toolshared.ToolSessionKey(ctx), toolshared.ToolAgentID(ctx)
}

func newStoreFromCtx(ctx context.Context, workspace string) *Store {
	return NewStore(workspace)
}

// ---------------------------------------------------------------------------
// 1. set_goal — create or replace the goal for the current session.
// ---------------------------------------------------------------------------

// SetGoalTool creates or replaces the active session's persistent goal.
//
// Philosophy: a goal is the LLM's working contract. Replace (not merge) on
// every call so the LLM is never confused about which plan is in effect.
// Updates that should preserve history should be made via goal_progress, not
// by re-calling set_goal.
type SetGoalTool struct {
	workspace string
}

func NewSetGoalTool(workspace string) *SetGoalTool {
	return &SetGoalTool{workspace: workspace}
}

func (t *SetGoalTool) Name() string {
	return "set_goal"
}

func (t *SetGoalTool) Description() string {
	return `Create or replace the persistent goal for the current session. REPLACES (not merges) any existing goal. Use goal_progress to log progress without rewriting the goal.

When to use:
- At the start of multi-turn work that spans more than one user message.
- When the user changes the objective (re-call with the new objective, don't try to mutate in place).
- After the previous goal is completed, to start a new one.

Hard requirements (omitting any will be rejected):
- name (short, stable identifier — used as the file basename)
- objective (one sentence describing success)
- success_criteria (non-empty list of testable criteria; each on its own line)

Soft fields (recommended but optional):
- in_scope, out_of_scope (helps the LLM avoid drift)
- cadence (e.g. "review weekly", "ship by 2026-08-15")

Returns a one-line confirmation + the rendered header so you can confirm the file is what you intended.`
}

func (t *SetGoalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Short stable identifier for this goal (used as file basename; ASCII / hyphen / underscore only).",
			},
			"objective": map[string]any{
				"type":        "string",
				"description": "One-sentence statement of what success looks like.",
			},
			"success_criteria": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"minItems":    1,
				"description": "List of testable criteria that prove the goal is met. Each item is a separate bullet in the rendered header.",
			},
			"in_scope": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional. Things that ARE part of this goal.",
			},
			"out_of_scope": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional. Things that are explicitly NOT part of this goal (use to prevent drift).",
			},
			"cadence": map[string]any{
				"type":        "string",
				"description": `Optional. Review/refresh cadence as free text, e.g. "review weekly" or "ship by 2026-08-15".`,
			},
		},
		"required": []string{"name", "objective", "success_criteria"},
	}
}

func (t *SetGoalTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	sessionKey, agentID := sessionKeyFromCtx(ctx)
	if sessionKey == "" {
		return invalidInputForLLM("set_goal: no session key on context (tool must run inside an agent turn)")
	}

	name := strings.TrimSpace(stringArg(args, "name"))
	objective := strings.TrimSpace(stringArg(args, "objective"))
	successCriteria := stringSliceArg(args, "success_criteria")
	inScope := stringSliceArg(args, "in_scope")
	outOfScope := stringSliceArg(args, "out_of_scope")
	cadence := strings.TrimSpace(stringArg(args, "cadence"))

	if name == "" {
		return invalidInputForLLM("set_goal: 'name' is required and must be non-empty")
	}
	if !goalNameRe.MatchString(name) {
		return invalidInputForLLM("set_goal: 'name' must match ^[A-Za-z0-9_-]{1,64}$ (ASCII letters, digits, hyphen, underscore; max 64 chars)")
	}
	if objective == "" {
		return invalidInputForLLM("set_goal: 'objective' is required and must be non-empty")
	}
	if len(successCriteria) == 0 {
		return invalidInputForLLM("set_goal: 'success_criteria' must contain at least one criterion")
	}
	// Detect replacement vs creation so we can phrase the user-facing line accurately.
	store := newStoreFromCtx(ctx, t.workspace)
	now := time.Now().UTC()
	replaced := false
	if existing, _ := store.Read(sessionKey); existing != nil {
		replaced = true
	}

	g := &Goal{
		Name:        name,
		Status:      StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
		Description: Description{
			Objective:       objective,
			SuccessCriteria: successCriteria,
			InScope:         inScope,
			OutOfScope:      outOfScope,
			Cadence:         cadence,
		},
	}
	// Preserve original CreatedAt if we're replacing an existing goal — Store.Write
	// would do this anyway, but we set it here so we don't lose the timestamp on
	// the in-memory path that the LLM sees.
	if replaced {
		if existing, _ := store.Read(sessionKey); existing != nil && !existing.CreatedAt.IsZero() {
			g.CreatedAt = existing.CreatedAt
		}
	}

	if err := g.Validate(); err != nil {
		return invalidInputForLLM("set_goal: validation failed: " + err.Error())
	}
	if err := store.Write(sessionKey, g); err != nil {
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	// Phase 8.1 — write the initial StatusSnapshot so cross-turn reminders
	// have something to inject even before goal_progress fires. Snapshot is
	// rendered from the goal's own state (Objective + first in_scope item);
	// no LLM call involved. Write failures are non-fatal: the goal file
	// itself is already persisted, and a missing snapshot just means the
	// [Task context reminder] slot stays empty until goal_progress runs.
	snapshot := RenderGoalSnapshot(g, nil)
	if snapshot != "" {
		if err := store.UpdateStatusSnapshot(sessionKey, snapshot); err != nil && err != ErrNoActiveGoal {
			// Log but don't fail the tool — snapshot is best-effort.
			// (callers may decide to wire a WarnCF here in 8.2 audit.)
			_ = err
		}
	}

	action := "created"
	if replaced {
		action = "replaced"
	}
	summary := fmt.Sprintf("Goal %s for session %s on agent %s.",
		action, shortSessionKey(sessionKey), shortOrDash(agentID))
	return &toolshared.ToolResult{
		ForLLM: summary + "\n\n" + g.RenderHeader(),
	}
}

// ---------------------------------------------------------------------------
// 2. view_goal — render the persisted goal back to the LLM (paginated).
// ---------------------------------------------------------------------------

// ViewGoalTool lets the LLM read the current goal (or any page of its progress
// log). header is always returned in full; progress supports pagination via
// start_line (0-indexed) and max_lines. Set max_lines to 0 (default) to return
// the entire progress body.
type ViewGoalTool struct {
	workspace string
}

func NewViewGoalTool(workspace string) *ViewGoalTool {
	return &ViewGoalTool{workspace: workspace}
}

func (t *ViewGoalTool) Name() string {
	return "view_goal"
}

func (t *ViewGoalTool) Description() string {
	return `View the goal and progress log for the current session. The header (name, objective, success criteria, scope, cadence) is always returned in full. The progress log supports the same line-based pagination as read_file:

- start_line: 0-indexed line number; defaults to 0 (the start). Past-EOF values are silently clamped to the last line.
- max_lines: defaults to 0 (return all lines). Set to a positive integer to cap response size.

If no goal exists for the current session the tool returns "<no goal set>" — use set_goal to create one.`
}

func (t *ViewGoalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"start_line": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "0-indexed start line for the progress log section. Defaults to 0.",
			},
			"max_lines": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"description": "Maximum number of progress log lines to return. 0 (default) means no cap.",
			},
		},
	}
}

func (t *ViewGoalTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	sessionKey, _ := sessionKeyFromCtx(ctx)
	if sessionKey == "" {
		return invalidInputForLLM("view_goal: no session key on context")
	}

	startLine := intArg(args, "start_line", 0)
	maxLines := intArg(args, "max_lines", 0)
	if startLine < 0 {
		startLine = 0
	}
	if maxLines < 0 {
		maxLines = 0
	}

	store := newStoreFromCtx(ctx, t.workspace)
	g, err := store.Read(sessionKey)
	if err != nil {
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	if g == nil {
		return &toolshared.ToolResult{
			ForLLM: "<no goal set for this session. Use set_goal to create one.>",
			Silent: false,
		}
	}

	body, total, hasMore := g.RenderProgress(startLine, maxLines)

	// Compose the response. Pin the header in front so the LLM sees the
	// contract first, then append the requested window of the progress body.
	var b strings.Builder
	b.WriteString(g.RenderHeader())
	b.WriteString("\n\n---\n\n")
	b.WriteString("## Progress log\n\n")
	if total > 0 {
		end := startLine + linesReturned(body)
		if linesReturned(body) == 0 && startLine >= total {
			fmt.Fprintf(&b, "<start_line %d is past the end of the log (%d lines)>\n", startLine, total)
		} else {
			fmt.Fprintf(&b, "_window: lines %d-%d of %d, has_more=%t_\n\n", startLine+1, end, total, hasMore)
			b.WriteString(body)
		}
	}
	return &toolshared.ToolResult{
		ForLLM: b.String(),
	}
}

// linesReturned counts lines in a body produced by RenderProgress, which uses
// trailing newline stripping consistent with read_file semantics.
func linesReturned(body string) int {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return 0
	}
	return strings.Count(body, "\n") + 1
}

// ---------------------------------------------------------------------------
// 3. goal_progress — append a new progress entry.
// ---------------------------------------------------------------------------

// GoalProgressTool appends one ProgressEntry to the persistent goal log.
// The LLM should call this AT LEAST once per turn that materially advances the
// goal, so future turns can read back what was decided / done / blocked.
type GoalProgressTool struct {
	workspace string
}

func NewGoalProgressTool(workspace string) *GoalProgressTool {
	return &GoalProgressTool{workspace: workspace}
}

func (t *GoalProgressTool) Name() string {
	return "goal_progress"
}

func (t *GoalProgressTool) Description() string {
	return `Append a progress entry to the current session's goal log. Each entry is timestamped.

When to use:
- At the end of any turn that moved the goal forward (decisions made, code shipped, blockers discovered).
- When drift is detected (set drift_detected=true and explain in next_action).

All fields are optional except that something must be set — calling with zero meaningful fields is rejected to prevent accidental empty entries. Recommended minimum: completed_steps OR remaining_steps OR next_action.

- completed_steps: things finished this turn
- blockers: anything blocking forward progress
- remaining_steps: explicit "not done yet" list (helps future turns resume)
- drift_detected: set true when the work is no longer aligned with the original objective
- next_action: one sentence describing the next concrete step (MUST be present if drift_detected is true)`
}

func (t *GoalProgressTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"completed_steps": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional. Steps finished in this turn.",
			},
			"blockers": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional. Anything blocking forward progress.",
			},
			"remaining_steps": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional. Explicit list of what's still pending (helps future turns resume context).",
			},
			"drift_detected": map[string]any{
				"type":        "boolean",
				"description": "Optional. Set true when current work is no longer aligned with the goal's objective.",
			},
			"next_action": map[string]any{
				"type":        "string",
				"description": "Optional. One sentence describing the next concrete step. Required when drift_detected is true.",
			},
		},
	}
}

func (t *GoalProgressTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	sessionKey, _ := sessionKeyFromCtx(ctx)
	if sessionKey == "" {
		return invalidInputForLLM("goal_progress: no session key on context")
	}

	completed := stringSliceArg(args, "completed_steps")
	blockers := stringSliceArg(args, "blockers")
	remaining := stringSliceArg(args, "remaining_steps")
	drift := boolArg(args, "drift_detected")
	next := strings.TrimSpace(stringArg(args, "next_action"))

	if len(completed) == 0 && len(blockers) == 0 && len(remaining) == 0 && next == "" && !drift {
		return invalidInputForLLM("goal_progress: at least one of completed_steps, blockers, remaining_steps, next_action, or drift_detected=true must be set")
	}
	if drift && next == "" {
		return invalidInputForLLM("goal_progress: when drift_detected=true you MUST also provide next_action describing how to realign")
	}

	store := newStoreFromCtx(ctx, t.workspace)
	g, err := store.Read(sessionKey)
	if err != nil {
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	if g == nil {
		return invalidInputForLLM("goal_progress: no goal set for this session. Call set_goal first.")
	}
	if g.Status == StatusCompleted {
		return invalidInputForLLM("goal_progress: goal status is \"completed\"; call set_goal to start a new one")
	}

	entry := ProgressEntry{
		Timestamp:      time.Now().UTC(),
		CompletedSteps: completed,
		Blockers:       blockers,
		RemainingSteps: remaining,
		DriftDetected:  drift,
		NextAction:     next,
	}
	g.AppendProgress(entry)
	g.UpdatedAt = entry.Timestamp
	if err := store.Write(sessionKey, g); err != nil {
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	// Phase 8.1 — refresh StatusSnapshot from the latest entry so the next
	// turn's [Task context reminder] reflects the LLM's most recent
	// self-evaluation. This is the replacement for the LLM-driven
	// extractTaskWithFallback path that previously ran once per turn.
	snapshot := RenderGoalSnapshot(g, &entry)
	if snapshot != "" {
		if err := store.UpdateStatusSnapshot(sessionKey, snapshot); err != nil && err != ErrNoActiveGoal {
			_ = err // best-effort; see SetGoalTool comment
		}
	}

	idx := len(g.Progress)
	summary := fmt.Sprintf("Logged progress entry #%d for session %s.\n\n%s",
		idx, shortSessionKey(sessionKey), lastEntryRendered(g.Progress[len(g.Progress)-1]))
	return &toolshared.ToolResult{ForLLM: summary}
}

// ---------------------------------------------------------------------------
// 4. complete_goal — mark the goal done and archive it.
// ---------------------------------------------------------------------------

// CompleteGoalTool marks the active goal as completed and moves the file into
// the archive directory. After archival, view_goal returns "<no goal set>"
// and subsequent set_goal calls start a fresh goal.
type CompleteGoalTool struct {
	workspace string
}

func NewCompleteGoalTool(workspace string) *CompleteGoalTool {
	return &CompleteGoalTool{workspace: workspace}
}

func (t *CompleteGoalTool) Name() string {
	return "complete_goal"
}

func (t *CompleteGoalTool) Description() string {
	return `Mark the current session's goal as completed and archive its file. The original file at <workspace>/memory/goal/<session>.md is moved to <workspace>/memory/goal/archive/<session>-<timestamp>.md. After this call:

- The goal is no longer discoverable via list_goals (archived by definition).
- view_goal returns "<no goal set>".
- A subsequent set_goal call will start a new goal.

If no goal exists, or the goal is already completed, the tool returns an invalid_input error. Use set_goal to start a new one before calling this.`
}

func (t *CompleteGoalTool) Parameters() map[string]any {
	// No required arguments. We deliberately do not ask for a "summary"
	// here — that would bloat the LLM prompt and the final progress entry
	// (added via goal_progress immediately before completion) is enough.
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *CompleteGoalTool) Execute(ctx context.Context, _ map[string]any) *toolshared.ToolResult {
	sessionKey, _ := sessionKeyFromCtx(ctx)
	if sessionKey == "" {
		return invalidInputForLLM("complete_goal: no session key on context")
	}
	store := newStoreFromCtx(ctx, t.workspace)
	// Use ReadAny so a second call (after Archive moved the active file)
	// can still detect the "already completed" state via the archive dir.
	g, err := store.ReadAny(sessionKey)
	if err != nil {
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	if g == nil {
		return invalidInputForLLM("complete_goal: no goal set for this session. Call set_goal first.")
	}
	if g.Status == StatusCompleted {
		return invalidInputForLLM("complete_goal: goal is already completed (archived). Call set_goal to start a new one.")
	}

	completedCount := len(g.Progress)
	g.Status = StatusCompleted
	g.UpdatedAt = time.Now().UTC()
	// Phase 8.1 — clear StatusSnapshot before archiving so the reminder
	// slot stays empty on the next turn (until a fresh set_goal is issued).
	// Without this, an archived goal's stale snapshot would still surface in
	// [Task context reminder] because Store.ReadAny still finds the file.
	g.StatusSnapshot = ""
	if err := store.Write(sessionKey, g); err != nil {
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	if err := store.Archive(sessionKey); err != nil {
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}

	return &toolshared.ToolResult{
		ForLLM: fmt.Sprintf(
			"Goal %q marked completed and archived (%d progress entries preserved). Use set_goal to start a new goal.",
			g.Name, completedCount,
		),
	}
}

// ---------------------------------------------------------------------------
// Argument helpers (mirroring the small conventions used by extend_turn.go).
// ---------------------------------------------------------------------------

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return def
}

func boolArg(args map[string]any, key string) bool {
	v, ok := args[key]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1" || t == "yes"
	}
	return false
}

func stringSliceArg(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, e := range s {
			str, ok := e.(string)
			if !ok {
				continue
			}
			if str = strings.TrimSpace(str); str != "" {
				out = append(out, str)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		// Allow newline-delimited form so non-JSON encoders can still call us.
		parts := strings.Split(s, "\n")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

func shortOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return shortSessionKey(s)
}

func lastEntryRendered(p ProgressEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Progress %s\n", p.Timestamp.Format("2006-01-02T15:04:05Z"))
	if len(p.CompletedSteps) > 0 {
		fmt.Fprintf(&b, "Completed: %s\n", strings.Join(p.CompletedSteps, "; "))
	}
	if len(p.Blockers) > 0 {
		fmt.Fprintf(&b, "Blockers: %s\n", strings.Join(p.Blockers, "; "))
	}
	if len(p.RemainingSteps) > 0 {
		fmt.Fprintf(&b, "Remaining: %s\n", strings.Join(p.RemainingSteps, "; "))
	}
	if p.DriftDetected {
		b.WriteString("Drift: true\n")
	}
	if p.NextAction != "" {
		fmt.Fprintf(&b, "Next action: %s\n", p.NextAction)
	}
	return strings.TrimRight(b.String(), "\n")
}

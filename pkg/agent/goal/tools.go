// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// Phase 10.1: extenderKey is the context key used by pipeline_execute.go
// to surface a turn's iteration extender (an opaque interface) to goal
// tools. Goal is the only consumer; the key is unexported so no other
// package can resolve it.
type extenderKey struct{}

// Phase 11: turnStateKey + TurnStateAccess interface let the goal tools
// mark the turn as finalized (after complete_goal) so the runtime loop
// breaks immediately. Defined here (consumer side) to keep the import
// graph one-way: pkg/agent/goal does NOT import pkg/agent, but it can
// read back a tiny interface satisfied by the agent's turnState type.
type turnStateKey struct{}

// TurnStateAccess is the minimal surface goal tools need from a turn.
// *agent.turnState satisfies this implicitly (Phase 11: no need to add
// a named type alias — the methods are enough).
//
// Phase 12.44: PublishToUser added for complete_goal self-publish.
// Best-effort fire-and-forget như mọi outbound khác — VOID, KHÔNG có error
// path (PublishResponseIfNeeded là void; hệ thống không có fail-path
// detectable, F14 fold). Goal tools (complete_goal) dùng để self-publish
// full summary mà không set ResponseHandled (loop continues to the
// final-report iter).
type TurnStateAccess interface {
	MarkGoalFinalized()
	// PublishToUser sends text directly to the user's channel. Void + best-
	// effort (system-wide norm — no outbound has a delivery guarantee).
	PublishToUser(ctx context.Context, text string)
}

// WithTurnState attaches a TurnStateAccess to ctx. Pipeline execute code
// calls this once per tool invocation; goal tools (specifically
// CompleteGoalTool) read it via TurnStateFromContext.
func WithTurnState(ctx context.Context, ts TurnStateAccess) context.Context {
	if ts == nil {
		return ctx
	}
	return context.WithValue(ctx, turnStateKey{}, ts)
}

// TurnStateFromContext retrieves a TurnStateAccess previously attached
// via WithTurnState. Returns nil if absent (e.g. tool invoked outside
// the normal turn loop, or in tests that don't wrap ctx).
func TurnStateFromContext(ctx context.Context) TurnStateAccess {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(turnStateKey{}).(TurnStateAccess)
	return v
}

// IterationExtender is the public interface goal tools use to extend a
// turn's iteration cap. Phase 10 removed the user-facing extend_turn_iteration
// tool; this interface is the lightweight replacement so the goal-progress
// self-extend wire can keep working.
//
// Phase 11 addition: MaxIterationsPerCheckpoint() returns the budget the
// runtime grants per checkpoint phase (e.g. the per-checkpoint
// "add N iterations" amount). The goal_progress self-extend uses this as
// its ExtendIterationCap amount (so each checkpoint effectively resets
// the budget by the same wall-clock amount).
type IterationExtender interface {
	RemainingIterations() int
	CanExtendIterationCap() bool
	ExtendIterationCap(n int, reason string) (newCap int, delta int)
	// IterationCap returns the *current* per-turn iteration cap (e.g. 20
	// initially, then 40 after the first goal_progress self-extend). Used
	// as a defensive fallback when MaxIterationsPerCheckpoint is zero
	// (misconfig).
	IterationCap() int
	// MaxIterationsPerCheckpoint returns the per-checkpoint iteration
	// budget (default = agent.MaxIterations, e.g. 20). Used by
	// goal_progress to pick the ExtendIterationCap amount.
	MaxIterationsPerCheckpoint() int
	// MaxIterationsCap returns the absolute ceiling (e.g. 200) — goal_progress
	// uses this to detect the ceiling-bound edge case where normal extend
	// would mostly clamp to ceiling and waste the budget.
	MaxIterationsCap() int
	// RequestExtendIterationCap is the deferred-extend counterpart of
	// ExtendIterationCap. Instead of immediately bumping the iteration cap
	// (which would flip phase mid-iter when the resolver reads `iter >= cap`),
	// it stages the request and the agent loop applies it at end of iter
	// (see Phase 12.35). Returns true if the request was accepted (caller
	// has room and the cap hasn't hit its ceiling), false otherwise.
	RequestExtendIterationCap(n int, reason string) bool
	// FlushPendingExtend applies any staged RequestExtendIterationCap request.
	// Called by the agent loop at end of body, after ExecuteTools returns
	// and before `continue` to the next iter. Returns true if a request was
	// applied (caller should NOT bump iter so the new cap survives).
	FlushPendingExtend() (applied bool, newCap int, delta int)
}

// WithIterationExtender attaches an IterationExtender to ctx. Pipeline
// execute code calls this once per tool invocation; goal tools read it via
// IterationExtenderFromContext.
func WithIterationExtender(ctx context.Context, ext IterationExtender) context.Context {
	if ext == nil {
		return ctx
	}
	return context.WithValue(ctx, extenderKey{}, ext)
}

// IterationExtenderFromContext retrieves an IterationExtender previously
// attached via WithIterationExtender. Returns nil if absent (e.g. tool
// invoked outside the normal turn loop, or in tests that don't wrap ctx).
func IterationExtenderFromContext(ctx context.Context) IterationExtender {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(extenderKey{}).(IterationExtender)
	return v
}

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
		log.Printf("DEBUG[12.16] set_goal Write FAILED session=%s name=%s replaced=%v err=%v", sessionKey, name, replaced, err)
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	log.Printf("DEBUG[12.16] set_goal Write OK session=%s name=%s status=%s replaced=%v workspace=%s", sessionKey, name, g.Status, replaced, t.workspace)
	// Phase 11: StatusSnapshot / cross-turn reminder mechanism removed
	// (per-turn scope — the goal exists only for this turn, no need to
	// seed cross-turn context). The prompt-injection slot that used to
	// read RenderGoalSnapshot now reads the full goal content directly
	// from the active goal file at turn start.

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
- When more concrete work remains for THIS turn (e.g., running tests, applying a follow-up commit, drafting the next iteration). The remaining_steps list lets the agent keep working through them within this turn.

When NOT to use:
- When your work is done and the user has no pending decision — call complete_goal instead.
- When you need to wait for user approval, decision, or review before continuing — call complete_goal with a summary describing the wait state. complete_goal accepts any 1-1000 char summary; "Waiting for Maple to review X" is a valid summary even when goal work is not yet complete.
- Calling goal_progress with empty remaining_steps is rejected to prevent "log then complete_goal at next iter" cycles.

Required field: remaining_steps MUST be non-empty (>= 1 entry). The runtime only grants more iterations when this field is populated.

- completed_steps: things finished this turn
- blockers: anything blocking forward progress
- remaining_steps: REQUIRED. Explicit list of what's still pending — lets the agent continue within this turn
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
				"minItems":    1,
				"items":       map[string]any{"type": "string"},
				"description": "REQUIRED. Non-empty list of what's still pending — drives iteration-cap extension so the agent can keep working within this turn. Empty or missing is rejected. If your work is done or you need user approval/decision before continuing, call complete_goal instead.",
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
	// Phase 12.20: remaining_steps is REQUIRED. Empty/missing list means the
	// LLM is signalling "nothing more to do right now" — which is exactly
	// the case where complete_goal should be called instead. We previously
	// allowed empty remaining_steps + blockers/next_action as a "wait for
	// user" primitive, but in practice this caused LLM to log progress and
	// then call complete_goal at the next iteration (extra round-trip with
	// no real work). Strict rule: any goal_progress call must extend the
	// iteration cap, which only happens when remaining_steps is non-empty.
	if len(remaining) == 0 {
		return invalidInputForLLM("goal_progress: remaining_steps is required and must be non-empty. If your work is finished or you need user approval/decision before continuing, call complete_goal instead (the summary field accepts a 'Waiting for X to review Y' message).")
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
	// Phase 11: StatusSnapshot refresh removed — per-turn scope means no
	// cross-turn reminder is needed. The progress entry is persisted
	// directly in g.Progress and is visible to the LLM in subsequent
	// iterations via the in-context tool result.

	idx := len(g.Progress)
	summary := fmt.Sprintf("Logged progress entry #%d for session %s.\n\n%s",
		idx, shortSessionKey(sessionKey), lastEntryRendered(g.Progress[len(g.Progress)-1]))

	// Phase 12.38 §5 D appended cap-extension lines to the tool result so
	// the LLM could see what happened to the iteration cap in its history.
	// Phase 12.40.1 (anh Maple spec, USER.md: hide extend mechanics
	// entirely): REMOVED — any cap claim written into history goes stale
	// after CHECKPOINT→OPEN and misleads the LLM (main-turn-3 trace
	// 2026-08-02: stale "cap had been reached" → premature complete_goal).
	// Phase progression is visible via the per-iter compass header
	// ("Goal phase: open (iter N / total M)"), so the result needs no
	// cap numbers. The only remaining notice is a cap-free "no further
	// extension" line when the ceiling is hit, so the LLM does not
	// re-attempt goal_progress endlessly.
	if ext := IterationExtenderFromContext(ctx); ext != nil && len(remaining) > 0 {
		if !ext.CanExtendIterationCap() {
			// CanExtend=false: at ceiling or otherwise capped. Keep a
			// cap-free notice so the LLM doesn't re-attempt extend.
			summary += "\nNo further extension is possible this turn."
		}
	}

	// Phase 12.23: self-extend the turn's iteration cap so the agent
	// can keep working through remaining_steps across iterations of this
	// turn. Wire: goal_progress -> ExtendIterationCap(n, ...) where n is
	// sourced from the extender's MaxIterationsPerCheckpoint field. The
	// amount matches the per-checkpoint budget the runtime grants at
	// Open → Checkpoint transition (default 20 iterations), so each
	// checkpoint effectively resets the budget. Without this, the LLM
	// would burn its last iteration just to discover the cap is hit.
	//
	// Phase 12.23 removed the `RemainingIterations() > 0` half of the
	// original guard. Phase 10.1.1 added it to dodge a dead-code path
	// under the legacy Tier 3 force-wrap (which stripped tools when
	// remaining==0, so the older `==0` guard never fired). Phase 11 then
	// replaced Tier 3 with the 4-phase allowlist, and Phase 12.8 deleted
	// Tier 3 outright — at GoalPhaseCheckpoint (iter == iterationCap)
	// goal_progress IS callable and is the only tool that can lift the
	// cap. The iterationCap++ from ExtendIterationCap takes effect
	// BEFORE the loop condition is re-evaluated, so extending at
	// remaining==0 is safe: the loop continues and the LLM can call
	// complete_goal on the next iteration.
	//
	// Phase 12.35: defer-extend. Instead of calling ExtendIterationCap
	// synchronously (which would bump iterationCap mid-iter and flip
	// phase CHECKPOINT→OPEN at the goal_progress tool result append
	// boundary, see main-turn-3 trace 2026-07-31), we stage the request
	// via RequestExtendIterationCap and the agent loop applies it at
	// end of body via FlushPendingExtend. This keeps phase stable
	// within the iter — the resolver reading `iter >= iterationCap`
	// still sees CHECKPOINT for the rest of the iter.
	if ext := IterationExtenderFromContext(ctx); ext != nil {
		if len(remaining) > 0 && ext.CanExtendIterationCap() {
			amount := ext.MaxIterationsPerCheckpoint()
			if amount <= 0 {
				// Defensive: if the agent's MaxIterations was zeroed out
				// (misconfig), fall back to the live iterationCap so we
				// still add *something* rather than no-oping (n<=0 = no-op
				// per turn_state.go:544).
				amount = ext.IterationCap()
			}
			// Phase 12.24e (anh Maple decision, 2026-07-27): always extend by
			// MaxIterationsPerCheckpoint at Checkpoint. The previous ceiling-bound
			// threshold (gap ≤ 3) added complexity without benefit — ExtendIterationCap
			// already clamps to MaxIterationsCap internally (turn_state.go:591-594),
			// so a single unconditional ExtendIterationCap(amount, ...) call handles
			// every case: small gaps clamp to ceiling, large gaps grant full amount,
			// zero gap is a no-op (CanExtendIterationCap guards above).
			//
			// Phase 12.35: switched from synchronous ExtendIterationCap to
			// RequestExtendIterationCap — deferred-extend semantics keep phase
			// stable mid-iter.
			_ = ext.RequestExtendIterationCap(amount, "goal_progress: checkpoint extend by MaxIterationsPerCheckpoint (clamped to ceiling if exceeded)")
		}
	}

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

If no goal exists, or the goal is already completed, the tool returns an invalid_input error. Use set_goal to start a new one before calling this.

Provide a ` + "`summary`" + ` argument (1-1000 chars) — the LLM's final user-facing reply. The tool stores it in the archive file as the goal's ` + "`summary`" + ` field (truncated to 1000 chars if longer) and ALSO publishes the FULL summary directly to the user inside the tool call, before the turn loop continues. If the LLM has already output a text reply this iteration (assistantText non-empty), that text is sent to the user instead of the summary. Either way, the LLM's final reply is guaranteed to reach the user before the turn loop breaks.

When to use complete_goal even when goal work is not yet "fully done":
- When you need user approval, decision, or review before continuing (e.g., "Waiting for Maple to review the 2 SOP candidates"). The summary field accepts any 1-1000 char message — including wait-state descriptions. complete_goal is the canonical way to pause for user input; do NOT call goal_progress with empty remaining_steps as a substitute (it is rejected as of Phase 12.20).
- When all remaining_steps are blocked by something external (human decision, upstream dependency) and there is no more tool work to do in this turn.`
}

func (t *CompleteGoalTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary": map[string]any{
				"type":        "string",
				"minLength":   1,
				"maxLength":   MaxGoalSummaryRunes,
				"description": "Final user-facing reply (1-1000 chars). Published in FULL to the user inside the tool call; the archive stores a copy truncated to 1000 chars if longer. Used as the final reply to the user when the LLM did not output text on this iteration. If you already output a text reply, that takes precedence.",
			},
		},
		"required": []string{"summary"},
	}
}

func (t *CompleteGoalTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	if ctx == nil {
		return invalidInputForLLM("complete_goal: nil context")
	}
	sessionKey, _ := sessionKeyFromCtx(ctx)
	if sessionKey == "" {
		return invalidInputForLLM("complete_goal: no session key on context")
	}
	// Phase 11: summary is required. Empty/missing → invalid_input so the
	// LLM retries in the same iteration. The runtime cannot fabricate a
	// final reply on the LLM's behalf — that would defeat the audit trail.
	summary := strings.TrimSpace(stringArg(args, "summary"))
	if summary == "" {
		return invalidInputForLLM("complete_goal: `summary` is required (1-1000 chars). Provide your final user-facing reply.")
	}
	// Phase 12.44 (owner decision 2026-08-03): summaries longer than the
	// wire cap are TRUNCATED for the archive (never rejected) — the FULL
	// summary still reaches the user via PublishToUser below. Replacing the
	// Phase 12.28.3 strict-reject (>500) wire gate.
	archivedSummary := summary
	if utf8.RuneCountInString(archivedSummary) > MaxGoalSummaryRunes {
		archivedSummary = truncateRunes(archivedSummary, MaxGoalSummaryRunes)
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
		// Phase 12.20.1 fix (C): idempotent success (was error pre-12.20.1).
		// Goal archive in Phase 11 moved active file to archive/, so a 2nd
		// complete_goal call at iter N+1 (final-report iter) previously
		// returned "already completed (archived)" — but LLM training data may
		// retry-complete_goal on errors, looping wastefully until cap. Now
		// returns success with explicit "you may output your final report"
		// signal so LLM switches to text-only mode at iter N+1.
		return &toolshared.ToolResult{
			ForLLM: fmt.Sprintf(
				"Goal %q is already completed (archived). This is a no-op — you may output your final user-facing report and exit. Do NOT call any more tools.",
				g.Name,
			),
		}
	}

	completedCount := len(g.Progress)
	g.Status = StatusCompleted
	g.UpdatedAt = time.Now().UTC()
	g.Summary = archivedSummary // Phase 12.44: archive stores the truncated (≤1000 runes) copy.
	if err := store.Write(sessionKey, g); err != nil {
		log.Printf("DEBUG[12.16] complete_goal Write FAILED session=%s name=%s err=%v", sessionKey, g.Name, err)
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	log.Printf("DEBUG[12.16] complete_goal Write OK session=%s name=%s summary_len=%d", sessionKey, g.Name, len(summary))
	if err := store.Archive(sessionKey); err != nil {
		log.Printf("DEBUG[12.16] complete_goal Archive FAILED session=%s name=%s err=%v", sessionKey, g.Name, err)
		if tr := mapStoreError(err); tr != nil {
			return tr
		}
	}
	log.Printf("DEBUG[12.16] complete_goal Archive OK session=%s name=%s workspace=%s", sessionKey, g.Name, t.workspace)

	// Phase 12.44 (F14/F15 fold): ordering Write → Archive → Publish →
	// MarkGoalFinalized (publish-then-flag, reviewer Q-A). User nhận FULL
	// summary TRƯỚC khi state báo done — nếu ai quan sát goalFinalized=true
	// thì summary đã ra ngoài. Crash window Archive→Publish = accepted
	// limitation (microsecond, no outbox by design; MarkGoalFinalized has
	// NO fail path — in-memory flag + mutex, F21A).
	if ts := TurnStateFromContext(ctx); ts != nil && summary != "" {
		ts.PublishToUser(ctx, summary) // FULL summary — no truncate, void, best-effort
	}
	// Phase 11: mark the turn as finalized so the iteration loop breaks
	// immediately after this tool result is processed. Without this, the
	// runtime would loop back and call the LLM again (with no goal to
	// reference), wasting iterations and confusing the user.
	if ts := TurnStateFromContext(ctx); ts != nil {
		ts.MarkGoalFinalized()
		log.Printf("DEBUG[12.16] complete_goal MarkGoalFinalized session=%s name=%s", sessionKey, g.Name)
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

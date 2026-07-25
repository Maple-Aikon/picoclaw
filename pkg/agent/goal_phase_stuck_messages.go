package agent

// Phase 12.13 — phase-stuck error messages.
//
// When PicoClaw gets stuck in a particular Goal Phase (Set, Checkpoint, or
// Final) because the phase-specific lifecycle tool fails repeatedly, the
// goal is archived with a phase-specific abort_reason. The
// applyFallbackForEmptyResponse chain in turn_coord.go recognizes these
// reasons and returns the matching message to the user so they understand
// the situation instead of seeing the generic "empty response" fallback.
//
// Design rationale: the "empty response" message is misleading — LLM
// responses are NOT empty (they're 300-2000 chars). The user assumes
// provider error or token limit when the actual cause is an LLM failure
// to make progress within a constrained phase. Naming the phase in the
// error message makes the failure mode actionable.

// GoalPhaseSetStuckMessage — return when the goal was archived because
// set_goal failed 2+ times consecutively in GoalPhaseSet. The LLM
// likely wrapped args in {"raw": "..."} or omitted required fields.
// %d = fail count, %s = last error message.
const GoalPhaseSetStuckMessage = `⚠️ Goal-Set phase stuck

PicoClaw couldn't escape the Set phase — ` + "`set_goal`" + ` failed %d times in a row.

**Last error**: %s

**What this means**: the Set phase only allows ` + "`set_goal`" + `; other tools are blocked until a valid goal is set. The model produced an invalid call shape (e.g. wrapped args in {"raw": "..."} or omitted required fields).

**Try again**: send a new message and PicoClaw will re-attempt with a fresh turn. The new hint will include the exact arg shape.`

// GoalPhaseCheckpointStuckMessage — goal_progress failed 2+ times in
// GoalPhaseCheckpoint (iteration cap reached but not yet at ceiling).
const GoalPhaseCheckpointStuckMessage = `⚠️ Goal-Checkpoint phase stuck

PicoClaw couldn't proceed at the iteration cap — ` + "`goal_progress`" + ` failed %d times in a row.

**Last error**: %s

**What this means**: the Checkpoint phase only allows ` + "`goal_progress`" + ` and ` + "`complete_goal`" + `. The model couldn't produce a valid continuation summary.

**Try again**: send a new message. PicoClaw will either extend the cap with a fresh progress report, or finalize the goal via complete_goal.`

// GoalPhaseFinalStuckMessage — complete_goal failed 2+ times in
// GoalPhaseFinal (iteration >= maxIterationsCap).
const GoalPhaseFinalStuckMessage = `⚠️ Goal-Final phase stuck

PicoClaw couldn't finalize the goal — ` + "`complete_goal`" + ` failed %d times in a row.

**Last error**: %s

**What this means**: the Final phase only allows ` + "`complete_goal`" + `. The model couldn't produce a valid summary.

**Try again**: send a new message. PicoClaw will archive the goal with a partial summary and start a fresh turn.`

// Phase-stuck abort reasons (written to goal.AbortReason when the goal is
// archived). These are the keys that applyFallbackForEmptyResponse matches
// against to decide which user-facing message to return.
const (
	GoalPhaseSetStuckAbortReason        = "goal_set_stuck"
	GoalPhaseCheckpointStuckAbortReason = "goal_checkpoint_stuck"
	GoalPhaseFinalStuckAbortReason      = "goal_final_stuck"
)

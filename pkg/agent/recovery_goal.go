// Package agent — Phase 5 goal-lifecycle retry triggers.
//
// 5 recovery triggers wire the same-iteration BoundedRetry pattern for
// the goal-lifecycle feature. They are called from pipeline_llm.go /
// pipeline_execute.go after each LLM response or tool execution. Each
// trigger returns a RecoveryAction hint that the caller uses to decide
// whether to retry within the iteration, escalate to force-complete, or
// archive the goal and stop.
//
// Triggers (per plan §5.2 + §8.3):
//   1. EmptyTextResponse — Goal Phase 1, LLM returns text="" with no tool calls
//   2. TextOnly2x        — Goal Phase 1, two consecutive text-only LLM responses
//   3. ToolExecError     — executor returned IsError=true (not signature)
//   4. BoundedRetryExhausted — any BoundedRetry loop hit cap
//   5. ProviderTransient — HTTP 5xx/timeout/429 exhausted existing retry
//
// Rules:
//   - Recovery retries do NOT consume iteration slots (per §5.3)
//   - Cap exhaustion always triggers goal archive (Hook 1 + Hook 3 §8.3)
//   - Non-retryable errors (auth, 404, context-overflow-exhausted) skip recovery
package agent

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// RecoveryAction is the hint returned by recovery triggers. The caller
// applies the action and continues the agent loop without bumping iteration.
type RecoveryAction int

const (
	// RecoveryNone means no recovery is needed — proceed as normal.
	RecoveryNone RecoveryAction = iota
	// RecoveryRetrySameIteration means inject a recovery message and re-run
	// the LLM call WITHIN THE SAME iteration. History:
	//   - Phase 5: original name (intent: sub-attempt within one iter)
	//   - Phase 12.10: renamed to RecoveryRetryNextIteration to match the
	//     broken iter-bump pattern (loop top re-entered, setIteration bumped
	//     ts.iteration, so the recovery prompt actually fired in iter N+1)
	//   - Phase 12.11: renamed BACK to RecoveryRetrySameIteration after
	//     replacing the iter-bump pattern with a same-iteration BoundedRetry
	//     loop (see handleGoalRecovery in pipeline_llm.go). Retries no
	//     longer bump the iteration counter.
	//
	// Design intent (Phase 5 plan §5.3): recovery is a sub-attempt within
	// one iteration, not a new iteration. The iter-bump pattern broke the
	// boundary case where a tool exec error at iter=iterationCap-1 forced
	// an early Checkpoint phase transition and stripped the failing tool
	// from the allowlist.
	RecoveryRetrySameIteration
	// RecoveryRetryNextIteration (Phase 12.27): carry the recovery message
	// forward to the NEXT iteration via ts.pendingRecoveryMessage, do NOT
	// retry within the current iteration. Used only for text-only recovery
	// at GoalPhaseOpen (RELATIVE allowlist — iter-bump naturally escalates
	// Open → Checkpoint at cap where goal_progress/complete_goal become
	// visible). NOT used at Set/Checkpoint/Final — those are ABSOLUTE
	// allowlist phases (Phase 12.21), so iter-bump has no progress signal
	// (next iter is still in the same restricted phase) — they keep using
	// RecoveryRetrySameIteration via RecallLLM (Phase 12.26).
	RecoveryRetryNextIteration
	// RecoveryForceComplete means strip non-goal tools and force the LLM to
	// emit complete_goal on the next call. Caller must inject force-complete
	// prompt and re-run the same iteration.
	RecoveryForceComplete
	// RecoveryArchiveGoal means the goal cannot be completed. Caller must
	// call ts.finalizeGoalOnTurnEnd (Phase 6 hook; stub for now) and end
	// the turn with status=aborted.
	RecoveryArchiveGoal
)

// RecoveryContext bundles the inputs needed by trigger evaluation. Created
// fresh per-iteration by the caller.
type RecoveryContext struct {
	Phase         string // current goal phase ("" / "set" / "open" / "checkpoint" / "final") — matches GoalPhase* constants in tool_allowlist_phase.go
	Iteration     int
	TextEmpty     bool   // LLM response text was empty
	HasToolCalls  bool   // LLM response included at least one tool call
	ToolName      string // for ToolExecError trigger: which tool failed
	ToolExecError string // for ToolExecError trigger: the error message from the tool executor
	ProviderError bool   // for ProviderTransient trigger: was this a provider-side transient error

	// PostCompleteGoalReport (Phase 12.27): for text-only triggers at
	// GoalPhaseFinal — has the post-complete_goal final-report iter already
	// fired (Phase 12.7)? If true, recovery is silent (RecoveryNone) because
	// the goal is finalized + report sent; no more action possible.
	PostCompleteGoalReport bool

	// IsTransient (Phase 12.6.1): for ToolExecError trigger only — was the
	// failure classified as transient (timeout / rate-limit / network)?
	// When true, the retry prompt appends ToolExecErrorTransientHint to
	// suggest wait-then-retry without arg changes. Distinct from
	// ProviderError (which gates whether recovery fires); IsTransient
	// changes the SUGGESTED action, not whether recovery fires.
	IsTransient bool

	MaxIterations int

	// ToolKnowledgeRegistry (Phase 12) — when set and a tool execution
	// error triggers retry, the relevant tool_knowledge section is fetched
	// and appended to the recovery message so the LLM gets lessons
	// learned from prior calls. May be nil (feature disabled, default).
	ToolKnowledgeRegistry *tools.ToolRegistry
}

// Constants for recovery prompts. These are user-visible messages injected
// into the conversation history when recovery fires.
const (
	// EmptyResponseRecoveryMessage tells the LLM that its previous response
	// was empty and asks it to produce a non-empty text response or invoke
	// a tool. Single line — no newlines (CF Pages esbuild strips them in
	// emitted JS; not relevant for Go but consistent with project policy).
	EmptyResponseRecoveryMessage = "Your previous response was empty. Please produce a non-empty text response or invoke a tool to continue working toward the goal."

	// EmptyResponseHardMessage (Phase 12.37) — fires on the 3rd consecutive
	// empty response within an iteration. Hard direction per spec 9:
	// soft (#1, #2) → hard (#3) → archive (#4). Fires only when
	// EmptyResponseRecoveryCap is reached.
	EmptyResponseHardMessage = "⚠️ Your response is still empty after retries. You MUST produce a non-empty text response or invoke a tool in your next message. If the response is still empty, this turn will be archived."

	// TextOnlySoftRetryMessage (Phase 12 + 12.38 A2 rewrite) — fires on the
	// first text-only retry within an iteration. Per F30 (Kimi HIGH): must
	// NOT recommend specific tools (no `complete_goal`, no "appropriate tool")
	// because the message persists in history and may outlive the phase
	// during which the recommendation is correct. Past-tense redirect to
	// "the current system prompt" is the only durable forward path.
	TextOnlySoftRetryMessage = "Your last response was text-only with no tool call. Inspect the current system prompt before selecting your next action."

	// TextOnlyHardRetryMessage (Phase 12 + 12.38 A2 rewrite) — fires on the
	// second text-only retry within an iteration. Same F30 invariant: no
	// specific tool recommendations, redirect to system prompt.
	TextOnlyHardRetryMessage = "⚠️ Second consecutive text-only response with no tool call. Inspect the current system prompt before selecting your next action. If your next response is still text-only, this turn will be archived."

	// TextOnlySoftRetryOpenMessage (Phase 12.27) — first text-only at Open
	// phase (RELATIVE allowlist). Carries forward to NEXT iteration via
	// ts.pendingRecoveryMessage — caller at turn_coord.go bumps iter.
	TextOnlySoftRetryOpenMessage = "Your last response was text-only with no tool call. The goal is still active: continue working with an appropriate tool, or call `complete_goal` if finished. This hint will be injected at the start of the next iteration."
	// TextOnlyHardRetryOpenMessage (Phase 12.27) — second consecutive
	// text-only at Open phase. Hints MUST-decide + carry forward to next iter.
	TextOnlyHardRetryOpenMessage = "⚠️ Two consecutive text-only responses with no tool call. You MUST decide in the next iteration: (1) call `complete_goal` if the goal is finished; (2) call `complete_goal` with a question if a critical decision needs user approval; (3) call a tool to continue working. If the next iteration is still text-only, this turn will be archived."

	// ToolExecErrorRetryMessage tells the LLM that a tool execution failed
	// and asks it to retry the call (possibly with different args). Phase 12
	// rewrites this as a builder function (buildToolExecErrorRetryMessage)
	// so it can include the relevant tool_knowledge section when available.
	//
	// Phase 12.6.1: %s placeholder is TWO-shot — first %s is the tool name,
	// second %s is the error message. The builder inserts BOTH so the LLM
	// knows which tool failed (was just `errMsg` before, which was unhelpful
	// when the LLM had multiple tool calls in flight).
	ToolExecErrorRetryMessage = "Tool %q failed: %s. You may retry the same call with adjusted arguments, invoke a different tool, or call complete_goal if the goal is unreachable."

	// ToolExecErrorTransientHint is appended to the retry message when the
	// tool failure is classified as transient (timeout / 5xx / 429 / connection
	// refused / etc.). Tells the LLM that a brief retry is likely to succeed
	// without changing arguments. English per USER.md preference.
	ToolExecErrorTransientHint = " The error looks transient (timeout, rate-limit, or network). Wait briefly and retry the SAME call — argument changes are unlikely to help."

	// Phase 12.15: ToolExecErrorSetPhaseHint is appended when a tool call
	// was rejected at GoalPhaseSet (iter 1, no active goal). The allowlist
	// is `[set_goal]` only — every other tool call is blocked at the
	// execution gate. Before Phase 12.15, GoalPhaseSet SKIPPED Trigger #3
	// (tool-exec recovery) entirely, so a MiniMax-M3 `unknown({})` quirk
	// silently looped into the `max_tool_iterations` fallback. With this
	// hint, the LLM receives a concrete redirect: call set_goal with
	// top-level fields.
	ToolExecErrorSetPhaseHint = " In the current goal phase (set), only `set_goal` is available — every other tool call is blocked. Call `set_goal` with top-level fields (name, objective, success_criteria[]) to start the goal: the tool name is `set_goal`, the field names are `name`, `objective`, `success_criteria[]` (array), and optional `in_scope[]`, `out_of_scope[]`, `cadence`. Do NOT wrap the arguments inside a `{\"raw\":\"...\"}` envelope — provide each field as a top-level key."

	// Phase 12.15: ToolExecErrorFinalPhaseHint is appended when a tool call
	// was rejected at GoalPhaseFinal (post-complete_goal final-report iter).
	// Allowlist is `[complete_goal]` only. If the LLM emits a broken tool
	// call here (e.g. MiniMax-M3 `unknown({})` quirk), the message redirects
	// back to complete_goal (the only allowed tool) until the goal is fully
	// closed. Skipped when postCompleteGoalReportSent=true (the final report
	// has already been published — no more iterations happen).
	ToolExecErrorFinalPhaseHint = " In the current goal phase (final), only `complete_goal` is available — every other tool call is blocked. The goal is already finalized; call `complete_goal` with a non-empty `summary` (1-1000 chars) to close out the turn, or reply with the final user-facing report directly. The summary field accepts wait-state descriptions such as 'Waiting for user approval before proceeding with X' — use this when you need user approval/decision before continuing."

	// Phase 12.18: ToolExecErrorCheckpointPhaseHint is appended when a tool
	// call was rejected at GoalPhaseCheckpoint (iter == iterationCap, hit the
	// cap). Allowlist is `[goal_progress, complete_goal]` only. The LLM has
	// hit the iteration cap and must either extend (goal_progress) or wrap
	// up (complete_goal) — calling any other tool is blocked at the
	// execution gate. Before Phase 12.18, recovery (Trigger #3) silently
	// MISSED this rejection because checkToolExecErrorRecovery only matched
	// the legacy "Tool execution failed:" prefix and the Phase 12.3
	// execution gate uses a different format ("tool %q is not available in
	// the current phase..."). Combined effect was a silent recovery blind
	// spot that ended the turn with a canned "max_tool_iterations" string.
	// Telegram user feedback 2026-07-26: main-turn-4 hit this at iter 25.
	//
	// Phase 12.38: replaced "final iteration" wording with a past-tense
	// consequence-based hint (F37/F44''). Phase 12.40 (anh Maple spec,
	// 2026-08-02): the consequence-based wording still poisoned history —
	// "the iteration cap had been reached" is a stale claim once the cap is
	// extended CHECKPOINT→OPEN, and main-turn-3 18:41 trace showed the LLM
	// reading it at OPEN and calling complete_goal prematurely. The hint
	// must NOT claim anything about the iteration cap; it names the 2
	// available tools and the 3 decision paths (save checkpoint via
	// goal_progress / end turn to ask user approval via wait-state summary /
	// end turn when goal is done via final summary).
	ToolExecErrorCheckpointPhaseHint = " In the current goal phase (checkpoint), only `goal_progress` and `complete_goal` are available — every other tool call is blocked. Choose one of three paths: (1) save a checkpoint and keep working: call `goal_progress` with at least 1 item in `remaining_steps`; (2) end the turn to ask the user for approval/decision: call `complete_goal` with a summary like \"Waiting for user approval before continuing with X\"; (3) end the turn because the goal is done: call `complete_goal` with a concise summary (1-1000 chars) of what was accomplished."

	// Phase 12.32: ToolExecErrorOpenPhaseHint is appended ONLY when a
	// lifecycle tool (set_goal / goal_progress) is rejected at OPEN phase.
	// Open is a RELATIVE phase — only 2 of 83 visible tools are blocked by
	// the lifecycle gate, so always-append (like Set/Checkpoint/Final)
	// would mislead. The hint directs the LLM to pivot to complete_goal
	// rather than retry the blocked lifecycle tool.
	ToolExecErrorOpenPhaseHint = " `set_goal` is LOCKED at OPEN phase (it only fires at the SET phase / iter 1). `goal_progress` is CHECKPOINT-only (it only fires at a checkpoint). At OPEN, only `view_goal` and `complete_goal` are available for lifecycle operations. Pivot to `complete_goal` with a non-empty `summary` (1-1000 chars) when the work is done, or call other non-lifecycle tools. Do NOT retry the same blocked lifecycle tool."

	// Phase 12.51b — Checkpoint/Final iter=cap escape hatch.
	// Defense-in-depth at the iter=cap boundary. State-agnostic per
	// Phase 12.40 spirit: NO iteration-cap claims (claims go stale after
	// extension/phase transition and poison history). Tool names + 3-path
	// decision tree OK for LLM-actionable text. Message persists to
	// history via pipeline_finalize.go:51-53 — wording must remain valid
	// across subsequent turns. Set/Open behavior unchanged: Set text-only
	// is silent (valid turn end), Open text-only falls through to
	// toolLimitResponse (Phase 12.27 D3 carry semantics).
	ToolLimitCheckpointRetryMessage = "You have reached a checkpoint. Your available tools are `goal_progress` (extend) and `complete_goal` (finalize). Choose: (a) save checkpoint with remaining steps, (b) end turn with a wait-state summary, (c) end turn with an accomplishment summary."

	ToolLimitFinalRetryMessage = "You have reached the final phase. Your available tool is `complete_goal` (finalize with a 1-1000 character summary)."
)

// phaseContextSuffix returns a short past-tense description of the goal
// phase at the time of the retry, suitable for appending to a recovery
// message body. Crucially, it does NOT claim current-phase availability
// (e.g., "Only X is available") because the message persists in
// conversation history and may outlive the current phase — the user can
// transition phase between the recovery message being appended and the
// LLM reading it. The system prompt (per-iter) is the source of truth
// for CURRENT-phase availability; this suffix only documents the phase
// AT THE MOMENT OF THE FAILURE for retrospective reasoning.
//
// Returns "" for unknown / empty phase (fail-closed — caller decides
// whether to drop the suffix or use a static fallback).
//
// Phase 12.38 v2 §3 / §1.3.
// F30 (Kimi HIGH): even past-tense tool claims ("most tools are routable",
// "only `complete_goal` could lift the cap") violate the F20/F30 invariant
// — feature flags, tenant policy, or dynamic tool registry can change
// between append and re-read. Fix: suffix describes ONLY phase name +
// STATE CONSEQUENCE ("the goal had not yet been seeded", "after reaching
// the iteration cap"). NEVER names a tool, NEVER says what was routable.
// Tool guidance lives in the per-iter system prompt (Task 3).
func phaseContextSuffix(phase string) string {
	// Phase 12.48b site 7: policy-driven lookup. Each phase's ContextSuffix
	// is the single source of truth for what gets appended to retry
	// messages. Empty / unknown phase → empty string (no suffix).
	if policy := PhasePolicyFor(GoalPhase(phase)); policy != nil {
		return policy.ContextSuffix
	}
	return ""
}

// buildEmptyResponseRetryMessageWithPhase constructs the empty-response
// retry message body for the given phase, appending the phase-context
// suffix. Used by both the empty-trigger and text-only-trigger return
// sites at lines 323/334/357 (Phase 12.38 A2).
//
// The `hard` flag selects between the soft (EmptyResponseRecoveryMessage)
// and hard (EmptyResponseHardMessage) variants — Phase 12.37 escalation.
func buildEmptyResponseRetryMessageWithPhase(phase string, hard bool) string {
	base := EmptyResponseRecoveryMessage
	if hard {
		base = EmptyResponseHardMessage
	}
	return base + phaseContextSuffix(phase)
}

// buildTextOnlyRetryMessageWithPhase constructs the text-only-restricted
// retry message body for the given phase, appending the phase-context
// suffix. Used by the restricted (non-OPEN) text-only return sites at
// lines 323/334 (Phase 12.38 A2).
//
// The `restricted` flag distinguishes the restricted-phase path (lines
// 323/334, same-iter retry with counter) from the OPEN-phase path
// (lines 374+, next-iter carry, EXEMPT from suffix per §3.4). When
// `restricted=false` (OPEN path), this helper returns the OPEN text
// with NO suffix — preserved by the OPEN path's existing next-iter
// carry mechanism (ts.pendingRecoveryMessage).
func buildTextOnlyRetryMessageWithPhase(phase string, hard bool, restricted bool) string {
	if !restricted {
		// OPEN path: next-iter carry uses the OPEN-specific constants,
		// NOT the restricted ones. No suffix appended (preserves OPEN's
		// next-iter semantics — suffix would persist in history for the
		// next iter even if phase flipped).
		if hard {
			return TextOnlyHardRetryOpenMessage
		}
		return TextOnlySoftRetryOpenMessage
	}
	base := TextOnlySoftRetryMessage
	if hard {
		base = TextOnlyHardRetryMessage
	}
	return base + phaseContextSuffix(phase)
}

// Caps for each trigger. Per §5.2 + §5.3 — these are sub-attempt counts
// inside one iteration, NOT iteration counts.
const (
	EmptyResponseRecoveryCap     = 3  // Phase 12.37: 3 same-iter retries per spec 9 (was 2; now soft-soft-hard)
	// Phase 12 redesign: text-only retries fire 2x per iteration with escalation.
	// Soft prompt (TextOnlySoftRetryMessage) fires first, then hard prompt
	// (TextOnlyHardRetryMessage). If both fire and LLM still produces
	// text-only, archive the goal.
	//
	// Both caps are PER-ITERATION counts (not cross-iteration streak). The
	// cross-iteration textOnlyStreak field still tracks consecutive text-only
	// iterations but is no longer the gate — recovery fires within iteration.
	//
	// Phase 12.37 D3: restricted-phase text-only uses 2 soft + 1 hard = 3
	// total same-iter attempts (spec 9). Open-phase text-only stays
	// next-iter carry (spec 7) — separate counters with cap=1+1 preserved
	// for that path so iter-bump remains the escalation path.
	TextOnlySoftRetryCap         = 2  // restricted phase: 2 soft prompts per iter (spec 9)
	TextOnlyHardRetryCap         = 1  // restricted phase: 1 hard prompt per iter (fires on 3rd text-only)
	TextOnlySoftRetryCapOpen     = 1  // open phase: 1 soft prompt before escalating via next-iter carry (spec 7)
	TextOnlyHardRetryCapOpen     = 1  // open phase: 1 hard prompt before archive (spec 7)
	ToolExecErrorRetryCap         = 3  // per-tool retry up to 3 within same iteration
	ProviderTransientRetryCap     = 3  // matches existing callLLMCore cap
)

// evaluateRecovery decides which recovery action to take based on the
// RecoveryContext and the per-iteration counters on ts. Returns the action
// plus an optional message to inject into the conversation.
//
// This function is pure (no side effects, no logger writes) so it can be
// unit-tested without mocking the full pipeline.
func evaluateRecovery(ts *turnState, ctx RecoveryContext) (RecoveryAction, string) {
	// Plan §3.3 site 4 (Phase 12.48b): policy-driven early-return.
	// PostCompleteGoalReportSent (runtime flag — not in policy) still
	// supersedes policy for the final-report iter. Empty phase (no
	// currentGoalPhase() value) = no recovery.
	if ctx.Phase == "" || ts.postCompleteGoalReportSent {
		return RecoveryNone, ""
	}
	// Resolve policy row (case-insensitive, nil-safe). Empty policy =
	// fail-CLOSED — unknown phase strings fall back to RecoveryNone.
	// Site 4 lock: EmptyAction == RecoveryNone ONLY at PostFinal
	// (R5 invariant — schema-locked at TestPhasePolicy_AgentSide_EmptyActionOnlyRecoveryNoneAtPostFinal).
	policy := PhasePolicyFor(GoalPhase(ctx.Phase))
	if policy == nil || policy.EmptyAction == RecoveryNone {
		return RecoveryNone, ""
	}

	// Provider transient (Trigger #5): always retry up to cap. Independent
	// of goal phase. The existing callLLMCore retry already runs 3 times;
	// when exhausted we archive the goal (Hook 3 §8.3).
	if ctx.ProviderError {
		// Bounded retry was exhausted by callLLMCore — escalate to archive.
		return RecoveryArchiveGoal, "Provider API retry exhausted; archiving goal."
	}

	// Trigger #3: tool execution error (executor returned IsError=true).
	// Skip if no tool name provided.
	// Phase 12.15: GoalPhaseSet and GoalPhaseFinal are NOW eligible for
	// tool-exec recovery (was previously skipped). The recovery prompt
	// appends a phase-specific redirect hint (ToolExecErrorSetPhaseHint /
	// ToolExecErrorFinalPhaseHint) so the LLM is told which tool is the
	// only one allowed in the current phase. This closes the recovery-
	// blind-spot where MiniMax-M3 `unknown({})` quirk at GoalPhaseSet
	// would silently loop into the `max_tool_iterations` fallback without
	// any feedback to the LLM (Telegram turn 16:06 ICT 2026-07-25).
	// Phase 12: when about to retry, fetch tool_knowledge for that tool
	// (lessons learned from prior calls) and append to the prompt so the
	// LLM gets relevant guidance instead of repeating the same mistake.
	if ctx.ToolName != "" {
		if ts.toolExecRecoveryAttempts == nil {
			ts.toolExecRecoveryAttempts = make(map[string]int)
		}
		if ts.toolExecRecoveryAttempts[ctx.ToolName] < ToolExecErrorRetryCap {
			ts.toolExecRecoveryAttempts[ctx.ToolName]++
			// Phase 12.6.1: thread `IsTransient` so the prompt can suggest
			// wait-then-retry (transient) vs diagnose-or-recomplete (permanent).
			// Caller (checkToolExecErrorRecovery / pipeline) sets this from
			// the tool result's error text + circuit-breaker state.
			msg := buildToolExecErrorRetryMessage(ctx.ToolName, ctx.ToolExecError, ctx.IsTransient, ctx.ToolKnowledgeRegistry, ctx.Phase)
			return RecoveryRetrySameIteration, msg
		}
		// Phase 12.55 Q2/Q3: tool-exec exhaustion archives ONLY at
		// Set/Checkpoint/Final. At Open (and PostFinal) the goal stays
		// active — the error result is already in history for the next
		// iteration, and the LLM may retry a different approach.
		if !shouldArchiveToolExecExhausted(GoalPhase(ctx.Phase)) {
			return RecoveryNone, ""
		}
		return RecoveryArchiveGoal, "Tool execution error retry exhausted for " + ctx.ToolName + "."
	}

	// Tool call resets counters (defensive) — applies in ALL phases when LLM
	// is productive. Must happen BEFORE phase-specific dispatch so counters
	// stay clean across Set/Checkpoint/Final too (not just Open).
	if ctx.HasToolCalls {
		ts.textOnlyStreak = 0
		ts.textOnlySoftRetriesDone = 0
		ts.textOnlyHardRetriesDone = 0
	}

	// Triggers #1 (empty) only fires in Open phase. Triggers #2 (text-only)
	// fires in ALL phases but with different action + counter semantics:
	//   - Open (RELATIVE allowlist): RecoveryRetryNextIteration (Phase 12.27),
	//     NO counter increment — iter-bump naturally escalates Open → Checkpoint
	//     at cap where goal_progress/complete_goal become visible. Carry msg
	//     forward via ts.pendingRecoveryMessage, inject at next iter's prompt.
	//   - Set/Checkpoint/Final (ABSOLUTE allowlist, Phase 12.21): RecoveryRetrySameIteration
	//     with counter increment — caller (pipeline_llm.go:727) wraps via RecallLLM
	//     (Phase 12.26 canonical helper). Same-iter retry is MANDATORY for
	//     restricted allowlist — iter-bump has no progress signal here.
	//   - Final with postCompleteGoalReportSent=true: silent (RecoveryNone)
	//     — goal is finalized + report already sent, no more action possible.
	// Phase 12.48b site 5: TextOnlyMode-driven dispatch.
	// Set/Checkpoint/Final → TextOnlyRestricted (2 soft + 1 hard same-iter).
	// Open → TextOnlyOpenCarry (1 soft + 1 hard next-iter via pendingRecoveryMessage).
	// Set text-only (direct reply) → TextOnlyOpenSilent — valid turn end.
	// PostFinal → TextOnlyNone — already handled by early-return above.
	// Phase 12.46: SET excluded from RESTRICTED text-only — direct text
	// reply at SET is a valid turn end (owner decision, anh Maple 2026-08-03).
	if !ctx.HasToolCalls && !ctx.TextEmpty && policy.TextOnlyMode == TextOnlyRestricted {
		// Final-with-postCompleteGoalReport = silent (Phase 12.27 — runtime
		// flag, not in policy; the only Phase 12.27 invariant that
		// overrides the policy table).
		if ctx.Phase == string(GoalPhaseFinal) && ctx.PostCompleteGoalReport {
			return RecoveryNone, ""
		}
		ts.textOnlyStreak++
		if ts.textOnlySoftRetriesDone < TextOnlySoftRetryCap {
			ts.textOnlySoftRetriesDone++
			logger.InfoCF("agent", "Text-only soft retry fired (restricted phase)", map[string]any{
				"agent_id":  agentIDFromTS(ts),
				"iteration": ctx.Iteration,
				"phase":     ctx.Phase,
				"soft_done": ts.textOnlySoftRetriesDone,
				"hard_done": ts.textOnlyHardRetriesDone,
			})
			// Phase 12.38 A2: append phase-context suffix for restricted phases.
			return RecoveryRetrySameIteration, buildTextOnlyRetryMessageWithPhase(ctx.Phase, false, true)
		}
		if ts.textOnlyHardRetriesDone < TextOnlyHardRetryCap {
			ts.textOnlyHardRetriesDone++
			logger.InfoCF("agent", "Text-only hard retry fired (restricted phase)", map[string]any{
				"agent_id":  agentIDFromTS(ts),
				"iteration": ctx.Iteration,
				"phase":     ctx.Phase,
				"soft_done": ts.textOnlySoftRetriesDone,
				"hard_done": ts.textOnlyHardRetriesDone,
			})
			// Phase 12.38 A2: append phase-context suffix for restricted phases.
			return RecoveryRetrySameIteration, buildTextOnlyRetryMessageWithPhase(ctx.Phase, true, true)
		}
		// Both soft + hard fired this iteration; archive the goal.
		logger.WarnCF("agent", "Text-only retry cap exhausted — archiving goal (restricted phase)", map[string]any{
			"agent_id":  agentIDFromTS(ts),
			"iteration": ctx.Iteration,
			"phase":     ctx.Phase,
			"streak":    ts.textOnlyStreak,
		})
		return RecoveryArchiveGoal, "Text-only retry cap exhausted (1 soft + 1 hard per iteration, restricted phase)."
	}

	// Phase 12.37 GAP #2: Trigger #1 (empty) fires at ALL phases —
	// previously Open-only. Restricted phases with an empty response fell
	// through to RecoveryNone → turn ended with DefaultResponse, never
	// retried (spec point 8a). Count-based cap: soft hint #1-#2, hard
	// direction #3 (spec point 9). Final+post-report stays silent.
	if ctx.TextEmpty && !ctx.HasToolCalls {
		if ctx.Phase == string(GoalPhaseFinal) && ctx.PostCompleteGoalReport {
			return RecoveryNone, ""
		}
		if ts.emptyResponseRecoveryCount < EmptyResponseRecoveryCap {
			ts.emptyResponseRecoveryCount++
			// Phase 12.38 A2: empty-response retry at all phases (Phase 12.37 GAP #2)
			// gets the phase-context suffix. Restricted phases would otherwise
			// leave no phase marker in the persisted history, making the
			// "Checkpoint→OPEN" transition confusing on re-read.
			hard := ts.emptyResponseRecoveryCount >= EmptyResponseRecoveryCap
			msg := buildEmptyResponseRetryMessageWithPhase(ctx.Phase, hard)
			if IsAgentDebugEnabled() {
				AgentDebugRecovery(ts.turnID, ts.sessionKey, ctx.Iteration, GoalPhase(ctx.Phase), "EmptyResponse", ts.emptyResponseRecoveryCount)
			}
			return RecoveryRetrySameIteration, msg
		}
		return RecoveryArchiveGoal, "Empty response retry exhausted (3 retries, all phases)."
	}

	// Open phase: Trigger #2 (text-only) with next-iter carry.
	// Phase 12.48b site 5: TextOnlyMode == TextOnlyOpenCarry dispatch.
	// Fire soft prompt first, then hard prompt, then archive.
	// At OPEN phase, we use RecoveryRetryNextIteration (NOT RecoveryRetrySameIteration):
	// carry the message forward via ts.pendingRecoveryMessage, caller at
	// turn_coord.go bumps iter naturally. NO counter increment — Open has
	// no per-iter cap because iter-bump is the escalation path.
	// Phase 12.46: explicit GoalPhaseOpen guard — SET text-only falls
	// through to RecoveryNone (TextOnlyOpenSilent) and must NOT hit this branch.
	if !ctx.HasToolCalls && !ctx.TextEmpty && policy.TextOnlyMode == TextOnlyOpenCarry {
		ts.textOnlyStreak++
		// Increment within-iteration escalation counters in order.
		// Phase 12.37 D3: OPEN path uses cap=1+1 (separate from
		// restricted path's 2+1) per spec 7 — next-iter carry is the
		// escalation path, NOT same-iter retry.
		if ts.textOnlySoftRetriesDone < TextOnlySoftRetryCapOpen {
			ts.textOnlySoftRetriesDone++
			logger.InfoCF("agent", "Text-only soft retry fired (Open phase, next-iter)", map[string]any{
				"agent_id":  agentIDFromTS(ts),
				"iteration": ctx.Iteration,
				"phase":     ctx.Phase,
				"soft_done": ts.textOnlySoftRetriesDone,
				"hard_done": ts.textOnlyHardRetriesDone,
			})
			return RecoveryRetryNextIteration, TextOnlySoftRetryOpenMessage
		}
		if ts.textOnlyHardRetriesDone < TextOnlyHardRetryCapOpen {
			ts.textOnlyHardRetriesDone++
			logger.InfoCF("agent", "Text-only hard retry fired (Open phase, next-iter)", map[string]any{
				"agent_id":  agentIDFromTS(ts),
				"iteration": ctx.Iteration,
				"phase":     ctx.Phase,
				"soft_done": ts.textOnlySoftRetriesDone,
				"hard_done": ts.textOnlyHardRetriesDone,
			})
			return RecoveryRetryNextIteration, TextOnlyHardRetryOpenMessage
		}
		// Both soft + hard fired this iteration; archive the goal.
		logger.WarnCF("agent", "Text-only retry cap exhausted — archiving goal (Open phase)", map[string]any{
			"agent_id":  agentIDFromTS(ts),
			"iteration": ctx.Iteration,
			"phase":     ctx.Phase,
			"streak":    ts.textOnlyStreak,
		})
		return RecoveryArchiveGoal, "Text-only retry cap exhausted (1 soft + 1 hard per iteration, Open phase)."
	} else if ctx.HasToolCalls {
		// Reset streak + per-iteration escalation counters when LLM calls
		// a tool (productive turn). Counters are now useless for the
		// current iteration but keep them clean for the next iteration
		// boundary (defensive — they will be reset at iteration bump
		// anyway, but resetting here documents intent).
		ts.textOnlyStreak = 0
		ts.textOnlySoftRetriesDone = 0
		ts.textOnlyHardRetriesDone = 0
	}

	return RecoveryNone, ""
}

// agentIDFromTS returns the agent ID for logging context, or empty if no agent.
func agentIDFromTS(ts *turnState) string {
	if ts.agent != nil {
		return ts.agent.ID
	}
	return ""
}

// checkToolExecErrorRecovery examines the most recent tool result message
// in exec.messages. If it's a tool message with an executor error
// (signaled via the legacy "Tool execution failed:" prefix OR via the
// Phase 12.3 execution gate's "tool %q is not available in the current
// phase..." format), this calls evaluateRecovery with the Trigger #3
// context. Returns (toolName, msg) when archive is requested, ("", "")
// otherwise.
//
// Phase 12.18: added Phase 12.3 execution gate format to the matcher.
// The execution gate in pkg/tools/registry.go rejects non-allowlist tool
// calls at runtime with `tool %q is not available in the current phase
// (allowed tools: %v)` — this format is what reaches the LLM but the
// legacy heuristic only matched `Tool execution failed:`. As a result,
// tool-exec recovery (Trigger #3) silently MISSED the most common
// checkpoint-phase error pattern (LLM calls a non-lifecycle tool after
// hitting iteration cap), leading to the canned "max_tool_iterations"
// fallback. We now also match the execution gate format and extract the
// failing tool name from the quoted name in the message.
//
// Tool-name extraction: `tool "X" is not available` -> X via prefix
// matching. Falls back to last.ToolCallID when format doesn't yield a
// quoted name (e.g. tool-not-found errors use `tool %q not found` which
// also starts with `tool "` so the same parser works).
func checkToolExecErrorRecovery(ts *turnState, exec *turnExecution) (string, string) {
	if exec == nil || len(exec.messages) == 0 {
		return "", ""
	}
	last := exec.messages[len(exec.messages)-1]
	// Only tool-role messages carry tool results.
	if last.Role != "tool" {
		return "", ""
	}

	// Phase 12.28.3 Fix B: typed ErrKind gate is the PRIMARY classifier.
	// Tool handlers call `toolshared.ErrorResult(...).WithErrorKind(...)`
	// which the executor threads into ts.lastToolResult — recover it
	// here so validation errors (Phase 12.20 complete_goal byte-count
	// check, Phase 12.x set_goal regex, etc.) trigger recovery even
	// when the error string doesn't match the legacy prefix.
	//
	// Recoverable kinds: ErrInvalidInput (validation), ErrTransient
	// (retry), ErrTimeout (retry). Non-recoverable kinds: ErrFatal,
	// ErrDependencyDown (upstream down) — retryable since Phase 12.55 Q1
	// (rate-limit style failures usually recover on retry 2-3).
	//
	// ts.lastToolResult is nil in legacy fixtures and any code path that
	// hasn't yet threaded it through pipeline_execute.go — fall back to
	// prefix heuristic (Phase 12.18 path) so we don't regress old tests.
	tsToolErrKind := toolshared.ErrorKind("")
	if ts != nil && ts.lastToolResult != nil {
		tsToolErrKind = ts.lastToolResult.ErrKind
	}
	const executorErrPrefix = "Tool execution failed:"
	isExecutorErr := len(last.Content) >= len(executorErrPrefix) && last.Content[:len(executorErrPrefix)] == executorErrPrefix

	recoverable := false
	switch {
	case tsToolErrKind != "":
		switch tsToolErrKind {
		case toolshared.ErrInvalidInput, toolshared.ErrTransient, toolshared.ErrTimeout, toolshared.ErrDependencyDown:
			recoverable = true
		default:
			return "", "" // non-recoverable typed error
		}
	default:
		// Legacy prefix fallback: "Tool execution failed:" — executor
		// error wrap (real runtime failures). Phase 12.46: the execution
		// gate rejection path (`tool "X" is not available...`) is GONE —
		// ExecuteTools pre-checks IsAllowed at every phase (Phase 12.35 +
		// 12.37 + 12.46) and stamps ErrInvalidInput, so gate blocks never
		// reach the executor and never need a prefix match here.
		recoverable = isExecutorErr
	}
	if !recoverable {
		return "", ""
	}
	toolName := last.ToolCallID
	if toolName == "" {
		toolName = "unknown"
	}
	action, msg := evaluateRecovery(ts, RecoveryContext{
		Phase:         string(ts.currentGoalPhase()),
		Iteration:     ts.iteration,
		TextEmpty:     false,
		HasToolCalls:  true,
		ToolName:      toolName,
		ToolExecError: last.Content,
		// Phase 12.28.3 Fix B: typed ErrKind wins over prefix heuristic.
		// ErrTransient / ErrTimeout → IsTransient=true (auto-retry hint).
		// ErrInvalidInput / unknown typed → IsTransient=false (arg-shape fix).
		// Empty ErrKind (legacy executor) → fall back to prefix heuristic
		// (Phase 12.6.1 + 12.18 behavior).
		IsTransient:   isTransientFromErrKind(tsToolErrKind, last.Content),
		MaxIterations: ts.iterationCap,
	})
	if action == RecoveryArchiveGoal {
		return toolName, msg
	}
	// Phase 12.18: also propagate msg for retry actions so callers
	// (tests + future same-iter recovery) can verify the injected hint
	// matches the current phase. Production callers (turn_coord.go:361
	// and pipeline_llm.go:728) only inspect toolName to decide between
	// archive vs continue, so returning "" here is safe.
	return "", msg
}

// buildToolExecErrorRetryMessage constructs the retry message for the
// tool-execution-error recovery trigger. Phase 12: when a tool knowledge
// store is configured and a lesson body exists for the failing tool, that
// body is appended to the message so the LLM gets concrete guidance from
// prior calls (avoid the same mistake / pick the right argument shape /
// surface a known workaround).
//
// Phase 12.6.1: now takes `isTransient` flag — when true, appends the
// transient-hint suffix (suggests wait-then-retry without arg changes).
// When false, the base message asks for diagnose-or-recomplete logic.
//
// Phase 12.18: now takes `phase` string — when Phase==GoalPhaseSet it
// appends ToolExecErrorSetPhaseHint (tells the LLM the only allowed tool
// is set_goal), when Phase==GoalPhaseFinal it appends
// ToolExecErrorFinalPhaseHint (only complete_goal is allowed), and when
// Phase==GoalPhaseCheckpoint it appends ToolExecErrorCheckpointPhaseHint
// (only goal_progress + complete_goal are allowed). Other phases (Open)
// get no suffix — the base message + transient hint + tool knowledge are
// sufficient for those.
//
// Format (non-transient, phase=Open/Checkpoint/empty):
//
//	"Tool "view_goal" failed: <errMsg>. You may retry the same call with
//	 adjusted arguments, invoke a different tool, or call complete_goal
//	 if the goal is unreachable.\n\n<Tool knowledge for <toolName>>:\n
//	 <body>"
//
// Format (transient) — appends ToolExecErrorTransientHint:
//
//	"Tool "view_goal" failed: <errMsg>. You may retry the same call with
//	 adjusted arguments, invoke a different tool, or call complete_goal
//	 if the goal is unreachable. The error looks transient (timeout,
//	 rate-limit, or network). Wait briefly and retry the SAME call —
//	 argument changes are unlikely to help.\n\n<Tool knowledge>:\n<body>"
//
// Format (phase=Set) — appends ToolExecErrorSetPhaseHint AFTER the base
// message but BEFORE tool knowledge: the LLM is told "set_goal is the
// only allowed tool, here is the argument shape".
//
// Returns just the standard message when registry is nil or no knowledge
// exists for the tool. Never returns an empty string.
func buildToolExecErrorRetryMessage(toolName, errMsg string, isTransient bool, registry *tools.ToolRegistry, phase string) string {
	base := fmt.Sprintf(ToolExecErrorRetryMessage, toolName, errMsg)
	if isTransient {
		base += ToolExecErrorTransientHint
	}
	// Phase 12.48b site 6: ToolExecHint lookup via policy table.
	// ABSOLUTE phases (Set/Checkpoint/Final) carry the hint in policy.ToolExecHint.
	// OPEN carries the hint in policy.ToolExecHint only when toolName ∈ {set_goal, goal_progress}
	// (RELATIVE allowlist — appending to read_file/exec errors would mislead).
	policy := PhasePolicyFor(GoalPhase(phase))
	if policy != nil && policy.ToolExecHint != "" {
		if phase == string(GoalPhaseOpen) {
			// Phase 12.32: Open phase is RELATIVE — only lifecycle tools
			// (set_goal / goal_progress) need the hint. Other tools
			// work fine at OPEN; the error is unrelated to the lifecycle gate.
			if toolName == "set_goal" || toolName == "goal_progress" {
				base += policy.ToolExecHint
			}
		} else {
			// ABSOLUTE phase — always append the phase-pinned hint.
			base += policy.ToolExecHint
		}
	}
	if registry == nil {
		return base
	}
	store := registry.ToolKnowledgeStore()
	if store == nil {
		return base
	}
	knowledge := store.LoadForEscalation(toolName)
	if knowledge == "" {
		return base
	}
	return base + "\n\n" + tools.AppendKnowledgeSection(knowledge)
}

// isTransientErrorText classifies a tool-execution error message as
// transient or permanent based on substring markers. Phase 12.6.1 — when
// true, the retry prompt appends ToolExecErrorTransientHint suggesting
// wait-then-retry without arg changes.
//
// Markers (intentionally substring matches — error wording varies across
// tools):
//
//   - "connection"      (refused / reset / closed) — network failures
//   - "timeout"         (i/o / handshake) — network failures
//   - "rate limit"      — provider-side throttle (HTTP 429)
//   - "429" / "502" / "503" / "504" — HTTP transient codes
//   - "no such host"    — DNS failures
//
// Heuristic — false positives and false negatives are both acceptable.
// Conservative bias: prefer false-negative (say permanent when actually
// transient) so the LLM gets the standard retry prompt instead of the
// wait-then-retry hint. The standard prompt still allows the LLM to retry
// the same call.
func isTransientErrorText(errMsg string) bool {
	lower := strings.ToLower(errMsg)
	transientMarkers := []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"timeout",
		"rate limit",
		"http 429",
		"http 502",
		"http 503",
		"http 504",
		"no such host",
		"tls handshake",
	}
	for _, m := range transientMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// isTransientFromErrKind combines the typed ErrKind classifier with the
// legacy prefix heuristic for transient-error detection. Used by
// checkToolExecErrorRecovery to populate RecoveryContext.IsTransient.
//
// Rules (Phase 12.28.3):
//  1. Typed transient kinds (ErrTransient, ErrTimeout) → true regardless
//     of message content. LLM should "wait and retry".
//  2. Typed non-transient kinds (ErrInvalidInput, ErrFatal,
//     ErrDependencyDown, unknown) → false. LLM should "fix args or use
//     a different tool".
//  3. Empty ErrKind (legacy executor / execution-gate-rejected path)
//     → fall back to the Phase 12.6.1 prefix heuristic.
//     Execution-gate rejections are always non-transient (Phase 12.18:
//     arg changes can't help — the tool is blocked by phase policy).
func isTransientFromErrKind(kind toolshared.ErrorKind, content string) bool {
	switch kind {
	case toolshared.ErrTransient, toolshared.ErrTimeout:
		return true
	case toolshared.ErrInvalidInput, toolshared.ErrDependencyDown:
		return false
	}
	// Empty / unknown typed kind — prefix heuristic.
	// Phase 12.46: the execution-gate param is gone (gate blocks are always
	// typed ErrInvalidInput by the pre-check); the executor-error heuristic
	// applies.
	return isTransientErrorText(content)
}

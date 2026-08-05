// Package agent: Phase 12.48b single-policy-module (agent side).
//
// File: pkg/agent/phase_policy.go
//
// Per Plan §3.3 — agent-side policy table that drives sites 4-11 (recovery,
// stuck, gate-skip, discovery rule, MCP availability). The 6 state-render
// sites (formatIterCompass + 5 hint contributors) are EXCLUDED per Q1=A
// decision — they read live iter/cap/goalFinalized state and stay as code
// (deferred to Phase 12.49+ per T15 gate).
//
// The pkg/tools side (ToolVisibilityPolicy) already exists at
// pkg/tools/phase_policy.go and is the source of truth for sites 1-3
// (Phase 12.48a shipped). This file is the AGENT side — sites 4-11. Both
// tables share the same PhaseToken keys (pkg/phases.AllTokens()) so they
// cannot drift.
//
// Schema lock invariants (TestPhasePolicy_AgentSide_SchemaLock):
//   1. Keys of agentPolicies == phases.AllTokens() (5 rows: Set/Open/Checkpoint/Final/PostFinal)
//   2. Each row's Phase field matches the map key
//   3. GateSkipText / DiscoveryRuleText / MCPAvailText are non-empty (PostFinal may have
//      special wording but still non-empty — guards against silent typos)
//   4. ToolExecHint for OPEN is empty (use toolName gate; Phase 12.32 ABSOLUTE/RELATIVE
//      distinction — OPEN hint only fires when toolName ∈ {set_goal, goal_progress})
//   5. EmptyAction == RecoveryNone ONLY at PostFinal (R5 lock)
//
package agent

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/phases"
)

// StuckBucket classifies a phase by which lifecycle tool's failure count
// should trigger a phase-stuck abort. Used by sites 8 + 11 (computePhaseStuck
// + OnExhausted) to centralize the per-phase counter + abort-reason wiring.
//
// Plan §3.3 F4: StuckBucket is the single source of truth for "which
// tool's failure count == 2 means stuck" semantics. Pre-12.48b, the
// relationship was spread across 2 switch statements + 3 constants.
type StuckBucket int

const (
	// StuckNone — no stuck detection (Open, PostFinal). The phase lets
	// LLM recover via tool selection or escalates to generic
	// GoalAbortReasonBexhausted.
	StuckNone StuckBucket = iota
	// StuckSet — setGoalFailCount >= 2 → GoalPhaseSetStuckAbortReason
	StuckSet
	// StuckCheckpoint — goalProgressFailCount >= 2 → GoalPhaseCheckpointStuckAbortReason
	StuckCheckpoint
	// StuckFinal — completeGoalFailCount >= 2 → GoalPhaseFinalStuckAbortReason
	StuckFinal
)

// AbortReason returns the canonical `_v1_` abort reason for this bucket.
// Returns "" for StuckNone (caller falls back to GoalAbortReasonBexhausted).
func (b StuckBucket) AbortReason() string {
	switch b {
	case StuckSet:
		return GoalPhaseSetStuckAbortReason
	case StuckCheckpoint:
		return GoalPhaseCheckpointStuckAbortReason
	case StuckFinal:
		return GoalPhaseFinalStuckAbortReason
	}
	return ""
}

// CounterField returns the int field name (string identifier — caller
// resolves to struct field) where the failure count for this bucket lives.
// Returns "" for StuckNone.
func (b StuckBucket) CounterField() string {
	switch b {
	case StuckSet:
		return "setGoalFailCount"
	case StuckCheckpoint:
		return "goalProgressFailCount"
	case StuckFinal:
		return "completeGoalFailCount"
	}
	return ""
}

// TextOnlyMode classifies how the text-only recovery trigger should behave
// at a given phase. Plan §3.3 site 5 lookup.
type TextOnlyMode int

const (
	// TextOnlyNone — text-only response is silent (no recovery). Used at
	// PostFinal (any text-only at post_final = valid turn end).
	TextOnlyNone TextOnlyMode = iota
	// TextOnlyRestricted — same-iter 2 soft + 1 hard prompt with escalation.
	// Used at Set/Checkpoint/Final. Counter caps: TextOnlySoftRetryCap / TextOnlyHardRetryCap.
	TextOnlyRestricted
	// TextOnlyOpenCarry — next-iter carry via ts.pendingRecoveryMessage.
	// Used at Open. Counter caps: TextOnlySoftRetryCapOpen / TextOnlyHardRetryCapOpen.
	// After caps exhausted, archive goal (D2).
	TextOnlyOpenCarry
	// TextOnlyOpenSilent — direct text reply is a valid turn end. Used at
	// Set's text-only path (Phase 12.46 owner decision — anh Maple 2026-08-03).
	// Note: open-phase text-only is TextOnlyOpenCarry, NOT TextOnlyOpenSilent.
	// TextOnlyOpenSilent is Set-only.
	TextOnlyOpenSilent
)

// AgentPhasePolicy is the per-phase policy row that drives sites 4-11 of
// the agent dispatch table. Single source of truth for "what does this
// phase do for each recovery trigger + each user-facing text".
//
// ABSOLUTE phases (Set/Checkpoint/Final) have hard-wired behavior — they
// don't read from the registry, they pin the lifecycle tools. The policy
// row captures that pinning in DATA instead of CODE.
//
// RELATIVE phase (Open) is the inverse — base + lifecycle tools, soft
// text-only prompt, no stuck detection (LLM can switch tools freely).
// PostFinal is a "1-shot terminal" — RecoveryNone for all triggers,
// no stuck detection, no tool affordance.
type AgentPhasePolicy struct {
	Phase GoalPhase

	// EmptyAction — which RecoveryAction to use for empty-response trigger.
	// PostFinal = RecoveryNone (silent). All others = RecoveryRetrySameIteration.
	EmptyAction RecoveryAction

	// TextOnlyMode — see TextOnlyMode enum above.
	TextOnlyMode TextOnlyMode

	// ToolExecHint — second-argument hint for buildToolExecErrorRetryMessage
	// when the failing tool is the phase-pinned lifecycle tool. Empty string
	// at Open (toolName-gate decides: set_goal/goal_progress → hint, else
	// no hint). Set/Checkpoint/Final have non-empty hints naming the
	// phase-pinned tool.
	ToolExecHint string

	// ContextSuffix — appended to recovery-retry messages so the LLM sees
	// phase context at re-read time. Past-tense, state-agnostic. Empty
	// string at PostFinal (no recovery happens there).
	ContextSuffix string

	// StuckBucket — see StuckBucket enum above. Open/PostFinal = StuckNone.
	StuckBucket StuckBucket

	// GateSkipText — template for gateSkipMessageForPhase. Formatted with
	// fmt.Sprintf(template, toolName). Always non-empty (guarded by schema).
	// PostFinal uses state-agnostic wording (D7/E5/F2, Phase 12.43 invariant).
	GateSkipText string

	// DiscoveryRuleText — phase-aware Rule 5 paragraph for formatToolDiscoveryRule.
	// Empty string at Open (default "must search" branch).
	DiscoveryRuleText string

	// MCPAvailText — phase-aware MCP availability phrasing for mcpServerPromptContributor.
	// "available as native tools" at Open; "locked at this phase" variants elsewhere.
	MCPAvailText string
}

// Hardcoded constants — single source of truth for gateway log keys (Phase 12.30).
const (
	// DefaultGateSkipSuffix — appended to every gate-skip message so the LLM
	// knows the live state is in the system prompt header (avoids history-
	// poison claim that says "the goal was at iter X" or similar).
	DefaultGateSkipSuffix = "[live state: see system prompt header]"
)

// agentPolicies is the canonical phase-keyed policy table for pkg/agent.
// Frozen post-Phase 12.47 (POST-FINAL added). Plan §3.3.
//
// 5 rows: Set/Open/Checkpoint/Final/PostFinal.
// Each row DIRECTLY mirrors the steps in evaluateRecovery + the per-site
// helpers. Adding a new phase = add a row here + 1 typed alias in
// tool_allowlist_phase.go + 1 typed alias in pkg/phases.
var agentPolicies = map[GoalPhase]*AgentPhasePolicy{
	GoalPhaseSet: {
		Phase: GoalPhaseSet,
		// EmptyAction: RecoveryRetrySameIteration (Phase 12.37 GAP #2 — same-iter ×3 at all phases).
		EmptyAction: RecoveryRetrySameIteration,
		// TextOnlyMode: TextOnlyOpenSilent (Phase 12.46 — direct text reply at SET is valid turn end).
		TextOnlyMode: TextOnlyOpenSilent,
		// ToolExecHint: SET phase hint naming set_goal as the only available tool.
		ToolExecHint: ToolExecErrorSetPhaseHint,
		// ContextSuffix: past-tense "the goal had not yet been seeded" (F30 invariant).
		ContextSuffix: " At the time of this retry, the turn was in the SET phase and the goal had not yet been seeded.",
		// StuckBucket: StuckSet → setGoalFailCount.
		StuckBucket: StuckSet,
		// GateSkipText: toolName is unavailable; set_goal is the only tool.
		GateSkipText: fmt.Sprintf("tool %%q is temporarily unavailable. set_goal is the only available tool — call it to define the goal. %s", DefaultGateSkipSuffix),
		// DiscoveryRuleText: SET rule 5 (locked, only call set_goal).
		DiscoveryRuleText: `5. **Tool Discovery** - At SET phase (iter 1), tool_search_tool_bm25 and tool_search_tool_regex are locked. Do not search; only call ` + `"set_goal"` + ` to seed your turn. Discovery will unlock at iter 2's OPEN phase.`,
		// MCPAvailText: hidden behind discovery, unlocks at next iter in OPEN.
		MCPAvailText: "hidden behind tool discovery; will unlock at next iter in OPEN phase",
	},
	GoalPhaseOpen: {
		Phase: GoalPhaseOpen,
		// EmptyAction: RecoveryRetrySameIteration (Phase 12.37 — Open catches all).
		EmptyAction: RecoveryRetrySameIteration,
		// TextOnlyMode: TextOnlyOpenCarry (next-iter carry via ts.pendingRecoveryMessage; iter-bump escalates).
		TextOnlyMode: TextOnlyOpenCarry,
		// ToolExecHint: ToolExecErrorOpenPhaseHint — Open uses toolName gate
		// (Phase 12.32 ABSOLUTE/RELATIVE). buildToolExecErrorRetryMessage
		// only appends this hint when toolName ∈ {set_goal, goal_progress}.
		// Other tools at OPEN (read_file/exec/etc.) work fine — the error
		// is unrelated to the lifecycle gate.
		ToolExecHint: ToolExecErrorOpenPhaseHint,
		// ContextSuffix: past-tense "in the OPEN phase".
		ContextSuffix: " At the time of this retry, the turn was in the OPEN phase.",
		// StuckBucket: StuckNone — Open has no stuck detection (LLM can switch tools).
		StuckBucket: StuckNone,
		// GateSkipText: toolName is unavailable; try different tool or complete_goal.
		GateSkipText: fmt.Sprintf("tool %%q is temporarily unavailable. Try a different tool, or call complete_goal to finalize the turn. %s", DefaultGateSkipSuffix),
		// DiscoveryRuleText: empty — falls through to default "must search" branch.
		DiscoveryRuleText: "",
		// MCPAvailText: deferred=true at OPEN — only deferred servers show
		// this. Phase 12.29 default fallback wording preserved here for
		// backward compat with the pre-Phase 12.48b switch.
		MCPAvailText: "hidden behind tool discovery until unlocked",
	},
	GoalPhaseCheckpoint: {
		Phase: GoalPhaseCheckpoint,
		// EmptyAction: RecoveryRetrySameIteration.
		EmptyAction: RecoveryRetrySameIteration,
		// TextOnlyMode: TextOnlyRestricted (2 soft + 1 hard same-iter).
		TextOnlyMode: TextOnlyRestricted,
		// ToolExecHint: CHECKPOINT phase hint naming goal_progress/complete_goal.
		ToolExecHint: ToolExecErrorCheckpointPhaseHint,
		// ContextSuffix: past-tense "in the CHECKPOINT phase".
		ContextSuffix: " At the time of this retry, the turn was in the CHECKPOINT phase.",
		// StuckBucket: StuckCheckpoint → goalProgressFailCount.
		StuckBucket: StuckCheckpoint,
		// GateSkipText: toolName is unavailable; goal_progress/complete_goal are the only tools.
		GateSkipText: fmt.Sprintf("tool %%q is temporarily unavailable. goal_progress and complete_goal are the only available tools — call goal_progress to continue, or complete_goal to finalize. %s", DefaultGateSkipSuffix),
		// DiscoveryRuleText: CHECKPOINT rule 5 (locked, only call goal_progress/complete_goal).
		DiscoveryRuleText: `5. **Tool Discovery** - At CHECKPOINT phase, tool_search_tool_bm25 and tool_search_tool_regex are locked. Do not search; only call ` + `"goal_progress"` + ` or ` + `"complete_goal"` + ` (the only 2 visible tools). Discovery will unlock at your next turn's OPEN phase.`,
		// MCPAvailText: locked at CHECKPOINT, will unlock at next turn's OPEN.
		MCPAvailText: "locked at this phase; will unlock at next turn's OPEN phase",
	},
	GoalPhaseFinal: {
		Phase: GoalPhaseFinal,
		// EmptyAction: RecoveryRetrySameIteration (Phase 12.37 — was skipped pre-12.15).
		EmptyAction: RecoveryRetrySameIteration,
		// TextOnlyMode: TextOnlyRestricted (subject to postCompleteGoalReportSent silent check).
		TextOnlyMode: TextOnlyRestricted,
		// ToolExecHint: FINAL phase hint naming complete_goal as the only tool.
		ToolExecHint: ToolExecErrorFinalPhaseHint,
		// ContextSuffix: past-tense "in the FINAL phase".
		ContextSuffix: " At the time of this retry, the turn was in the FINAL phase.",
		// StuckBucket: StuckFinal → completeGoalFailCount.
		StuckBucket: StuckFinal,
		// GateSkipText: toolName is unavailable; complete_goal is the only tool.
		GateSkipText: fmt.Sprintf("tool %%q is temporarily unavailable. complete_goal is the only available tool — call it to finalize the turn. %s", DefaultGateSkipSuffix),
		// DiscoveryRuleText: FINAL rule 5 (locked, only call complete_goal).
		DiscoveryRuleText: `5. **Tool Discovery** - At FINAL phase, tool_search_tool_bm25 and tool_search_tool_regex are locked. Do not search; only call ` + `"complete_goal"` + ` (the only visible tool). Discovery will not unlock this turn.`,
		// MCPAvailText: locked at FINAL, won't unlock this turn.
		MCPAvailText: "locked at this terminal phase; will not unlock this turn",
	},
	GoalPhasePostFinal: {
		Phase: GoalPhasePostFinal,
		// EmptyAction: RecoveryNone (Phase 12.47 F1 — POST-FINAL silent).
		EmptyAction: RecoveryNone,
		// TextOnlyMode: TextOnlyNone (no recovery — text-only at post_final = valid turn end).
		TextOnlyMode: TextOnlyNone,
		// ToolExecHint: empty — no tool exec error recovery at POST-FINAL.
		ToolExecHint: "",
		// ContextSuffix: empty — no recovery happens at POST-FINAL.
		ContextSuffix: "",
		// StuckBucket: StuckNone — no stuck detection at POST-FINAL.
		StuckBucket: StuckNone,
		// GateSkipText: state-agnostic (Phase 12.43 invariant — no phase/iter/cap claims).
		GateSkipText: fmt.Sprintf("tool %%q is temporarily unavailable. No tools are available — output your final report text directly. %s", DefaultGateSkipSuffix),
		// DiscoveryRuleText: POST-FINAL rule 5 (locked, output final report text directly).
		DiscoveryRuleText: `5. **Tool Discovery** - At POST-FINAL phase, tool discovery is locked. Do not search; output the final report text directly.`,
		// MCPAvailText: terminal phase, won't unlock.
		MCPAvailText: "locked at this terminal phase; will not unlock this turn",
	},
}

// PhasePolicyFor returns the canonical agent-side policy row for a phase,
// or nil if phase is unknown / empty. Empty phase is "no phase set" —
// callers use nil as a signal to fall back to legacy behavior OR
// fail-CLOSED (the 2 sensitive sites — see Plan §3.3 F6 + R6-F1).
//
// Maps GoalPhaseLock alias to GoalPhaseSet (Phase 11 backward compat).
// Lookup is case-insensitive on the lowercase string form (Phase 12.1
// regression guard).
func PhasePolicyFor(phase GoalPhase) *AgentPhasePolicy {
	if string(phase) == "" {
		return nil
	}
	// Normalize: GoalPhaseLock is the same value as GoalPhaseSet (alias
	// in tool_allowlist_phase.go:50); both map to the SET row.
	if phase == GoalPhaseLock {
		phase = GoalPhaseSet
	}
	// Trim + lowercase for case-insensitive lookup (Phase 12.1 fix).
	trimmed := trimASCII(string(phase))
	key := GoalPhase(stringToLowerCopy(trimmed))
	return agentPolicies[key]
}

// stringToLowerCopy is a tiny helper to avoid the strings import (used
// only here). Implements ASCII-only lowercase.
func stringToLowerCopy(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out)
}

// trimASCII is a tiny helper to avoid the strings import (used only here).
// Trims leading + trailing ASCII whitespace (space, tab, CR, LF).
func trimASCII(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// AllAgentPhasePolicies returns the ordered slice of policy rows (matches
// pkg/phases.AllTokens() order). Used by schema lock tests + cross-table
// equality assertions.
//
// Note: GoalPhaseLock has the same value as GoalPhaseSet (alias, see
// tool_allowlist_phase.go:50) — pkg/phases exposes no Lock token, so this
// helper iterates the 5 canonical tokens and does NOT skip anything.
func AllAgentPhasePolicies() []*AgentPhasePolicy {
	tokens := phases.AllTokens()
	out := make([]*AgentPhasePolicy, 0, len(tokens))
	for _, tok := range tokens {
		goalPhase := GoalPhase(tok)
		if p := PhasePolicyFor(goalPhase); p != nil {
			out = append(out, p)
		}
	}
	return out
}

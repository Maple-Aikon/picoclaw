// Package agent: Phase 12.48b single-policy-module (agent side).
//
// File: pkg/agent/phase_policy_table.go (Phase 12.49 — split out for strict mode)
//
// Per Plan §3.3 — agent-side policy table that drives sites 4-11 (recovery,
// stuck, gate-skip, discovery rule, MCP availability). The 6 state-render
// sites (formatIterCompass + 5 hint contributors) are EXCLUDED per Q1=A
// decision — they read live iter/cap/goalFinalized state and stay as code
// (deferred to Phase 12.49+ per T15 gate).
//
// The pkg/tools side (ToolVisibilityPolicy) already exists at
// pkg/tools/phase_policy_table.go and is the source of truth for sites 1-3
// (Phase 12.48a shipped). This file is the AGENT side — sites 4-11. Both
// tables share the same PhaseToken keys (pkg/phases.AllTokens()) so they
// cannot drift.
//
// Schema lock invariants (TestPhasePolicy_AgentSide_SchemaLock):
//   1. Keys of agentPolicies == phases.AllTokens() (5 rows)
//   2. Each row's Phase field matches the map key
//   3. GateSkipText / DiscoveryRuleText / MCPAvailText are non-empty
//   4. ToolExecHint for OPEN uses toolName gate
//   5. EmptyAction == RecoveryNone ONLY at PostFinal (R5 lock)
//
// Phase 12.49: this file has NO build tag — the data table + helpers are
// canonical across modes. The `PhasePolicyFor` lookup function is in
//   - phase_policy.go (default build, fail-OPEN)
//   - phase_policy_strict.go (build tag `strict_phases`, panic)
package agent

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/phases"
)

// StuckBucket classifies a phase by which lifecycle tool's failure count
// should trigger a phase-stuck abort.
type StuckBucket int

const (
	StuckNone       StuckBucket = iota
	StuckSet                    // setGoalFailCount
	StuckCheckpoint             // goalProgressFailCount
	StuckFinal                  // completeGoalFailCount
)

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
// at a given phase.
type TextOnlyMode int

const (
	TextOnlyNone         TextOnlyMode = iota
	TextOnlyRestricted                // 2 soft + 1 hard same-iter
	TextOnlyOpenCarry                 // next-iter carry via ts.pendingRecoveryMessage
	TextOnlyOpenSilent                // direct text reply = valid turn end (Set)
)

// AgentPhasePolicy is the per-phase policy row that drives sites 4-11.
type AgentPhasePolicy struct {
	Phase            GoalPhase
	EmptyAction      RecoveryAction
	TextOnlyMode     TextOnlyMode
	ToolExecHint     string
	ContextSuffix    string
	StuckBucket      StuckBucket
	GateSkipText     string
	DiscoveryRuleText string
	MCPAvailText     string
}

// Hardcoded constants — single source of truth for gateway log keys.
const (
	DefaultGateSkipSuffix = "[live state: see system prompt header]"
)

// agentPolicies is the canonical phase-keyed policy table for pkg/agent.
var agentPolicies = map[GoalPhase]*AgentPhasePolicy{
	GoalPhaseSet: {
		Phase:             GoalPhaseSet,
		EmptyAction:       RecoveryRetrySameIteration,
		TextOnlyMode:      TextOnlyOpenSilent,
		ToolExecHint:      ToolExecErrorSetPhaseHint,
		ContextSuffix:     " At the time of this retry, the turn was in the SET phase and the goal had not yet been seeded.",
		StuckBucket:       StuckSet,
		GateSkipText:      fmt.Sprintf("tool %%q is temporarily unavailable. set_goal is the only available tool — call it to define the goal. %s", DefaultGateSkipSuffix),
		DiscoveryRuleText: `5. **Tool Discovery** - At SET phase (iter 1), tool_search_tool_bm25 and tool_search_tool_regex are locked. Do not search; only call ` + `"set_goal"` + ` to seed your turn. Discovery will unlock at iter 2's OPEN phase.`,
		MCPAvailText:      "hidden behind tool discovery; will unlock at next iter in OPEN phase",
	},
	GoalPhaseOpen: {
		Phase:             GoalPhaseOpen,
		EmptyAction:       RecoveryRetrySameIteration,
		TextOnlyMode:      TextOnlyOpenCarry,
		ToolExecHint:      ToolExecErrorOpenPhaseHint,
		ContextSuffix:     " At the time of this retry, the turn was in the OPEN phase.",
		StuckBucket:       StuckNone,
		GateSkipText:      fmt.Sprintf("tool %%q is temporarily unavailable. Try a different tool, or call complete_goal to finalize the turn. %s", DefaultGateSkipSuffix),
		DiscoveryRuleText: "",
		MCPAvailText:      "hidden behind tool discovery until unlocked",
	},
	GoalPhaseCheckpoint: {
		Phase:             GoalPhaseCheckpoint,
		EmptyAction:       RecoveryRetrySameIteration,
		TextOnlyMode:      TextOnlyRestricted,
		ToolExecHint:      ToolExecErrorCheckpointPhaseHint,
		ContextSuffix:     " At the time of this retry, the turn was in the CHECKPOINT phase.",
		StuckBucket:       StuckCheckpoint,
		GateSkipText:      fmt.Sprintf("tool %%q is temporarily unavailable. goal_progress and complete_goal are the only available tools — call goal_progress to continue, or complete_goal to finalize. %s", DefaultGateSkipSuffix),
		DiscoveryRuleText: `5. **Tool Discovery** - At CHECKPOINT phase, tool_search_tool_bm25 and tool_search_tool_regex are locked. Do not search; only call ` + `"goal_progress"` + ` or ` + `"complete_goal"` + ` (the only 2 visible tools). Discovery will unlock at your next turn's OPEN phase.`,
		MCPAvailText:      "locked at this phase; will unlock at next turn's OPEN phase",
	},
	GoalPhaseFinal: {
		Phase:             GoalPhaseFinal,
		EmptyAction:       RecoveryRetrySameIteration,
		TextOnlyMode:      TextOnlyRestricted,
		ToolExecHint:      ToolExecErrorFinalPhaseHint,
		ContextSuffix:     " At the time of this retry, the turn was in the FINAL phase.",
		StuckBucket:       StuckFinal,
		GateSkipText:      fmt.Sprintf("tool %%q is temporarily unavailable. complete_goal is the only available tool — call it to finalize the turn. %s", DefaultGateSkipSuffix),
		DiscoveryRuleText: `5. **Tool Discovery** - At FINAL phase, tool_search_tool_bm25 and tool_search_tool_regex are locked. Do not search; only call ` + `"complete_goal"` + ` (the only visible tool). Discovery will not unlock this turn.`,
		MCPAvailText:      "locked at this terminal phase; will not unlock this turn",
	},
	GoalPhasePostFinal: {
		Phase:             GoalPhasePostFinal,
		EmptyAction:       RecoveryNone,
		TextOnlyMode:      TextOnlyNone,
		ToolExecHint:      "",
		ContextSuffix:     "",
		StuckBucket:       StuckNone,
		GateSkipText:      fmt.Sprintf("tool %%q is temporarily unavailable. No tools are available — output your final report text directly. %s", DefaultGateSkipSuffix),
		DiscoveryRuleText: `5. **Tool Discovery** - At POST-FINAL phase, tool discovery is locked. Do not search; output the final report text directly.`,
		MCPAvailText:      "locked at this terminal phase; will not unlock this turn",
	},
}

// AllAgentPhasePolicies returns the ordered slice of policy rows.
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

// stringToLowerCopy is a tiny helper to avoid the strings import.
func stringToLowerCopy(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out)
}

// trimASCII trims leading + trailing ASCII whitespace.
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

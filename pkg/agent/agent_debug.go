package agent

// Phase 12.30 — per-iteration agent-loop debug logger.
//
// When PICOCLAW_AGENT_DEBUG=1 is set, this helper emits one structured
// log line per meaningful agent-loop event: phase transitions, LLM
// calls (with tool names + arg summaries), tool execution results
// (success/error + duration), and BoundedRetry attempt indices. The
// goal is to make multi-iter live-verify reproducible: a single
// `grep AGENT_DEBUG=~/.picoclaw/logs/gateway.log` over the saved log
// file shows the entire turn arc in chronological order.
//
// Design choices:
//   - Use the existing logger.DebugCF so log rotation, file routing,
//     and field serialization stay consistent with the rest of the
//     agent (no new file handle to manage).
//   - Env-var gated, not config-gated. The cost is one env-var read
//     at process start and one `if enabled { ... }` per call site —
//     zero overhead in production.
//   - Field values are short: tool names truncated to 64 chars, args
//     summarized to first 200 chars, no full LLM payload. A full
//     agent turn emits ~50-200 log lines; full payloads would
//     bloat the gateway log to MB per turn.
//
// Field keys (stable, grep-friendly):
//
//	event           — phase_start | phase_classify | llm_call |
//	                  llm_response | tool_exec | tool_exec_end |
//	                  retry_attempt | recovery | turn_end
//	turn_id         — e.g. main-turn-3
//	session_key     — opaque sk_v1_<hex>
//	iter            — 1-indexed iteration counter
//	phase           — GOAL-SET | GOAL-OPEN | GOAL-CHECKPOINT |
//	                  GOAL-FINAL (post-Phase 11 vocabulary)
//	attempt         — BoundedRetry attempt index (0=first, 1=retry 1, ...)
//	tool            — tool name (short string)
//	tool_calls      — number of tool calls in LLM response (int)
//	tools_visible   — number of tool defs sent to LLM this iter (int)
//	args_summary    — short JSON-encoded args (truncated)
//	duration_ms     — tool execution wall-clock (int)
//	is_error        — true if tool returned IsError=true
//	recovery_reason — empty unless event=recovery
//	goal_finalized  — true if goalFinalized flag is set
//	extra           — free-form map for caller-specific fields

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/sipeed/picoclaw/pkg/logger"
)

const agentDebugComponent = "agent_debug"

// agentDebugEnabled is the runtime cache of the env-var lookup. Read
// once via sync.Once-style atomic flag; subsequent reads are hot-path
// cheap. Default false (production).
var agentDebugEnabled atomic.Bool

func init() {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PICOCLAW_AGENT_DEBUG")))
	switch v {
	case "1", "true", "yes", "on":
		agentDebugEnabled.Store(true)
	default:
		agentDebugEnabled.Store(false)
	}
}

// IsAgentDebugEnabled returns whether the agent debug logger is on.
// Test fixtures can override via SetAgentDebugEnabled.
func IsAgentDebugEnabled() bool {
	return agentDebugEnabled.Load()
}

// SetAgentDebugEnabled toggles the debug logger at runtime. Test-only
// override; production code must rely on the env-var.
func SetAgentDebugEnabled(enabled bool) {
	agentDebugEnabled.Store(enabled)
}

// agentDebugf is the internal emitter. All public helpers funnel here.
// Format is key=value pairs joined by spaces, with the message at the
// end so log readers see the high-signal context first.
//
// Skip the call entirely when disabled — caller-side hot-path check.
func agentDebugf(event string, fields map[string]any) {
	if !agentDebugEnabled.Load() {
		return
	}
	merged := make(map[string]any, len(fields)+2)
	merged["event"] = event
	for k, v := range fields {
		merged[k] = v
	}
	logger.DebugCF(agentDebugComponent, event, merged)
}

// --- helpers ---

// summarizeArgs returns a short JSON-encoded preview of the args map.
// Values are coerced to string via strconv when not native types, so
// the log line stays parseable with grep / awk.
//
// The 200-char cap prevents large goal_progress payloads (with
// remaining_steps arrays) from bloating the log. Full args are
// available in prompt_history.log for the same session.
func summarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(args))
	for k, v := range args {
		parts = append(parts, k+"="+strconv.Quote(toString(v)))
	}
	s := "{" + strings.Join(parts, ",") + "}"
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// toString converts an arbitrary value to a short string. Maps and
// slices get a JSON-ish preview; everything else gets fmt %v.
func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		return "[" + strconv.Itoa(len(t)) + "]"
	case []string:
		return "[" + strconv.Itoa(len(t)) + "]"
	case map[string]any:
		return "[" + strconv.Itoa(len(t)) + "]"
	default:
		// Last-resort: stringify via %v.
		s := strings.Builder{}
		fmt.Fprintf(&s, "%v", v)
		if len(s.String()) > 50 {
			return s.String()[:50] + "..."
		}
		return s.String()
	}
}

// partsOrLen is no longer needed; remove once toString is settled.
// Kept as a stub for any leftover callers; safe to delete in a future
// commit.

// AgentDebugPhaseStart logs a per-iter start marker. Called at the
// top of the turn loop body, before LLM call.
//
// toolsTotal is the full registered registry count (unfiltered). The
// count the LLM actually sees after SetAllowlist + ToProviderDefs
// projection is logged separately on the llm_call event as a different
// field — keeping the two distinct so live-verify can detect
// regressions like the Phase 12.3.1 SetAllowlist(nil) wire bug.
func AgentDebugPhaseStart(turnID, sessionKey string, iter int, phase GoalPhase, toolsTotal int, goalFinalized bool) {
	if !agentDebugEnabled.Load() {
		return
	}
	agentDebugf("phase_start", map[string]any{
		"turn_id":        turnID,
		"session_key":    sessionKey,
		"iter":           iter,
		"phase":          string(phase),
		"tools_total":    toolsTotal,
		"goal_finalized": goalFinalized,
	})
}

// AgentDebugLLMCall logs the LLM request event with tool names.
// toolsVisible is the count the LLM actually sees this iter (after
// phase-allowlist projection). Compare against tools_total on the
// matching phase_start event — divergence means the allowlist gate
// suppressed tools.
func AgentDebugLLMCall(turnID, sessionKey string, iter int, phase GoalPhase, toolsVisible int) {
	if !agentDebugEnabled.Load() {
		return
	}
	agentDebugf("llm_call", map[string]any{
		"turn_id":       turnID,
		"session_key":   sessionKey,
		"iter":          iter,
		"phase":         string(phase),
		"tools_visible": toolsVisible,
	})
}

// AgentDebugLLMResponse logs the LLM response event with tool call
// count and a per-tool summary. Call after parsing the response.
//
// toolSummaries is a slice of (name, argsSummary) pairs in the order
// the LLM emitted them.
func AgentDebugLLMResponse(turnID, sessionKey string, iter int, phase GoalPhase, toolSummaries []AgentDebugToolCall) {
	if !agentDebugEnabled.Load() {
		return
	}
	fields := map[string]any{
		"turn_id":     turnID,
		"session_key": sessionKey,
		"iter":        iter,
		"phase":       string(phase),
		"tool_calls":  len(toolSummaries),
	}
	if len(toolSummaries) > 0 {
		names := make([]string, len(toolSummaries))
		for i, tc := range toolSummaries {
			names[i] = tc.Name
		}
		fields["tools"] = strings.Join(names, ",")
		if len(toolSummaries) > 0 {
			tc := toolSummaries[0]
			fields["tool"] = tc.Name
			fields["args_summary"] = tc.ArgsSummary
		}
	}
	agentDebugf("llm_response", fields)
}

// AgentDebugToolCall is one parsed tool call from the LLM response.
// Stored in agent_tool_calls field for grep-friendly iteration logs.
type AgentDebugToolCall struct {
	Name        string
	ArgsSummary string
}

// AgentDebugToolExec logs a tool execution start. Called right before
// the dispatcher invokes the tool.
func AgentDebugToolExec(turnID, sessionKey string, iter int, phase GoalPhase, tool string, args map[string]any, attempt int) {
	if !agentDebugEnabled.Load() {
		return
	}
	agentDebugf("tool_exec", map[string]any{
		"turn_id":     turnID,
		"session_key": sessionKey,
		"iter":        iter,
		"phase":       string(phase),
		"tool":        tool,
		"attempt":     attempt,
		"args_summary": summarizeArgs(args),
	})
}

// AgentDebugToolExecEnd logs a tool execution result.
func AgentDebugToolExecEnd(turnID, sessionKey string, iter int, phase GoalPhase, tool string, isError bool, forLLMLen, durationMs int, attempt int) {
	if !agentDebugEnabled.Load() {
		return
	}
	agentDebugf("tool_exec_end", map[string]any{
		"turn_id":     turnID,
		"session_key": sessionKey,
		"iter":        iter,
		"phase":       string(phase),
		"tool":        tool,
		"attempt":     attempt,
		"is_error":    isError,
		"for_llm_len": forLLMLen,
		"duration_ms": durationMs,
	})
}

// AgentDebugRetryAttempt logs a BoundedRetry attempt. Pass attempt=0
// for the original call, attempt=1+ for retries. recoveryReason is
// empty for the original.
func AgentDebugRetryAttempt(turnID, sessionKey string, iter int, phase GoalPhase, attempt int, recoveryReason string) {
	if !agentDebugEnabled.Load() {
		return
	}
	agentDebugf("retry_attempt", map[string]any{
		"turn_id":         turnID,
		"session_key":     sessionKey,
		"iter":            iter,
		"phase":           string(phase),
		"attempt":         attempt,
		"recovery_reason": recoveryReason,
	})
}

// AgentDebugRecovery logs a recovery trigger fire. recoveryReason
// values match the keys in pkg/agent/recovery_goal.go (EmptyResponse,
// TextOnlySoft, TextOnlyHard, ToolExecError, ProviderTransient).
func AgentDebugRecovery(turnID, sessionKey string, iter int, phase GoalPhase, recoveryReason string, attempt int) {
	if !agentDebugEnabled.Load() {
		return
	}
	agentDebugf("recovery", map[string]any{
		"turn_id":         turnID,
		"session_key":     sessionKey,
		"iter":            iter,
		"phase":           string(phase),
		"recovery_reason": recoveryReason,
		"attempt":         attempt,
	})
}

// AgentDebugTurnEnd logs the per-turn end marker. summary maps to
// the final user-facing response status (success / tool_limit /
// aborted / recovered).
func AgentDebugTurnEnd(turnID, sessionKey string, totalIter int, summary string, goalFinalized bool) {
	if !agentDebugEnabled.Load() {
		return
	}
	agentDebugf("turn_end", map[string]any{
		"turn_id":        turnID,
		"session_key":    sessionKey,
		"iter":           totalIter,
		"summary":        summary,
		"goal_finalized": goalFinalized,
	})
}

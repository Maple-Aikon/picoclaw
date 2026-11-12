// Auto-load injection telemetry — pkg/agent side (recovery path).
//
// Plan: tool-knowledge-instrumentation-đếm-auto-load-injected-mục-3-tách-từ-plan-10-21-20261112
//
// # WHY A SEPARATE COUNTER
//
// pkg/tools cannot import pkg/agent (import cycle: pkg/agent imports
// pkg/tools for the knowledge store). So the recovery-path counter
// lives here and the escalate-path counter lives in pkg/tools. Two
// counters, one per package — the same shape as the production
// precedent (phase_policy_telemetry.go local counter +
// strictPhaseMissCounter in pkg/agent).
//
// THE THREE INJECTION POINTS (all counted here, none in buildToolExecErrorRetryMessage)
//
//	R1 — pkg/agent/pipeline_llm.go:109        next-iteration carry (OPEN)
//	R2 — pkg/agent/pipeline_llm.go:1340-1345  same-iteration retry
//	R3 — pkg/agent/retry_execute_tool_chain.go:449-455  restricted-phase retry
//
// # WHY NOT count in buildToolExecErrorRetryMessage
//
// That helper BUILDS the message. `checkToolExecErrorRecovery` has two
// callsites (turn_coord.go:475 and retry_execute_tool_chain.go:348)
// and `ts.toolExecRecoveryAttempts` is NOT reset between them, so the
// same failure can BUILD the message twice while INJECTING it once.
// Counting at the build site over-counts.
//
// # WHY NOT count in evaluateRecovery
//
// recovery_goal.go:333 documents evaluateRecovery as pure (no side
// effects, no logger writes). A counter write there would break that
// contract. (The code already mutates ts.toolExecRecoveryAttempts at
// :374-378, so the comment has drifted — but the plan's invariant is
// to leave it alone, and the three injection points are all OUTSIDE
// evaluateRecovery, so nothing needs to change there.)
//
// # WHY ALL THREE POINTS
//
// The same failure can travel more than one of these paths. Counting
// only one under-counts; counting at the build site over-counts.
// Three injection points = exactly one increment per real injection.
package agent

import (
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// agentAutoLoadCounter is the pkg/agent counter (recovery path).
var agentAutoLoadCounter = &tools.AutoLoadCounter{}

// AgentAutoLoadCounter returns the pkg/agent counter.
// Exported so cross-package tests can inspect it.
func AgentAutoLoadCounter() *tools.AutoLoadCounter { return agentAutoLoadCounter }

// AutoLoadStatsSnapshot returns the pkg/agent counter snapshot.
func AutoLoadStatsSnapshot() tools.AutoLoadStats { return agentAutoLoadCounter.Stats() }

// ResetAgentAutoLoadCounterForTest clears the pkg/agent counter.
// Test-only seam — the counters are telemetry, never persisted.
func ResetAgentAutoLoadCounterForTest() { agentAutoLoadCounter.ResetForTest() }

// countRecoveryAutoLoadInject counts ONE recovery-path injection when the
// message that is entering the LLM context actually carries a
// tool_knowledge lesson.
//
// We DETECT the lesson in the message rather than ASSUME it is there.
// The recovery message is built by buildToolExecErrorRetryMessage, which
// appends the lesson section ONLY when the tool has a stored lesson. A
// recovery message without a lesson (the common case) must NOT be
// counted — counting it would inflate the total with recoveries that
// carried no lesson at all, which is precisely the number the baseline
// log probe could not separate.
//
// The tool name is read from the message shape
// (`Tool "<name>" failed: ...`, ToolExecErrorRetryMessage). When the
// shape does not match, AutoLoadUnknownTool is used — the counter must
// never fail on an unexpected message shape.
func countRecoveryAutoLoadInject(ts *turnState, message string) int64 {
	if ts == nil || message == "" {
		return agentAutoLoadCounter.Total.Load()
	}
	body, section, ok := tools.ExtractInjectedKnowledgeSection(message)
	if !ok {
		return agentAutoLoadCounter.Total.Load()
	}
	tool := tools.AutoLoadToolNameFromMessage(message)
	return recordAutoLoadInject(ts, tool, body, len(section))
}

// recordAutoLoadInject records ONE recovery-path injection of a
// tool_knowledge lesson and emits the matching log line.
//
// tool         — the tool whose lesson was injected.
// knowledge    — the lesson body (no markers). Empty ⇒ nothing was
//
//	injected ⇒ nothing is counted.
//
// sectionBytes — len of the full section actually sent to the LLM.
//
// The log line is InfoCF (not DebugCF) so the measurement survives a
// drop to "warn" level; the observed rate is ~8 injections/day, so the
// cost is negligible.
//
// An empty channel/chatID (CLI, tests) is normalised to "<anon>" —
// the log must never print "<nil>".
func recordAutoLoadInject(
	ts *turnState,
	tool string,
	knowledge string,
	sectionBytes int,
) int64 {
	if ts == nil || knowledge == "" {
		return agentAutoLoadCounter.Total.Load()
	}
	bodyBytes := len(knowledge)
	total := agentAutoLoadCounter.RecordAutoLoadInject(
		ts.channel, ts.chatID, tool, tools.AutoLoadPathRecovery,
		bodyBytes, sectionBytes,
	)
	logger.InfoCF("agent", "tool_knowledge auto-load injected", map[string]any{
		"tool":          tool,
		"channel":       autoLoadLogValue(ts.channel),
		"chat_id":       autoLoadLogValue(ts.chatID),
		"path":          tools.AutoLoadPathRecovery,
		"body_bytes":    bodyBytes,
		"section_bytes": sectionBytes,
		"total":         total,
	})
	return total
}

// autoLoadLogValue normalises an empty channel/chatID for the log.
func autoLoadLogValue(v string) string {
	if v == "" {
		return tools.AutoLoadAnonValue
	}
	return v
}

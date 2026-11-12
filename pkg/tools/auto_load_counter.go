// Auto-load injection telemetry — pkg/tools side (escalate path).
//
// Plan: tool-knowledge-instrumentation-đếm-auto-load-injected-mục-3-tách-từ-plan-10-21-20261112
//
// # WHAT THIS COUNTS
//
// One increment per REAL injection of a tool_knowledge lesson into a
// message that reaches the LLM. Not a "load", not a "build".
//
// # WHY NOT LoadForEscalation
//
// At registry.go:757 and registry.go:889, `r.toolKnowledgeFor(name)` is
// evaluated as an ARGUMENT to `EscalateIfNeeded` — so LoadForEscalation
// runs on EVERY transient/invalid-input tool failure, even when the
// signature tracker is still BELOW its threshold (3) and nothing is
// injected at all. A counter placed there would count LOADS, not
// INJECTIONS.
//
// # WHY NOT buildToolExecErrorRetryMessage
//
// That helper BUILDS the retry message. `checkToolExecErrorRecovery` has
// two callsites (turn_coord.go:475 and retry_execute_tool_chain.go:348)
// and `ts.toolExecRecoveryAttempts` is NOT reset between them, so the
// same failure can build the message twice while injecting once. Worse:
// when evaluateRecovery returns RecoveryArchiveGoal the message is BUILT
// but NEVER injected (turn_coord.go:477 archives the goal and returns) —
// a build-site counter would count a non-event.
//
// THE ONE PLACE THAT COUNTS (escalate path)
//
// appendEscalatorHint is the only site where the escalate payload
// touches what is sent to the LLM (`result.ForLLM += hint`). Counting
// there counts exactly the injections.
//
// # KEY
//
// "<channel>|<chatID>|<tool>|<path>" — chatID alone is not unique (two
// channels can share an id), and channel alone is not unique either.
//
// BYTES
//
//	section_bytes — len(AppendKnowledgeSection(body)) — what actually
//	                pushes tokens into the context window.
//	body_bytes    — len(body) — the lesson payload without the markers.
//
// pkg/tools cannot import pkg/agent (import cycle: pkg/agent imports
// pkg/tools). Hence TWO counters, one per package, mirroring the
// production precedent in phase_policy_telemetry.go (local counter) +
// callback.go (cross-package bridge). The counter TYPE lives here so
// pkg/agent can reuse it without duplicating the logic.
package tools

import (
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// Auto-load injection paths. The path is part of the counter key because
// the SAME tool can fail along BOTH paths and the baseline log probe
// cannot tell them apart.
const (
	// AutoLoadPathEscalate = the signature-tracker escalation path.
	// The hint rides on result.ForLLM (role [TOOL]).
	AutoLoadPathEscalate = "escalate"

	// AutoLoadPathRecovery = the tool-exec recovery path.
	// The hint rides on the LLM call messages (role [USER]).
	AutoLoadPathRecovery = "recovery"
)

// AutoLoadPathUnset is the placeholder used when a caller cannot
// attribute the injection to a known path.
const AutoLoadPathUnset = ""

// AutoLoadAnonValue marks an EMPTY channel or chatID in the counter key
// and in log fields.
//
// WHY: an empty chatID (CLI, tests) must not collide with a real one,
// and the log must never print "<nil>" — that reads like a bug to
// whoever reads the log. "<anon>" is explicitly "we do not know".
const AutoLoadAnonValue = "<anon>"

// AutoLoadKeySeparator joins the key segments. A pipe is used because
// channel/chatID/tool/path cannot contain it (chatID is numeric or a
// session id; tool names are alphanumeric + . _ -).
const AutoLoadKeySeparator = "|"

// AutoLoadUnknownTool is the placeholder used when the tool name cannot
// be determined from the injected message.
const AutoLoadUnknownTool = "unknown"

// ExtractInjectedKnowledge returns the tool_knowledge lesson body carried
// by an already-assembled message, and whether such a lesson was present.
//
// The lesson section is delimited by KnowledgeSectionHeader /
// KnowledgeSectionFooter, appended by AppendKnowledgeSection. Detecting
// the section in the FINAL message (rather than at the place where the
// lesson is loaded) is what makes the counter measure INJECTIONS: the
// message either carries the section into the LLM context or it does not.
//
// Returns (body, true) only when the section exists AND its body is not
// blank — an empty lesson body would be a zero-byte injection.
func ExtractInjectedKnowledge(content string) (string, bool) {
	body, _, ok := ExtractInjectedKnowledgeSection(content)
	return body, ok
}

// ExtractInjectedKnowledgeSection returns the lesson body AND the exact
// section text (header + body + footer) carried by an assembled message.
//
// The section text is what actually lands in the LLM context, so its
// length is the real context cost — measured on the exact bytes rather
// than reconstructed from the trimmed body.
func ExtractInjectedKnowledgeSection(content string) (string, string, bool) {
	if content == "" {
		return "", "", false
	}
	head := strings.Index(content, KnowledgeSectionHeader)
	if head < 0 {
		return "", "", false
	}
	tail := strings.Index(content[head:], KnowledgeSectionFooter)
	if tail < 0 {
		return "", "", false
	}
	end := head + tail + len(KnowledgeSectionFooter)
	section := content[head:end]
	body := strings.TrimSpace(content[head+len(KnowledgeSectionHeader) : head+tail])
	if body == "" {
		return "", "", false
	}
	return body, section, true
}

// AutoLoadToolNameFromMessage extracts the tool name from a tool-exec
// recovery message.
//
// The recovery message starts with `Tool "<name>" failed: ...`
// (ToolExecErrorRetryMessage in pkg/agent/recovery_goal.go). When the
// shape does not match, AutoLoadUnknownTool is returned — the counter
// must never fail on an unexpected message shape.
func AutoLoadToolNameFromMessage(content string) string {
	const prefix = "Tool \""
	if !strings.HasPrefix(content, prefix) {
		return AutoLoadUnknownTool
	}
	rest := content[len(prefix):]
	end := strings.Index(rest, "\"")
	if end <= 0 {
		return AutoLoadUnknownTool
	}
	return rest[:end]
}

// AutoLoadCounter holds the injection counters for one package.
//
// Total / TotalBytes are plain atomics (read-mostly). ByKey is a
// sync.Map: the key count is small (tens), writes are rare (~8/day per
// the 2026-11-12 baseline) and reads must not take a lock.
type AutoLoadCounter struct {
	Total      atomic.Int64
	TotalBytes atomic.Int64
	TotalBody  atomic.Int64
	ByKey      sync.Map // string → *AutoLoadKeyStat
}

// AutoLoadKeyStat is the per-key tally.
//
// The counter keeps atomic fields (written on the hot path); the
// snapshot struct below is the plain-value copy handed to callers.
type AutoLoadKeyStat struct {
	Count     atomic.Int64
	Bytes     atomic.Int64
	BodyBytes atomic.Int64
}

// AutoLoadKeyStatSnapshot is the plain-value view of one key.
type AutoLoadKeyStatSnapshot struct {
	Count     int64
	Bytes     int64
	BodyBytes int64
}

// AutoLoadStats is a point-in-time snapshot of the counters.
// Returned by value so the caller can read it without holding a lock.
type AutoLoadStats struct {
	Total      int64
	TotalBytes int64
	TotalBody  int64
	ByKey      map[string]AutoLoadKeyStatSnapshot
}

// toolsAutoLoadCounter is the pkg/tools counter (escalate path).
var toolsAutoLoadCounter = &AutoLoadCounter{}

// ToolsAutoLoadCounter returns the pkg/tools counter.
// Exported so cross-package tests can inspect it.
func ToolsAutoLoadCounter() *AutoLoadCounter { return toolsAutoLoadCounter }

// AutoLoadKey builds the counter key for one injection.
//
// channel and chatID may be empty (CLI / tests) — they are normalised to
// AutoLoadAnonValue so an anonymous session never collides with a real
// one, and so the log never shows "<nil>".
func AutoLoadKey(channel, chatID, tool, path string) string {
	if strings.TrimSpace(channel) == "" {
		channel = AutoLoadAnonValue
	}
	if strings.TrimSpace(chatID) == "" {
		chatID = AutoLoadAnonValue
	}
	if path == "" {
		path = AutoLoadPathUnset
	}
	return strings.Join([]string{channel, chatID, tool, path}, AutoLoadKeySeparator)
}

// RecordAutoLoadInject records ONE injection and returns the new total.
//
// bodyBytes    — len of the lesson body (no markers).
// sectionBytes — len of the full "=== Saved Knowledge === ... === End
//
//	Knowledge ===" section — the real context cost.
//
// Call this ONLY at a site where the payload actually reaches the LLM.
func (c *AutoLoadCounter) RecordAutoLoadInject(
	channel, chatID, tool, path string,
	bodyBytes, sectionBytes int,
) int64 {
	if c == nil {
		return 0
	}
	key := AutoLoadKey(channel, chatID, tool, path)

	c.Total.Add(1)
	c.TotalBytes.Add(int64(sectionBytes))
	c.TotalBody.Add(int64(bodyBytes))

	statAny, _ := c.ByKey.LoadOrStore(key, &AutoLoadKeyStat{})
	stat, ok := statAny.(*AutoLoadKeyStat)
	if !ok {
		// LoadOrStore only ever stores *AutoLoadKeyStat, so this is
		// unreachable in practice. Fall back to a fresh stat rather
		// than panic — a telemetry counter must never break the tool
		// path.
		stat = &AutoLoadKeyStat{}
		c.ByKey.Store(key, stat)
	}
	stat.Count.Add(1)
	stat.Bytes.Add(int64(sectionBytes))
	stat.BodyBytes.Add(int64(bodyBytes))

	return c.Total.Load()
}

// Stats returns a snapshot of the counters.
func (c *AutoLoadCounter) Stats() AutoLoadStats {
	out := AutoLoadStats{ByKey: map[string]AutoLoadKeyStatSnapshot{}}
	if c == nil {
		return out
	}
	out.Total = c.Total.Load()
	out.TotalBytes = c.TotalBytes.Load()
	out.TotalBody = c.TotalBody.Load()
	c.ByKey.Range(func(k, v any) bool {
		stat, ok := v.(*AutoLoadKeyStat)
		if !ok {
			return true
		}
		out.ByKey[k.(string)] = AutoLoadKeyStatSnapshot{
			Count:     stat.Count.Load(),
			Bytes:     stat.Bytes.Load(),
			BodyBytes: stat.BodyBytes.Load(),
		}
		return true
	})
	return out
}

// ResetForTest clears every counter. Test-only seam — the counters are
// telemetry, never persisted.
func (c *AutoLoadCounter) ResetForTest() {
	if c == nil {
		return
	}
	c.Total.Store(0)
	c.TotalBytes.Store(0)
	c.TotalBody.Store(0)
	c.ByKey.Range(func(k, _ any) bool {
		c.ByKey.Delete(k)
		return true
	})
}

// ResetToolsAutoLoadCounterForTest clears the pkg/tools counter.
func ResetToolsAutoLoadCounterForTest() { toolsAutoLoadCounter.ResetForTest() }

// ToolsAutoLoadStatsSnapshot returns the pkg/tools counter snapshot.
func ToolsAutoLoadStatsSnapshot() AutoLoadStats { return toolsAutoLoadCounter.Stats() }

// RecordEscalateAutoLoadInject records ONE escalate-path injection of a
// tool_knowledge lesson and emits the matching log line.
//
// tool — the tool whose lesson was injected.
// hint — the escalation payload appended to result.ForLLM. It may or
//
//	may not carry a lesson section: the tracker only adds one when the
//	tool has a stored lesson. The lesson is DETECTED in the payload (not
//	assumed), so a hint without a lesson is NOT counted — counting it
//	would inflate the total with escalations that carried no lesson.
//
// The log line is InfoCF (not DebugCF) so the measurement survives a
// drop to "warn" level; the observed rate is ~8 injections/day, so the
// cost is negligible.
//
// An empty channel/chatID (CLI, tests) is normalised to "<anon>" —
// the log must never print "<nil>".
func RecordEscalateAutoLoadInject(
	channel, chatID, tool, hint string,
) int64 {
	body, section, ok := ExtractInjectedKnowledgeSection(hint)
	if !ok {
		return toolsAutoLoadCounter.Total.Load()
	}
	total := toolsAutoLoadCounter.RecordAutoLoadInject(
		channel, chatID, tool, AutoLoadPathEscalate,
		len(body), len(section),
	)
	logger.InfoCF("tool", "tool_knowledge auto-load injected", map[string]any{
		"tool":          tool,
		"channel":       autoLoadLogValue(channel),
		"chat_id":       autoLoadLogValue(chatID),
		"path":          AutoLoadPathEscalate,
		"body_bytes":    len(body),
		"section_bytes": len(section),
		"total":         total,
	})
	return total
}

// autoLoadLogValue normalises an empty channel/chatID for the log.
func autoLoadLogValue(v string) string {
	if strings.TrimSpace(v) == "" {
		return AutoLoadAnonValue
	}
	return v
}

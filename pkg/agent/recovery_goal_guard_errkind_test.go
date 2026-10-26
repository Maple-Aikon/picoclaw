// Phase 12.63 (2026-10-26) — END-TO-END proof that a safety-guard
// rejection actually surfaces the tool_knowledge lesson to the LLM.
//
// WHY THIS FILE EXISTS:
//
// The Phase 12.62 E2E test (recovery_goal_toolname_test.go) used the
// content "Tool execution failed: connection refused" — a string with
// the LEGACY prefix. That content reaches the recovery layer through
// the legacy prefix heuristic, so the test PASSED even when the tool
// itself produced a message WITHOUT that prefix.
//
// Real safety-guard messages look like:
//
//	Command blocked by safety guard (dangerous pattern detected)
//
// — no legacy prefix. Before Phase 12.63 the exec safety guard left
// ErrKind empty, so the recovery layer's legacy-prefix fallback was
// the only classifier left, it did not match, and the recovery never
// fired. The lesson for `exec` (which is FULL of guard workarounds)
// was never shown to the LLM.
//
// These tests therefore use the PRODUCTION content string verbatim and
// assert on BOTH:
//   1. the positive case — the lesson reaches the retry message, and
//   2. the negative control — with an empty ErrKind the recovery does
//      NOT fire (proving the shell.go fix is load-bearing, not
//      cosmetic).

package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
	"github.com/sipeed/picoclaw/pkg/tools/shared"
)

// safetyGuardContent is the EXACT production message emitted by
// pkg/tools/shell.go when the deny-pattern guard trips. Copied
// verbatim so the test cannot drift from production wording.
const safetyGuardContent = "Command blocked by safety guard (dangerous pattern detected)"

// safetyGuardLesson is a marker string planted in the `exec` knowledge
// entry. The test asserts it appears in the retry message — that is
// the proof the lesson was injected, not merely that a message exists.
const safetyGuardLesson = "SAFETY_GUARD_LESSON_MARKER_7f3a1c"

// newSafetyGuardRegistry builds a registry whose `exec` knowledge entry
// carries the marker lesson.
func newSafetyGuardRegistry(t *testing.T) *tools.ToolRegistry {
	t.Helper()
	ws := t.TempDir()
	store, err := tools.NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}
	// NOTE: the newline is built at runtime on purpose. Writing a
	// backslash-n inside a string literal that travels through a tool
	// call gets JSON-decoded into a REAL newline, which terminates the
	// literal and breaks compilation. Build it from a rune instead.
	nl := string(rune(10))
	lesson := "## What goes wrong" + nl +
		"The exec safety guard blocks dangerous patterns; " +
		safetyGuardLesson + " tells the LLM the workaround." + nl
	if _, _, err := store.Replace("exec", lesson); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	registry := tools.NewToolRegistry()
	registry.SetToolKnowledgeStore(store)
	return registry
}

// TestCheckToolExecErrorRecovery_SafetyGuard_LoadsExecLesson is the
// POSITIVE case: a safety-guard rejection (production content, typed
// ErrInvalidInput) must produce a retry message that contains the
// `exec` lesson.
func TestCheckToolExecErrorRecovery_SafetyGuard_LoadsExecLesson(t *testing.T) {
	registry := newSafetyGuardRegistry(t)

	ts := newPhase5TurnState(t)
	ts.agent.Tools = registry

	res := toolshared.ErrorResult(safetyGuardContent).
		WithErrorKind(toolshared.ErrInvalidInput)
	ts.lastToolResult = res

	exec := &turnExecution{
		messages: []providers.Message{
			{
				Role: "assistant",
				ToolCalls: []providers.ToolCall{
					{ID: "call_guard_1_1", Name: "exec"},
				},
			},
			{Role: "tool", ToolCallID: "call_guard_1_1", Content: res.ForLLM},
		},
	}

	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool == "" && msg == "" {
		t.Fatalf("safety-guard rejection must fire recovery; "+
			"the retry message must reach the LLM (tool=%q msg=%q)", tool, msg)
	}
	if !strings.Contains(msg, safetyGuardLesson) {
		t.Fatalf("the exec lesson was NOT surfaced in the retry message: %q", msg)
	}
	if !strings.Contains(msg, `"exec"`) {
		t.Fatalf("retry message must name the failing tool: %q", msg)
	}
	if !strings.Contains(msg, safetyGuardContent) {
		t.Fatalf("retry message must carry the original guard error: %q", msg)
	}
}

// TestCheckToolExecErrorRecovery_SafetyGuard_EmptyErrKind_DoesNotFire
// is the NEGATIVE CONTROL. Same production content, but the ErrKind is
// empty — i.e. the state the code was in before Phase 12.63.
//
// This test PROVES the shell.go fix is load-bearing: without a typed
// ErrKind the legacy-prefix heuristic is the only classifier, the
// guard message does not carry that prefix, and the recovery never
// fires. If this test ever starts failing (recovery fires with an
// empty ErrKind), the legacy heuristic changed and the E2E coverage
// needs re-checking.
func TestCheckToolExecErrorRecovery_SafetyGuard_EmptyErrKind_DoesNotFire(t *testing.T) {
	registry := newSafetyGuardRegistry(t)

	ts := newPhase5TurnState(t)
	ts.agent.Tools = registry

	// NO WithErrorKind — exactly the pre-fix state.
	res := toolshared.ErrorResult(safetyGuardContent)
	if res.ErrKind != "" {
		t.Fatalf("precondition failed: ErrKind must be empty, got %q", res.ErrKind)
	}
	ts.lastToolResult = res

	exec := &turnExecution{
		messages: []providers.Message{
			{
				Role: "assistant",
				ToolCalls: []providers.ToolCall{
					{ID: "call_guard_2_1", Name: "exec"},
				},
			},
			{Role: "tool", ToolCallID: "call_guard_2_1", Content: res.ForLLM},
		},
	}

	tool, msg := checkToolExecErrorRecovery(ts, exec)
	if tool != "" || msg != "" {
		t.Fatalf("with an empty ErrKind the recovery MUST NOT fire "+
			"(this is the pre-Phase-12.63 state); got tool=%q msg=%q", tool, msg)
	}
	if strings.Contains(msg, safetyGuardLesson) {
		t.Fatalf("lesson must NOT be surfaced when the recovery does not fire: %q", msg)
	}
}

// TestCheckToolExecErrorRecovery_SafetyGuard_NonTransientPrompt pins
// the PROMPT SHAPE: a guard rejection is not transient, so the retry
// message must NOT tell the LLM to wait-and-retry-the-same-call. It
// must instead invite a different approach — which is exactly the
// guidance the `exec` lesson carries.
func TestCheckToolExecErrorRecovery_SafetyGuard_NonTransientPrompt(t *testing.T) {
	registry := newSafetyGuardRegistry(t)

	ts := newPhase5TurnState(t)
	ts.agent.Tools = registry

	res := toolshared.ErrorResult(safetyGuardContent).
		WithErrorKind(toolshared.ErrInvalidInput)
	ts.lastToolResult = res

	exec := &turnExecution{
		messages: []providers.Message{
			{
				Role: "assistant",
				ToolCalls: []providers.ToolCall{
					{ID: "call_guard_3_1", Name: "exec"},
				},
			},
			{Role: "tool", ToolCallID: "call_guard_3_1", Content: res.ForLLM},
		},
	}

	_, msg := checkToolExecErrorRecovery(ts, exec)
	if msg == "" {
		t.Fatal("expected a retry message")
	}
	if strings.Contains(strings.ToLower(msg), "wait briefly and retry the same call") {
		t.Fatalf("a guard rejection must NOT be presented as transient "+
			"(retrying the same command cannot help): %q", msg)
	}
}

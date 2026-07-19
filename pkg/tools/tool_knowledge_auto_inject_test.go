package tools

// =============================================================================
// Tool-Knowledge soft-prompt auto-inject tests (Phase 3)
//
// Plan: tool-knowledge-experiential-memory-for-tool-failures-3-phases-20260718
//
// These tests pin the contract that:
//   1. On FIRST successful execution per (session, tool) in a turn, the
//      registry appends SoftPromptFirstSuccess to result.ForLLM — exactly once.
//   2. On REPEATED transient failures (count ∈ [2, threshold-1]), the registry
//      appends SoftPromptRepeatedFailure to result.ForLLM alongside the
//      canonical transientHint. The soft prompt bridges the gap between
//      "first failure (no nudge)" and "threshold reached (escalation)"
//      by encouraging the LLM to save a workaround lesson.
//   3. When the circuit breaker trips (count >= threshold), the registry emits
//      escalationHint (from circuit_breaker.go) — NOT SoftPromptRepeatedFailure.
//      escalationHint takes priority over the signature-tracker's
//      "STOP and reconsider" message because the breaker fires first.
//   4. Validation errors never trigger any soft prompt (validationHint only).
//   5. ResetSignatureFailures (turn boundary) clears seenFirstSuccess.
//
// Why these tests matter: the soft-prompt feature is invisible from the CLI
// perspective — the nudge appears in the LLM-facing message, not in the
// user-facing error. Without these tests, a regression that breaks the wiring
// would only surface when the agent fails to save a lesson at the right time.
//
// Pattern: each tool call must return a FRESH *ToolResult, NOT a shared one.
// mockRegistryTool.result is set at registration time and Execute returns the
// same pointer on every call — registry mutations accumulate. We define
// mockResultFactoryTool here that constructs a fresh result per call.
// =============================================================================

import (
	"context"
	"strings"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// mockResultFactoryTool returns a FRESH *ToolResult on every Execute call.
// This is required for soft-prompt tests because registry.go mutates the
// returned *ToolResult in place (e.g. result.ForLLM += SoftPromptFirstSuccess).
// If the mock returns a shared pointer, those mutations accumulate across
// calls and tests see cumulative state instead of per-call behavior.
type mockResultFactoryTool struct {
	name    string
	factory func() *toolshared.ToolResult
}

func (m *mockResultFactoryTool) Name() string { return m.name }
func (m *mockResultFactoryTool) Description() string {
	return "factory mock for soft-prompt tests"
}
func (m *mockResultFactoryTool) Parameters() map[string]any {
	return map[string]any{"type": "object"}
}
func (m *mockResultFactoryTool) Execute(_ context.Context, _ map[string]any) *toolshared.ToolResult {
	return m.factory()
}

// freshSuccess / freshTransient / freshValidation factories — each call yields
// a new *ToolResult so registry mutations don't leak across calls.
func freshSuccess() *toolshared.ToolResult {
	return toolshared.SilentResult("hello")
}

func freshTransient() *toolshared.ToolResult {
	return toolshared.ErrorResult("network blip").WithErrorKind(toolshared.ErrTransient)
}

func freshValidation() *toolshared.ToolResult {
	return toolshared.ErrorResult("bad arg").WithErrorKind(toolshared.ErrInvalidInput)
}

// TestSoftPrompt_FirstSuccess_AppearsOncePerTurn asserts the FIRST successful
// execution in a named session appends SoftPromptFirstSuccess, but the SECOND
// success within the same turn does NOT (deduped per (session, tool)).
//
// Revert-guard: if the soft-prompt block is removed, both ForLLM strings
// would equal "hello" only, and the first assert would fail loudly.
func TestSoftPrompt_FirstSuccess_AppearsOncePerTurn(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockResultFactoryTool{name: "greet", factory: freshSuccess})

	result1 := r.ExecuteWithContext(
		context.Background(), "greet", nil, "telegram", "chat-42", nil,
	)
	if result1.IsError {
		t.Fatalf("expected success, got error: %s", result1.ForLLM)
	}
	wantFirst := "hello" + SoftPromptFirstSuccess
	if result1.ForLLM != wantFirst {
		t.Errorf("first success: expected %q, got %q", wantFirst, result1.ForLLM)
	}
	if !strings.Contains(result1.ForLLM, "tool_knowledge") {
		t.Errorf("first success must reference tool_knowledge, got %q", result1.ForLLM)
	}

	// Second call — fresh *ToolResult, fresh factory invocation.
	result2 := r.ExecuteWithContext(
		context.Background(), "greet", nil, "telegram", "chat-42", nil,
	)
	if result2.ForLLM != "hello" {
		t.Errorf("second success: expected %q (no soft-prompt), got %q",
			"hello", result2.ForLLM)
	}
}

// TestSoftPrompt_FirstSuccess_DifferentSessionsEachGetPrompt asserts the
// dedup is per-(session,tool), not global. Two different chat IDs each
// receive their own first-success nudge.
func TestSoftPrompt_FirstSuccess_DifferentSessionsEachGetPrompt(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockResultFactoryTool{name: "greet", factory: freshSuccess})

	a := r.ExecuteWithContext(context.Background(), "greet", nil, "telegram", "chat-A", nil)
	b := r.ExecuteWithContext(context.Background(), "greet", nil, "telegram", "chat-B", nil)

	want := "hello" + SoftPromptFirstSuccess
	if a.ForLLM != want {
		t.Errorf("session A: expected %q, got %q", want, a.ForLLM)
	}
	if b.ForLLM != want {
		t.Errorf("session B: expected %q, got %q", want, b.ForLLM)
	}
}

// TestSoftPrompt_RepeatedFailure_FiresOnSecond asserts that on the second
// transient failure (count=2, between [2, threshold-1]=2), the registry
// appends SoftPromptRepeatedFailure ALONGSIDE the canonical transientHint.
//
// Order in result.ForLLM: result body → SoftPromptRepeatedFailure → transientHint
// (the soft-prompt block runs before the canonical-hint append at registry.go:~827).
//
// Failure #1 (count=1) does NOT include the soft-prompt — the LLM just learned
// about the failure and hasn't had a chance to retry yet, so the nudge would
// be premature.
//
// Failure #3 trips the breaker (threshold=3) → escalationHint replaces transientHint
// and no soft-prompt is added (StatusBlocked path, covered by Test 4).
//
// Revert-guard: if `&& fb.Message == ""` is re-introduced at registry.go:807,
// the soft-prompt block is dead because RecordResult unconditionally sets
// transientHint for StatusTransient. See git log for "fix(tools): Phase 3
// soft-prompt gate" to find the original fix.
func TestSoftPrompt_RepeatedFailure_FiresOnSecond(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockResultFactoryTool{name: "flaky", factory: freshTransient})

	// Failure #1 — count=1, transientHint only, NO soft-prompt.
	r1 := r.ExecuteWithContext(
		context.Background(), "flaky", nil, "telegram", "chat-X", nil,
	)
	if r1.IsError != true || r1.ErrKind != toolshared.ErrTransient {
		t.Fatalf("expected transient error, got IsError=%v ErrKind=%q",
			r1.IsError, r1.ErrKind)
	}
	if !strings.Contains(r1.ForLLM, "more attempt") {
		t.Errorf("failure #1 must include transientHint (canonical hint), got %q",
			r1.ForLLM)
	}
	if strings.Contains(r1.ForLLM, SoftPromptRepeatedFailure) {
		t.Errorf("failure #1 must NOT include SoftPromptRepeatedFailure (count=1 < 2), got %q",
			r1.ForLLM)
	}

	// Failure #2 — count=2 ∈ [2, threshold-1=2], soft-prompt MUST fire alongside
	// the canonical hint. This is the regression-pin: if `&& fb.Message == ""`
	// returns to registry.go:807, this assertion fails because the soft-prompt
	// block becomes dead code.
	r2 := r.ExecuteWithContext(
		context.Background(), "flaky", nil, "telegram", "chat-X", nil,
	)
	if !strings.Contains(r2.ForLLM, SoftPromptRepeatedFailure) {
		t.Errorf("failure #2 must include SoftPromptRepeatedFailure (count=2 in [2, threshold-1]): got %q",
			r2.ForLLM)
	}
	if !strings.Contains(r2.ForLLM, "more attempt") {
		t.Errorf("failure #2 must also include transientHint (composes with soft-prompt): got %q",
			r2.ForLLM)
	}
}

// TestSoftPrompt_ThresholdReached_BreakerEscalation asserts that when the
// circuit breaker trips (count >= failureThreshold=3), the registry surfaces
// escalationHint (from circuit_breaker.go:355) — NOT the signature-tracker's
// "STOP and reconsider" message, because the breaker fires first.
//
// The breaker hint format is:
//
//	System: Tool "X" is temporarily disabled (Circuit Open) due to 3
//	consecutive failures. DO NOT attempt to call it again right now.
//
// 4th call hits the breaker's Allow()==false path and returns the same
// escalationHint via blockedHint.
func TestSoftPrompt_ThresholdReached_BreakerEscalation(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockResultFactoryTool{name: "flaky", factory: freshTransient})

	// Drive 3 transient failures — count reaches default failureThreshold=3.
	for i := 1; i <= 3; i++ {
		r.ExecuteWithContext(
			context.Background(), "flaky", nil, "telegram", "chat-Y", nil,
		)
	}

	// 4th call — breaker is Open, Allow() returns false, blockedHint is
	// escalationHint (lastErrorKind is ErrTransient, not ErrDependencyDown).
	r4 := r.ExecuteWithContext(
		context.Background(), "flaky", nil, "telegram", "chat-Y", nil,
	)

	// escalationHint format check.
	if !strings.Contains(r4.ForLLM, "Circuit Open") {
		t.Errorf("expected escalationHint ('Circuit Open') in 4th failure, got %q",
			r4.ForLLM)
	}
	if !strings.Contains(r4.ForLLM, "DO NOT attempt to call it again") {
		t.Errorf("expected escalationHint directive, got %q", r4.ForLLM)
	}
}

// TestSoftPrompt_ValidationError_NeverNudges asserts that ErrInvalidInput
// (which is non-retryable and does NOT count toward failure threshold) does
// NOT trigger any soft-prompt, even after multiple calls.
func TestSoftPrompt_ValidationError_NeverNudges(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockResultFactoryTool{name: "strict", factory: freshValidation})

	for i := 1; i <= 4; i++ {
		res := r.ExecuteWithContext(
			context.Background(), "strict", nil, "telegram", "chat-Z", nil,
		)
		if strings.Contains(res.ForLLM, SoftPromptFirstSuccess) {
			t.Errorf("validation error must never include SoftPromptFirstSuccess (call #%d): %q",
				i, res.ForLLM)
		}
		if strings.Contains(res.ForLLM, SoftPromptRepeatedFailure) {
			t.Errorf("validation error must never include SoftPromptRepeatedFailure (call #%d): %q",
				i, res.ForLLM)
		}
		// validationHint IS present (canonical hint).
		if !strings.Contains(res.ForLLM, "validation failed") {
			t.Errorf("validation error must include validationHint (call #%d): %q",
				i, res.ForLLM)
		}
	}
}

// TestSoftPrompt_ResetBetweenTurns_ClearsState asserts that
// ResetSignatureFailures (called at turn boundaries by turn_coord.go) clears
// the seenFirstSuccess flag, so the next turn re-receives the soft prompt.
func TestSoftPrompt_ResetBetweenTurns_ClearsState(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockResultFactoryTool{name: "greet", factory: freshSuccess})

	// Turn 1: first success includes soft prompt.
	r1 := r.ExecuteWithContext(
		context.Background(), "greet", nil, "telegram", "chat-T", nil,
	)
	if r1.ForLLM != "hello"+SoftPromptFirstSuccess {
		t.Errorf("turn 1 first success: expected soft-prompt, got %q", r1.ForLLM)
	}

	// Turn boundary reset.
	r.ResetSignatureFailures("telegram", "chat-T")

	// Turn 2: first success again must include soft prompt.
	r2 := r.ExecuteWithContext(
		context.Background(), "greet", nil, "telegram", "chat-T", nil,
	)
	if r2.ForLLM != "hello"+SoftPromptFirstSuccess {
		t.Errorf("turn 2 first success: expected soft-prompt after reset, got %q",
			r2.ForLLM)
	}
}

// TestSoftPrompt_AnonNamespace_AlwaysEligible asserts that when channel and
// chatID are both empty (the "anon" namespace), the soft-prompt is still
// appended on first success. Per registry.go:448-449, anon returns false
// from seenFirstSuccessBefore (always eligible), so first success gets the
// prompt and subsequent anon successes also get it (no dedup map to check).
func TestSoftPrompt_AnonNamespace_AlwaysEligible(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockResultFactoryTool{name: "greet", factory: freshSuccess})

	r1 := r.ExecuteWithContext(context.Background(), "greet", nil, "", "", nil)
	if r1.ForLLM != "hello"+SoftPromptFirstSuccess {
		t.Errorf("anon call 1: expected soft-prompt, got %q", r1.ForLLM)
	}

	// Anon call 2 — should also include prompt (no dedup in anon).
	r2 := r.ExecuteWithContext(context.Background(), "greet", nil, "", "", nil)
	if r2.ForLLM != "hello"+SoftPromptFirstSuccess {
		t.Errorf("anon call 2: expected soft-prompt (anon always eligible), got %q",
			r2.ForLLM)
	}
}
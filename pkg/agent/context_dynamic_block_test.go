// Package agent — Tests for Plan Q1=B "Layout B" cache-utilization fix.
//
// Background (from plan anthropic-cache-utilization-v1-passive-cache-dynamic-block-split-observability-20260822):
//   - MiniMax-M3 has PASSIVE cache (prefix identity match, 20-block lookback window, 5-min TTL).
//   - Root cause: `buildDynamicContext` returns content with real `time.Now()` — always changing per call.
//   - T0 Scenario D gate (live verified 2026-08-22): D2 FAIL — cache_read=128→128→128 when content changes per call
//     even if placed at end of system. D1+D3 PASS — content identity IS the real constraint, not position.
//   - Fix (Q1=B điều chỉnh): move dynamic content OUT of system → into user[0] (last user message).
//   - Static prefix system[0..N] becomes 100% identity-stable per call sequence.
//   - User[0] gets a `<dynamic_context>` block prepended with the actual user content.
//
// Wire contract verified by these tests:
//   1. Dynamic content MUST NOT appear in any system ContentBlock (system prefix stays identity-stable).
//   2. Dynamic content MUST be prepended to user[0] when user[0] is added to messages.
//   3. The dynamic block format MUST be `<dynamic_context>...</dynamic_context>` for round-trip parse stability.
//   4. If `BuildMessagesFromPrompt` is called with no user message (CurrentMessage empty + no media),
//      a stub user[0] is appended containing only the dynamic context.
//   5. If the default system prompt is suppressed (`SuppressDefaultSystemPrompt=true`), NO dynamic block is
//      built at all and the user message is unchanged — no `<dynamic_context>` wrapper added.
//   6. PRODUCTION WIRE (auto-stash, C3 fact-check 2026-08-23): `buildDynamicContext` runs INTERNALLY and
//      stashes into req.DynamicContext; no production caller sets the field. A default request therefore
//      ALWAYS ships the dynamic block inside user[0] — guarded by
//      TestBuildMessagesFromPrompt_AutoStashDynamicInUserZero.
//
// Tests must FAIL before the production code change; pass after.
package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// stubContextBuilderForDynamic returns a minimal ContextBuilder suitable for
// BuildMessagesFromPrompt tests. Uses t.TempDir() — same pattern as turn_state_phase_rebuild_test.go.
func stubContextBuilderForDynamic(t *testing.T) *ContextBuilder {
	t.Helper()
	return NewContextBuilder(t.TempDir())
}

// TestBuildMessagesFromPrompt_DynamicBlockNotInSystem verifies the critical
// invariant: dynamic content (time/runtime/session) MUST NOT appear in any
// system ContentBlock. Otherwise MiniMax-M3 cache hit rate stays at 0%.
func TestBuildMessagesFromPrompt_DynamicBlockNotInSystem(t *testing.T) {
	cb := stubContextBuilderForDynamic(t)

	dynCtx := "## Current Time\n2026-08-22 22:30 (Saturday)\n\n## Runtime\nlinux arm64, Go go1.26.3"
	req := PromptBuildRequest{
		CurrentMessage: "Hello world",
		DynamicContext: dynCtx,
	}
	msgs := cb.BuildMessagesFromPrompt(req)

	if len(msgs) == 0 {
		t.Fatalf("expected at least 1 message, got 0")
	}

	// The very first message MUST be system role. Verify no DynamicContext inside it.
	if msgs[0].Role != "system" {
		t.Fatalf("expected first message role=system, got %q", msgs[0].Role)
	}

	// Walk all content-bearing forms of msgs[0] to assert no dynamic substring.
	if hasDynamicInContent(msgs[0], dynCtx) {
		t.Fatalf("BUG: dynamic context %q found inside system message — should be in user[0] only.\nFull content: %v",
			dynCtx, extractStringContent(msgs[0]))
	}
}

// TestBuildMessagesFromPrompt_DynamicBlockInUserZero verifies the move target:
// dynamic content MUST appear in user[0] (the last user message), prepended
// in a `<dynamic_context>` wrapper so the LLM can parse it deterministically.
//
// The test fixture dynCtx matches the actual buildDynamicContext output shape
// ("## Current Time\n...\n\n## Runtime\n...") so we can use exact substring match.
func TestBuildMessagesFromPrompt_DynamicBlockInUserZero(t *testing.T) {
	cb := stubContextBuilderForDynamic(t)

	userMsg := "Em chạy horus protocol nhé"
	// Use a marker substring (the static "## Current Time" header) instead of
	// exact time string — exact time varies per test run and isn't the contract.
	dynCtx := "## Current Time\n2026-08-22 22:30 (Saturday)\n\n## Runtime\nlinux arm64, Go go1.26.3"
	// Just check the marker sections appear, not the full exact string.
	dynMarker1 := "## Current Time"
	dynMarker2 := "## Runtime"
	req := PromptBuildRequest{
		CurrentMessage: userMsg,
		DynamicContext: dynCtx,
	}
	msgs := cb.BuildMessagesFromPrompt(req)

	// Find user role message.
	var userContent string
	for _, m := range msgs {
		if m.Role == "user" {
			userContent = extractStringContent(m)
			break
		}
	}

	if !strings.Contains(userContent, "<dynamic_context>") {
		t.Fatalf("expected user message to contain <dynamic_context> wrapper, got: %q", userContent)
	}
	if !strings.Contains(userContent, dynMarker1) {
		t.Fatalf("expected user message to contain marker %q, got: %q", dynMarker1, userContent)
	}
	if !strings.Contains(userContent, dynMarker2) {
		t.Fatalf("expected user message to contain marker %q, got: %q", dynMarker2, userContent)
	}
	if !strings.Contains(userContent, userMsg) {
		t.Fatalf("expected user message to contain original content %q, got: %q", userMsg, userContent)
	}

	// Verify dynamic block comes BEFORE user content (prepended, not appended).
	idxDyn := strings.Index(userContent, "<dynamic_context>")
	idxUser := strings.Index(userContent, userMsg)
	if idxDyn > idxUser {
		t.Fatalf("expected dynamic block to be PREPENDED (idx < user content idx).\n  dynamic at %d, user at %d\n  full: %q",
			idxDyn, idxUser, userContent)
	}
	_ = dynCtx // mark used
}

// TestBuildMessagesFromPrompt_NoDynamicContextLeavesUserIntact verifies the
// suppressed-dynamic case: when the default system prompt is suppressed
// (SuppressDefaultSystemPrompt=true), buildDynamicContext never runs, the auto-stash
// stays empty, and the user message must NOT contain the `<dynamic_context>` wrapper.
func TestBuildMessagesFromPrompt_NoDynamicContextLeavesUserIntact(t *testing.T) {
	cb := stubContextBuilderForDynamic(t)

	userMsg := "Just a hello, no dynamic context provided"
	req := PromptBuildRequest{
		CurrentMessage:            userMsg,
		DynamicContext:            "",     // explicitly empty
		SuppressDefaultSystemPrompt: true, // no internal buildDynamicContext → no stash
	}
	msgs := cb.BuildMessagesFromPrompt(req)

	// Find the LAST user message (History may inject earlier user role msgs;
	// we want the appended user[0] built by BuildMessagesFromPrompt itself).
	userContent := lastUserMessageContent(msgs)

	if strings.Contains(userContent, "<dynamic_context>") {
		t.Fatalf("BUG: <dynamic_context> block present though dynamic suppressed.\n  user content: %q", userContent)
	}
	if !strings.Contains(userContent, userMsg) {
		t.Fatalf("expected user message to contain %q unchanged, got: %q", userMsg, userContent)
	}
}

// TestBuildMessagesFromPrompt_NoUserMessageStillIncludesDynamic verifies the
// edge case where CurrentMessage == "" and Media == nil but DynamicContext is non-empty.
// In that case the helper appends a stub user[0] — that stub MUST contain the dynamic block.
func TestBuildMessagesFromPrompt_NoUserMessageStillIncludesDynamic(t *testing.T) {
	cb := stubContextBuilderForDynamic(t)

	dynCtx := "## Current Time\n2026-08-22 22:30 (Saturday)\n\n## Runtime\nlinux arm64, Go go1.26.3"
	req := PromptBuildRequest{
		CurrentMessage: "", // no user text
		Media:          nil,
		DynamicContext: dynCtx,
	}
	msgs := cb.BuildMessagesFromPrompt(req)

	var userContent string
	for _, m := range msgs {
		if m.Role == "user" {
			userContent = extractStringContent(m)
			break
		}
	}

	if !strings.Contains(userContent, "<dynamic_context>") {
		t.Fatalf("expected stub user message to contain <dynamic_context> block when no CurrentMessage provided, got: %q", userContent)
	}
	// Dynamic content is auto-stashed from buildDynamicContext (real time.Now()) —
	// assert marker subtrings, never an exact time string.
	if !strings.Contains(userContent, "## Current Time") {
		t.Fatalf("expected stub user message to contain dynamic context marker (## Current Time), got: %q", userContent)
	}
}

// hasDynamicInContent checks whether substring `needle` appears anywhere in a
// message's content. Fact-check 2026-08-23 (HEAD fc781a2b + working tree):
// providers.Message.Content is a plain string and SystemParts is []ContentBlock
// {Type, Text, ...}; both are covered here (the system message body is Content,
// the block-split form lives in SystemParts).
func hasDynamicInContent(m providers.Message, needle string) bool {
	if strings.Contains(m.Content, needle) {
		return true
	}
	for _, b := range m.SystemParts {
		if strings.Contains(b.Text, needle) {
			return true
		}
	}
	return false
}

// extractStringContent returns the combined text of a providers.Message.
func extractStringContent(m providers.Message) string {
	var sb strings.Builder
	sb.WriteString(m.Content)
	for _, b := range m.SystemParts {
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// lastUserMessageContent returns the combined text of the LAST user-role message.
// BuildMessagesFromPrompt appends the current user turn at the end; History may
// inject earlier user-role messages, so callers looking for the appended user[0]
// content must target the last user message.
func lastUserMessageContent(msgs []providers.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return extractStringContent(msgs[i])
		}
	}
	return ""
}

// TestBuildMessagesFromPrompt_AutoStashDynamicInUserZero verifies the PRODUCTION
// wire (C3 fact-check 2026-08-23): BuildMessagesFromPrompt generates the dynamic
// block INTERNALLY via cb.buildDynamicContext(...) and stashes it into
// req.DynamicContext — NO production caller sets the field explicitly (grep
// `DynamicContext:` only matched test files). This test guards the auto-stash
// path: default request (field empty, SuppressDefaultSystemPrompt false) must
// still produce the `<dynamic_context>` block in user[0]. If a future refactor
// removes the internal build call, this test fails — the cache-utilization
// feature silently dies with it.
func TestBuildMessagesFromPrompt_AutoStashDynamicInUserZero(t *testing.T) {
	cb := stubContextBuilderForDynamic(t)

	// Deliberately NOT setting req.DynamicContext — production callers never do.
	req := PromptBuildRequest{
		CurrentMessage: "hello from production wire",
	}
	msgs := cb.BuildMessagesFromPrompt(req)

	userContent := lastUserMessageContent(msgs)
	if !strings.Contains(userContent, "<dynamic_context>") {
		t.Fatalf("BUG: production wire produced NO <dynamic_context> block in user[0] — auto-stash missing. got: %q", userContent)
	}
	if !strings.Contains(userContent, "## Current Time") {
		t.Fatalf("BUG: user[0] lacks buildDynamicContext output (## Current Time). got: %q", userContent)
	}
	if !strings.Contains(userContent, "hello from production wire") {
		t.Fatalf("BUG: user[0] lost the original user message. got: %q", userContent)
	}
}

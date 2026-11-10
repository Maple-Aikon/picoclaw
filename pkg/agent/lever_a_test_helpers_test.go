package agent

// Test helpers for lever (a) — 2026-11-10.
//
// LEVER (a) moved the phase-specific affordance (phase hint + goal snapshot
// + compass + tool-discovery/MCP note) OUT of the SYSTEM prompt and into the
// PHASE CHANNEL, which rides in the turn's user message.
//
// These helpers let the EXISTING wire tests assert against the NEW location
// without rewriting their intent (the contract says: update, do not delete).

import (
	"strings"
	"testing"
)

// phaseChannelOf returns the phase-channel text carried by the messages
// built for req. Returns "" when the build carries no phase channel.
func phaseChannelOf(t *testing.T, cb *ContextBuilder, req PromptBuildRequest) string {
	t.Helper()
	msgs := cb.BuildMessagesFromPrompt(req)
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		if i := strings.Index(m.Content, phaseChannelMarker); i >= 0 {
			return m.Content[i:]
		}
	}
	return ""
}

// assertPhaseChannelContains asserts that the phase channel built for req
// contains needle.
func assertPhaseChannelContains(t *testing.T, cb *ContextBuilder, req PromptBuildRequest, needle, rationale string) {
	t.Helper()
	ch := phaseChannelOf(t, cb, req)
	if ch == "" {
		t.Fatalf("phase channel absent; expected it to contain %q (%s)", needle, rationale)
	}
	if !strings.Contains(ch, needle) {
		t.Errorf("phase channel missing %q (%s); got: %s", needle, rationale, ch)
	}
}

// assertPhaseChannelAbsent asserts that the phase channel built for req does
// NOT contain needle.
func assertPhaseChannelAbsent(t *testing.T, cb *ContextBuilder, req PromptBuildRequest, needle, rationale string) {
	t.Helper()
	ch := phaseChannelOf(t, cb, req)
	if ch == "" {
		return // no channel at all => needle absent by definition
	}
	if strings.Contains(ch, needle) {
		t.Errorf("phase channel must NOT contain %q (%s); got: %s", needle, rationale, ch)
	}
}

// phaseChannelReq builds a request for the given phase with the standard
// cap dims, so the compass renders (MaxIterationsCap > 0).
func phaseChannelReq(phase string, iter int, snapshot string) PromptBuildRequest {
	return phaseChannelReqCaps(phase, iter, snapshot, 25, 300)
}

// phaseChannelReqCaps is phaseChannelReq with explicit cap dims - needed
// by tests that assert on a specific "total N turn iters" string.
func phaseChannelReqCaps(phase string, iter int, snapshot string, cap, maxCap int) PromptBuildRequest {
	return PromptBuildRequest{
		GoalPhase:        phase,
		Iteration:        iter,
		GoalSnapshot:     snapshot,
		IterationCap:     cap,
		MaxIterationsCap: maxCap,
		PhaseStartIter:   2,
		CurrentMessage:   "do the thing",
	}
}

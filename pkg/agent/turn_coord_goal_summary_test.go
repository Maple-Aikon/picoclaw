package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// Phase 12.6.0 regression: when complete_goal fires and the LLM emits short
// prose + complete_goal in the same iteration, turnCoord used to emit the
// "empty response" DefaultResponse because line 366 (post-loop fallback) ran
// BEFORE line 381-385 (goal.Summary fallback). The ordering was wrong — the
// DefaultResponse clobbered finalContent so the goal.Summary branch never
// fired.
//
// This test exercises applyFallbackForEmptyResponse directly so we don't
// have to drive a full agent loop with mock tools. The helper is the single
// source of truth for the fallback chain (introduced in Phase 12.6.0).

// TestApplyFallbackForEmptyResponse_GoalSummaryPreferred verifies that when
// ts.goalFinalized is set and the LLM emitted no prose, the persisted
// goal.Summary is returned (NOT DefaultResponse).
func TestApplyFallbackForEmptyResponse_GoalSummaryPreferred(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const wantSummary = "Done. uv upgraded 0.11.30 → 0.11.31 via `uv self update`."
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "update-uv",
		Description: goal.Description{
			Objective:       "Upgrade uv",
			SuccessCriteria: []string{"uv >= 0.11.30 reports installed"},
		},
		Status:  goal.StatusCompleted,
		Summary: wantSummary,
	}
	if err := st.Write("test-session-summary", g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts("test-session-summary"), turnEventScope{
		turnID:  "turn-summary",
		context: newTurnContext(nil, nil, nil),
	})
	ts.goalFinalized = true
	ts.assistantText = "" // LLM did not emit prose (the bug scenario)
	ts.sessionKey = "test-session-summary" // override since makeTestProcessOpts sets top-level only

	got := al.applyFallbackForEmptyResponse(ts)
	if got != wantSummary {
		t.Fatalf("expected goal.Summary %q, got %q", wantSummary, got)
	}
}

// TestApplyFallbackForEmptyResponse_IterationCapHitsToolLimit verifies that
// when we hit the iteration cap (no goal involved), toolLimitResponse is
// returned in preference to DefaultResponse.
func TestApplyFallbackForEmptyResponse_IterationCapHitsToolLimit(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	ts := newTurnState(agent, makeTestProcessOpts("test-session-cap"), turnEventScope{
		turnID:  "turn-cap",
		context: newTurnContext(nil, nil, nil),
	})
	// Force iteration >= cap without running the loop.
	ts.iteration = ts.iterationCap

	got := al.applyFallbackForEmptyResponse(ts)
	if got != toolLimitResponse {
		t.Fatalf("expected toolLimitResponse, got %q", got)
	}
	if strings.Contains(got, "empty response") {
		t.Fatalf("toolLimitResponse should not contain the 'empty response' string; got %q", got)
	}
}

// TestApplyFallbackForEmptyResponse_DefaultsToDefaultResponse verifies the
// base case — no goal context, under iteration cap, returns DefaultResponse.
func TestApplyFallbackForEmptyResponse_DefaultsToDefaultResponse(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	ts := newTurnState(agent, makeTestProcessOpts("test-session-default"), turnEventScope{
		turnID:  "turn-default",
		context: newTurnContext(nil, nil, nil),
	})
	want := "I couldn't process your request." // from makeTestProcessOpts
	got := al.applyFallbackForEmptyResponse(ts)
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// TestApplyFallbackForEmptyResponse_AssistantTextPrevents verifies that even
// when the goal is finalized, if the LLM already emitted a prose reply we
// do NOT overwrite with goal.Summary. assistantText takes precedence
// (Phase 11 design intent).
func TestApplyFallbackForEmptyResponse_AssistantTextPrevents(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "with-prose",
		Description: goal.Description{
			Objective:       "test",
			SuccessCriteria: []string{"placeholder"},
		},
		Status:  goal.StatusCompleted,
		Summary: "should NOT be used; LLM wrote prose",
	}
	if err := st.Write("test-session-prose", g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts("test-session-prose"), turnEventScope{
		turnID:  "turn-prose",
		context: newTurnContext(nil, nil, nil),
	})
	ts.goalFinalized = true
	ts.assistantText = "All done, here's the result."
	ts.sessionKey = "test-session-prose" // see test-session-summary comment

	got := al.applyFallbackForEmptyResponse(ts)
	if got == g.Summary {
		t.Fatal("should NOT return goal.Summary when assistantText is non-empty")
	}
	if got != "I couldn't process your request." {
		t.Fatalf("expected DefaultResponse, got %q", got)
	}
}

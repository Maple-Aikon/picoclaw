package agent

// Phase 12.60 — bexhausted:* abort reasons must map to a stuck message in
// phaseStuckFallbackMessage (instead of falling through to the misleading
// toolLimitResponse "Increase max_tool_iterations" advice).
//
// Bug: main-turn-12 (2026-08-08) goal archived with abort_reason
// "bexhausted:goal_recovery" (handleGoalRecovery BoundedRetry exhausted
// after 3 reasoning-only recall attempts). phaseStuckFallbackMessage only
// mapped goal_stuck_v1_* reasons → returned "" → user got the generic
// "Increase max_tool_iterations" advice even though 25 iters were enough.

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

func TestPhaseStuckFallbackMessage_BexhaustedPrefix(t *testing.T) {
	al, _, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	ts := &turnState{
		lastPhaseStuckError: "BoundedRetry exhausted",
	}

	cases := []struct {
		name        string
		abortReason string
		wantEmpty   bool
	}{
		{name: "goal_recovery", abortReason: "bexhausted:goal_recovery"},
		{name: "tool_exec", abortReason: "bexhausted:tool_exec"},
		{name: "hook_replay", abortReason: "bexhausted:hook_replay"},
		{name: "recovery_trigger", abortReason: "bexhausted:recovery_trigger"},
		{name: "user_abort_non_bexhausted", abortReason: "user_abort", wantEmpty: true},
		{name: "empty_reason", abortReason: "", wantEmpty: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &goal.Goal{Status: goal.StatusAborted, AbortReason: tc.abortReason}
			msg := al.phaseStuckFallbackMessage(ts, g)
			if tc.wantEmpty {
				if msg != "" {
					t.Fatalf("abortReason %q: got %q, want empty", tc.abortReason, msg)
				}
				return
			}
			if msg == "" {
				t.Fatalf("abortReason %q: want non-empty stuck message", tc.abortReason)
			}
			if !strings.Contains(msg, "recovery retries were exhausted") {
				t.Errorf("abortReason %q: message must describe recovery exhaustion, got %q", tc.abortReason, msg)
			}
			if !strings.Contains(msg, "BoundedRetry exhausted") {
				t.Errorf("abortReason %q: message must include last error, got %q", tc.abortReason, msg)
			}
			if !strings.Contains(msg, "1 attempt") {
				t.Errorf("abortReason %q: message must show attempt count, got %q", tc.abortReason, msg)
			}
			// Phase 12.43: no phase/iter/cap claims in LLM-visible text.
			assertNoPhaseClaim(t, msg)
		})
	}

	// Active goal with a bexhausted-looking reason → still empty (only
	// archived goals surface the stuck message).
	if msg := al.phaseStuckFallbackMessage(ts, &goal.Goal{Status: goal.StatusActive, AbortReason: "bexhausted:goal_recovery"}); msg != "" {
		t.Fatalf("active goal: got %q, want empty", msg)
	}
}

func TestApplyFallbackForEmptyResponse_Bexhausted_Preferred(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-session-bexhausted"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name: "local-lm-scenarios",
		Description: goal.Description{
			Objective:       "Run 3 scenarios with local LM",
			SuccessCriteria: []string{"sort_5 passes", "reverse_string passes", "fib passes"},
		},
		Status:      goal.StatusAborted,
		AbortReason: "bexhausted:goal_recovery",
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-bexhausted",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.lastPhaseStuckError = "BoundedRetry exhausted"

	got := al.applyFallbackForEmptyResponse(ts)
	if !strings.Contains(got, "recovery retries were exhausted") {
		t.Fatalf("expected bexhausted stuck message, got %q", got)
	}
	if !strings.Contains(got, "BoundedRetry exhausted") {
		t.Fatalf("expected last error in message, got %q", got)
	}
	if strings.Contains(got, "Increase max_tool_iterations") {
		t.Fatalf("bexhausted fallback must NOT use the misleading toolLimitResponse advice; got %q", got)
	}
	assertNoPhaseClaim(t, got)
}

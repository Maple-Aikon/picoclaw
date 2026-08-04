package agent

import (
	"testing"
)

// Phase 12.47 (T4 + F1): POST-FINAL recovery is SILENT — a single guard at
// the top of evaluateRecovery returns RecoveryNone for every trigger. The
// goal is archived and the user already received the full summary via
// complete_goal self-publish (Phase 12.44); the report iter only emits
// text, so any recovery prompt would be redundant.
//
// TDD note (L1-1): only the EMPTY row is truly RED — without the guard,
// Phase="post_final" falls through to the empty branch (the
// `Phase == Final && PostCompleteGoalReport` silent check does NOT match
// "post_final") and fires a ×3 retry. text-only and tool-exec rows PASS
// even pre-guard (no restricted/Open branch matches) — they are
// regression-lock rows asserting the guard does not break shared paths.
func TestEvaluateRecovery_PostFinal_Silent_AllTriggers(t *testing.T) {
	ts := newPhase5TurnState(t)

	t.Run("empty → RecoveryNone (RED row)", func(t *testing.T) {
		ctx := RecoveryContext{
			Phase:        string(GoalPhasePostFinal),
			Iteration:    1,
			TextEmpty:    true,
			HasToolCalls: false,
		}
		action, msg := evaluateRecovery(ts, ctx)
		if action != RecoveryNone {
			t.Fatalf("want RecoveryNone, got %v (msg=%q)", action, msg)
		}
		if msg != "" {
			t.Fatalf("want empty msg, got %q", msg)
		}
		if ts.emptyResponseRecoveryCount != 0 {
			t.Fatalf("emptyResponseRecoveryCount must stay 0 at post_final, got %d", ts.emptyResponseRecoveryCount)
		}
	})

	t.Run("text-only → RecoveryNone (regression-lock)", func(t *testing.T) {
		ctx := RecoveryContext{
			Phase:        string(GoalPhasePostFinal),
			Iteration:    1,
			TextEmpty:    false,
			HasToolCalls: false,
		}
		action, msg := evaluateRecovery(ts, ctx)
		if action != RecoveryNone {
			t.Fatalf("want RecoveryNone, got %v (msg=%q)", action, msg)
		}
		if msg != "" {
			t.Fatalf("want empty msg, got %q", msg)
		}
	})

	t.Run("tool-exec error → RecoveryNone (regression-lock)", func(t *testing.T) {
		ctx := RecoveryContext{
			Phase:         string(GoalPhasePostFinal),
			Iteration:     1,
			TextEmpty:     false,
			HasToolCalls:  true,
			ToolName:      "complete_goal",
			ToolExecError: "tool execution failed: boom",
		}
		action, msg := evaluateRecovery(ts, ctx)
		if action != RecoveryNone {
			t.Fatalf("want RecoveryNone, got %v (msg=%q)", action, msg)
		}
		if msg != "" {
			t.Fatalf("want empty msg, got %q", msg)
		}
	})
}

package agent

import (
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
)

// =====================================================================
// T8 — Integration loop simulation: complete_goal @ iter 4 (cap 5) →
// iter 5 = POST-FINAL, LLM request tools_visible=0 (post-allowlist projected),
// registry tools_total=N (unfiltered registry count, post-12.50 F3 semantics),
// exactly 1 report iter, then the loop exits cleanly.
// =====================================================================

func TestPhase12_47_T8_CompleteGoalThenPostFinalIter(t *testing.T) {
	_, agent, workspace := newPhaseTestLoop(t)
	agent.MaxIterations = 5
	key := "p47-t8"
	writeGoalFile(t, workspace, key, string(goal.StatusActive))
	ts := newPhaseTestTurnState(agent, key, workspace)
	ts.iteration = 4
	ts.iterationCap = 5
	ts.maxIterationsCap = 15

	// --- iter 4 body: complete_goal tool exec fires ---
	ts.goalFinalized = true

	// F6: phase flips to POST-FINAL immediately after complete_goal, no
	// oscillation back to FINAL.
	if got := ts.currentGoalPhase(); got != GoalPhasePostFinal {
		t.Fatalf("want PostFinal ngay sau complete_goal, got %s", got)
	}

	// bumpCapForFinalReportIter (turn_coord.go:710): iter4+1=5 not > cap 5
	// → no bump; loop condition 4 < 5 stays true → iter 5 runs.
	al := &AgentLoop{}
	al.bumpCapForFinalReportIter(ts)
	if ts.iterationCap != 5 {
		t.Fatalf("cap phải giữ 5 (iter4+1 không vượt), got %d", ts.iterationCap)
	}
	if !(ts.currentIteration() < ts.iterationCap) {
		t.Fatal("loop condition phải true để iter 5 (POST-FINAL) chạy")
	}

	// --- iter 5 top-of-body: iteration bump + phase allowlist ---
	ts.iteration = 5
	ts.applyPhaseAllowlist(GoalPhasePostFinal)
	if got := ts.currentGoalPhase(); got != GoalPhasePostFinal {
		t.Fatalf("iter 5 phase phải là PostFinal, got %s", got)
	}
	// L1-2: LLM request trên iter 5 phải thấy 0 tools (tools_visible=0).
	// Post-12.50 F3: tools_total field is now registry count (Tools.Count),
	// NOT post-allowlist projected. ToProviderDefs returns 0 visible at
	// POST-FINAL (correct behavior, allowlist = []).
	if defs := agent.Tools.ToProviderDefs(); len(defs) != 0 {
		t.Fatalf("LLM request iter 5 phải thấy 0 tools, got %d", len(defs))
	}

	// --- iter 5 body: LLM text-only final report (strip fire —
	// pipeline_llm.go:821, normalizedToolCalls=[]) ---
	// post-body marker: sent=true → loop top `5 < 5` false → exit.
	ts.postCompleteGoalReportSent = true
	al.bumpCapForFinalReportIter(ts) // no-op (sent)
	if ts.iterationCap != 5 {
		t.Fatalf("cap phải giữ 5 sau report iter, got %d", ts.iterationCap)
	}
	if ts.currentIteration() < ts.iterationCap {
		t.Fatal("loop phải exit sau đúng 1 report iter (5 < 5 false)")
	}
}

// =====================================================================
// T8b — E1 regression-proof: persistent LLM error cannot infinite-loop.
// Evidence folded from E1 (REJECTED): (1) CallLLM error → runTurn return
// ngay (turn_coord.go:380-382); (2) loop condition has `iter < iterationCap`
// (always bounded); (3) bumpCapForFinalReportIter raises cap ONLY when
// `!postCompleteGoalReportSent`.
// =====================================================================

func TestPhase12_47_T8b_ProviderErrorPersistent_NoInfiniteLoop(t *testing.T) {
	_, agent, workspace := newPhaseTestLoop(t)
	agent.MaxIterations = 5
	key := "p47-t8b"
	writeGoalFile(t, workspace, key, string(goal.StatusActive))
	ts := newPhaseTestTurnState(agent, key, workspace)
	ts.iteration = 5
	ts.iterationCap = 5
	ts.maxIterationsCap = 5
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = true // report iter đã xong

	al := &AgentLoop{}
	al.bumpCapForFinalReportIter(ts) // no-op vì sent=true
	if ts.iterationCap != 5 {
		t.Fatalf("bump phải no-op khi sent=true, cap=5 (got %d)", ts.iterationCap)
	}
	if ts.currentIteration() < ts.iterationCap {
		t.Fatal("loop condition 5<5 false → turn kết thúc, không vòng lặp vô hạn")
	}
}

// =====================================================================
// T9a — POST-FINAL has no phase-stuck semantics: recordPhaseStuckTool-
// AllowedBlockInPhase must be a no-op (no counter increment, no
// lastPhaseStuckError mutation).
// =====================================================================

func TestPhase12_47_T9a_PostFinal_StuckNoOp(t *testing.T) {
	ts := newPhase5TurnState(t)
	before := ts.setGoalAttemptCount + ts.goalProgressAttemptCount + ts.completeGoalAttemptCount
	recordPhaseStuckToolAllowedBlockInPhase(ts, GoalPhasePostFinal, "complete_goal", "boom")
	after := ts.setGoalAttemptCount + ts.goalProgressAttemptCount + ts.completeGoalAttemptCount
	if after != before {
		t.Fatalf("stuck counters phải không đổi tại post_final (before=%d after=%d)", before, after)
	}
	if ts.lastPhaseStuckError != "" {
		t.Fatalf("lastPhaseStuckError phải không đổi (rỗng), got %q", ts.lastPhaseStuckError)
	}
}

// =====================================================================
// T9b — Prompt rebuild: FINAL→POST-FINAL transition fires rebuild (D8);
// POST-FINAL→POST-FINAL does not.
// =====================================================================

func TestPhase12_47_T9b_PostFinal_RebuildFires(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, string(GoalPhaseFinal), GoalPhasePostFinal)
	result := ts.maybeRebuildPromptForPhaseChange(messages, exec, nil, 6)
	if ts.lastBuiltPromptPhase != string(GoalPhasePostFinal) {
		t.Fatalf("lastBuiltPromptPhase phải = post_final, got %q", ts.lastBuiltPromptPhase)
	}
	if len(result) < 1 || result[0].Content == "ORIGINAL_SYSTEM_PROMPT" {
		t.Fatalf("messages[0] phải được rebuild (lastBuilt=final → phase=post_final)")
	}
}

func TestPhase12_47_T9c_PostFinal_NoRebuildSamePhase(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, string(GoalPhasePostFinal), GoalPhasePostFinal)
	result := ts.maybeRebuildPromptForPhaseChange(messages, exec, nil, 6)
	if len(result) < 1 || result[0].Content != "ORIGINAL_SYSTEM_PROMPT" {
		t.Fatalf("không rebuild khi cùng phase post_final; got %q", result[0].Content)
	}
}

// =====================================================================
// T14 — 5×2 matrix: phase × (allowlist shape, empty-response recovery).
// Locks the POST-FINAL row (allowlist=[] non-nil, recovery=None) against
// drift in the other 4 rows.
// =====================================================================

func TestPhase12_47_T14_PhaseMatrix(t *testing.T) {
	def := AgentContextDefinition{Agent: &AgentPromptDefinition{
		Frontmatter: AgentFrontmatter{
			Fields: map[string]any{"tools": true},
			Tools:  []string{"read_file"},
		},
	}}
	cases := []struct {
		phase       GoalPhase
		wantAllow   []string
		emptyAction RecoveryAction
	}{
		{GoalPhaseSet, []string{"set_goal"}, RecoveryRetrySameIteration},
		{GoalPhaseOpen, []string{"complete_goal", "read_file", "view_goal"}, RecoveryRetrySameIteration},
		{GoalPhaseCheckpoint, []string{"goal_progress", "complete_goal"}, RecoveryRetrySameIteration},
		{GoalPhaseFinal, []string{"complete_goal"}, RecoveryRetrySameIteration},
		{GoalPhasePostFinal, []string{}, RecoveryNone},
	}
	for _, c := range cases {
		t.Run(string(c.phase), func(t *testing.T) {
			got := resolveAgentToolAllowlistWithPhase(def, c.phase)
			if !reflect.DeepEqual(got, c.wantAllow) {
				t.Fatalf("allowlist %s: got %v want %v", c.phase, got, c.wantAllow)
			}
			ts := newPhase5TurnState(t)
			action, _ := evaluateRecovery(ts, RecoveryContext{
				Phase:        string(c.phase),
				Iteration:    1,
				TextEmpty:    true,
				HasToolCalls: false,
			})
			if action != c.emptyAction {
				t.Fatalf("empty recovery %s: got %v want %v", c.phase, action, c.emptyAction)
			}
		})
	}
}

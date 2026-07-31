package agent

import (
	"strings"
	"testing"
)

// Phase 12.21 — GoalPhaseCheckpoint hint contributor tests. Analogous to
// goalPhaseSetHintContributor (Phase 12.3) but for Checkpoint phase.

// TestGoalPhaseCheckpointHint_FiresOnlyInCheckpointPhase verifies that the
// hint contributor only returns a non-nil PromptPart when
// req.GoalPhase == string(GoalPhaseCheckpoint) and returns nil for all
// other phases (Open/Set/Final + empty phase).
func TestGoalPhaseCheckpointHint_FiresOnlyInCheckpointPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase string
		want  bool
	}{
		{"Checkpoint_fires", string(GoalPhaseCheckpoint), true},
		{"Open_silent", string(GoalPhaseOpen), false},
		{"Set_silent", string(GoalPhaseSet), false},
		{"Final_silent", string(GoalPhaseFinal), false},
		{"Empty_silent", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := PromptBuildRequest{GoalPhase: tt.phase, Iteration: 25}
			got := goalPhaseCheckpointHintContributor(req)
			if (got != nil) != tt.want {
				t.Fatalf("CheckpointHint(%q) non-nil=%v want=%v", tt.phase, got != nil, tt.want)
			}
		})
	}
}

// TestGoalPhaseCheckpointHint_ContentMentionsLifecycleTools ensures the
// hint text explicitly names goal_progress + complete_goal so the LLM
// does not retry with malformed args (Phase 12.20 wire guard).
func TestGoalPhaseCheckpointHint_ContentMentionsLifecycleTools(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint), Iteration: 25}
	got := goalPhaseCheckpointHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	mustContain := []string{
		"goal_progress",
		"complete_goal",
		"remaining_steps",
		"summary",
		"iter 25",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got.Content, sub) {
			t.Errorf("CheckpointHint missing %q\n--- content ---\n%s", sub, got.Content)
		}
	}
}

// TestGoalPhaseCheckpointHint_PlacementCapabilityTooling ensures the hint
// lands in the Capability / Tooling slot (matches goal_phase_set_hint
// convention from Phase 12.3).
func TestGoalPhaseCheckpointHint_PlacementCapabilityTooling(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint), Iteration: 25}
	got := goalPhaseCheckpointHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	if got.Layer != PromptLayerCapability {
		t.Errorf("Layer=%q want=%q", got.Layer, PromptLayerCapability)
	}
	if got.Slot != PromptSlotTooling {
		t.Errorf("Slot=%q want=%q", got.Slot, PromptSlotTooling)
	}
	if got.Source.ID != PromptSourceGoalPhaseCheckpointHint {
		t.Errorf("Source.ID=%q want=%q", got.Source.ID, PromptSourceGoalPhaseCheckpointHint)
	}
}

// TestGoalPhaseFinalHint_FiresOnlyInFinalPhase verifies hint scoping.
func TestGoalPhaseFinalHint_FiresOnlyInFinalPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase string
		want  bool
	}{
		{"Final_fires", string(GoalPhaseFinal), true},
		{"Checkpoint_silent", string(GoalPhaseCheckpoint), false},
		{"Open_silent", string(GoalPhaseOpen), false},
		{"Set_silent", string(GoalPhaseSet), false},
		{"Empty_silent", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := PromptBuildRequest{GoalPhase: tt.phase}
			got := goalPhaseFinalHintContributor(req)
			if (got != nil) != tt.want {
				t.Fatalf("FinalHint(%q) non-nil=%v want=%v", tt.phase, got != nil, tt.want)
			}
		})
	}
}

// TestGoalPhaseFinalHint_ContentMentionsCompleteGoal ensures the hint
// text explicitly names complete_goal + summary + idempotency so the LLM
// does not retry or call disallowed tools.
func TestGoalPhaseFinalHint_ContentMentionsCompleteGoal(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseFinal)}
	got := goalPhaseFinalHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Final")
	}
	mustContain := []string{
		"complete_goal",
		"summary",
		"idempotent",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got.Content, sub) {
			t.Errorf("FinalHint missing %q\n--- content ---\n%s", sub, got.Content)
		}
	}
}

// TestGoalPhaseFinalHint_PlacementCapabilityTooling ensures the hint
// lands in the Capability / Tooling slot.
func TestGoalPhaseFinalHint_PlacementCapabilityTooling(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseFinal)}
	got := goalPhaseFinalHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Final")
	}
	if got.Layer != PromptLayerCapability {
		t.Errorf("Layer=%q want=%q", got.Layer, PromptLayerCapability)
	}
	if got.Slot != PromptSlotTooling {
		t.Errorf("Slot=%q want=%q", got.Slot, PromptSlotTooling)
	}
	if got.Source.ID != PromptSourceGoalPhaseFinalHint {
		t.Errorf("Source.ID=%q want=%q", got.Source.ID, PromptSourceGoalPhaseFinalHint)
	}
}

// TestGoalPhaseHint_AllThreeAreLayerSeparated verifies that all 3
// phase hints (Set/Checkpoint/Final) return different Source.IDs so
// the prompt-build can compose all 3 simultaneously if a phase
// transition happens mid-turn (Phase 12.16.1 cache-bypass lesson).
func TestGoalPhaseHint_AllThreeAreLayerSeparated(t *testing.T) {
	set := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	cph := goalPhaseCheckpointHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint)})
	fin := goalPhaseFinalHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseFinal)})
	if set == nil || cph == nil || fin == nil {
		t.Fatal("expected all 3 hints to fire in their own phase")
	}
	ids := map[PromptSourceID]bool{
		set.Source.ID:  true,
		cph.Source.ID:  true,
		fin.Source.ID:  true,
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 distinct Source.IDs, got %v", ids)
	}
}

// Phase 12.29 — TestGoalPhaseCheckpointHint_DoesNotSuggestToolDiscovery
// Regression-proof: lock that the Checkpoint hint does NOT mention tool
// discovery as available — this combined with the new phase-aware tool
// discovery rule (formatToolDiscoveryRule) prevents LLM from reasoning
// "tool_search_tool_bm25 is hidden, let me search".
func TestGoalPhaseCheckpointHint_DoesNotSuggestToolDiscovery(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint), Iteration: 25}
	got := goalPhaseCheckpointHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	forbidden := []string{
		"tool_search_tool_bm25",
		"tool_search_tool_regex",
		"you MUST search",
		"search using",
	}
	for _, f := range forbidden {
		if strings.Contains(got.Content, f) {
			t.Errorf("Checkpoint hint should NOT mention %q as available, got: %q", f, got.Content)
		}
	}
}

// Phase 12.29 — TestGoalPhaseFinalHint_DoesNotSuggestToolDiscovery
// Same regression-proof for Final phase.
func TestGoalPhaseFinalHint_DoesNotSuggestToolDiscovery(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseFinal), Iteration: 30}
	got := goalPhaseFinalHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Final")
	}
	forbidden := []string{
		"tool_search_tool_bm25",
		"tool_search_tool_regex",
		"you MUST search",
		"search using",
	}
	for _, f := range forbidden {
		if strings.Contains(got.Content, f) {
			t.Errorf("Final hint should NOT mention %q as available, got: %q", f, got.Content)
		}
	}
}

// Phase 12.34 Task 2 — GoalSnapshot prepending + multi-turn guidance
// regression-proof tests. These lock the contract that:
//  1. GoalSnapshot is prepended to the hint body when present
//  2. Hint still works (no crash, no missing fields) when no snapshot
//  3. Iteration placeholder renders the actual iter value (replaces
//     zero-clamp with explicit value)
//  4. Multi-turn guidance phase is included in the hint text
//  5. Snapshot-only emits snapshot content (does not drop template)

// TestGoalPhaseCheckpointHint_PrependsGoalSnapshot verifies that when
// req.GoalSnapshot is non-empty, the snapshot is prepended to the hint
// content so the LLM sees goal context before the decision tree.
func TestGoalPhaseCheckpointHint_PrependsGoalSnapshot(t *testing.T) {
	snapshot := "Goal: upgrade uv\nObjective: upgrade uv and verify tests pass"
	req := PromptBuildRequest{
		GoalPhase:    string(GoalPhaseCheckpoint),
		Iteration:    25,
		GoalSnapshot: snapshot,
	}
	got := goalPhaseCheckpointHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	if !strings.Contains(got.Content, snapshot) {
		t.Errorf("expected hint content to contain GoalSnapshot %q; got:\n%s", snapshot, got.Content)
	}
	// Snapshot MUST come before the decision tree block (so LLM sees goal
	// context first, then decision tree).
	snapIdx := strings.Index(got.Content, snapshot)
	dtIdx := strings.Index(got.Content, "Decision tree")
	if snapIdx < 0 || dtIdx < 0 {
		t.Fatalf("expected both snapshot and Decision tree in content; got:\n%s", got.Content)
	}
	if snapIdx > dtIdx {
		t.Errorf("GoalSnapshot should be PREPENDED (before Decision tree); snap_idx=%d decision_idx=%d",
			snapIdx, dtIdx)
	}
}

// TestGoalPhaseCheckpointHint_NoSnapshotBackwardCompat verifies that the
// hint still works when GoalSnapshot is empty (no active goal, or non-turn
// caller). The text should still contain all required fields.
func TestGoalPhaseCheckpointHint_NoSnapshotBackwardCompat(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint), Iteration: 25}
	got := goalPhaseCheckpointHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	if strings.Contains(got.Content, "GoalSnapshot") {
		t.Errorf("content should not mention internal field name when snapshot empty; got:\n%s", got.Content)
	}
	mustContain := []string{
		"goal_progress",
		"complete_goal",
		"remaining_steps",
		"summary",
		"iter 25",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got.Content, sub) {
			t.Errorf("CheckpointHint missing %q\n--- content ---\n%s", sub, got.Content)
		}
	}
}

// TestGoalPhaseCheckpointHint_IterationPlaceholderRendered locks the
// per-iter render — must replace the literal "iter %d" with the actual
// iter value. Per Finding #8, NO `iter <= 0` clamp: iter=0 must render as
// "iter 0" (real signal, not silent fixup).
func TestGoalPhaseCheckpointHint_IterationPlaceholderRendered(t *testing.T) {
	tests := []struct {
		name     string
		iter     int
		wantSub  string
	}{
		{"iter_5", 5, "iter 5"},
		{"iter_25", 25, "iter 25"},
		{"iter_1", 1, "iter 1"},
		// Edge case: iter=0 is a real signal, not masked (Phase 12.34 Finding #8).
		{"iter_0_unchanged", 0, "iter 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint), Iteration: tt.iter}
			got := goalPhaseCheckpointHintContributor(req)
			if got == nil {
				t.Fatal("expected non-nil hint at Checkpoint")
			}
			if !strings.Contains(got.Content, tt.wantSub) {
				t.Errorf("expected %q in content; got:\n%s", tt.wantSub, got.Content)
			}
		})
	}
}

// TestGoalPhaseCheckpointHint_IncludesMultiTurnGuidance is the
// regression-proof for Phase 12.34's "Multi-turn goal guidance" sub-section.
// The 4 hardcoded phrases are the LLM-visible contract that prevents the
// reasoning bug where LLM picks (c) "wait for next turn" via complete_goal
// for a multi-turn goal. Each phrase addresses a specific failure mode.
func TestGoalPhaseCheckpointHint_IncludesMultiTurnGuidance(t *testing.T) {
	req := PromptBuildRequest{GoalPhase: string(GoalPhaseCheckpoint), Iteration: 25}
	got := goalPhaseCheckpointHintContributor(req)
	if got == nil {
		t.Fatal("expected non-nil hint at Checkpoint")
	}
	// Phrases that lock the multi-turn guidance. Each is a distinct
	// failure-mode inoculation. Renaming any of these requires updating
	// this test deliberately.
	multiTurnPhrases := []string{
		// Phrase 1: explicitly forbid complete_goal as a pause mechanism.
		"Do NOT use complete_goal as a pause mechanism",
		// Phrase 2: "wait for next turn" alone is not a blocker.
		"complete_goal with a summary like \"Waiting for next turn",
		// Phrase 3: case (c) is reserved for external signals only.
		"only appropriate when you need an external signal",
		// Phrase 4: multi-turn goal must use goal_progress, not complete_goal.
		"Multi-turn goal",
	}
	for _, sub := range multiTurnPhrases {
		if !strings.Contains(got.Content, sub) {
			t.Errorf("CheckpointHint missing multi-turn phrase %q\n--- content ---\n%s", sub, got.Content)
		}
	}
}

// TestGoalPhaseCheckpointHint_DoesNotBreakSetSilent ensures the snapshot
// prepending does not change behavior for non-Checkpoint phases.
func TestGoalPhaseCheckpointHint_DoesNotBreakSetSilent(t *testing.T) {
	for _, phase := range []string{
		string(GoalPhaseOpen),
		string(GoalPhaseSet),
		string(GoalPhaseFinal),
		"",
	} {
		t.Run("phase="+phase, func(t *testing.T) {
			req := PromptBuildRequest{
				GoalPhase:    phase,
				Iteration:    25,
				GoalSnapshot: "should not appear",
			}
			got := goalPhaseCheckpointHintContributor(req)
			if got != nil {
				t.Errorf("expected nil at phase %q; got:\n%s", phase, got.Content)
			}
		})
	}
}

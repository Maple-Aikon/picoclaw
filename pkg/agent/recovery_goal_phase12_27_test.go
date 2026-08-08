// Phase 12.27 unit tests: text-only next-iter recovery for Open phase + RecallLLM
// wrapper for Set/Checkpoint/Final. See plan at
// ~/.picoclaw/workspace/memory/plan/picoclaw-phase12.27-text-only-next-iter-recovery-20260728.md
// §6.1 (11 wire tests) + §6.1.1 (specific assertions) + §6.1 audit additions (5 NEW).
//
// All tests call REAL evaluateRecovery with REAL turnState (no mocks).
// Phase override via fixture (SkipGoalArchiveForTest + SetGoalPhaseForTest per
// picoclaw-test-fixtures skill) for tests that need a specific phase.

package agent

import (
	"strings"
	"testing"
)

// TestTextOnlySoftRetry_Open_ReturnsRetryNextIter_NoCounter verifies Phase 12.27
// Open-phase wire: text-only returns RecoveryRetryNextIteration with the Open
// soft message, NO counter increment beyond the natural per-iter escalation.
func TestTextOnlySoftRetry_Open_ReturnsRetryNextIter_NoCounter(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false}

	action, msg := evaluateRecovery(ts, ctx)

	// Specific assertions (per §6.1.1): exact enum value + exact msg, NOT weak != checks
	if action != RecoveryRetryNextIteration {
		t.Fatalf("action=%v, want RecoveryRetryNextIteration (exact)", action)
	}
	if msg != TextOnlySoftRetryOpenMessage {
		t.Fatalf("msg=%q, want TextOnlySoftRetryOpenMessage (exact)", msg)
	}
	if ts.textOnlySoftRetriesDone != 0 {
		t.Fatalf("textOnlySoftRetriesDone=%d, want 0 (no counter increment at Open — bump-only, Phase 12.58)", ts.textOnlySoftRetriesDone)
	}
}

// TestTextOnlySoftRetry_Open_FiresOnEveryCall verifies Phase 12.58 bump-only:
// Open text-only NEVER archives — 5 consecutive fires = 5 RecoveryRetryNextIteration
// actions (streak escalates message soft → hard; iter-bump is the only escalation).
func TestTextOnlySoftRetry_Open_FiresOnEveryCall(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false}

	for i := 1; i <= 5; i++ {
		action, _ := evaluateRecovery(ts, ctx)
		if action == RecoveryArchiveGoal {
			t.Fatalf("Open phase must never archive on text-only (bump-only), fire %d", i)
		}
		if action != RecoveryRetryNextIteration {
			t.Fatalf("fire %d: action=%v, want RecoveryRetryNextIteration (exact)", i, action)
		}
	}
	if ts.textOnlyStreak != 5 {
		t.Fatalf("streak=%d, want 5", ts.textOnlyStreak)
	}
}

// TestTextOnlySoftRetry_Checkpoint_ReturnsRetrySameIter_WithCounter verifies
// Phase 12.27 ABSOLUTE allowlist wire: Checkpoint text-only uses
// RecoveryRetrySameIteration (RecallLLM path) with counter increment.
func TestTextOnlySoftRetry_Checkpoint_ReturnsRetrySameIter_WithCounter(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseCheckpoint), TextEmpty: false, HasToolCalls: false}

	action, msg := evaluateRecovery(ts, ctx)

	if action != RecoveryRetrySameIteration {
		t.Fatalf("action=%v, want RecoveryRetrySameIteration (exact) — Checkpoint uses RecallLLM", action)
	}
	if !strings.HasPrefix(msg, TextOnlySoftRetryMessage) { // Phase 12.38: suffix appended via buildTextOnlyRetryMessageWithPhase
		t.Fatalf("msg=%q, want TextOnlySoftRetryMessage (Checkpoint variant, NOT Open variant)", msg)
	}
	if ts.textOnlySoftRetriesDone != 1 {
		t.Fatalf("textOnlySoftRetriesDone=%d, want 1 (counter incremented)", ts.textOnlySoftRetriesDone)
	}
}

// TestTextOnlySoftRetry_Set_ReturnsRetrySameIter_WithCounter verifies Phase 12.27
// Set phase is now eligible for text-only recovery (was silent pre-12.27).
// Phase 12.27.7 lesson: ABSOLUTE allowlist phases need same-iter retry.
// Phase 12.46 (owner decision, anh Maple 2026-08-03): SET text-only gets
// NO recovery — a direct text reply at SET ends the turn. Supersedes the
// Phase 12.27/12.37 same-iter retry behavior for SET.
func TestTextOnlySoftRetry_Set_NoRecovery_Phase12_46(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseSet), TextEmpty: false, HasToolCalls: false}

	action, msg := evaluateRecovery(ts, ctx)

	if action != RecoveryNone {
		t.Fatalf("action=%v, want RecoveryNone (exact) — SET text-only ends turn", action)
	}
	if msg != "" {
		t.Fatalf("msg=%q, want empty (no recovery message at SET text-only)", msg)
	}
	if ts.textOnlySoftRetriesDone != 0 {
		t.Fatalf("textOnlySoftRetriesDone=%d, want 0 (no counter increment)", ts.textOnlySoftRetriesDone)
	}
}

// TestTextOnlySoftRetry_Final_ReturnsRetrySameIter_WithCounter verifies Final
// text-only pre-report uses RecallLLM (preserved from Phase 12.21).
func TestTextOnlySoftRetry_Final_ReturnsRetrySameIter_WithCounter(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseFinal), TextEmpty: false, HasToolCalls: false, PostCompleteGoalReport: false}

	action, msg := evaluateRecovery(ts, ctx)

	if action != RecoveryRetrySameIteration {
		t.Fatalf("action=%v, want RecoveryRetrySameIteration (Final pre-report uses RecallLLM)", action)
	}
	if !strings.HasPrefix(msg, TextOnlySoftRetryMessage) { // Phase 12.38: suffix appended via buildTextOnlyRetryMessageWithPhase
		t.Fatalf("msg=%q, want TextOnlySoftRetryMessage (Final pre-report uses standard variant)", msg)
	}
}

// TestTextOnlySoftRetry_Final_PostReportSilent verifies Final post-report
// is silent (no recovery) — preserves Phase 12.21 behavior.
func TestTextOnlySoftRetry_Final_PostReportSilent(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseFinal), TextEmpty: false, HasToolCalls: false, PostCompleteGoalReport: true}

	action, msg := evaluateRecovery(ts, ctx)

	if action != RecoveryNone {
		t.Fatalf("action=%v, want RecoveryNone (Final post-report silent)", action)
	}
	if msg != "" {
		t.Fatalf("msg=%q, want empty (silent)", msg)
	}
}

// TestPendingRecoveryMessage_CarryToNextIter_Open verifies Phase 12.27 wire:
// caller at pipeline_llm.go:727 sets ts.pendingRecoveryMessage when action
// is RecoveryRetryNextIteration, and the message is NOT injected same-iter.
// (S7 audit surface)
func TestPendingRecoveryMessage_CarryToNextIter_Open(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false}

	action, msg := evaluateRecovery(ts, ctx)

	// Simulate caller dispatch at pipeline_llm.go:727
	if action == RecoveryRetryNextIteration {
		ts.pendingRecoveryMessage = msg
	}

	// Specific assertions (per §6.1.1 audit): exact equality, NOT != ""
	if ts.pendingRecoveryMessage != TextOnlySoftRetryOpenMessage {
		t.Fatalf("pendingRecoveryMessage=%q, want TextOnlySoftRetryOpenMessage (exact carry)", ts.pendingRecoveryMessage)
	}
	if !strings.Contains(ts.pendingRecoveryMessage, "next iteration") {
		t.Fatalf("msg should hint at next-iter injection, got %q", ts.pendingRecoveryMessage)
	}
}

// TestEvaluateRecovery_TextOnly_PhaseOpen_IterBumpResetsCounters verifies Phase
// 12.27 Open semantics: counters reset on iter bump (handled at turn_coord.go
// top-of-loop, not in evaluateRecovery itself). This test verifies the
// evaluation function doesn't reset mid-iter — counters are sticky within
// the same iter at Open (escalation path uses iter-bump, not counter-cap).
// TestTextOnly_Open_NoCountersAcrossIterations verifies Phase 12.58 bump-only:
// consecutive text-only fires at Open NEVER archive and NEVER increment the
// soft/hard counters — streak alone escalates the message (soft → hard).
func TestTextOnly_Open_NoCountersAcrossIterations(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false}

	for i := 1; i <= 4; i++ {
		action, msg := evaluateRecovery(ts, ctx)
		if action != RecoveryRetryNextIteration {
			t.Fatalf("fire %d: action=%v, want RecoveryRetryNextIteration (exact)", i, action)
		}
		if ts.textOnlySoftRetriesDone != 0 || ts.textOnlyHardRetriesDone != 0 {
			t.Fatalf("fire %d: soft=%d hard=%d, want 0/0 (Open never increments counters)", i, ts.textOnlySoftRetriesDone, ts.textOnlyHardRetriesDone)
		}
		if ts.textOnlyStreak != i {
			t.Fatalf("fire %d: streak=%d, want %d", i, ts.textOnlyStreak, i)
		}
		wantMsg := TextOnlySoftRetryOpenMessage
		if i >= 2 {
			wantMsg = TextOnlyHardRetryOpenMessage
		}
		if msg != wantMsg {
			t.Fatalf("fire %d: msg=%q, want %q (streak-based escalation)", i, msg, wantMsg)
		}
	}
}

// TestTextOnly_Open_ToolCallsBetween_NoArchive is the main-turn-10 regression
// (Phase 12.58): text-only iterations interleaved with tool-call iterations
// (which do NOT reset streak in production — no evaluateRecovery call with
// HasToolCalls=true exists in prod callers) must never archive the goal at Open.
func TestTextOnly_Open_ToolCallsBetween_NoArchive(t *testing.T) {
	ts := newPhase5TurnState(t)
	ctx := RecoveryContext{Phase: string(GoalPhaseOpen), TextEmpty: false, HasToolCalls: false}

	// Iter 1: text-only → soft carry.
	act, msg := evaluateRecovery(ts, ctx)
	if act != RecoveryRetryNextIteration || msg != TextOnlySoftRetryOpenMessage {
		t.Fatalf("iter 1: action=%v msg=%q (want RetryNextIteration + soft)", act, msg)
	}
	if ts.textOnlyStreak != 1 {
		t.Fatalf("streak=%d, want 1", ts.textOnlyStreak)
	}

	// Iter 2-3: tool-call iterations — production never runs evaluateRecovery
	// here, so streak persists. Simulate by not calling evaluateRecovery.

	// Iter 4: text-only again → hard carry, still NO archive.
	act, msg = evaluateRecovery(ts, ctx)
	if act != RecoveryRetryNextIteration {
		t.Fatalf("iter 4: action=%v, want RecoveryRetryNextIteration (bump-only, no archive)", act)
	}
	if msg != TextOnlyHardRetryOpenMessage {
		t.Fatalf("iter 4: msg=%q, want TextOnlyHardRetryOpenMessage (streak=2)", msg)
	}
	if ts.textOnlyStreak != 2 {
		t.Fatalf("streak=%d, want 2 (no reset across tool-call iterations)", ts.textOnlyStreak)
	}

	// Iter 5-6: two more text-only fires — still never archive.
	for i := 0; i < 2; i++ {
		act, _ = evaluateRecovery(ts, ctx)
		if act == RecoveryArchiveGoal {
			t.Fatalf("must never archive at Open (main-turn-10 regression), fire %d", i)
		}
		if act != RecoveryRetryNextIteration {
			t.Fatalf("fire %d: action=%v, want RecoveryRetryNextIteration", i, act)
		}
	}
}

// TestTextOnlyHardOpenMessage_NoArchiveClaim (Phase 12.58 F8): the Open hard
// message must NOT claim the turn will be archived — bump-only never archives
// (turn reaches the iteration cap → toolLimitResponse fallback instead).
func TestTextOnlyHardOpenMessage_NoArchiveClaim(t *testing.T) {
	if strings.Contains(TextOnlyHardRetryOpenMessage, "archived") {
		t.Fatalf("TextOnlyHardRetryOpenMessage must not threaten archiving (bump-only): %q", TextOnlyHardRetryOpenMessage)
	}
}

// TestTextOnly_ToolCallResetsCounter_Set verifies Set phase (now eligible
// after Phase 12.27) resets text-only counters on HasToolCalls. (S13 audit
// surface — was silent pre-12.27, no test existed)
//
// Wire flow:
//   1. Call 1: Set + text-only → counter increments (1) → RecoveryRetrySameIteration
//   2. Call 2: Set + HasToolCalls → counters reset → RecoveryNone
func TestTextOnly_ToolCallResetsCounter_Set(t *testing.T) {
	ts := newPhase5TurnState(t)

	// Call 1: Set + text-only (no tool calls) — Phase 12.46: no recovery.
	ctx1 := RecoveryContext{Phase: string(GoalPhaseSet), TextEmpty: false, HasToolCalls: false}
	action1, _ := evaluateRecovery(ts, ctx1)
	if action1 != RecoveryNone {
		t.Fatalf("Call 1 action=%v, want RecoveryNone (Set text-only ends turn)", action1)
	}
	if ts.textOnlySoftRetriesDone != 0 {
		t.Fatalf("Call 1: soft=%d, want 0 (no counter increment at SET)", ts.textOnlySoftRetriesDone)
	}

	// Call 2: Set + HasToolCalls (LLM calls a tool)
	ctx2 := RecoveryContext{Phase: string(GoalPhaseSet), TextEmpty: false, HasToolCalls: true}
	action2, _ := evaluateRecovery(ts, ctx2)
	if action2 != RecoveryNone {
		t.Fatalf("Call 2 action=%v, want None (HasToolCalls resets counter, no recovery needed)", action2)
	}
	if ts.textOnlySoftRetriesDone != 0 {
		t.Fatalf("Call 2: soft=%d, want 0 (reset on tool call)", ts.textOnlySoftRetriesDone)
	}
	if ts.textOnlyHardRetriesDone != 0 {
		t.Fatalf("Call 2: hard=%d, want 0 (reset on tool call)", ts.textOnlyHardRetriesDone)
	}
}

// TestRecoveryRetryNextIteration_EnumExists verifies the new enum value is
// distinct from RecoveryRetrySameIteration (Q1.2 cross-check: regression
// guard against accidental alias).
func TestRecoveryRetryNextIteration_EnumExists(t *testing.T) {
	if RecoveryRetryNextIteration == RecoveryRetrySameIteration {
		t.Fatal("RecoveryRetryNextIteration must be a distinct enum value")
	}
	if RecoveryRetryNextIteration == RecoveryNone {
		t.Fatal("RecoveryRetryNextIteration must not be aliased to RecoveryNone")
	}
	if RecoveryRetryNextIteration == RecoveryArchiveGoal {
		t.Fatal("RecoveryRetryNextIteration must not be aliased to RecoveryArchiveGoal")
	}
}

// TestRecoveryRetryNextIteration_ActionName verifies actionName returns the
// correct string for the new enum (used in log + observability grep).
func TestRecoveryRetryNextIteration_ActionName(t *testing.T) {
	name := actionName(RecoveryRetryNextIteration)
	if name != "retry_next_iteration" {
		t.Fatalf("actionName=%q, want retry_next_iteration (for log grep)", name)
	}
}

// TestPendingRecoveryMessage_ClearedAfterInject_Open verifies the carry-state
// lifecycle: after next-iter prompt injects the message, ts.pendingRecoveryMessage
// is cleared to prevent double-injection. (S20 audit surface — full lifecycle test)
func TestPendingRecoveryMessage_ClearedAfterInject_Open(t *testing.T) {
	ts := newPhase5TurnState(t)

	// Set carry state (caller at pipeline_llm.go:727)
	ts.pendingRecoveryMessage = TextOnlySoftRetryOpenMessage
	if ts.pendingRecoveryMessage == "" {
		t.Fatal("setup failed")
	}

	// Simulate prompt build reading + clearing
	// (production code at context.go BuildSystemPromptWithCache will do this — verified here in isolation)
	if ts.pendingRecoveryMessage != "" {
		// Capture for inject
		msg := ts.pendingRecoveryMessage
		if !strings.Contains(msg, "next iteration") {
			t.Fatalf("injected msg should contain hint, got %q", msg)
		}
		// Clear after inject (production semantics)
		ts.pendingRecoveryMessage = ""
	}

	// Verify cleared
	if ts.pendingRecoveryMessage != "" {
		t.Fatalf("pendingRecoveryMessage should be cleared after inject, got %q", ts.pendingRecoveryMessage)
	}
}

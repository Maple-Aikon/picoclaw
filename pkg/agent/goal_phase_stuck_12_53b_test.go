package agent

// Phase 12.53b (B/D/F/G) test file — items:
//   T5  (D)  branch 1.5 iter==1 guard — characterization, NOT RED
//   T6  (G)  0-lifecycle-tools no-panic sweep
//   T7  (B)  StuckBucket.counterTarget pointer identity
//   T7c (F4) finalizePhaseStuckArchive Open → no stuck_attempt_count on disk
//   T9a (D-F1) stuckArchiveParams unit (phase consistency reason+count)
//   T9b (D-F1) static wire check — 2 handleGoalRecovery sites use
//              finalizeGoalOnTurnEndWithCount
//
// Base commit: 3c7cea82 (12.53a SHIPPED).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// T5 (Item D) — branch 1.5 must NOT fire at iter==1 cap==1: ResolveGoalPhase
// pins iter<=1 → GoalPhaseSet, Set falls through to toolLimitResponse.
// Characterization test: written BEFORE the guard, passes before AND after
// (guard makes the already-correct behavior explicit).
func TestApplyFallbackForEmptyResponse_Iter1Cap1_FallsThroughToToolLimit(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-53b-t5-iter1-cap1"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name:        "t5",
		Description: goal.Description{Objective: "iter1 cap1", SuccessCriteria: []string{"done"}},
		Status:      goal.StatusActive,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-53b-t5",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	ts.iteration = 1
	ts.iterationCap = 1

	got := al.applyFallbackForEmptyResponse(ts)
	if got != toolLimitResponse {
		t.Fatalf("got = %q, want toolLimitResponse (Set fall-through)", got)
	}
	if got == ToolLimitCheckpointRetryMessage || got == ToolLimitFinalRetryMessage {
		t.Fatalf("branch 1.5 fired at iter==1: got %q — phase must be Set, not Checkpoint/Final", got)
	}
}

// T6 (Item G) — agent with ZERO lifecycle tools registered must not panic
// anywhere in the policy/registry layer. Fails open (allowlist nil) for
// unknown tools, and execution returns an ErrorResult, not a panic.
func TestZeroLifecycleTools_NoPanic(t *testing.T) {
	// (a) PhasePolicyFor is a pure map lookup — nil-safe for every token
	// plus the empty phase (fail-open legacy).
	for _, phase := range []GoalPhase{GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal, GoalPhasePostFinal} {
		if p := PhasePolicyFor(phase); p == nil {
			t.Fatalf("PhasePolicyFor(%q) = nil, want policy row", string(phase))
		}
	}
	if p := PhasePolicyFor(""); p != nil {
		t.Fatalf("PhasePolicyFor(\"\") = %v, want nil (fail-open legacy)", p)
	}

	// (b) registry with an unregistered tool name → ErrorResult, no panic.
	const missing = "nonexistent_tool_12_53b"
	r := tools.NewToolRegistry()
	if !r.IsAllowed(missing) {
		t.Error("fail-open legacy: allowlist nil → IsAllowed must be true")
	}
	res := r.ExecuteWithContext(context.Background(), missing, nil, "", "", nil)
	if res == nil {
		t.Fatal("nil result — ExecuteWithContext must return *ToolResult, not panic")
	}
	if !res.IsError {
		t.Errorf("missing tool: expected IsError=true, got ForLLM=%q", res.ForLLM)
	}
	if res.Err == nil {
		t.Error("missing tool: expected Err to be set")
	}
	if !strings.Contains(res.ForLLM, "not found") {
		t.Errorf("missing tool: expected 'not found' in ForLLM, got %q", res.ForLLM)
	}
}

// T7 (Item B) — StuckBucket.counterTarget returns a POINTER to the bucket's
// phase counter (pointer identity, not value) and nil for StuckNone.
func TestStuckBucketCounterTarget_PointerIdentity(t *testing.T) {
	ts := &turnState{}

	if got := StuckSet.counterTarget(ts); got != &ts.setGoalAttemptCount {
		t.Fatalf("StuckSet.counterTarget = %p, want %p (setGoalAttemptCount)", got, &ts.setGoalAttemptCount)
	}
	if got := StuckCheckpoint.counterTarget(ts); got != &ts.goalProgressAttemptCount {
		t.Fatalf("StuckCheckpoint.counterTarget = %p, want %p (goalProgressAttemptCount)", got, &ts.goalProgressAttemptCount)
	}
	if got := StuckFinal.counterTarget(ts); got != &ts.completeGoalAttemptCount {
		t.Fatalf("StuckFinal.counterTarget = %p, want %p (completeGoalAttemptCount)", got, &ts.completeGoalAttemptCount)
	}
	if got := StuckNone.counterTarget(ts); got != nil {
		t.Fatalf("StuckNone.counterTarget = %p, want nil", got)
	}

	// Mutation through the pointer must reach the real field.
	*StuckCheckpoint.counterTarget(ts) = 2
	if ts.goalProgressAttemptCount != 2 {
		t.Errorf("mutation via counterTarget: goalProgressAttemptCount = %d, want 2", ts.goalProgressAttemptCount)
	}
}

// T7c (F4) — finalizePhaseStuckArchive at a phase WITHOUT a stuck bucket
// (Open/PostFinal) must NOT stamp stuck_attempt_count onto the goal file.
// Behavior change: pre-12.53b the stamp switch defaulted to 1.
func TestFinalizePhaseStuckArchive_Open_DoesNotStampStuckAttemptCount(t *testing.T) {
	_, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	ws := t.TempDir()
	agent.Workspace = ws

	const sessionKey = "test-53b-t7c-open"
	st := goal.NewStore(ws)
	now := time.Now().UTC()
	g := &goal.Goal{
		Name:        "t7c",
		Description: goal.Description{Objective: "open archive", SuccessCriteria: []string{"done"}},
		Status:      goal.StatusActive,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-53b-t7c",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey

	finalizePhaseStuckArchive(ts, GoalPhaseOpen, "BoundedRetry exhausted at phase open")
	if ts.goalArchiveRequested {
		// finalizePhaseStuckArchive does not set the requested flag; the
		// caller does. Nothing to assert here — keep for symmetry.
	}

	onDisk, err := st.Read(sessionKey)
	if err != nil {
		t.Fatalf("read archived goal: %v", err)
	}
	if onDisk.Status != goal.StatusAborted {
		t.Fatalf("status = %s, want aborted", onDisk.Status)
	}
	if onDisk.AbortReason != GoalAbortReasonBexhausted+":tool_exec" {
		t.Fatalf("abortReason = %q, want %q (Open is not phase-stuck)", onDisk.AbortReason, GoalAbortReasonBexhausted+":tool_exec")
	}
	if onDisk.StuckAttemptCount != 0 {
		t.Fatalf("StuckAttemptCount = %d, want 0 (no stuck bucket at Open)", onDisk.StuckAttemptCount)
	}

	raw, err := os.ReadFile(filepath.Join(ws, "memory", "goal", sessionKey+".md"))
	if err != nil {
		t.Fatalf("read raw goal file: %v", err)
	}
	if strings.Contains(string(raw), "stuck_attempt_count") {
		t.Errorf("raw goal file must NOT contain stuck_attempt_count (F4: omitempty absent), got:\n%s", string(raw))
	}
}

// T9a (D-F1) — stuckArchiveParams resolves reason + count from ONE phase
// snapshot: Checkpoint with counter=2 → CheckpointStuckAbortReason + count 2;
// Open → default reason + count 0 (no field stamped).
func TestStuckArchiveParams_PhaseConsistency(t *testing.T) {
	_, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	const sessionKey = "test-53b-t9a-params"
	st := goal.NewStore(agent.Workspace)
	g := &goal.Goal{
		Name:        "t9a",
		Description: goal.Description{Objective: "params", SuccessCriteria: []string{"done"}},
		Status:      goal.StatusActive,
	}
	if err := st.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}

	ts := newTurnState(agent, makeTestProcessOpts(sessionKey), turnEventScope{
		turnID:  "turn-53b-t9a",
		context: newTurnContext(nil, nil, nil),
	})
	ts.sessionKey = sessionKey
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false

	defaultReason := GoalAbortReasonBexhausted + ":goal_recovery"

	t.Run("checkpoint counter=2", func(t *testing.T) {
		ts.iteration = ts.iterationCap // iter==cap → Checkpoint (below maxCap)
		ts.goalProgressAttemptCount = 2
		ts.goalProgressArchiveFlag = false
		reason, count := ts.stuckArchiveParams(defaultReason)
		if reason != GoalPhaseCheckpointStuckAbortReason {
			t.Fatalf("reason = %q, want %q", reason, GoalPhaseCheckpointStuckAbortReason)
		}
		if count != 2 {
			t.Fatalf("count = %d, want 2 (persist real stuck count)", count)
		}
	})

	t.Run("open no bucket", func(t *testing.T) {
		ts.iteration = 2
		ts.iterationCap = 5
		ts.setGoalAttemptCount = 0
		ts.goalProgressAttemptCount = 0
		ts.completeGoalAttemptCount = 0
		reason, count := ts.stuckArchiveParams(defaultReason)
		if reason != defaultReason {
			t.Fatalf("reason = %q, want default %q (Open not phase-stuck)", reason, defaultReason)
		}
		if count != 0 {
			t.Fatalf("count = %d, want 0 (no bucket → no stamp)", count)
		}
	})
}

// T9b (D-F1) — static wire check (Phase 12.36 canary pattern): both
// handleGoalRecovery archive sites must call finalizeGoalOnTurnEndWithCount,
// and no bare finalizeGoalOnTurnEnd may remain inside handleGoalRecovery.
func TestHandleGoalRecovery_WiresFinalizeWithCount(t *testing.T) {
	src, err := os.ReadFile("pipeline_llm.go")
	if err != nil {
		t.Fatalf("read pipeline_llm.go: %v", err)
	}
	body := string(src)

	start := strings.Index(body, "func (p *Pipeline) handleGoalRecovery(")
	if start < 0 {
		t.Fatal("handleGoalRecovery not found in pipeline_llm.go")
	}
	end := strings.Index(body[start:], "// actionName returns")
	if end < 0 {
		t.Fatal("end marker of handleGoalRecovery not found")
	}
	block := body[start : start+end]

	if got := strings.Count(block, "ts.finalizeGoalOnTurnEndWithCount(abortReason, stuckCount)"); got != 2 {
		t.Fatalf("finalizeGoalOnTurnEndWithCount calls in handleGoalRecovery = %d, want 2 (OnExhausted + RecoveryArchiveGoal)", got)
	}
	if strings.Contains(block, "ts.finalizeGoalOnTurnEnd(") {
		t.Error("bare finalizeGoalOnTurnEnd( still present in handleGoalRecovery — D-F1 not wired")
	}
}

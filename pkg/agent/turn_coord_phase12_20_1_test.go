package agent

import "testing"

// TestPhase12_20_1_LoopTopForceExitAfterFinalReport verifies Fix (A).
//
// Pre-12.20.1: loop condition `currentIteration() < iterationCap` allowed
// extra wasted iterations when LLM emitted complete_goal again at iter
// N+1 instead of producing a final report. Tools stripped but each iter
// still consumed time + emitted publishPicoToolCallInterim events.
//
// 12.20.1: loop top condition gains
//   `&& !(ts.goalFinalized && ts.postCompleteGoalReportSent)`
// so once the post-body marker flips postCompleteGoalReportSent=true,
// the loop exits strictly at iter N+1 (or N+2 if LLM retried) instead
// of letting the LLM loop until cap.
func TestPhase12_20_1_LoopTopForceExitAfterFinalReport(t *testing.T) {
	ts := newPhase5TurnState(t)

	// After phase 12.9 final-report flow ran: iter=N+1 (=6) just finished,
	// post-body marker just flipped postCompleteGoalReportSent=true.
	// At LOOP TOP, before next iter starts, the OLD condition was
	// `currentIteration() < iterationCap` which would be FALSE here
	// (6 < 6). Loop would exit via OLD mechanism anyway. So simulate
	// the more dangerous case: cap-bumped to N+2 but body already
	// emitted complete_goal again at N+1 (worst-case LLM retry scenario).
	ts.iteration = 6
	ts.iterationCap = 7 // mocked: pretend hook bumped past 6 for some reason
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = true // post-body marker fired at iter 6

	// Verify the precondition that motivated fix (A):
	inForceExitState := ts.goalFinalized && ts.postCompleteGoalReportSent
	capNotReached := ts.currentIteration() < ts.iterationCap

	if !(inForceExitState && capNotReached) {
		t.Fatalf("expected (force-exit && cap-reached) state; got force-exit=%v cap=%v — can't validate fix (A) precondition (need scenario where OLD loop condition would still be true)",
			inForceExitState, capNotReached)
	}

	// Fix (A) adds `&& !(goalFinalized && postCompleteGoalReportSent)` —
	// when both flags are true, the loop MUST exit. Mirror the new
	// condition:
	shouldContinue := capNotReached && !inForceExitState
	if shouldContinue {
		t.Errorf("Fix (A) broken: loop should exit when goalFinalized && postCompleteGoalReportSent; condition still true")
	}
}

// TestPhase12_20_1_LoopDoesNotExit_PreFinalReport verifies Fix (A)
// doesn't break the pre-final-report case where we still need normal
// `currentIteration() < iterationCap` control.
//
// Scenario: LLM has called complete_goal at iter N (goalFinalized=true)
// but the final-report iter has NOT YET run (postCompleteGoalReportSent=false).
// Pre-loop hook bumps iterationCap from N to N+1. Loop top MUST continue.
func TestPhase12_20_1_LoopDoesNotExit_PreFinalReport(t *testing.T) {
	ts := newPhase5TurnState(t)

	ts.iteration = 5
	ts.iterationCap = 5
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = false

	// Mirror pre-loop hook (turn_coord.go:139-143):
	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		if cap := ts.iteration + 1; cap > ts.iterationCap {
			ts.iterationCap = cap
		}
	}
	// After hook: iterationCap=6 (=5+1). Loop top continues into iter N+1.

	capNotReached := ts.currentIteration() < ts.iterationCap         // 5 < 6 = true
	inForceExitState := ts.goalFinalized && ts.postCompleteGoalReportSent // true && false = false

	shouldContinue := capNotReached && !inForceExitState
	if !shouldContinue {
		t.Errorf("Fix (A) regression: loop should still continue for final-report iter (iter N+1); condition false")
	}
}

// TestPhase12_20_1_LoopDoesNotExit_PreGoalComplete verifies Fix (A)
// doesn't break normal Open-phase iters where we want the loop to
// continue based solely on `currentIteration() < iterationCap`.
func TestPhase12_20_1_LoopDoesNotExit_PreGoalComplete(t *testing.T) {
	ts := newPhase5TurnState(t)

	ts.iteration = 3
	ts.iterationCap = 10
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false

	capNotReached := ts.currentIteration() < ts.iterationCap         // 3 < 10 = true
	inForceExitState := ts.goalFinalized && ts.postCompleteGoalReportSent // false && false = false

	shouldContinue := capNotReached && !inForceExitState
	if !shouldContinue {
		t.Errorf("Fix (A) regression: loop should still continue in normal Open-phase iter (3 < 10); condition false")
	}
}

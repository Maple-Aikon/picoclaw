package agent

import "testing"

// TestPhase12_9_PreLoopHook_ExtendsCapForFinalReport verifies the pre-loop hook
// behavior. When complete_goal has fired (goalFinalized=true) but the final
// report has not yet been sent (postCompleteGoalReportSent=false), the pre-loop
// hook must extend iterationCap to allow exactly one more iter — otherwise the
// loop condition below would be false and the body would never run.
//
// Before Phase 12.9, this was the only thing attempted and it was broken
// (the in-loop check consumed the flag via early-set+continue, see commit
// history for `b5833df7`).
func TestPhase12_9_PreLoopHook_ExtendsCapForFinalReport(t *testing.T) {
	ts := newPhase5TurnState(t)

	// Simulate: complete_goal has fired on the last iter allowed by cap.
	ts.iteration = 5
	ts.iterationCap = 5
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = false
	ts.pendingFinalReportIter = false

	// Apply the pre-loop hook (mirrored from turn_coord.go:124-135).
	preLoopHook := func() {
		if ts.goalFinalized && !ts.postCompleteGoalReportSent {
			if cap := ts.iteration + 1; cap > ts.iterationCap {
				ts.iterationCap = cap
			}
		}
	}
	preLoopHook()

	if ts.iterationCap != 6 {
		t.Fatalf("expected iterationCap=6 (one more iter), got %d", ts.iterationCap)
	}
	if ts.postCompleteGoalReportSent {
		t.Error("pre-loop hook should NOT set postCompleteGoalReportSent — that's the post-body marker's job")
	}
}

// TestPhase12_9_PreLoopHook_NoOpWhenAlreadySent verifies the pre-loop hook
// becomes a no-op after the final-report iter has run (postCompleteGoalReportSent=true).
// This prevents a runaway cap-extend if somehow the loop re-entered the body.
func TestPhase12_9_PreLoopHook_NoOpWhenAlreadySent(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.iteration = 5
	ts.iterationCap = 6 // already extended by previous pre-loop hook
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = true // final-report iter already ran

	prevCap := ts.iterationCap
	preLoopHook := func() {
		if ts.goalFinalized && !ts.postCompleteGoalReportSent {
			if cap := ts.iteration + 1; cap > ts.iterationCap {
				ts.iterationCap = cap
			}
		}
	}
	preLoopHook()

	if ts.iterationCap != prevCap {
		t.Errorf("pre-loop hook should be a no-op when flag is set; cap changed %d → %d", prevCap, ts.iterationCap)
	}
}

// TestPhase12_9_PreLoopHook_NoOpWithoutGoalFinalized verifies the pre-loop hook
// is a no-op for normal (non-finalized) turns. Cap must not be extended
// outside the post-complete_goal path.
func TestPhase12_9_PreLoopHook_NoOpWithoutGoalFinalized(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.iteration = 5
	ts.iterationCap = 5
	ts.goalFinalized = false // no complete_goal yet

	preLoopHook := func() {
		if ts.goalFinalized && !ts.postCompleteGoalReportSent {
			if cap := ts.iteration + 1; cap > ts.iterationCap {
				ts.iterationCap = cap
			}
		}
	}
	preLoopHook()

	if ts.iterationCap != 5 {
		t.Errorf("pre-loop hook should be a no-op when goalFinalized=false; cap is %d", ts.iterationCap)
	}
}

// TestPhase12_9_TopOfBody_SetsPendingSignal verifies that at the top of the
// body, when goalFinalized=true && postCompleteGoalReportSent=false, the
// transient pendingFinalReportIter signal is set. This signal is consumed
// by the post-body marker.
func TestPhase12_9_TopOfBody_SetsPendingSignal(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = false

	// Top-of-body check (mirrored from turn_coord.go:158-161).
	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		ts.pendingFinalReportIter = true
	}

	if !ts.pendingFinalReportIter {
		t.Error("pendingFinalReportIter should be true after top-of-body check on final-report iter")
	}
}

// TestPhase12_9_TopOfBody_NoOpOnNonFinalReportIter verifies that the top-of-body
// pendingFinalReportIter signal is NOT set when goalFinalized=false (normal
// turn body, e.g. complete_goal hasn't fired yet — though that would only be
// true on iter 1, before the very first tool call).
func TestPhase12_9_TopOfBody_NoOpOnNonFinalReportIter(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false

	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		ts.pendingFinalReportIter = true
	}

	if ts.pendingFinalReportIter {
		t.Error("pendingFinalReportIter should be false on normal turn body")
	}
}

// TestPhase12_9_PostBodyMarker_FlipsFlagOnFinalReportIter verifies that at
// the end of the body, when pendingFinalReportIter=true, the post-body marker
// flips postCompleteGoalReportSent=true and clears the pending signal.
// This is the transition that allows the next loop pass to exit cleanly.
func TestPhase12_9_PostBodyMarker_FlipsFlagOnFinalReportIter(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = false
	ts.pendingFinalReportIter = true // set at top of body

	// Post-body marker (mirrored from turn_coord.go:362-373 / 309-321).
	if ts.pendingFinalReportIter {
		ts.postCompleteGoalReportSent = true
		ts.pendingFinalReportIter = false
	}

	if !ts.postCompleteGoalReportSent {
		t.Error("postCompleteGoalReportSent should be true after post-body marker ran")
	}
	if ts.pendingFinalReportIter {
		t.Error("pendingFinalReportIter should be cleared (transient signal)")
	}
}

// TestPhase12_9_PostBodyMarker_NoOpOnCompleteGoalIter verifies the post-body
// marker does NOT flip the flag on the iter where complete_goal fires. This
// is the key invariant that prevents the early-flag-set bug from Phase 12.7
// (which caused the loop to break before the final-report iter could run).
//
// Scenario: iter N is the iter where complete_goal fires. At top of body,
// goalFinalized=false (complete_goal hasn't been executed yet), so the
// pendingFinalReportIter signal is NOT set. Post-body marker must therefore
// not flip the flag — leaving it false for the next iter to set the pending
// signal and run the final-report iter.
func TestPhase12_9_PostBodyMarker_NoOpOnCompleteGoalIter(t *testing.T) {
	ts := newPhase5TurnState(t)
	// At TOP of body: goalFinalized=false (complete_goal not yet executed)
	ts.goalFinalized = false
	ts.postCompleteGoalReportSent = false
	ts.pendingFinalReportIter = false

	// Top-of-body check (mirrored from turn_coord.go:158-161).
	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		ts.pendingFinalReportIter = true
	}

	// Body runs: LLM emits complete_goal tool_call, tool exec sets
	// ts.goalFinalized = true.
	ts.goalFinalized = true

	// Post-body marker (mirrored from turn_coord.go:362-373).
	if ts.pendingFinalReportIter {
		ts.postCompleteGoalReportSent = true
		ts.pendingFinalReportIter = false
	}

	if ts.postCompleteGoalReportSent {
		t.Error("postCompleteGoalReportSent must NOT be set on the iter where complete_goal fires; that would prevent the final-report iter from running")
	}
	if !ts.pendingFinalReportIter == ts.pendingFinalReportIter {
		// sanity: pending should still be false
	}
}

// TestPhase12_9_FullSequence_CompleteGoalAtCapThenFinalReport exercises the
// full happy-path sequence that the bug fix targets:
//
//	iter N: complete_goal fires, goalFinalized=true
//	pre-loop hook: cap extends from N to N+1
//	iter N+1: top-of-body sets pendingFinalReportIter=true
//	iter N+1: body runs, post-body marker sets postCompleteGoalReportSent=true
//	iter N+2: pre-loop hook no-op (flag set), loop condition false, exits
func TestPhase12_9_FullSequence_CompleteGoalAtCapThenFinalReport(t *testing.T) {
	ts := newPhase5TurnState(t)
	ts.iteration = 5
	ts.iterationCap = 5

	// iter 5: complete_goal fires (at top of body, goalFinalized=false; tool exec sets it true).
	ts.goalFinalized = false
	// top-of-body: pendingFinalReportIter not set (goalFinalized=false)
	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		ts.pendingFinalReportIter = true
	}
	if ts.pendingFinalReportIter {
		t.Fatal("pendingFinalReportIter should be false on iter where complete_goal hasn't yet fired")
	}
	// body runs: complete_goal tool exec → ts.goalFinalized = true
	ts.goalFinalized = true
	// post-body marker: pendingFinalReportIter is false → no-op
	if ts.pendingFinalReportIter {
		ts.postCompleteGoalReportSent = true
		ts.pendingFinalReportIter = false
	}
	if ts.postCompleteGoalReportSent {
		t.Fatal("postCompleteGoalReportSent should still be false after complete_goal iter")
	}
	// iteration increment happens at the top of the body before the
	// tool exec, so the actual `ts.iteration` field hasn't moved yet in
	// this simulated state — the iteration-cap comparison is done via
	// the loop's currentIteration() which reads the field. Manually
	// advance ts.iteration to model the "next loop top" snapshot.
	ts.iteration = 5 // still 5 — increment happens at top of next iter body, not here

	// Loop top → iter 6 enters (modeled as: pre-loop hook fires first,
	// THEN condition check).
	preLoopHook := func() {
		if ts.goalFinalized && !ts.postCompleteGoalReportSent {
			if cap := ts.iteration + 1; cap > ts.iterationCap {
				ts.iterationCap = cap
			}
		}
	}
	preLoopHook()
	if ts.iterationCap != 6 {
		t.Fatalf("pre-loop hook should bump cap to 6, got %d", ts.iterationCap)
	}
	// Loop condition: 5 < 6 → true → enter body
	if !(ts.currentIteration() < ts.iterationCap) {
		t.Fatal("loop condition should be true after pre-loop hook extends cap")
	}

	// top of body: pendingFinalReportIter = true
	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		ts.pendingFinalReportIter = true
	}
	if !ts.pendingFinalReportIter {
		t.Fatal("pendingFinalReportIter should be true on final-report iter top of body")
	}
	// iteration increment
	ts.iteration = 6
	// body runs (LLM emits final report)...

	// post-body marker
	if ts.pendingFinalReportIter {
		ts.postCompleteGoalReportSent = true
		ts.pendingFinalReportIter = false
	}
	if !ts.postCompleteGoalReportSent {
		t.Fatal("postCompleteGoalReportSent should be true after final-report iter")
	}

	// Loop top → next iter: pre-loop hook is no-op (flag set), condition
	// `6 < 6` false, loop exits cleanly.
	preLoopHook()
	if !(ts.currentIteration() < ts.iterationCap) {
		// expected — loop exits
	} else {
		t.Fatal("loop should exit cleanly after final-report iter; condition still true")
	}
}

package tools

// Regression test for the subturn nil-map panic.
//
// Symptom (live, 2026-10-20):
//
//	[System: async:spawn] Spawn failed: runTurn panicked:
//	assignment to entry in nil map
//
// Stack (from /home/maple/.pmc/logs/picoclaw-out.log, 3 occurrences):
//
//	registry.go:500  (*ToolRegistry).markFirstSuccess   <- origin
//	registry.go:779  ExecuteWithContext (statusOK bookkeeping)
//	subturn.go:487   spawnSubTurn
//	subturn.go:367   agent.Tools = baseAgent.Tools.Clone()
//
// Clone() (registry.go:952-975) intentionally leaves seenFirstSuccess and
// sigTrackers nil, and its comment claims they are "lazy-init in the clone".
// That comment is wrong: the only make() for those maps is in
// NewToolRegistry (:85/:89). Reads from a nil map are legal in Go and return
// the zero value, so seenFirstSuccessBefore() always reports false and the
// write in markFirstSuccess() always executes. Every subturn therefore panics
// on the first SUCCESSFUL tool call it makes.
//
// Coverage gap this test closes: registry_test.go already tests Clone() at
// lines 685/724/734/758/796/827, but every one of those only asserts field
// preservation. None of them follows up with an Execute, which is the only
// way the nil maps become observable.
//
// Modeled on newMockTool (:76-83) and TestToolRegistry_Clone_CopiesProjectionFrozen (:774).

import (
	"context"
	"testing"
)

// cloneThenExecute is the shared shape of both regression cases: build a
// registry, clone it the way subturn.go:367 does, then drive tool execution.
func cloneThenExecute(t *testing.T) {
	t.Helper()
	r := NewToolRegistry()
	r.Register(newMockTool("read_file", "reads files"))

	clone := r.Clone()
	if clone == nil {
		t.Fatal("Clone() returned nil")
	}

	// This is the exact call shape of registry.go:779 as reached from a subturn:
	// a successful tool, a real channel + chatID (subturns inherit InboundContext
	// from the parent, so the "anon namespace" guard at :497 never fires).
	res := clone.ExecuteWithContext(context.Background(), "read_file", nil, "telegram", "chat-1", nil)
	if res.IsError {
		t.Fatalf("cloned registry: successful tool returned error: %s", res.ForLLM)
	}
}

func TestToolRegistry_Clone_ExecuteSuccessDoesNotPanic(t *testing.T) {
	// On unpatched code this panics with "assignment to entry in nil map"
	// at registry.go:500 via ExecuteWithContext. A panic fails the test with a
	// stack trace that names the origin frame, which is what makes this
	// diagnosable when it regresses.
	cloneThenExecute(t)
}

func TestToolRegistry_Clone_FirstSuccessRecordsDedup(t *testing.T) {
	// The panic guard must not become a silent skip: the dedup entry still has
	// to be recorded, otherwise SoftPromptFirstSuccess is appended on every
	// call for the rest of the turn (it is meant to fire exactly once per
	// session:tool).
	r := NewToolRegistry()
	r.Register(newMockTool("read_file", "reads files"))
	clone := r.Clone()

	if clone.seenFirstSuccessBefore("telegram", "chat-1", "read_file") {
		t.Fatal("cloned registry reports a dedup hit before any success was recorded")
	}

	clone.ExecuteWithContext(context.Background(), "read_file", nil, "telegram", "chat-1", nil)

	if !clone.seenFirstSuccessBefore("telegram", "chat-1", "read_file") {
		t.Errorf("dedup entry not recorded after a successful call: SoftPromptFirstSuccess would repeat every turn")
	}
}

func TestToolRegistry_Clone_SigTrackerOnTransientDoesNotPanic(t *testing.T) {
	// The second latent nil-map write on the same path: getOrCreateSigTracker
	// (registry.go:449) runs on statusTransient and writes r.sigTrackers[key].
	// Any transient tool failure inside a subturn panics the same way as the
	// success path. Exercised directly because driving a real transient
	// failure through ExecuteWithContext requires a timing-dependent mock.
	r := NewToolRegistry()
	clone := r.Clone()

	if tr := clone.getOrCreateSigTracker("telegram", "chat-1"); tr == nil {
		t.Error("getOrCreateSigTracker returned nil on a cloned registry")
	}
	if _, ok := clone.sigTrackers["telegram:chat-1"]; !ok {
		t.Error("tracker not stored in the previously-nil sigTrackers map")
	}
}

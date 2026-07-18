package tools

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// keyWithSig is a small helper for tests to build a SignatureKey with the
// given tool, errKind, and a synthetic ArgSig. ArgSig is left empty in most
// tests (the default behavior per Decision 5 in the plan).
func keyWithSig(tool string, kind ErrorKind, argSig string) SignatureKey {
	return SignatureKey{Tool: tool, ErrKind: kind, ArgSig: argSig}
}

// 1. Below threshold: 1-2 fails return "" (no escalation).
func TestEscalation_BelowThreshold_NoEscalation(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k := keyWithSig("fs.read", ErrInvalidInput, "")

	if got := tr.EscalateIfNeeded(k, "missing arg 'path'", ""); got != "" {
		t.Fatalf("1st fail: want empty, got %q", got)
	}
	if got := tr.EscalateIfNeeded(k, "missing arg 'path'", ""); got != "" {
		t.Fatalf("2nd fail: want empty, got %q", got)
	}
	if got := tr.Count(k); got != 2 {
		t.Fatalf("count: want 2, got %d", got)
	}
}

// 2. At threshold (3rd fail) returns full escalation message.
func TestEscalation_AtThreshold_Escalates(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k := keyWithSig("fs.read", ErrInvalidInput, "")

	tr.EscalateIfNeeded(k, "missing arg 'path'", "")
	tr.EscalateIfNeeded(k, "missing arg 'path'", "")
	got := tr.EscalateIfNeeded(k, "missing arg 'path'", "")
	if got == "" {
		t.Fatal("3rd fail: want escalation message, got empty")
	}
	// Verify content: tool name, count, kind, last error, and the
	// "Stop retrying" anchor phrase from the template.
	if !strings.Contains(got, `"fs.read"`) {
		t.Errorf("escalation missing tool name: %q", got)
	}
	if !strings.Contains(got, "3 times") {
		t.Errorf("escalation missing count: %q", got)
	}
	if !strings.Contains(got, "invalid_input") {
		t.Errorf("escalation missing kind: %q", got)
	}
	if !strings.Contains(got, "missing arg 'path'") {
		t.Errorf("escalation missing last err: %q", got)
	}
	if !strings.Contains(got, "Stop retrying") {
		t.Errorf("escalation missing anchor phrase: %q", got)
	}
}

// 3. Above threshold (4th, 5th fail) stays escalated — no spam, no reset.
func TestEscalation_AboveThreshold_StaysEscalated(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k := keyWithSig("fs.read", ErrInvalidInput, "")

	tr.EscalateIfNeeded(k, "e1", "")
	tr.EscalateIfNeeded(k, "e2", "")
	tr.EscalateIfNeeded(k, "e3", "")
	got4 := tr.EscalateIfNeeded(k, "e4", "")
	if got4 == "" {
		t.Fatal("4th fail: want escalation, got empty")
	}
	got5 := tr.EscalateIfNeeded(k, "e5", "")
	if got5 == "" {
		t.Fatal("5th fail: want escalation, got empty")
	}
	// Count and last error should reflect the latest call.
	if !strings.Contains(got5, "5 times") {
		t.Errorf("5th escalation: want '5 times', got %q", got5)
	}
	if !strings.Contains(got5, "e5") {
		t.Errorf("5th escalation: want 'e5', got %q", got5)
	}
}

// 4. MarkSuccess on a signature resets that signature's counter only.
func TestEscalation_ResetOnSuccess(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k := keyWithSig("fs.read", ErrInvalidInput, "")

	tr.EscalateIfNeeded(k, "e1", "")
	tr.EscalateIfNeeded(k, "e2", "")
	tr.MarkSuccess(k)
	if got := tr.Count(k); got != 0 {
		t.Fatalf("after MarkSuccess: want count=0, got %d", got)
	}
	// Next fail after MarkSuccess starts from 0 again.
	if got := tr.EscalateIfNeeded(k, "e3", ""); got != "" {
		t.Fatalf("1st fail after MarkSuccess: want empty, got %q", got)
	}
}

// 5. Different signatures are independent counters.
func TestEscalation_DifferentSignatures_Independent(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k1 := keyWithSig("fs.read", ErrInvalidInput, "")
	k2 := keyWithSig("fs.read", ErrTimeout, "")
	k3 := keyWithSig("web.fetch", ErrInvalidInput, "")

	tr.EscalateIfNeeded(k1, "e1", "")
	tr.EscalateIfNeeded(k1, "e2", "")
	// k1 is at 2, k2 and k3 should still be at 0.
	if tr.Count(k1) != 2 || tr.Count(k2) != 0 || tr.Count(k3) != 0 {
		t.Fatalf("counts after k1x2: k1=%d k2=%d k3=%d", tr.Count(k1), tr.Count(k2), tr.Count(k3))
	}

	// Push k1 to 3 → escalates. k2 and k3 remain independent.
	if got := tr.EscalateIfNeeded(k1, "e3", ""); got == "" {
		t.Fatal("k1 should escalate at 3rd fail")
	}
	if got := tr.EscalateIfNeeded(k2, "timeout1", ""); got != "" {
		t.Fatalf("k2 1st fail: want empty, got %q", got)
	}
	if got := tr.EscalateIfNeeded(k3, "missing arg", ""); got != "" {
		t.Fatalf("k3 1st fail: want empty, got %q", got)
	}

	// MarkSuccess on k1 must NOT touch k2 or k3.
	tr.MarkSuccess(k1)
	if tr.Count(k1) != 0 {
		t.Errorf("k1 after MarkSuccess: want 0, got %d", tr.Count(k1))
	}
	if tr.Count(k2) != 1 {
		t.Errorf("k2 after k1.MarkSuccess: want 1, got %d", tr.Count(k2))
	}
	if tr.Count(k3) != 1 {
		t.Errorf("k3 after k1.MarkSuccess: want 1, got %d", tr.Count(k3))
	}
}

// 6. Thread-safety under concurrent calls (run with `go test -race`).
func TestEscalation_ThreadSafe(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k := keyWithSig("fs.read", ErrInvalidInput, "")

	const goroutines = 50
	const callsEach = 20

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < callsEach; i++ {
				tr.EscalateIfNeeded(k, "concurrent", "")
			}
		}()
	}
	wg.Wait()

	want := goroutines * callsEach
	if got := tr.Count(k); got != want {
		t.Fatalf("concurrent count: want %d, got %d", want, got)
	}
}

// 7. Snapshot test for the EscalationHint template content (Decision 6).
func TestEscalationHint_FormatAccuracy(t *testing.T) {
	got := EscalationHint("fs.read", "invalid_input", 3, "missing required arg 'path'")

	anchors := []string{
		`"fs.read"`,
		"3 times",
		`"invalid_input"`,
		"missing required arg 'path'",
		"Stop retrying with the same approach",
		"Common workarounds that WON'T change the outcome",
		"varying parameters that violate the same validation",
		"calling different tools that hit the same dependency",
		"retrying without first understanding why the previous attempt failed",
		"ask them to clarify or try a fundamentally different approach",
	}
	for _, a := range anchors {
		if !strings.Contains(got, a) {
			t.Errorf("EscalationHint missing anchor %q\nfull output:\n%s", a, got)
		}
	}
}

// 8. Reset() clears all signatures in the tracker.
func TestEscalation_ResetMethodClearsAll(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k1 := keyWithSig("fs.read", ErrInvalidInput, "")
	k2 := keyWithSig("web.fetch", ErrTimeout, "")
	k3 := keyWithSig("shell.run", ErrTransient, "")

	tr.EscalateIfNeeded(k1, "e1", "")
	tr.EscalateIfNeeded(k2, "e2", "")
	tr.EscalateIfNeeded(k3, "e3", "")
	if tr.Count(k1) == 0 || tr.Count(k2) == 0 || tr.Count(k3) == 0 {
		t.Fatalf("pre-reset counts should be > 0: %d %d %d",
			tr.Count(k1), tr.Count(k2), tr.Count(k3))
	}

	tr.Reset()

	if tr.Count(k1) != 0 || tr.Count(k2) != 0 || tr.Count(k3) != 0 {
		t.Errorf("post-reset counts should be 0: k1=%d k2=%d k3=%d",
			tr.Count(k1), tr.Count(k2), tr.Count(k3))
	}
	// After reset, next fail starts from 0 again.
	if got := tr.EscalateIfNeeded(k1, "fresh", ""); got != "" {
		t.Fatalf("post-reset 1st fail: want empty, got %q", got)
	}
}

// Constructor defaults to 3 when threshold <= 0.
func TestNewSignatureFailureTracker_DefaultThreshold(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		tr := NewSignatureFailureTracker(n)
		if tr.Threshold() != defaultSigThreshold {
			t.Errorf("threshold=%d: want default %d", n, defaultSigThreshold)
		}
	}
	// Custom threshold honored.
	tr5 := NewSignatureFailureTracker(5)
	if tr5.Threshold() != 5 {
		t.Fatalf("custom threshold=5: got %d", tr5.Threshold())
	}
}

// Cheap compile-time sanity that EscalationHint output is multi-line.
func TestEscalationHint_MultilineShape(t *testing.T) {
	got := EscalationHint("t", "k", 1, "x")
	if strings.Count(got, "\n") < 5 {
		t.Errorf("expected multi-line output, got %d newlines:\n%s",
			strings.Count(got, "\n"), got)
	}
	// Also: fmt.Sprintf with %q wraps strings in quotes, so "t" appears.
	if !strings.Contains(got, fmt.Sprintf("%q", "t")) {
		t.Errorf("expected %q in output, got %q", fmt.Sprintf("%q", "t"), got)
	}
}// --- Phase 2 knowledge wire tests (tool-knowledge-...-20260718) ---

// At threshold: knowledge body, when non-empty, is appended inside the
// "=== Saved Knowledge ===" section so the LLM sees prior lessons for
// the same tool together with the escalation directive.
func TestEscalation_WithKnowledge_AppendsSection(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k := keyWithSig("fs.read", ErrInvalidInput, "")

	tr.EscalateIfNeeded(k, "missing arg 'path'", "")
	tr.EscalateIfNeeded(k, "missing arg 'path'", "")
	got := tr.EscalateIfNeeded(k, "missing arg 'path'", "use fs.stat first")
	if got == "" {
		t.Fatal("3rd fail: want escalation message, got empty")
	}
	if !strings.Contains(got, "=== Saved Knowledge ===") {
		t.Errorf("escalation missing knowledge header: %q", got)
	}
	if !strings.Contains(got, "use fs.stat first") {
		t.Errorf("escalation missing knowledge body: %q", got)
	}
	if !strings.Contains(got, "=== End Knowledge ===") {
		t.Errorf("escalation missing knowledge footer: %q", got)
	}
}

// Below threshold: knowledge is ignored (no escalation message produced
// at all — caller stays on transientHint). Empty knowledge must NOT
// leak the markers into a transient hint either.
func TestEscalation_NoKnowledge_OmitsSection(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k := keyWithSig("fs.read", ErrInvalidInput, "")

	got := tr.EscalateIfNeeded(k, "missing arg 'path'", "")
	if got != "" {
		t.Errorf("1st fail: want empty below threshold, got %q", got)
	}
	if strings.Contains(got, "=== Saved Knowledge ===") {
		t.Errorf("below-threshold output must not contain knowledge marker: %q", got)
	}
}

// At threshold with empty knowledge: escalation still fires (markers
// must NOT appear because knowledge == "" suppresses the section).
func TestEscalation_BelowThreshold_IgnoresKnowledge(t *testing.T) {
	tr := NewSignatureFailureTracker(3)
	k := keyWithSig("fs.read", ErrInvalidInput, "")

	// 1st call — below threshold; even with knowledge, no escalation.
	got := tr.EscalateIfNeeded(k, "e", "lesson A")
	if got != "" {
		t.Errorf("below-threshold with knowledge: want empty, got %q", got)
	}

	// Drive to threshold with a different lesson — only THIS lesson
	// appears in the knowledge section, not the prior one.
	tr.EscalateIfNeeded(k, "e", "lesson A")
	got = tr.EscalateIfNeeded(k, "e", "lesson B")
	if got == "" {
		t.Fatal("threshold reached: want escalation, got empty")
	}
	if !strings.Contains(got, "lesson B") {
		t.Errorf("expected latest knowledge body in section; got %q", got)
	}
	if strings.Contains(got, "lesson A") {
		t.Errorf("stale knowledge leaked into section; got %q", got)
	}
}
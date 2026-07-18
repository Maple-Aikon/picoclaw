package tools

import (
	"fmt"
	"sync"
	"time"
)

// SignatureKey identifies a unique failure signature for escalation tracking.
// The combination (Tool, ErrKind, ArgSig) lets us distinguish "the same tool
// failing for the same reason" from "the same tool failing for different
// reasons" — only the former should escalate.
type SignatureKey struct {
	Tool    string
	ErrKind ErrorKind
	ArgSig  string
}

// defaultSigThreshold matches nanobot's _MAX_REPEAT_WORKSPACE_VIOLATIONS.
// Tunable per-registry via ToolRegistry.SetEscalationThreshold(n).
const defaultSigThreshold = 3

// SignatureFailureTracker counts repeated failures with the same signature
// and produces an escalation message once the threshold is exceeded.
//
// Scope: one tracker per (channel, chatID) session, owned by ToolRegistry.
// Counter scope: per signature within the tracker; cleared via Reset() at
// turn boundaries by the caller.
//
// Concurrency: safe for concurrent calls via sync.Mutex.
type SignatureFailureTracker struct {
	mu        sync.Mutex
	counts    map[SignatureKey]*signatureCount
	threshold int
}

type signatureCount struct {
	count    int
	lastErr  string
	lastSeen time.Time
}

// NewSignatureFailureTracker creates a tracker. If threshold <= 0 the
// default of 3 is used (matches nanobot's _MAX_REPEAT_WORKSPACE_VIOLATIONS).
func NewSignatureFailureTracker(threshold int) *SignatureFailureTracker {
	if threshold <= 0 {
		threshold = defaultSigThreshold
	}
	return &SignatureFailureTracker{
		counts:    make(map[SignatureKey]*signatureCount),
		threshold: threshold,
	}
}

// EscalateIfNeeded records a failure for the given signature and returns an
// escalation message iff the failure count has reached the threshold.
// Returns "" when under threshold — caller should keep the original message.
//
// Counter increments on every call regardless of outcome; once a signature
// has escalated it stays escalated for subsequent calls (until Reset or
// MarkSuccess for that key).
//
// knowledge, if non-empty, is appended to the escalation message inside a
// "=== Saved Knowledge ===" section so the LLM sees prior lessons learned
// for this exact tool (Phase 2 wire — registry.go loads from ToolKnowledgeStore).
func (t *SignatureFailureTracker) EscalateIfNeeded(key SignatureKey, lastErr, knowledge string) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	c, ok := t.counts[key]
	if !ok {
		c = &signatureCount{}
		t.counts[key] = c
	}
	c.count++
	c.lastErr = lastErr
	c.lastSeen = time.Now()

	if c.count >= t.threshold {
		msg := EscalationHint(key.Tool, string(key.ErrKind), c.count, c.lastErr)
		if knowledge != "" {
			msg += "\n\n" + AppendKnowledgeSection(knowledge)
		}
		return msg
	}
	return ""
}

// MarkSuccess resets the counter for the given signature. Called when the
// tool returns a non-error result, signaling that the LLM has corrected
// its approach and we should clear stale failure counts.
func (t *SignatureFailureTracker) MarkSuccess(key SignatureKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.counts, key)
}

// Reset clears all counters in this tracker. Called at turn boundaries.
func (t *SignatureFailureTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts = make(map[SignatureKey]*signatureCount)
}

// Count returns the current failure count for a signature. Useful for tests
// and observability; not used in hot path.
func (t *SignatureFailureTracker) Count(key SignatureKey) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.counts[key]; ok {
		return c.count
	}
	return 0
}

// Threshold returns the configured escalation threshold.
func (t *SignatureFailureTracker) Threshold() int {
	return t.threshold
}

// EscalationHint builds the structured escalation message surfaced to the
// LLM once the same tool+errKind signature has failed threshold times in
// a turn. Pure function — exported for snapshot testing.
//
// Adapted from nanobot's escalation template (utils/runtime.py:170-198)
// listing concrete workarounds that will NOT help, to discourage the LLM
// from hallucinating minor variations of the same failing approach.
func EscalationHint(tool, kind string, count int, lastErr string) string {
	return fmt.Sprintf(
		"Error: tool %q has failed %d times in this turn with kind %q.\n"+
			"Last error: %s\n\n"+
			"Stop retrying with the same approach. Common workarounds that WON'T change the outcome:\n"+
			"  - varying parameters that violate the same validation\n"+
			"  - calling different tools that hit the same dependency\n"+
			"  - retrying without first understanding why the previous attempt failed\n\n"+
			"If the user genuinely needs this resource, ask them to clarify or try a fundamentally different approach (different tool, different arguments, or escalate to user).",
		tool, count, kind, lastErr,
	)
}
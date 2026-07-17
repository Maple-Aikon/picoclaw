// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"time"
)

// RetryDecision tells the BoundedRetry loop what to do next.
//
// BoundedRetry is a reusable primitive for any bounded-sub-iteration retry
// pattern in PicoClaw. Phase 1 of plan
// same-iteration-replay-loop-with-reusable-boundedretry-primitive-20260717
// uses it for AfterLLM hook replay (HookActionReplay); Phase 2 will reuse it
// for circuit breaker auto-recovery; Phase 3 for LLM network/transient retry;
// Phase 4 for streaming recovery.
type RetryDecision int

const (
	// RetryDecisionDone: success or final state reached. Exit loop with no error.
	RetryDecisionDone RetryDecision = iota
	// RetryDecisionRetry: caller wants to retry. Loop will run again if under cap.
	RetryDecisionRetry
	// RetryDecisionAbort: caller wants to abort (fatal). Exit loop immediately.
	RetryDecisionAbort
)

// RetryContext provides the loop state to the attempt function.
//
// Attempt is 0-indexed (0 = first call). Remaining is MaxAttempts - Attempt - 1
// and is convenient for logging/observability without arithmetic.
type RetryContext struct {
	Attempt     int           // 0-indexed attempt number (0 = first call)
	MaxAttempts int           // configured cap (may differ from caller cap if defaulted)
	Remaining   int           // MaxAttempts - Attempt - 1
	Elapsed     time.Duration // total time spent in loop so far
}

// AttemptFunc runs one attempt. Returns decision + optional error.
//
// Semantics:
//   - decision=Done + err=nil       → success, exit loop
//   - decision=Retry + err=nil      → retry if under cap, else OnExhausted + return last
//   - decision=Abort + err=nil      → abort gracefully, exit loop (no OnExhausted fired)
//   - err != nil (any decision)     → exit loop with error, no further retries
type AttemptFunc func(ctx context.Context, rc RetryContext) (RetryDecision, error)

// RetryConfig configures a BoundedRetry loop.
//
// Required: Name (for logging/events).
// OnRetry and OnExhausted are optional callbacks; nil = no-op.
type RetryConfig struct {
	// Name identifies the loop in logs and events (e.g. "hook_replay",
	// "circuit_breaker_recovery"). Required for observability.
	Name string

	// MaxAttempts hard-caps the total number of attempts including the first
	// call. Default 5 if <= 0. Set to 1 to disable retry (one attempt only).
	MaxAttempts int

	// OnRetry is invoked before each retry attempt (i.e. before attempt N+1
	// when attempt N returned RetryDecisionRetry under cap). Common use cases:
	// emit observability event, log debug line, schedule delay.
	//
	// The reason parameter is empty by default — callers may pass a fixed
	// value via the closure they supply to BoundedRetry if needed. Future
	// versions may thread the most recent error message through here.
	OnRetry func(rc RetryContext, reason string)

	// OnExhausted is invoked when MaxAttempts is reached while the loop would
	// otherwise retry. It runs exactly once per BoundedRetry invocation.
	// Common use cases: log warning, emit exhaustion event.
	OnExhausted func(rc RetryContext)
}

// BoundedRetry runs `attempt` up to MaxAttempts times.
//
// Loop semantics (in order):
//  1. attempt 0 always runs first
//  2. If attempt returns RetryDecisionDone → exit immediately (success)
//  3. If attempt returns RetryDecisionAbort → exit immediately (graceful abort)
//  4. If attempt returns error → exit immediately with error
//  5. If attempt returns RetryDecisionRetry:
//     - if next attempt would exceed MaxAttempts → fire OnExhausted, exit
//     - else → fire OnRetry, run attempt N+1
//
// OnExhausted is NOT fired when the loop exits due to Done, Abort, or error.
// It is fired exactly once when MaxAttempts is hit while retrying.
//
// BoundedRetry never panics on nil callbacks — defaults to no-ops.
func BoundedRetry(
	ctx context.Context,
	cfg RetryConfig,
	attempt AttemptFunc,
) (RetryDecision, error) {
	max := cfg.MaxAttempts
	if max <= 0 {
		max = defaultRetryMaxAttempts
	}

	onRetry := cfg.OnRetry
	if onRetry == nil {
		onRetry = func(rc RetryContext, reason string) {}
	}
	onExhausted := cfg.OnExhausted
	if onExhausted == nil {
		onExhausted = func(rc RetryContext) {}
	}

	start := time.Now()
	var lastErr error
	var lastDecision RetryDecision

	for i := 0; i < max; i++ {
		// Honor caller cancellation each iteration (cheap + correct).
		if err := ctx.Err(); err != nil {
			return lastDecision, err
		}

		rc := RetryContext{
			Attempt:     i,
			MaxAttempts: max,
			Remaining:   max - i - 1,
			Elapsed:     time.Since(start),
		}

		decision, err := attempt(ctx, rc)
		lastDecision = decision
		lastErr = err

		if err != nil {
			// Hard error: exit immediately, no further retries.
			return decision, err
		}

		if decision == RetryDecisionDone {
			return decision, nil
		}

		if decision == RetryDecisionAbort {
			return decision, nil
		}

		// decision == RetryDecisionRetry
		if i+1 >= max {
			// Cap hit: signal exhaustion but do not loop again.
			onExhausted(rc)
			return decision, nil
		}

		// Schedule next attempt.
		onRetry(rc, "")
	}

	// Should not be reachable (loop body handles all exit paths), but kept
	// for defensive correctness.
	return lastDecision, lastErr
}

// defaultRetryMaxAttempts is the safe default when RetryConfig.MaxAttempts <= 0.
// Chosen to give recovery headroom without runaway risk (5 attempts = roughly
// nanobot's _MAX_LENGTH_RECOVERIES=2 scaled up).
const defaultRetryMaxAttempts = 5

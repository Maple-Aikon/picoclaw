// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Phase 12.55 T1: RetryDelays backoff schedule in BoundedRetry (Q4).
// Delay semantics: after attempt i returns RetryDecisionRetry (under cap),
// sleep RetryDelays[i] before attempt i+1. Index >= len(delays) → use the
// LAST delay (fallback-last). nil → no sleep.

func TestBoundedRetry_RetryDelays_SleepsBetweenAttempts(t *testing.T) {
	var calls atomic.Int32
	start := time.Now()
	d, err := BoundedRetry(context.Background(), RetryConfig{
		Name:        "test",
		MaxAttempts: 3,
		RetryDelays: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		calls.Add(1)
		return RetryDecisionRetry, nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != RetryDecisionRetry {
		t.Errorf("decision = %v, want Retry (exhausted state)", d)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
	// 2 retries → delays 10ms + 20ms = 30ms. Tolerance: >= 25ms.
	if elapsed < 25*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 25ms (10ms+20ms delays applied)", elapsed)
	}
}

func TestBoundedRetry_RetryDelays_NilMeansNoSleep(t *testing.T) {
	var calls atomic.Int32
	start := time.Now()
	_, err := BoundedRetry(context.Background(), RetryConfig{
		Name:        "test",
		MaxAttempts: 3,
		// RetryDelays nil → no sleep (zero-value safety, F06)
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		calls.Add(1)
		return RetryDecisionRetry, nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("elapsed = %v, want < 50ms (no delay with nil RetryDelays)", elapsed)
	}
}

func TestBoundedRetry_RetryDelays_ContextCancelMidDelay(t *testing.T) {
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	d, err := BoundedRetry(ctx, RetryConfig{
		Name:        "test",
		MaxAttempts: 3,
		RetryDelays: []time.Duration{500 * time.Millisecond},
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		calls.Add(1)
		return RetryDecisionRetry, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (cancel mid-delay must abort loop)", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry after cancel)", calls.Load())
	}
	_ = d
}

// Phase 12.55 T2: fallback-last — when attempt index >= len(delays), use
// the last delay value.
func TestBoundedRetry_RetryDelays_FallbackLast(t *testing.T) {
	var calls atomic.Int32
	start := time.Now()
	_, err := BoundedRetry(context.Background(), RetryConfig{
		Name:        "test",
		MaxAttempts: 4,
		RetryDelays: []time.Duration{10 * time.Millisecond},
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		calls.Add(1)
		return RetryDecisionRetry, nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls.Load() != 4 {
		t.Errorf("calls = %d, want 4", calls.Load())
	}
	// 3 retries, all with the fallback-last 10ms → >= 25ms.
	if elapsed < 25*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 25ms (3x fallback-last 10ms)", elapsed)
	}
}

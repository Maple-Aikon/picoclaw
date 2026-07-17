// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedRetry_DoneOnFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	d, err := BoundedRetry(context.Background(), RetryConfig{Name: "test", MaxAttempts: 5},
		func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
			calls.Add(1)
			return RetryDecisionDone, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != RetryDecisionDone {
		t.Errorf("decision = %v, want Done", d)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestBoundedRetry_RetryThenDone(t *testing.T) {
	var calls atomic.Int32
	d, err := BoundedRetry(context.Background(), RetryConfig{Name: "test", MaxAttempts: 5},
		func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
			n := calls.Add(1)
			if n < 3 {
				return RetryDecisionRetry, nil
			}
			return RetryDecisionDone, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != RetryDecisionDone {
		t.Errorf("decision = %v, want Done", d)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestBoundedRetry_Exhausted(t *testing.T) {
	var calls atomic.Int32
	var exhaustedFired atomic.Bool
	d, err := BoundedRetry(context.Background(), RetryConfig{
		Name: "test", MaxAttempts: 3,
		OnExhausted: func(rc RetryContext) { exhaustedFired.Store(true) },
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		calls.Add(1)
		return RetryDecisionRetry, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != RetryDecisionRetry {
		t.Errorf("decision = %v, want Retry (exhausted state)", d)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (capped)", calls.Load())
	}
	if !exhaustedFired.Load() {
		t.Error("OnExhausted should fire when cap hit")
	}
}

func TestBoundedRetry_Abort(t *testing.T) {
	var calls atomic.Int32
	var exhaustedFired atomic.Bool
	d, err := BoundedRetry(context.Background(), RetryConfig{
		Name: "test", MaxAttempts: 5,
		OnExhausted: func(rc RetryContext) { exhaustedFired.Store(true) },
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		calls.Add(1)
		return RetryDecisionAbort, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != RetryDecisionAbort {
		t.Errorf("decision = %v, want Abort", d)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (abort exits immediately)", calls.Load())
	}
	if exhaustedFired.Load() {
		t.Error("OnExhausted should NOT fire on Abort")
	}
}

func TestBoundedRetry_ErrorExitsImmediately(t *testing.T) {
	sentinel := errors.New("hard fail")
	var calls atomic.Int32
	var exhaustedFired atomic.Bool
	d, err := BoundedRetry(context.Background(), RetryConfig{
		Name: "test", MaxAttempts: 5,
		OnExhausted: func(rc RetryContext) { exhaustedFired.Store(true) },
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		calls.Add(1)
		return RetryDecisionRetry, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if d != RetryDecisionRetry {
		t.Errorf("decision = %v, want Retry (the last decision returned)", d)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (error exits immediately)", calls.Load())
	}
	if exhaustedFired.Load() {
		t.Error("OnExhausted should NOT fire on error")
	}
}

func TestBoundedRetry_DefaultMaxAttempts(t *testing.T) {
	// MaxAttempts <= 0 -> default 5
	var calls atomic.Int32
	d, err := BoundedRetry(context.Background(), RetryConfig{Name: "test", MaxAttempts: 0},
		func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
			calls.Add(1)
			if rc.MaxAttempts != defaultRetryMaxAttempts {
				t.Errorf("MaxAttempts in ctx = %d, want %d", rc.MaxAttempts, defaultRetryMaxAttempts)
			}
			return RetryDecisionDone, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != RetryDecisionDone {
		t.Errorf("decision = %v, want Done", d)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestBoundedRetry_OneAttemptNoRetry(t *testing.T) {
	// MaxAttempts=1 = effectively "no retry allowed"
	var calls atomic.Int32
	var onRetryFired atomic.Bool
	var exhaustedFired atomic.Bool
	d, err := BoundedRetry(context.Background(), RetryConfig{
		Name: "test", MaxAttempts: 1,
		OnRetry:     func(rc RetryContext, _ string) { onRetryFired.Store(true) },
		OnExhausted: func(rc RetryContext) { exhaustedFired.Store(true) },
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		calls.Add(1)
		return RetryDecisionRetry, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != RetryDecisionRetry {
		t.Errorf("decision = %v, want Retry (exhausted state)", d)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	if onRetryFired.Load() {
		t.Error("OnRetry should NOT fire when cap=1 (no further attempts)")
	}
	if !exhaustedFired.Load() {
		t.Error("OnExhausted should fire on the lone attempt that wants to retry")
	}
}

func TestBoundedRetry_OnRetryFiresBetweenAttempts(t *testing.T) {
	var calls atomic.Int32
	var retriesFired atomic.Int32
	d, err := BoundedRetry(context.Background(), RetryConfig{
		Name: "test", MaxAttempts: 5,
		OnRetry: func(rc RetryContext, _ string) { retriesFired.Add(1) },
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		n := calls.Add(1)
		if n < 4 {
			return RetryDecisionRetry, nil
		}
		return RetryDecisionDone, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != RetryDecisionDone {
		t.Errorf("decision = %v, want Done", d)
	}
	if calls.Load() != 4 {
		t.Errorf("calls = %d, want 4", calls.Load())
	}
	// OnRetry fires between attempts: 4 calls → 3 retries (between 1-2, 2-3, 3-4)
	if retriesFired.Load() != 3 {
		t.Errorf("OnRetry fired %d times, want 3", retriesFired.Load())
	}
}

func TestBoundedRetry_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	var calls atomic.Int32
	_, err := BoundedRetry(ctx, RetryConfig{Name: "test", MaxAttempts: 5},
		func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
			calls.Add(1)
			return RetryDecisionRetry, nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls.Load() != 0 {
		t.Errorf("calls = %d, want 0 (cancelled before first attempt)", calls.Load())
	}
}

func TestBoundedRetry_NilCallbacksAreSafe(t *testing.T) {
	// Nil OnRetry/OnExhausted must not panic.
	_, err := BoundedRetry(context.Background(), RetryConfig{
		Name: "test", MaxAttempts: 3,
		// OnRetry: nil
		// OnExhausted: nil
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		return RetryDecisionRetry, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBoundedRetry_ElapsedIncreases(t *testing.T) {
	_, err := BoundedRetry(context.Background(), RetryConfig{Name: "test", MaxAttempts: 3},
		func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
			if rc.Attempt == 1 {
				if rc.Elapsed < 5*time.Millisecond {
					t.Errorf("Elapsed = %v after 5ms sleep, want >= 5ms", rc.Elapsed)
				}
			}
			if rc.Attempt >= 2 {
				return RetryDecisionDone, nil
			}
			time.Sleep(5 * time.Millisecond)
			return RetryDecisionRetry, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

package channels

// rate_limit_test.go — B8: the Manager-side rate-limit policy
// (plan telegram-429-flood-classification-fix, AC-2 and §4.4).
//
// THE POLICY UNDER TEST
//
//	wait = max(rateLimitDelay, platform retry_after)
//	wait = min(wait, rateLimitMaxWait)   <- 5 minutes, anh chốt 2026-11-11
//
// and when the platform asks for MORE than the cap, the Manager does
// NOT retry - it hands the error back to the caller instead of
// parking the channel worker for hours (Telegram can ask for ~6.9
// hours).

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRateLimitWaitForCap(t *testing.T) {
	// 1. No retry_after: the default delay applies.
	delay, ok := rateLimitWaitFor(fmt.Errorf("%w: boom", ErrRateLimit))
	if !ok {
		t.Fatal("a rate-limit error without retry_after must still be retried")
	}
	if delay != rateLimitDelay {
		t.Errorf("default delay: got %s want %s", delay, rateLimitDelay)
	}

	// 2. A SHORT retry_after is honoured verbatim.
	delay, ok = rateLimitWaitFor(NewRateLimitError(30*time.Second, errors.New("boom")))
	if !ok {
		t.Fatal("a 30s retry_after must be retried")
	}
	if delay != 30*time.Second {
		t.Errorf("short retry_after: got %s want 30s", delay)
	}

	// 3. A retry_after LONGER than the cap is NOT honoured: the wait
	//    would park the worker for hours.
	delay, ok = rateLimitWaitFor(NewRateLimitError(6*time.Hour, errors.New("boom")))
	if ok {
		t.Errorf("a 6h retry_after must exceed the cap, but it was accepted with delay=%s", delay)
	}
	if delay != 0 {
		t.Errorf("an over-cap retry_after must yield no delay, got %s", delay)
	}

	// 4. Exactly at the cap is still retryable.
	delay, ok = rateLimitWaitFor(NewRateLimitError(rateLimitMaxWait, errors.New("boom")))
	if !ok {
		t.Errorf("a retry_after exactly at the cap (%s) must be retryable", rateLimitMaxWait)
	}
	if delay != rateLimitMaxWait {
		t.Errorf("cap boundary: got %s want %s", delay, rateLimitMaxWait)
	}

	// 5. Just over the cap is not.
	_, ok = rateLimitWaitFor(NewRateLimitError(rateLimitMaxWait+time.Second, errors.New("boom")))
	if ok {
		t.Error("a retry_after one second over the cap must not be retried")
	}

	// 6. The cap itself is 5 minutes (anh's decision, 2026-11-11).
	if rateLimitMaxWait != 5*time.Minute {
		t.Errorf("the rate-limit cap must be 5 minutes, got %s", rateLimitMaxWait)
	}
}

func TestRateLimitErrorCarriesRetryAfter(t *testing.T) {
	// errors.Is must find the sentinel.
	err := NewRateLimitError(42*time.Second, errors.New("Too Many Requests"))
	if !errors.Is(err, ErrRateLimit) {
		t.Error("errors.Is(err, ErrRateLimit) must be true")
	}
	// errors.Is must NOT find the wrong sentinel.
	if errors.Is(err, ErrTemporary) {
		t.Error("a rate-limit error must not satisfy errors.Is(err, ErrTemporary)")
	}
	if errors.Is(err, ErrSendFailed) {
		t.Error("a rate-limit error must not satisfy errors.Is(err, ErrSendFailed)")
	}

	// errors.As must find the typed error, and the number must survive.
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatal("errors.As must find *RateLimitError")
	}
	if rl.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter: got %s want 42s", rl.RetryAfter)
	}

	// The helper must agree.
	if got := RateLimitRetryAfter(err); got != 42*time.Second {
		t.Errorf("RateLimitRetryAfter: got %s want 42s", got)
	}
	if got := RateLimitRetryAfter(errors.New("plain")); got != 0 {
		t.Errorf("RateLimitRetryAfter on a plain error: got %s want 0", got)
	}
	if got := RateLimitRetryAfter(nil); got != 0 {
		t.Errorf("RateLimitRetryAfter(nil): got %s want 0", got)
	}
}

func TestClassifySendErrorWithRetry(t *testing.T) {
	// 429 -> ErrRateLimit with the retry_after attached.
	err := ClassifySendErrorWithRetry(429, 60*time.Second, errors.New("Too Many Requests"))
	if !errors.Is(err, ErrRateLimit) {
		t.Error("429 must classify as ErrRateLimit")
	}
	if got := RateLimitRetryAfter(err); got != 60*time.Second {
		t.Errorf("429 retry_after: got %s want 60s", got)
	}
	if errors.Is(err, ErrTemporary) {
		t.Error("429 must not also satisfy errors.Is(err, ErrTemporary)")
	}

	// 500 -> ErrTemporary, no retry_after.
	err = ClassifySendErrorWithRetry(500, 0, errors.New("boom"))
	if !errors.Is(err, ErrTemporary) {
		t.Error("500 must classify as ErrTemporary")
	}
	if errors.Is(err, ErrRateLimit) {
		t.Error("500 must not satisfy errors.Is(err, ErrRateLimit)")
	}

	// 400 -> ErrSendFailed.
	err = ClassifySendErrorWithRetry(400, 0, errors.New("Bad Request"))
	if !errors.Is(err, ErrSendFailed) {
		t.Error("400 must classify as ErrSendFailed")
	}
}

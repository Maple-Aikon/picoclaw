package channels

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ClassifySendError wraps a raw error with the appropriate sentinel based on
// an HTTP status code. Channels that perform HTTP API calls should use this
// in their Send path.
func ClassifySendError(statusCode int, rawErr error) error {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %w", ErrRateLimit, rawErr)
	case statusCode >= 500:
		return fmt.Errorf("%w: %w", ErrTemporary, rawErr)
	case statusCode >= 400:
		return fmt.Errorf("%w: %w", ErrSendFailed, rawErr)
	default:
		return rawErr
	}
}

// ClassifyNetError wraps a network/timeout error as ErrTemporary.
func ClassifyNetError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrTemporary, err)
}

// RateLimitError is a rate-limit failure that carries the number of
// seconds the platform asked us to wait.
//
// # WHY A TYPED ERROR
//
// ErrRateLimit on its own is a bare sentinel: it says "rate limited"
// but not "for how long". Telegram's flood control returns a
// `retry_after` that can be as long as ~24 713 seconds (~6.9 hours).
// Without the number, the Manager can only retry on a fixed 1-second
// delay, so it hammers the API again while Telegram is still blocking
// us - which is exactly what turned one rate-limit response into a
// 6-hour burst of editMessageText calls.
//
// The type carries the number so the Manager can wait the right
// amount (capped by rateLimitMaxWait).
type RateLimitError struct {
	// RetryAfter is how long the platform asked us to wait. Zero means
	// the platform did not say, and the caller should use its default.
	RetryAfter time.Duration
	// Err is the underlying error, if any.
	Err error
}

// NewRateLimitError builds a RateLimitError with the given wait.
func NewRateLimitError(retryAfter time.Duration, err error) *RateLimitError {
	return &RateLimitError{RetryAfter: retryAfter, Err: err}
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "rate limited"
	}
	if e.Err == nil {
		return fmt.Sprintf("rate limited (retry after %s)", e.RetryAfter)
	}
	return fmt.Sprintf("%v (retry after %s)", e.Err, e.RetryAfter)
}

// Unwrap exposes the underlying error so errors.Is/As keep working
// through the chain.
func (e *RateLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is makes errors.Is(err, ErrRateLimit) true for this type.
//
// Note: the sentinel chain is ALSO carried by Unwrap (the inner error
// is normally built as fmt.Errorf("%w: %w", ErrRateLimit, raw)), so
// errors.Is already succeeds through Unwrap. This method makes the
// guarantee independent of how the inner error was constructed.
func (e *RateLimitError) Is(target error) bool {
	return target == ErrRateLimit
}

// ClassifySendErrorWithRetry classifies like ClassifySendError, but
// attaches the platform's retry_after to a 429.
func ClassifySendErrorWithRetry(statusCode int, retryAfter time.Duration, rawErr error) error {
	if statusCode == http.StatusTooManyRequests {
		return &RateLimitError{
			RetryAfter: retryAfter,
			Err:        fmt.Errorf("%w: %w", ErrRateLimit, rawErr),
		}
	}
	return ClassifySendError(statusCode, rawErr)
}

// RateLimitRetryAfter returns the platform-requested wait if err is a
// rate-limit error, and 0 otherwise.
func RateLimitRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl.RetryAfter
	}
	return 0
}

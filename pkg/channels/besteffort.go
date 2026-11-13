package channels

import (
	"context"
	"errors"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// LogBestEffortFailure reports an error that a best-effort call swallowed.
//
// WHY THIS EXISTS (plan telegram-429-flood-classification-fix, §S.8):
// several cleanup calls (delete placeholder, edit placeholder, send
// placeholder, dismiss tracked tool feedback) intentionally ignore their
// error because there is no useful recovery: the message is already
// delivered or already gone, and retrying would ADD outbound traffic.
//
// Ignoring the error is fine for CONTROL FLOW but bad for
// OBSERVABILITY: a failing delete left no trace at all, so an operator
// had no evidence to act on.
//
// This helper ONLY logs. It never retries and never changes the
// caller's control flow, so the "best effort, no retry" invariant is
// preserved.
//
// WHY A RATE LIMIT IS CALLED OUT SEPARATELY:
// the platform's flood control is the one failure mode where the
// absence of a log is actively misleading - the outbound traffic keeps
// flowing (other messages still get sent) while the platform silently
// rejects ours. Flagging it makes that visible in the log.
//
// Exported so channel sub-packages (telegram, ...) can use the same
// helper instead of each inventing its own log line.
func LogBestEffortFailure(channel, chatID, messageID, op string, err error) {
	if err == nil {
		return
	}

	fields := map[string]any{
		"channel": channel,
		"chat_id": chatID,
		"op":      op,
		"error":   err.Error(),
	}
	if messageID != "" {
		fields["message_id"] = messageID
	}

	if errors.Is(err, ErrRateLimit) {
		fields["rate_limited"] = true
		fields["retry_after"] = RateLimitRetryAfter(err).String()
		logger.WarnCF("channels", "best-effort "+op+" failed (rate limited)", fields)
		return
	}

	logger.WarnCF("channels", "best-effort "+op+" failed", fields)
}

// rateLimitFallbackWait returns how long to wait before falling back to a
// different send path after a rate-limited call failed.
//
// The platform's `retry_after` wins when it is LONGER than the default
// delay. The result is CAPPED at rateLimitMaxWait (5 minutes): Telegram's
// flood control can ask for ~6.9 hours, and parking a channel worker for
// that long would block every later message queued behind it.
//
// Unlike rateLimitWaitFor (used by the Send retry loop, which gives up
// when the platform asks for more than the cap), this helper CLAMPS
// instead of giving up: the caller still has a message to deliver, so it
// waits as long as it is allowed to and then tries the next path.
func rateLimitFallbackWait(err error) time.Duration {
	delay := rateLimitDelay
	if ra := RateLimitRetryAfter(err); ra > delay {
		delay = ra
	}
	if delay > rateLimitMaxWait {
		delay = rateLimitMaxWait
	}
	return delay
}

// waitForRateLimit sleeps for the platform-requested wait and reports
// whether the wait completed.
//
// It returns false when ctx is canceled first, so the caller can stop
// instead of firing another request into a shutdown.
func waitForRateLimit(ctx context.Context, err error) bool {
	delay := rateLimitFallbackWait(err)
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

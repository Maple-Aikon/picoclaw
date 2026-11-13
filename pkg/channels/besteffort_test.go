package channels

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// mockFailingPlaceholderChannel is a PlaceholderCapable channel whose
// placeholder always fails.
type mockFailingPlaceholderChannel struct {
	mockChannel
	err error
}

func (m *mockFailingPlaceholderChannel) SendPlaceholder(_ context.Context, _ string) (string, error) {
	return "", m.err
}

// mockFailingDeleteEditor is a MessageEditor + MessageDeleter whose delete
// always fails.
type mockFailingDeleteEditor struct {
	mockMessageEditor
	err         error
	deleteCalls int
}

func (m *mockFailingDeleteEditor) DeleteMessage(_ context.Context, _, _ string) error {
	m.deleteCalls++
	return m.err
}

// mockFailingEditEditor is a MessageEditor whose edit always fails.
type mockFailingEditEditor struct {
	mockMessageEditor
	err       error
	editCalls int
}

func (m *mockFailingEditEditor) EditMessage(_ context.Context, _, _, _ string) error {
	m.editCalls++
	return m.err
}

// rateLimitError builds an error that carries a `retry_after`, the way a
// channel classifies a platform 429.
func rateLimitError(retryAfter time.Duration) error {
	return NewRateLimitError(retryAfter, fmt.Errorf("429 too many requests"))
}

// TestRateLimitFallbackWait_ClampsInsteadOfGivingUp pins the difference
// between the two rate-limit helpers:
//
//   - rateLimitWaitFor (used by the Send retry loop) GIVES UP when the
//     platform asks for more than the cap.
//   - rateLimitFallbackWait (used before falling back to another send
//     path) CLAMPS to the cap, because the caller still has a message to
//     deliver.
func TestRateLimitFallbackWait_ClampsInsteadOfGivingUp(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
	}{
		{"no retry_after uses default delay", 0, rateLimitDelay},
		{"retry_after below default keeps default", 100 * time.Millisecond, rateLimitDelay},
		{"retry_after above default wins", 3 * time.Second, 3 * time.Second},
		{"retry_after above cap is clamped", 24 * time.Hour, rateLimitMaxWait},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewRateLimitError(tt.retryAfter, fmt.Errorf("429"))
			if got := rateLimitFallbackWait(err); got != tt.want {
				t.Fatalf("rateLimitFallbackWait() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestPreSend_PlaceholderEditRateLimited_WaitsBeforeFallingThrough is the
// regression test for the OPEN ISSUE (plan telegram-429-flood-classification-fix,
// §S.5): a rate-limited placeholder edit used to fall through to a normal
// Send IMMEDIATELY, firing a new request at the exact moment the platform
// was flood-blocking us.
//
// The fix waits for the platform's `retry_after` before falling through.
func TestPreSend_PlaceholderEditRateLimited_WaitsBeforeFallingThrough(t *testing.T) {
	m := newTestManager()

	// Longer than rateLimitDelay (1s) so the platform value is the one
	// that decides the wait.
	retryAfter := 1100 * time.Millisecond

	ch := &mockMessageEditor{
		editFn: func(_ context.Context, _, _, _ string) error {
			return rateLimitError(retryAfter)
		},
	}

	m.RecordPlaceholder("test", "123", "456")

	msg := testOutboundMessage(bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"})

	start := time.Now()
	_, handled := m.preSend(context.Background(), "test", msg, ch)
	elapsed := time.Since(start)

	if handled {
		t.Fatal("expected preSend to still fall through after a rate-limited edit")
	}
	if elapsed < retryAfter {
		t.Fatalf("expected preSend to wait at least %s before falling through, waited %s", retryAfter, elapsed)
	}
}

// TestPreSend_PlaceholderEditGenericFailure_FallsThroughImmediately pins
// that ONLY a rate limit introduces a wait. Any other edit failure keeps
// the pre-existing behaviour (fall through right away) so the patch does
// not add latency to unrelated failures.
func TestPreSend_PlaceholderEditGenericFailure_FallsThroughImmediately(t *testing.T) {
	m := newTestManager()

	ch := &mockMessageEditor{
		editFn: func(_ context.Context, _, _, _ string) error {
			return fmt.Errorf("edit failed")
		},
	}

	m.RecordPlaceholder("test", "123", "456")

	msg := testOutboundMessage(bus.OutboundMessage{Channel: "test", ChatID: "123", Content: "hello"})

	start := time.Now()
	_, handled := m.preSend(context.Background(), "test", msg, ch)
	elapsed := time.Since(start)

	if handled {
		t.Fatal("expected preSend to fall through when the edit fails")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("expected no wait for a non-rate-limit failure, waited %s", elapsed)
	}
}

// TestSendWithRetry_PlaceholderEditRateLimited_SendNotImmediate is the
// end-to-end version of the same regression: the fallback Send must not
// be fired at the platform while it is flood-blocking us.
func TestSendWithRetry_PlaceholderEditRateLimited_SendNotImmediate(t *testing.T) {
	m := newTestManager()
	retryAfter := 1100 * time.Millisecond

	var sendTimes []time.Time
	ch := &mockMessageEditor{
		mockChannel: mockChannel{
			sendFn: func(_ context.Context, _ bus.OutboundMessage) error {
				sendTimes = append(sendTimes, time.Now())
				return nil
			},
		},
		editFn: func(_ context.Context, _, _, _ string) error {
			return rateLimitError(retryAfter)
		},
	}
	w := &channelWorker{
		ch:      ch,
		limiter: rate.NewLimiter(rate.Inf, 1),
	}

	m.RecordPlaceholder("test", "1", "456")
	msg := testOutboundMessage(bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "hello"})

	start := time.Now()
	m.sendWithRetry(context.Background(), "test", w, msg)

	if len(sendTimes) != 1 {
		t.Fatalf("expected exactly 1 Send call, got %d", len(sendTimes))
	}
	if waited := sendTimes[0].Sub(start); waited < retryAfter {
		t.Fatalf("expected Send to be delayed by at least %s, was called after %s", retryAfter, waited)
	}
}

// TestBestEffortFailure_LogsWithoutRetrying checks the other half of the
// task: the call sites that used to swallow their error entirely now log
// it, while the "best effort, no retry" behaviour is preserved.
//
// The three log sources are checked in ONE test on purpose: enabling
// file logging mutates package-level logger state.
func TestBestEffortFailure_LogsWithoutRetrying(t *testing.T) {
	logFile := t.TempDir() + "/best-effort.log"
	prevLevel := logger.GetLevel()
	logger.SetLevel(logger.WARN)
	if err := logger.EnableFileLogging(logFile); err != nil {
		t.Fatalf("EnableFileLogging() error = %v", err)
	}
	defer func() {
		logger.DisableFileLogging()
		logger.SetLevel(prevLevel)
	}()

	m := newTestManager()

	// 1. SendPlaceholder failure is logged.
	m.channels["test"] = &mockFailingPlaceholderChannel{
		err: fmt.Errorf("placeholder boom"),
	}
	if m.SendPlaceholder(context.Background(), "test", "123") {
		t.Fatal("expected SendPlaceholder to report failure")
	}

	// 2. DeleteMessage failure is logged and NOT retried.
	// The "thought" message kind bypasses the placeholder edit, so the
	// delete path is the one exercised here.
	deleting := &mockFailingDeleteEditor{
		err: fmt.Errorf("delete boom"),
	}
	m.RecordPlaceholder("test", "456", "789")
	msg := testOutboundMessage(bus.OutboundMessage{
		Channel: "test",
		ChatID:  "456",
		Content: "hello",
		Context: bus.InboundContext{
			Channel: "test",
			ChatID:  "456",
			Raw: map[string]string{
				"message_kind": "thought",
			},
		},
	})
	if _, handled := m.preSend(context.Background(), "test", msg, deleting); handled {
		t.Fatal("expected preSend to fall through when the placeholder delete fails")
	}
	if deleting.deleteCalls != 1 {
		t.Fatalf("expected exactly 1 delete attempt (no retry), got %d", deleting.deleteCalls)
	}

	// 2b. EditMessage failure is logged and NOT retried.
	editing := &mockFailingEditEditor{
		err: fmt.Errorf("edit boom"),
	}
	m.RecordPlaceholder("test", "321", "654")
	editMsg := testOutboundMessage(bus.OutboundMessage{Channel: "test", ChatID: "321", Content: "hello"})
	if _, handled := m.preSend(context.Background(), "test", editMsg, editing); handled {
		t.Fatal("expected preSend to fall through when the placeholder edit fails")
	}
	if editing.editCalls != 1 {
		t.Fatalf("expected exactly 1 edit attempt (no retry), got %d", editing.editCalls)
	}

	// 3. A rate-limited failure is logged with the retry_after value.
	LogBestEffortFailure("test", "123", "789", "edit placeholder", rateLimitError(7*time.Second))

	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", logFile, err)
	}
	out := string(raw)

	for _, want := range []string{
		"best-effort send placeholder failed",
		"placeholder boom",
		"best-effort delete placeholder (bypass edit) failed",
		"delete boom",
		"best-effort edit placeholder failed",
		"edit boom",
		"best-effort edit placeholder failed (rate limited)",
		"retry_after",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected log to contain %q, got: %s", want, out)
		}
	}
}

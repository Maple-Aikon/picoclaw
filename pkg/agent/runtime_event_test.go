package agent

import (
	"testing"
	"time"

	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
)

func subscribeRuntimeEventsForTest(
	t *testing.T,
	al *AgentLoop,
	buffer int,
	kinds ...runtimeevents.Kind,
) (<-chan runtimeevents.Event, func()) {
	t.Helper()

	if al == nil {
		t.Fatal("agent loop is nil")
	}
	channel := al.RuntimeEvents()
	if channel == nil {
		t.Fatal("runtime event channel is nil")
	}
	if len(kinds) > 0 {
		channel = channel.OfKind(kinds...)
	}
	sub, ch, err := channel.SubscribeChan(
		t.Context(),
		runtimeevents.SubscribeOptions{Name: "agent-runtime-test", Buffer: buffer},
	)
	if err != nil {
		t.Fatalf("SubscribeChan failed: %v", err)
	}
	return ch, func() {
		if err := sub.Close(); err != nil {
			t.Errorf("runtime subscription close failed: %v", err)
		}
	}
}

func waitForRuntimeEvent(
	t *testing.T,
	ch <-chan runtimeevents.Event,
	timeout time.Duration,
	match func(runtimeevents.Event) bool,
) runtimeevents.Event {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatal("runtime event stream closed before expected event arrived")
			}
			if match(evt) {
				return evt
			}
		case <-timer.C:
			t.Fatal("timed out waiting for expected runtime event")
		}
	}
}

// collectRuntimeEventStream drains the runtime event channel until it has been
// quiescent for a short window (i.e. no new events arrive within
// drainQuietWindow). This is more reliable than the previous implementation,
// which used a non-blocking `default:` branch and could drop events that
// arrived between the main turn's final send and this caller's first select
// iteration.
//
// The overall drain is bounded by drainTotalTimeout as a safety net so a
// pathological producer can never wedge a test forever.
func collectRuntimeEventStream(ch <-chan runtimeevents.Event) []runtimeevents.Event {
	const (
		drainQuietWindow  = 50 * time.Millisecond
		drainTotalTimeout = 5 * time.Second
	)

	var events []runtimeevents.Event
	deadline := time.NewTimer(drainTotalTimeout)
	defer deadline.Stop()
	quiet := time.NewTimer(drainQuietWindow)
	defer quiet.Stop()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, evt)
			// Reset the quiet window — we just saw activity.
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(drainQuietWindow)
		case <-quiet.C:
			// No activity for drainQuietWindow → producer is quiescent;
			// whatever events were buffered are now ours.
			return events
		case <-deadline.C:
			// Total budget exhausted — return whatever we have so the test
			// can fail with a meaningful diff instead of hanging.
			return events
		}
	}
}

func findRuntimeEvent(
	events []runtimeevents.Event,
	kind runtimeevents.Kind,
) (runtimeevents.Event, bool) {
	for _, evt := range events {
		if evt.Kind == kind {
			return evt, true
		}
	}
	return runtimeevents.Event{}, false
}

func filterRuntimeEvents(events []runtimeevents.Event, kind runtimeevents.Kind) []runtimeevents.Event {
	var filtered []runtimeevents.Event
	for _, evt := range events {
		if evt.Kind == kind {
			filtered = append(filtered, evt)
		}
	}
	return filtered
}

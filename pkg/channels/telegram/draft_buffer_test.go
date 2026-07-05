package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePusher records PushDraft/ClearDraft calls and can be configured to
// fail on the Nth call. It implements draftPusher.
type fakePusher struct {
	mu          sync.Mutex
	pushCalls   []string
	clearCalls  int
	pushErrAt   int    // 0 = never fail; N = fail on the Nth push
	pushErr     error
	pushDelay   time.Duration
	currentCall int32 // atomic counter matching len(pushCalls) but lock-free
}

func (f *fakePusher) PushDraft(_ context.Context, text string) error {
	idx := int(atomic.AddInt32(&f.currentCall, 1))
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushDelay > 0 {
		time.Sleep(f.pushDelay)
	}
	if f.pushErrAt > 0 && idx == f.pushErrAt {
		return f.pushErr
	}
	f.pushCalls = append(f.pushCalls, text)
	return nil
}

func (f *fakePusher) ClearDraft(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearCalls++
	return nil
}

func (f *fakePusher) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.pushCalls))
	copy(out, f.pushCalls)
	return out, f.clearCalls
}

// Test 1: Update buffers, no PushDraft called until Finalize.
func TestDraftBuffer_DefersAllPushesUntilFinalize(t *testing.T) {
	fp := &fakePusher{}
	b := newDraftBuffer(fp, 10*time.Second) // long timeout, should NOT trigger

	updates := []string{"Hello, ", "this is ", "a test."}
	for _, u := range updates {
		if err := b.Update(context.Background(), u); err != nil {
			t.Fatalf("Update(%q) returned error: %v", u, err)
		}
	}

	// No push should have happened yet.
	calls, clears := fp.snapshot()
	if len(calls) != 0 {
		t.Errorf("expected 0 PushDraft calls during buffering, got %d: %v", len(calls), calls)
	}
	if clears != 0 {
		t.Errorf("expected 0 ClearDraft calls, got %d", clears)
	}

	// Finalize should flush the concatenated text.
	if err := b.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize returned error: %v", err)
	}

	calls, _ = fp.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PushDraft call after Finalize, got %d", len(calls))
	}
	if calls[0] != "Hello, this is a test." {
		t.Errorf("final text = %q, want %q", calls[0], "Hello, this is a test.")
	}
	if !b.IsCommitted() {
		t.Error("buffer should be marked committed after Finalize")
	}
}

// Test 2: Cancel drops the buffer and emits a single ClearDraft.
func TestDraftBuffer_CancelDropsAndClears(t *testing.T) {
	fp := &fakePusher{}
	b := newDraftBuffer(fp, 10*time.Second)

	if err := b.Update(context.Background(), "raw text that should never reach the user"); err != nil {
		t.Fatal(err)
	}
	if err := b.Update(context.Background(), " [tool_use: evil"); err != nil {
		t.Fatal(err)
	}

	if err := b.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}

	calls, clears := fp.snapshot()
	if len(calls) != 0 {
		t.Errorf("Cancel should drop buffer → 0 PushDraft calls, got %d: %v", len(calls), calls)
	}
	if clears != 1 {
		t.Errorf("expected exactly 1 ClearDraft call, got %d", clears)
	}
	if !b.IsCanceled() {
		t.Error("buffer should be marked canceled")
	}
	if got := b.Buffered(); got != "" {
		t.Errorf("buffered text after Cancel = %q, want empty", got)
	}

	// Cancel is idempotent.
	if err := b.Cancel(context.Background()); err != nil {
		t.Errorf("second Cancel returned %v, want nil", err)
	}
	calls, clears = fp.snapshot()
	if clears != 1 {
		t.Errorf("second Cancel should not re-clear, got %d ClearDraft calls", clears)
	}
}

// Test 3: Update/Finalize after Cancel return errBufferAlreadyCommitted.
func TestDraftBuffer_LifecycleGuards(t *testing.T) {
	fp := &fakePusher{}
	b := newDraftBuffer(fp, 10*time.Second)

	if err := b.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := b.Update(context.Background(), "should be rejected"); !errors.Is(err, errBufferAlreadyCommitted) {
		t.Errorf("Update after Cancel = %v, want errBufferAlreadyCommitted", err)
	}
	if err := b.Finalize(context.Background()); !errors.Is(err, errBufferAlreadyCommitted) {
		t.Errorf("Finalize after Cancel = %v, want errBufferAlreadyCommitted", err)
	}

	// Symmetric: Finalize first, then Update/Cancel
	b2 := newDraftBuffer(fp, 10*time.Second)
	if err := b2.Update(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if err := b2.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b2.Update(context.Background(), "second"); !errors.Is(err, errBufferAlreadyCommitted) {
		t.Errorf("Update after Finalize = %v, want errBufferAlreadyCommitted", err)
	}
	if err := b2.Cancel(context.Background()); err != nil {
		t.Errorf("Cancel after Finalize should be a no-op, got %v", err)
	}
}

// Test 4: flush timer fires and commits without Finalize.
func TestDraftBuffer_TimerFlushPreventsStuck(t *testing.T) {
	fp := &fakePusher{}
	// Use a short timeout to keep the test fast.
	b := newDraftBuffer(fp, 50*time.Millisecond)

	if err := b.Update(context.Background(), "thinking..."); err != nil {
		t.Fatal(err)
	}

	// Wait for the timer to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.Buffered() == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	calls, _ := fp.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PushDraft after timer flush, got %d: %v", len(calls), calls)
	}
	if calls[0] != "thinking..." {
		t.Errorf("flushed text = %q, want %q", calls[0], "thinking...")
	}

	// Now Finalize should see pending=false and return errBufferFlushTimeout.
	err := b.Finalize(context.Background())
	if !errors.Is(err, errBufferFlushTimeout) {
		t.Errorf("Finalize after timer flush = %v, want errBufferFlushTimeout", err)
	}
}

// Test 5: PushDraft failure surfaces, future updates return the error.
func TestDraftBuffer_PushFailurePropagates(t *testing.T) {
	pushErr := errors.New("telegram rate limited")
	fp := &fakePusher{pushErrAt: 1, pushErr: pushErr}
	b := newDraftBuffer(fp, 10*time.Second)

	if err := b.Update(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	err := b.Finalize(context.Background())
	if err == nil {
		t.Fatal("Finalize should return error when pusher fails")
	}
	if !strings.Contains(err.Error(), "telegram rate limited") {
		t.Errorf("error chain = %v, want it to wrap %q", err, pushErr)
	}

	// Subsequent Update returns the recorded error.
	if err := b.Update(context.Background(), "more"); !errors.Is(err, pushErr) {
		t.Errorf("Update after failure = %v, want %v", err, pushErr)
	}
}

// Test 6: Empty / whitespace-only updates are no-ops (matches pipeline filter).
func TestDraftBuffer_EmptyUpdatesAreNoOps(t *testing.T) {
	fp := &fakePusher{}
	b := newDraftBuffer(fp, 10*time.Second)

	for _, u := range []string{"", "   ", "\n\n", "\t"} {
		if err := b.Update(context.Background(), u); err != nil {
			t.Errorf("Update(%q) returned %v, want nil", u, err)
		}
	}

	if got := b.Buffered(); got != "" {
		t.Errorf("buffered after empty updates = %q, want empty", got)
	}

	// First real update after empties should work normally.
	if err := b.Update(context.Background(), "real content"); err != nil {
		t.Fatal(err)
	}
	if err := b.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, _ := fp.snapshot()
	if len(calls) != 1 || calls[0] != "real content" {
		t.Errorf("calls = %v, want [real content]", calls)
	}
}

// Test 7: Concurrent Update calls do not race or lose data.
// Run with: go test -race
func TestDraftBuffer_ConcurrentUpdatesAreSafe(t *testing.T) {
	fp := &fakePusher{}
	b := newDraftBuffer(fp, 10*time.Second)

	const writers = 8
	const perWriter = 50
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				_ = b.Update(context.Background(), "x")
			}
		}(i)
	}
	wg.Wait()

	if err := b.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, _ := fp.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 final push, got %d", len(calls))
	}
	if want := strings.Repeat("x", writers*perWriter); calls[0] != want {
		t.Errorf("final length = %d, want %d", len(calls[0]), len(want))
	}
}

// Test 8: Re-arm timer — second Update within flush window resets the timer.
func TestDraftBuffer_TimerReArmsOnEachUpdate(t *testing.T) {
	fp := &fakePusher{}
	b := newDraftBuffer(fp, 80*time.Millisecond)

	if err := b.Update(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := b.Update(context.Background(), "b"); err != nil {
		t.Fatal(err)
	} // re-arms timer
	time.Sleep(40 * time.Millisecond)
	// At t=80ms from first Update, no flush yet because second Update
	// re-armed the timer to t=120ms total.
	calls, _ := fp.snapshot()
	if len(calls) != 0 {
		t.Errorf("flush fired too early: %v", calls)
	}
	time.Sleep(80 * time.Millisecond)
	calls, _ = fp.snapshot()
	if len(calls) != 1 || calls[0] != "ab" {
		t.Errorf("expected single flush of 'ab', got %v", calls)
	}
}

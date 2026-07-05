package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// draftBuffer accumulates streaming content instead of pushing it to the
// Telegram draft API on every chunk.
//
// Why: Telegram's sendMessageDraft API commits text in real time. The agent
// pipeline streams each LLM chunk immediately (telegramStreamer.Update),
// which means a malformed tool-call pattern (e.g. "[tool_use: ...") leaks to
// the user chunk-by-chunk before the AfterLLM hook can intercept and trigger
// HookActionReplay. The hook only fires AFTER the LLM response is complete —
// by then the draft is already visible.
//
// How: Update() appends to an in-memory buffer instead of calling the draft
// API. The buffer is committed in three ways:
//
//  1. Finalize() — normal end-of-stream. Flush the buffered text to the
//     draft API (throttled) and let the final message be sent.
//  2. Cancel() — hook detected a problem. Drop the buffer and emit a
//     clearDraft() placeholder (preserves existing " " behavior).
//  3. flushTimeout — if neither Finalize nor Cancel arrives within
//     flushAfter since the last Update, flush anyway. Prevents a stuck
//     "thinking..." state when the hook chain stalls.
//
// Threading: all state is protected by mu. The streamer methods
// (Update/Finalize/Cancel) and the timer goroutine synchronize on the same
// mutex, so concurrent calls are safe.
type draftBuffer struct {
	// Pusher abstracts the Telegram draft API so the buffer can be unit-tested
	// without a real bot. In production this is the *telegramStreamer itself
	// (or a thin adapter wrapping it).
	pusher draftPusher

	// flushAfter is the maximum time to hold a chunk without a commit signal.
	// 5s default; matches Maple's "buffer-5s" decision (2026-07-05).
	flushAfter time.Duration

	mu         sync.Mutex
	buffered   strings.Builder
	pending    bool
	updatedAt  time.Time
	timer      *time.Timer
	com        bool // committed (Finalize called)
	canceled   bool // canceled (Cancel called)
	failed     bool // pusher reported a permanent failure
	finalError error
}

// draftPusher is the minimal surface the buffer needs to talk to the
// Telegram draft API. Defined here so tests can supply a fake.
type draftPusher interface {
	// PushDraft sends the current accumulated text to the draft API. Returns
	// an error if the call fails; on first failure the buffer marks itself
	// failed and stops trying.
	PushDraft(ctx context.Context, text string) error
	// ClearDraft blanks the visible draft (Telegram " " trick).
	ClearDraft(ctx context.Context) error
}

// errBufferAlreadyCommitted is returned by Update/Finalize after Finalize or
// Cancel has been called. It is not a hard error — it just means the streamer
// fired another event after the lifecycle ended, which can happen if the
// hook chain re-invokes the streamer during recovery replay.
var errBufferAlreadyCommitted = errors.New("draft buffer: lifecycle already ended")

// errBufferFlushTimeout is returned by Finalize if the buffer was committed
// by the timer goroutine before Finalize arrived. The caller can treat this
// as success — the user already saw the message.
var errBufferFlushTimeout = errors.New("draft buffer: flushed by timeout before finalize")

// newDraftBuffer constructs a buffer with the given flush timeout. A
// non-positive timeout is treated as "no auto-flush" (the timer is disabled
// and only Finalize/Cancel will commit).
func newDraftBuffer(pusher draftPusher, flushAfter time.Duration) *draftBuffer {
	return &draftBuffer{
		pusher:     pusher,
		flushAfter: flushAfter,
	}
}

// Update appends content to the buffer. Returns nil on success. Returns
// errBufferAlreadyCommitted if Finalize or Cancel was already called.
// Returns the underlying pusher error and stops accepting further updates
// if PushDraft fails (handled lazily on flush; Update itself never calls
// the pusher).
func (b *draftBuffer) Update(ctx context.Context, content string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.failed {
		return b.finalError
	}
	if b.com || b.canceled {
		return errBufferAlreadyCommitted
	}

	// Empty / whitespace-only chunks: ignore (matches streamingChunkPublisher
	// filter at pkg/agent/pipeline_streaming.go:381).
	if strings.TrimSpace(content) == "" {
		return nil
	}

	b.buffered.WriteString(content)
	b.pending = true
	b.updatedAt = time.Now()

	// (Re)arm the flush timer. We do NOT call the pusher here — the whole
	// point of the buffer is to avoid that.
	b.armTimerLocked(ctx)

	return nil
}

// Finalize commits the buffered content to the draft API and clears the
// timer. If the buffer was already flushed by the timer, returns
// errBufferFlushTimeout to signal the caller that the user has already seen
// the message.
func (b *draftBuffer) Finalize(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.canceled {
		return errBufferAlreadyCommitted
	}
	if b.com {
		// Re-entry from timer + Finalize race; idempotent.
		return nil
	}

	b.stopTimerLocked()
	b.com = true

	// If the timer goroutine already committed (and set pending=false)
	// we surface that to the caller so it can avoid re-sending the final message.
	if !b.pending {
		return errBufferFlushTimeout
	}

	text := b.buffered.String()
	b.pending = false
	b.buffered.Reset()

	if err := b.pusher.PushDraft(ctx, text); err != nil {
		b.failed = true
		b.finalError = err
		return fmt.Errorf("draft buffer finalize push: %w", err)
	}
	return nil
}

// Cancel drops the buffer and emits a clearDraft() placeholder. Idempotent:
// calling Cancel more than once is a no-op. Returns nil.
func (b *draftBuffer) Cancel(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.com || b.canceled {
		return nil
	}

	b.stopTimerLocked()
	b.canceled = true
	b.pending = false
	b.buffered.Reset()

	// Best-effort clear. If it fails the draft stays visible; that matches
	// the pre-fix behavior of telegramStreamer.Cancel → clearDraft.
	_ = b.pusher.ClearDraft(ctx)
	return nil
}

// armTimerLocked starts a goroutine that will flush the buffer after
// flushAfter. Must be called with b.mu held.
func (b *draftBuffer) armTimerLocked(ctx context.Context) {
	if b.flushAfter <= 0 {
		return
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(b.flushAfter, func() {
		b.flushFromTimer(ctx)
	})
}

// stopTimerLocked cancels any pending timer. Must be called with b.mu held.
func (b *draftBuffer) stopTimerLocked() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
}

// flushFromTimer is the timer goroutine callback. It commits the buffer
// even if Finalize has not arrived, so the user always sees SOMETHING
// within flushAfter of the last chunk.
func (b *draftBuffer) flushFromTimer(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.com || b.canceled || !b.pending {
		return
	}

	text := b.buffered.String()
	b.pending = false
	b.buffered.Reset()

	// Use a fresh context if the caller's context was canceled — the timer
	// must still try to deliver.
	callCtx := ctx
	if callCtx.Err() != nil {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
	}

	if err := b.pusher.PushDraft(callCtx, text); err != nil {
		b.failed = true
		b.finalError = err
	}
}

// Buffered returns the current accumulated text (snapshot, not a live view).
// Useful for tests and diagnostics.
func (b *draftBuffer) Buffered() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffered.String()
}

// IsCommitted reports whether Finalize has been called.
func (b *draftBuffer) IsCommitted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.com
}

// IsCanceled reports whether Cancel has been called.
func (b *draftBuffer) IsCanceled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.canceled
}

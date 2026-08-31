package telegram

import (
	"strings"
	"sync"
)

// SlidingTailBuffer (plan prompt-cache-optimization-goal-phase-adherence-architecture-plan-20260830
// §2.1 task 1.4 / Phase 3 task 3.4) maintains a 16-character sliding tail
// across streaming chunks so a cross-chunk `[tool_use: ...]` leak can be
// detected BEFORE it surfaces to the user via Telegram's draft API.
//
// Why a sliding tail window (vs a static delimiter check):
//
//   - The LLM may stream `[tool_use:` split across two chunks, e.g.:
//     chunk1="Hello [tool_", chunk2="use: ...]". A static regex on each
//     chunk alone misses the leak.
//   - Holding back the last 16 characters of every chunk (sliding tail)
//     means the sanitizer sees `[tool_` + `use: ` together on the
//     boundary of the next call and can suppress both.
//   - 16 chars is the minimum width needed to reliably catch the
//     `[tool_use:` (10 chars) token WITH a 6-char buffer for partial
//     tool-call prefixes (e.g. `<tool_call>` 11 chars).
//
// Threading: SlidingTailBuffer is safe for concurrent use via a sync.Mutex.
// The streamer calls SanitizeStreamChunk from a single goroutine today, but
// the mutex is cheap and future-proofs against fan-out.
type SlidingTailBuffer struct {
	mu          sync.Mutex
	slidingTail string    // last 16 chars of the previous chunk (or carry-over across a [tool_use: detect)
	toolUseSeen bool      // true when a [tool_use: leak has been detected and we're in suppression mode
	chunkCount  int       // monotonic counter for diagnostics
}

// NewSlidingTailBuffer returns an empty buffer ready for use.
func NewSlidingTailBuffer() *SlidingTailBuffer {
	return &SlidingTailBuffer{}
}

// slidingTailWindow is the maximum number of trailing characters carried
// across chunk boundaries. Must be >= len("[tool_use:") to detect a leak.
const slidingTailWindow = 16

// toolUseLeakPrefix is the literal prefix the streamer uses to mark tool
// calls in raw form (before the response parser rewrites them into a
// proper tool_use ContentBlock). We suppress everything from this prefix
// onward until the closing bracket arrives or the buffer is reset.
const toolUseLeakPrefix = "[tool_use:"

// SanitizeStreamChunk is the entry point for the streamer. It returns the
// portion of `chunk` that is SAFE to forward to the Telegram draft API:
//
//   - Chunks that contain no `[tool_use:` and don't cross a previous
//     suppressed region: returned in full (minus the trailing 16 chars
//     which become the new slidingTail).
//   - Chunks that contain `[tool_use:` anywhere in the combined
//     (slidingTail + chunk) text: returned up to (but not including)
//     the prefix; the tail is dropped into the suppression buffer.
//   - Chunks that fall inside an active suppression (after a [tool_use:
//     was detected): returned as "" until Reset() is called.
//
// This method is the safety net that complements the draftBuffer (commit
// semantics) in draft_buffer.go. The draftBuffer holds chunks until
// Finalize/Cancel/flushTimeout; the SlidingTailBuffer ensures even pre-commit
// chunks that leak tool-call syntax don't reach the user.
func (db *SlidingTailBuffer) SanitizeStreamChunk(chunk string) string {
	db.mu.Lock()
	defer db.mu.Unlock()

	db.chunkCount++

	if db.toolUseSeen {
		// Already in suppression mode — keep dropping until Reset().
		// We still need to update slidingTail for the LATER detection
		// of `]` (closing bracket) so we can auto-recover, but for now
		// the plan spec only requires suppression; Reset is the canonical
		// release path (called by the AfterLLM hook once a real tool
		// call has been parsed).
		if len(chunk) > slidingTailWindow {
			db.slidingTail = chunk[len(chunk)-slidingTailWindow:]
		} else {
			db.slidingTail = chunk
		}
		return ""
	}

	combined := db.slidingTail + chunk

	if idx := strings.Index(combined, toolUseLeakPrefix); idx != -1 {
		// LEAK DETECTED: suppress everything from the prefix onward.
		// Output only the safe portion BEFORE the prefix; store the
		// prefix + suffix in slidingTail so we can resume cleanly when
		// Reset is called.
		db.toolUseSeen = true
		db.slidingTail = combined[idx:]
		return combined[:idx]
	}

	// No leak — flush all but the last 16 chars (which become the
	// slidingTail for the next call).
	if len(combined) > slidingTailWindow {
		flushLen := len(combined) - slidingTailWindow
		safe := combined[:flushLen]
		db.slidingTail = combined[flushLen:]
		return safe
	}

	// Combined shorter than 16 — buffer the whole thing, emit nothing.
	db.slidingTail = combined
	return ""
}

// Reset clears the suppression flag and tail buffer. Called by the
// AfterLLM hook once a tool call has been parsed and the draft should
// resume, or by Finalize/Cancel on the parent draftBuffer when a stream
// completes cleanly.
func (db *SlidingTailBuffer) Reset() {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.slidingTail = ""
	db.toolUseSeen = false
}

// SlidingTail returns the current trailing 16 chars (or fewer). Useful for
// diagnostics and tests.
func (db *SlidingTailBuffer) SlidingTail() string {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.slidingTail
}

// ChunkCount returns how many chunks have been processed since construction
// or last Reset. Diagnostics only.
func (db *SlidingTailBuffer) ChunkCount() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.chunkCount
}

// IsSuppressing returns true when a [tool_use: leak has been detected and
// the buffer is dropping chunks until Reset.
func (db *SlidingTailBuffer) IsSuppressing() bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.toolUseSeen
}
package telegram

import (
	"strings"
	"sync"
	"testing"
)

// TestSlidingTailBuffer_BasicStreaming covers the happy path: chunks longer
// than the sliding tail window emit (chunk - last-16-chars); nothing is
// dropped and nothing leaks.
func TestSlidingTailBuffer_BasicStreaming(t *testing.T) {
	db := NewSlidingTailBuffer()

	// 27-char chunk → combined = 27 > 16 → emit first 11, hold last 16.
	out1 := db.SanitizeStreamChunk("Hello, world! This is fine.")
	if out1 != "Hello, worl" {
		t.Errorf("expected first 11 chars emitted; got %q", out1)
	}
	if tail := db.SlidingTail(); tail != "d! This is fine." {
		t.Errorf("expected last 16 chars as tail; got %q", tail)
	}

	// Next chunk: combined = tail (16) + " Yes really." (13) = 29 chars
	// emit first 29-16=13, hold last 16. "d! This is fine." + " Yes really."
	// = "d! This is fine. Yes really." — wait that's 29 chars.
	// First 13 chars = "d! This is f" → that's not 13, "d! This is fine" is 15
	// Actually "d! This is fine." is 16 chars (including leading "d! "). 16+13=29.
	// Emit combined[:13] = "d! This is f". Hold combined[13:] = "ine. Yes really."
	out2 := db.SanitizeStreamChunk(" Yes really.")
	if out2 != "d! This is f" {
		t.Errorf("expected combined-safe output %q; got %q", "d! This is f", out2)
	}
}

// TestSlidingTailBuffer_ShortChunkKeptInBuffer asserts that a chunk shorter
// than 16 chars (or combined-with-tail shorter than 16) emits "" and stays
// buffered.
func TestSlidingTailBuffer_ShortChunkKeptInBuffer(t *testing.T) {
	db := NewSlidingTailBuffer()

	out := db.SanitizeStreamChunk("hi")
	if out != "" {
		t.Errorf("expected empty output for short chunk; got %q", out)
	}
	if db.SlidingTail() != "hi" {
		t.Errorf("expected tail=%q; got %q", "hi", db.SlidingTail())
	}

	out = db.SanitizeStreamChunk("yo")
	if out != "" {
		t.Errorf("expected empty output when combined < 16; got %q", out)
	}
	if db.SlidingTail() != "hiyo" {
		t.Errorf("expected tail=%q; got %q", "hiyo", db.SlidingTail())
	}
}

// TestSlidingTailBuffer_ToolUseLeakInOneChunk covers the simple leak path:
// a single chunk contains "[tool_use:" verbatim. Everything up to (not
// including) the prefix is emitted; from the prefix onward is suppressed.
func TestSlidingTailBuffer_ToolUseLeakInOneChunk(t *testing.T) {
	db := NewSlidingTailBuffer()

	out := db.SanitizeStreamChunk("here is some text [tool_use: bad_data] more")
	// "here is some text " (19 chars) is safe; the rest suppressed.
	if out != "here is some text " {
		t.Errorf("expected safe portion emitted; got %q", out)
	}
	if !db.IsSuppressing() {
		t.Error("expected buffer to be in suppression mode after leak")
	}

	// Subsequent chunks are dropped while suppressing.
	if out := db.SanitizeStreamChunk("ignored"); out != "" {
		t.Errorf("expected suppression to drop subsequent chunks; got %q", out)
	}

	// Reset clears suppression.
	db.Reset()
	if db.IsSuppressing() {
		t.Error("Reset should clear suppression flag")
	}
	if db.SlidingTail() != "" {
		t.Errorf("Reset should clear tail; got %q", db.SlidingTail())
	}
}

// TestSlidingTailBuffer_ToolUseLeakSplitAcrossChunks is the actual motivating
// scenario for the sliding tail: chunk1 ends with "[tool_" and chunk2 starts
// with "use: ...". The buffer MUST catch the leak on the boundary.
func TestSlidingTailBuffer_ToolUseLeakSplitAcrossChunks(t *testing.T) {
	db := NewSlidingTailBuffer()

	// First build up some safe content with a 16-char sliding tail.
	// Use exactly 16 chars "0123456789abcdef" so the buffer state is
	// known.
	db.SanitizeStreamChunk("0123456789abcdef") // tail = 16 chars, emit ""
	if db.SlidingTail() != "0123456789abcdef" {
		t.Fatalf("expected tail='0123456789abcdef'; got %q", db.SlidingTail())
	}

	// Now stream a chunk that ends with "[tool_" (16 chars including the
	// prefix-in-progress). Combined with the prior tail, the buffer
	// should NOT yet contain "[tool_use:" — only "[tool_".
	out := db.SanitizeStreamChunk("Hello world [tool_")
	_ = out // some text is emitted (the safe portion before "[tool_" begins)
	if db.IsSuppressing() {
		t.Error("should NOT be in suppression yet (only '[tool_' seen, not '[tool_use:')")
	}

	// Next chunk starts with "use: ..." which completes the leak
	// pattern. Combined = previous tail + last 16 of previous chunk + new
	// chunk → "[tool_use:" appears → suppression fires.
	out = db.SanitizeStreamChunk("use: name=foo")
	if out != "" {
		// The entire emitted text from the previous chunk up to and
		// including the leak's prefix-position is what gets flushed here.
		// The sanitizer returns combined[:idx] where idx is the offset
		// of [tool_use: in the combined text. Let's just verify it
		// doesn't contain "[tool_use" (catches accidental leak).
		if strings.Contains(out, "[tool_use") {
			t.Errorf("leak made it to output: %q", out)
		}
	}
	if !db.IsSuppressing() {
		t.Error("expected suppression after split-across-chunks leak")
	}
}

// TestSlidingTailBuffer_ResetMidStreamAfterSuppression ensures after a leak
// has been suppressed and then Reset is called, subsequent safe chunks
// stream normally.
func TestSlidingTailBuffer_ResetMidStreamAfterSuppression(t *testing.T) {
	db := NewSlidingTailBuffer()
	db.SanitizeStreamChunk("safe text [tool_use: leak") // triggers suppression
	if !db.IsSuppressing() {
		t.Fatal("suppression should be active")
	}

	db.Reset()

	out := db.SanitizeStreamChunk("0123456789abcdefHello")
	// 21-char combined → emit first 5 chars ("01234"), hold last 16.
	if out != "01234" {
		t.Errorf("expected clean stream after Reset, got %q (expected %q)", out, "01234")
	}
	if db.IsSuppressing() {
		t.Error("Reset should clear suppression flag")
	}
}

// TestSlidingTailBuffer_ConcurrentSafe stresses concurrent calls to ensure
// the mutex actually protects the tail. Run with -race to detect lost
// updates.
func TestSlidingTailBuffer_ConcurrentSafe(t *testing.T) {
	db := NewSlidingTailBuffer()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db.SanitizeStreamChunk("0123456789abcdefXYZ") // 20 chars
		}()
	}
	wg.Wait()

	if db.ChunkCount() != 16 {
		t.Errorf("expected 16 chunks processed; got %d", db.ChunkCount())
	}
	// Tail should be at most 16 chars (window size).
	if tail := db.SlidingTail(); len(tail) > 16 {
		t.Errorf("tail exceeds window: len=%d tail=%q", len(tail), tail)
	}
}

// TestSlidingTailBuffer_ResetOnFirstConstruction ensures a fresh buffer
// emits nothing and resets cleanly with no prior state.
func TestSlidingTailBuffer_ResetOnFirstConstruction(t *testing.T) {
	db := NewSlidingTailBuffer()
	if got := db.SlidingTail(); got != "" {
		t.Errorf("fresh buffer should have empty tail; got %q", got)
	}
	if db.IsSuppressing() {
		t.Error("fresh buffer should not be suppressing")
	}
	if db.ChunkCount() != 0 {
		t.Errorf("fresh buffer should have 0 chunks; got %d", db.ChunkCount())
	}
}
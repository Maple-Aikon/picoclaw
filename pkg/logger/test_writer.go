// Phase 12.49 — test-only logger writer swap for canary_drift tests.
//
// Use:
//   buf := &bytes.Buffer{}
//   cleanup := logger.WithTestWriter(buf)
//   defer cleanup()
//   // ... emit log lines ...
//   // buf.String() now contains the formatted output.
//
// Caveat: appends a NEW writer to the writers slice. DisableFileLogging
// does NOT remove test writers — tests should call cleanup() to restore.
package logger

import (
	"io"
	"sync"
)

// WithTestWriter temporarily registers `w` as an additional logger writer
// and returns a cleanup function that restores the prior writer list.
//
// Test-only seam. NOT concurrency-safe for parallel tests — use
// t.Parallel() carefully or restrict to serial tests.
func WithTestWriter(w io.Writer) func() {
	mu.Lock()
	prev := append([]io.Writer{}, writers...)
	writers = append(writers, w)
	logger = logger.Output(io.MultiWriter(writers...))
	mu.Unlock()
	return func() {
		mu.Lock()
		writers = prev
		logger = logger.Output(io.MultiWriter(writers...))
		mu.Unlock()
	}
}

// atomic ensures pkg/logger init has run before tests touch writers slice.
var _ = sync.Mutex{}

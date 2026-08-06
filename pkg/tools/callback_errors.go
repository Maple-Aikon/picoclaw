// Phase 12.50 §3.3 — error sentinels for callback registry.

package tools

import "errors"

// ErrLookupMissHandlerNil returned when RegisterLookupMissHandler called
// with nil handler. Mirrors hook_mount.go RegisterBuiltinHook precedent.
var ErrLookupMissHandlerNil = errors.New("pkg/tools: lookup miss handler is nil")

// ErrAlreadyRegistered returned when RegisterLookupMissHandler called twice.
// Double-register is a programming error (init runs once per process).
// Mirrors hook_mount.go RegisterBuiltinHook precedent.
var ErrAlreadyRegistered = errors.New("pkg/tools: lookup miss handler already registered")
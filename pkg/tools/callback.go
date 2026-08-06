// Phase 12.50 §3.3 — pkg/tools callback registry.
//
// Replaces the no-op `recordPhaseLookupMiss` stub in pkg/tools/phase_policy_telemetry.go.
// Both pkg/tools.ToolPolicyForPhase and pkg/agent.PhasePolicyFor must
// publish into the same miss stream (single source of truth for
// canary_drift telemetry).
//
// pkg/tools cannot import pkg/agent (import cycle: pkg/agent imports
// pkg/tools for ToolPolicyForPhase). Solution: callback registry with
// pkg/agent registering the handler at startup via init().
//
// Pattern mirrors `agentDebugEnabled atomic.Bool` in agent_debug.go:55
// (global read-only flag set once at process start). The handler is
// write-once + double-register panic guard, then called as a regular
// function (no locking on hot path after init).

package tools

import (
	"sync"
)

// LookupMissHandler is invoked when ToolPolicyForPhase (or any future
// tool-layer phase lookup) hits an unknown non-empty phase. Single
// argument = phase token (e.g., "bogus").
//
// Handler is called outside any lock (init() runs before goroutines,
// so the registration is "happens-before" any potential call).
type LookupMissHandler func(site, phase string)

// lookupMissHandlerMu protects the global singleton. Read path
// (getLookupMissHandler) takes RLock; write path (register) takes Lock.
// On hot path after init(), the handler is stored in a local variable
// so we don't take the RLock on every call.
var (
	lookupMissHandlerMu sync.RWMutex
	lookupMissHandler   LookupMissHandler
)

// RegisterLookupMissHandler installs the handler at process startup.
// Called from pkg/agent's init() — once per process. Double-register
// returns ErrAlreadyRegistered (not panic) to mirror hook_mount.go:60-77
// RegisterBuiltinHook pattern.
//
// Test seam: lookupMissHandlerResetForTest() clears the registered handler
// for parallel tests with t.Parallel().
func RegisterLookupMissHandler(h LookupMissHandler) error {
	if h == nil {
		return ErrLookupMissHandlerNil
	}
	lookupMissHandlerMu.Lock()
	defer lookupMissHandlerMu.Unlock()
	if lookupMissHandler != nil {
		return ErrAlreadyRegistered
	}
	lookupMissHandler = h
	return nil
}

// getLookupMissHandler returns the registered handler, or nil if none.
// Hot-path read takes RLock (no contention after init since init writes
// the handler before any goroutine starts).
func getLookupMissHandler() LookupMissHandler {
	lookupMissHandlerMu.RLock()
	defer lookupMissHandlerMu.RUnlock()
	return lookupMissHandler
}

// ResetLookupMissHandlerForTest clears the registered handler. Test-only
// seam — call from t.Cleanup() in parallel-safe tests.
//
// Exported (capital R) so cross-package tests (pkg/agent tests that
// register their own handler for isolation) can clean up.
func ResetLookupMissHandlerForTest() {
	lookupMissHandlerMu.Lock()
	defer lookupMissHandlerMu.Unlock()
	lookupMissHandler = nil
}

// publishLookupMiss invokes the registered handler if any. No-op when
// no handler installed (test environments, optional registration).
func publishLookupMiss(site, phase string) {
	h := getLookupMissHandler()
	if h != nil {
		h(site, phase)
	}
}
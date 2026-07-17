package tools

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

type CircuitState int

const (
	StateClosed   CircuitState = iota // Normal operation
	StateOpen                         // Circuit broken, fail fast
	StateHalfOpen                     // Retry timeout elapsed, allow one test call
)

// ToolFeedback is the structured outcome of recording a tool result into the
// circuit breaker. It tells the caller (ToolRegistry.ExecuteWithContext) what
// to append to ToolResult.ForLLM and whether the breaker just tripped.
//
// Status semantics:
//
//	StatusTransient — retryable, breaker is still Closed or HalfOpen. Hint
//	  tells the LLM what to do (retry, back off, escalate).
//	StatusValidationError — invalid input. The breaker is NEVER tripped by
//	  validation errors (per circuit-breaker-3-tier-errkind-semantics-toolfeedback-20260717
//	  Change 1: ErrInvalidInput is a contract violation, not a tool fault).
//	StatusBlocked — circuit is Open (or was just tripped). The hint is a
//	  direct system directive: stop calling this tool, surface to user.
//	StatusOK — success; failures reset and circuit closes (or stays closed).
type ToolFeedback struct {
	Status ToolFeedbackStatus
	// Message is the human-readable hint to append to ToolResult.ForLLM.
	// Empty for StatusOK.
	Message string
	// JustTripped is true when this RecordResult call is the one that
	// transitioned Closed/HalfOpen → Open. Registry uses this for the
	// once-per-trip event emission (idempotent across subsequent Open calls).
	JustTripped bool
}

// ToolFeedbackStatus is the categorical outcome of recording a tool result.
type ToolFeedbackStatus int

const (
	FeedbackStatusNone ToolFeedbackStatus = iota
	StatusOK
	StatusTransient
	StatusValidationError
	StatusBlocked
)

// CircuitBreaker prevents repeated execution of failing tools to save tokens and time.
type CircuitBreaker struct {
	mu               sync.Mutex
	name             string // tool name; populated by ToolRegistry on lazy allocate, empty for legacy callers
	state            CircuitState
	failures         int
	failureThreshold int
	recoveryTimeout  time.Duration
	openedAt         time.Time
	// lastErrorKind records the ErrorKind of the result that brought the
	// breaker to its current state. Used by ToolHealthContributor to tell
	// the LLM why a tool is unavailable (e.g. "transient/network" vs
	// "dependency down"). Empty when Closed.
	lastErrorKind ErrorKind
	// recoveryAttempts is the per-TryRecover probe budget. 0 means use
	// defaultRecoveryAttempts (2). Set to 1 to disable the second probe.
	recoveryAttempts int
}

// SetRecoveryAttempts configures the per-breaker recovery probe budget.
// Pass 0 to fall back to defaultRecoveryAttempts, 1 to disable the
// second probe (one wait + check, then give up).
func (cb *CircuitBreaker) SetRecoveryAttempts(n int) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.recoveryAttempts = n
}

// defaultRecoveryAttempts is the fallback for TryRecover when
// recoveryAttempts is unset (or 0). 2 = one wait + check, plus one
// follow-up after another recoveryTimeout window. This is intentionally
// small: recovery is a "is the upstream back yet?" probe, not a busy loop.
const defaultRecoveryAttempts = 2

// NewCircuitBreaker initializes a new CircuitBreaker with default thresholds.
// The breaker has no name; callers that want self-identifying breakers
// (e.g. for prompt health reporting) should use NewCircuitBreakerWithName.
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: 3,               // Break after 3 consecutive failures
		recoveryTimeout:  1 * time.Minute, // Wait 1 minute before retrying
		// recoveryAttempts left at 0 → falls back to defaultRecoveryAttempts.
	}
}

// NewCircuitBreakerWithName is like NewCircuitBreaker but records the tool
// name so later readers can attribute the breaker to a specific tool
// (used by ToolRegistry.OpenTools and the ToolHealthContributor).
func NewCircuitBreakerWithName(name string) *CircuitBreaker {
	cb := NewCircuitBreaker()
	cb.name = name
	return cb
}

// Name returns the tool name this breaker is scoped to, or "" when the
// breaker was created via NewCircuitBreaker() without a name.
func (cb *CircuitBreaker) Name() string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.name
}

// Snapshot returns a consistent read of (state, openedAt, failures,
// lastErrorKind). Callers (e.g. the prompt health contributor) use this to
// surface "tool unavailable" directives to the LLM without mutating breaker
// state.
func (cb *CircuitBreaker) Snapshot() (CircuitState, time.Time, int, ErrorKind) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state, cb.openedAt, cb.failures, cb.lastErrorKind
}

// LastErrorKind returns the ErrorKind that brought the breaker to its
// current state, or "" when the breaker is Closed. Safe to call from any
// goroutine; intended for ToolHealthContributor and OpenToolInfo.
func (cb *CircuitBreaker) LastErrorKind() ErrorKind {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.lastErrorKind
}

// breakerKey builds the composite map key used to scope a circuit breaker to
// a (channel, chatID, toolName) tuple. Callers that omit session context
// (channel == "" && chatID == "") fall back to the "_anon" namespace so they
// are isolated from real sessions and cannot silently trip a session breaker.
func breakerKey(channel, chatID, name string) string {
	if channel == "" && chatID == "" {
		return "_anon:" + name
	}
	return channel + ":" + chatID + ":" + name
}

// Allow returns true if the tool execution should proceed.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateOpen {
		if time.Since(cb.openedAt) > cb.recoveryTimeout {
			cb.state = StateHalfOpen // Allow one test execution
			return true
		}
		return false // Still open
	}
	return true
}

// Failures returns the current consecutive failure count.
func (cb *CircuitBreaker) Failures() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failures
}

// RecordResult updates the circuit breaker state based on the execution result.
// Returns a ToolFeedback describing what to surface to the LLM and whether the
// breaker just transitioned to Open (for once-per-trip event emission).
//
// Three-tier error semantics (plan circuit-breaker-3-tier-errkind-semantics-toolfeedback-20260717):
//
//	ErrInvalidInput  → ToolStatusValidationError. NEVER counts toward failures.
//	                   Resets failures if the breaker was already Open and we
//	                   somehow got here (defensive — Allow() should have blocked).
//	ErrDependencyDown → ToolStatusBlocked. Opens the breaker IMMEDIATELY
//	                   (regardless of failure count). JustTripped=true on the
//	                   transition from Closed/HalfOpen → Open.
//	ErrTransient / ErrTimeout → ToolStatusTransient. Increments failures; if
//	                   failures reach threshold, transitions Closed → Open
//	                   with JustTripped=true and ToolStatusBlocked + escalation
//	                   hint. Otherwise ToolStatusTransient with retry hint.
//	isError == false → ToolStatusOK. Resets failures=0, closes circuit
//	                   (HalfOpen→Closed), clears lastErrorKind.
//
// Event emission is NOT done here — callers (ToolRegistry) emit
// KindAgentToolBreakerTripped once per JustTripped=true transition to keep
// the breaker package decoupled from the event bus.
func (cb *CircuitBreaker) RecordResult(toolName string, isError bool, kind ErrorKind) ToolFeedback {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if !isError {
		// Success resets failures and closes the circuit
		cb.failures = 0
		cb.lastErrorKind = ""
		if cb.state != StateClosed {
			logger.InfoCF("tool", "Circuit breaker closed (recovered)", map[string]any{"tool": toolName})
		}
		cb.state = StateClosed
		return ToolFeedback{Status: StatusOK}
	}

	// Tier 3: validation errors do NOT count toward the breaker. A bad
	// argument is the LLM's mistake, not a tool fault. Returning a hint
	// (rather than silent success) keeps the LLM honest — it sees the
	// validation error message in ToolResult.ForLLM regardless.
	if kind == ErrInvalidInput {
		return ToolFeedback{
			Status:  StatusValidationError,
			Message: validationHint(),
		}
	}

	// Tier 2: dependency down opens the circuit immediately (regardless of
	// failure count). One 503 from upstream is enough — there's no point
	// burning another LLM call on the same dead endpoint.
	if kind == ErrDependencyDown {
		justTripped := cb.state != StateOpen
		if justTripped {
			logger.WarnCF("tool", "Circuit breaker opened (dependency down)", map[string]any{"tool": toolName})
			cb.state = StateOpen
			cb.openedAt = time.Now()
			cb.lastErrorKind = ErrDependencyDown
		}
		return ToolFeedback{
			Status:      StatusBlocked,
			Message:     dependencyDownHint(toolName),
			JustTripped: justTripped,
		}
	}

	// Tier 1: transient / timeout. Increment failures, possibly trip.
	cb.failures++
	cb.lastErrorKind = kind
	if cb.state == StateClosed && cb.failures >= cb.failureThreshold {
		logger.WarnCF("tool", "Circuit breaker opened (consecutive failures)", map[string]any{
			"tool":     toolName,
			"failures": cb.failures,
		})
		cb.state = StateOpen
		cb.openedAt = time.Now()
		return ToolFeedback{
			Status:      StatusBlocked,
			Message:     escalationHint(toolName),
			JustTripped: true,
		}
	} else if cb.state == StateHalfOpen {
		// Failed the test execution, open circuit again
		logger.WarnCF("tool", "Circuit breaker re-opened (half-open test failed)", map[string]any{"tool": toolName})
		cb.state = StateOpen
		cb.openedAt = time.Now()
		return ToolFeedback{
			Status:      StatusBlocked,
			Message:     escalationHint(toolName),
			JustTripped: true,
		}
	}
	// Below threshold — transient, retryable.
	return ToolFeedback{
		Status:  StatusTransient,
		Message: transientHint(toolName, cb.failures, cb.failureThreshold),
	}
}

// TryRecover runs a bounded recovery probe against the breaker. It waits for
// recoveryTimeout to elapse, then re-polls state. If the breaker is Closed or
// HalfOpen, recovery succeeded and we return nil. If we exhaust the budget
// without success, returns the last observed feedback so the caller can
// surface it.
//
// Uses pkg/agent/retry.BoundedRetry so this primitive composes with the
// circuit-breaker-3-tier-errkind recovery loop and any future retry callers.
// Default budget: 2 attempts (1 probe + 1 follow-up) over recoveryTimeout.
// 1 attempt means "just wait + check", 2 means "wait, check, optional second
// wait + check after another recoveryTimeout".
// TryRecover runs a bounded recovery probe against the breaker. It waits
// up to recoveryTimeout for the breaker to transition Closed/HalfOpen
// (Allow() flips on its own when enough time has elapsed since openedAt),
// re-checking at most maxRecoveryAttempts times. Returns nil on success,
// ctx.Err() on context cancellation, or a non-nil error after the budget is
// exhausted.
//
// NOTE: This deliberately does NOT import pkg/agent/retry.BoundedRetry
// because pkg/agent imports pkg/tools — importing the other direction
// would be a cycle. The loop is small enough that the duplication is
// cheaper than the import cycle. The semantic equivalent:
//
//	1. attempt 1: wait recoveryTimeout, re-poll
//	2. if still Open → attempt 2: wait again, re-poll
//	3. if still Open → OnExhausted-equivalent: return exhaustion error
//
// Default budget: 2 attempts (≈ 2 * recoveryTimeout wall-clock). Configured
// per-breaker via SetRecoveryAttempts; 1 disables the second probe.
func (cb *CircuitBreaker) TryRecover(ctx context.Context, toolName string) error {
	if cb == nil {
		return errors.New("circuit breaker is nil")
	}
	maxAttempts := cb.recoveryAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultRecoveryAttempts
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Cheap fast path: state may have already advanced.
		state, _, _, _ := cb.Snapshot()
		if state == StateClosed || state == StateHalfOpen {
			return nil
		}
		if attempt == maxAttempts {
			// Last attempt exhausted without recovery.
			logger.WarnCF("tool", "Circuit breaker recovery exhausted",
				map[string]any{"tool": toolName, "attempts": attempt})
			return fmt.Errorf("circuit breaker for %q did not recover after %d attempts", toolName, attempt)
		}
		// Wait the remainder of the recovery window before re-polling.
		wait := cb.recoveryTimeout
		_, openedAt, _, _ := cb.Snapshot()
		if elapsed := time.Since(openedAt); elapsed < wait {
			wait -= elapsed
		}
		logger.InfoCF("tool", "Circuit breaker recovery probe",
			map[string]any{"tool": toolName, "attempt": attempt, "wait_ms": wait.Milliseconds()})
		select {
		case <-time.After(wait):
			// Loop again; re-poll state.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// --- hint helpers ---
//
// These are the canonical strings the registry appends to ToolResult.ForLLM
// when a circuit-breaker event occurs. Centralizing them keeps the registry
// code small and the messaging consistent across validation / transient /
// blocked paths. Keep these messages terse — they go straight into the LLM's
// context budget.

// transientHint: tool failed transiently, below breaker threshold. LLM may
// retry after a short wait.
func transientHint(toolName string, failures, threshold int) string {
	return fmt.Sprintf("System: Tool %q failed (transient). You have %d more attempt(s) before this tool is temporarily locked — consider waiting a moment before retrying.",
		toolName, threshold-failures)
}

// escalationHint: tool just tripped the circuit breaker. Direct directive:
// stop calling, surface to user.
func escalationHint(toolName string) string {
	return fmt.Sprintf(
		"System: Tool %q is temporarily disabled (Circuit Open) due to %d consecutive failures. DO NOT attempt to call it again right now. You must inform the user that this tool is locked and explain why, then wait for the user's instructions on how to proceed.",
		toolName, failureThresholdDefaultForHint,
	)
}

// dependencyDownHint: upstream 503 / dead endpoint. Opens breaker on first
// occurrence. Even more forceful than escalationHint: this isn't our fault.
func dependencyDownHint(toolName string) string {
	return fmt.Sprintf(
		"System: Tool %q is temporarily disabled (dependency unavailable). DO NOT attempt to call it again right now. You must inform the user that this tool is locked and explain why, then wait for the user's instructions on how to proceed.",
		toolName,
	)
}

// validationHint: invalid arguments. Counts toward the consecutive-failures
// tally in the OLD code, which was wrong: a bad-args mistake from the LLM
// should not lock the tool. Plan Change 1 removes that count. This hint
// mirrors the existing "2 more attempts" warnings in registry.go.
func validationHint() string {
	return "System: Tool argument validation failed. Check the schema and retry with corrected arguments. (This does not count toward the tool's circuit-breaker limit.)"
}

// failureThresholdDefaultForHint is the default breaker threshold used in
// the escalation hint string. Hardcoded to match NewCircuitBreaker() so the
// hint stays accurate for default-config breakers. If you change
// failureThreshold, you should also update this constant.
const failureThresholdDefaultForHint = 3

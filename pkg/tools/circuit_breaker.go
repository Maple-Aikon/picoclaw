package tools

import (
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

// CircuitBreaker prevents repeated execution of failing tools to save tokens and time.
type CircuitBreaker struct {
	mu               sync.Mutex
	name             string // tool name; populated by ToolRegistry on lazy allocate, empty for legacy callers
	state            CircuitState
	failures         int
	failureThreshold int
	recoveryTimeout  time.Duration
	openedAt         time.Time
}

// NewCircuitBreaker initializes a new CircuitBreaker with default thresholds.
// The breaker has no name; callers that want self-identifying breakers
// (e.g. for prompt health reporting) should use NewCircuitBreakerWithName.
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: 3,               // Break after 3 consecutive failures
		recoveryTimeout:  1 * time.Minute, // Wait 1 minute before retrying
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

// Snapshot returns a consistent read of (state, openedAt, failures).
// Callers (e.g. the prompt health contributor) use this to surface
// "tool unavailable" directives to the LLM without mutating breaker state.
func (cb *CircuitBreaker) Snapshot() (CircuitState, time.Time, int) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state, cb.openedAt, cb.failures
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
func (cb *CircuitBreaker) RecordResult(toolName string, isError bool, kind ErrorKind) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if !isError {
		// Success resets failures and closes the circuit
		cb.failures = 0
		if cb.state != StateClosed {
			logger.InfoCF("tool", "Circuit breaker closed (recovered)", map[string]any{"tool": toolName})
		}
		cb.state = StateClosed
		return
	}

	// Dependency down opens the circuit immediately
	if kind == ErrDependencyDown {
		if cb.state != StateOpen {
			logger.WarnCF("tool", "Circuit breaker opened (dependency down)", map[string]any{"tool": toolName})
		}
		cb.state = StateOpen
		cb.openedAt = time.Now()
		return
	}

	// Failure case
	cb.failures++
	if cb.state == StateClosed && cb.failures >= cb.failureThreshold {
		logger.WarnCF("tool", "Circuit breaker opened (consecutive failures)", map[string]any{
			"tool":     toolName,
			"failures": cb.failures,
		})
		cb.state = StateOpen
		cb.openedAt = time.Now()
	} else if cb.state == StateHalfOpen {
		// Failed the test execution, open circuit again
		logger.WarnCF("tool", "Circuit breaker re-opened (half-open test failed)", map[string]any{"tool": toolName})
		cb.state = StateOpen
		cb.openedAt = time.Now()
	}
}

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
	state            CircuitState
	failures         int
	failureThreshold int
	recoveryTimeout  time.Duration
	openedAt         time.Time
}

// NewCircuitBreaker initializes a new CircuitBreaker with default thresholds.
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: 3,               // Break after 3 consecutive failures
		recoveryTimeout:  5 * time.Minute, // Wait 5 minutes before retrying
	}
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
			"tool": toolName,
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

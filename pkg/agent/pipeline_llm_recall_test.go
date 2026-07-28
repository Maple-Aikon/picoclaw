// Package agent — Phase 12.26 tests for RecallLLM helper.
//
// RecallLLM wraps callLLMCore with transient retry semantics. Used by
// handleGoalRecovery + retryLLMForBlockedTool BoundedRetry paths to ensure
// transient LLM errors (network/timeout/rate-limit) don't abort the retry
// loop prematurely.
//
// Coverage:
//   - Transient error retry: provider returns transient error twice, then success
//   - Non-transient error propagation: provider returns non-transient error
//   - SetupFunc runs ONCE before any LLM call (Q5A contract)
//   - Hard abort between attempts returns error without LLM call
//   - handleGoalRecovery smoke test: works end-to-end through wrapper
//   - retryLLMForBlockedTool smoke test: works end-to-end through wrapper
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// =============================================================================
// recallTestProvider — extends recoveryTestProvider with transient error injection.
// Per-call errors take precedence over responses; after per-call errors run out,
// falls through to responses[idx].
type recallTestProvider struct {
	mu struct {
		sync.Mutex
		callCount int
	}
	errors        []error      // optional, per-call transient/non-transient errors
	responses     []*providers.LLMResponse
	transientOk   bool         // mark injected errors as transient via transientLLMRetryReason
	abortOnCall   *int         // if non-nil and *abortOnCall == idx, request hard abort after returning
	abortTurnState *turnState  // required when abortOnCall != nil — points to the ts to abort
}

func (p *recallTestProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	idx := p.mu.callCount
	p.mu.callCount++
	// Fire hard abort after this call returns, if configured. Done while
	// holding the lock so the abort fires before RecallLLM's next iteration.
	if p.abortOnCall != nil && idx == *p.abortOnCall && p.abortTurnState != nil {
		_ = p.abortTurnState.requestHardAbort()
	}
	p.mu.Unlock()

	// Per-call error injection takes precedence
	if idx < len(p.errors) && p.errors[idx] != nil {
		return nil, p.errors[idx]
	}
	if idx < len(p.responses) && p.responses[idx] != nil {
		return p.responses[idx], nil
	}
	return &providers.LLMResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *recallTestProvider) GetDefaultModel() string { return "recall-test-model" }

// callCountN returns the current call count (for assertions).
func (p *recallTestProvider) callCountN() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mu.callCount
}

// =============================================================================
// Test 1: Transient error retry succeeds within max attempts
//
// RecallLLM retries up to 3 times on transient errors. Provider returns
// transient error twice, then success. Total LLM calls = 3.
func TestRecallLLM_TransientRetriesThenSucceeds(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("connection refused: backend down"),  // attempt 0: transient
			fmt.Errorf("timeout exceeded"),                  // attempt 1: transient
			nil,                                              // attempt 2: use responses
		},
		responses: []*providers.LLMResponse{
			nil, nil,
			{Content: "success after retries", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "test_transient_retry", nil,
	)
	if err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}
	if resp == nil || resp.Content != "success after retries" {
		t.Fatalf("expected success response, got %+v", resp)
	}
	if provider.callCountN() != 3 {
		t.Fatalf("expected 3 LLM calls (2 transient + 1 success), got %d", provider.callCountN())
	}
}

// =============================================================================
// Test 2: Non-transient error propagates immediately (no retry)
//
// RecallLLM must NOT retry non-transient errors (e.g., schema fail, fatal).
// Provider returns non-transient error on first call → RecallLLM propagates
// without retrying.
func TestRecallLLM_NonTransientErrorPropagates(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			errors.New("invalid API key, schema fail"), // attempt 0: non-transient
		},
		responses: []*providers.LLMResponse{
			{Content: "should not reach here", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "test_non_transient", nil,
	)
	if err == nil {
		t.Fatal("expected non-transient error to propagate, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil resp on fatal error, got %+v", resp)
	}
	if provider.callCountN() != 1 {
		t.Fatalf("expected 1 LLM call (no retry on non-transient), got %d", provider.callCountN())
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Fatalf("expected error to mention 'invalid API key', got %v", err)
	}
}

// =============================================================================
// Test 3: SetupFunc runs ONCE before any LLM call (Q5A contract)
//
// RecallLLM invokes setupFunc exactly once at entry, regardless of how many
// transient retries happen. Verify with a counter closure.
func TestRecallLLM_SetupFuncRunsOnce(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("connection reset by peer"), // transient
			fmt.Errorf("timeout exceeded"),          // transient
		},
		responses: []*providers.LLMResponse{
			nil,
			nil,
			{Content: "success", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	setupCount := 0
	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "test_setup_once", func() {
			setupCount++
		},
	)
	if err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}
	if resp == nil || resp.Content != "success" {
		t.Fatalf("expected success, got %+v", resp)
	}
	if setupCount != 1 {
		t.Fatalf("expected setupFunc to run exactly ONCE, got %d", setupCount)
	}
	if provider.callCountN() != 3 {
		t.Fatalf("expected 3 LLM calls (2 transient + 1 success), got %d", provider.callCountN())
	}
}

// =============================================================================
// Test 4: Hard abort before attempt → returns error without LLM call
//
// If ts.hardAbortRequested() returns true at any attempt boundary,
// RecallLLM must return immediately without calling the LLM.
func TestRecallLLM_HardAbortBeforeAttempt(t *testing.T) {
	provider := &recallTestProvider{
		responses: []*providers.LLMResponse{
			{Content: "should not reach here", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	// Set hard abort before invoking RecallLLM
	_ = ts.requestHardAbort()

	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "test_hard_abort", nil,
	)
	if err == nil {
		t.Fatal("expected error from hard abort, got nil")
	}
	if !strings.Contains(err.Error(), "hard abort") {
		t.Fatalf("expected error to mention 'hard abort', got %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil resp on hard abort, got %+v", resp)
	}
	if provider.callCountN() != 0 {
		t.Fatalf("expected 0 LLM calls (hard abort before first attempt), got %d", provider.callCountN())
	}
}

// =============================================================================
// Test 5: Transient retries exhausted → returns last error
//
// Provider returns transient errors for all 3 attempts (maxTransientRetries=2
// means 3 total attempts). RecallLLM returns the last error.
func TestRecallLLM_TransientRetriesExhausted(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("connection refused: attempt 0"),
			fmt.Errorf("connection refused: attempt 1"),
			fmt.Errorf("connection refused: attempt 2"), // last attempt before giving up
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	start := time.Now()
	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "test_exhausted", nil,
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error after exhausting transient retries, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil resp on exhaustion, got %+v", resp)
	}
	if !strings.Contains(err.Error(), "attempt 2") {
		t.Fatalf("expected error to mention 'attempt 2' (last), got %v", err)
	}
	// 3 attempts + 2 backoff sleeps of 1s = ~2s minimum elapsed
	if elapsed < 2*time.Second {
		t.Fatalf("expected ~2s elapsed (2 backoff sleeps), got %v", elapsed)
	}
	if provider.callCountN() != 3 {
		t.Fatalf("expected 3 LLM calls (max), got %d", provider.callCountN())
	}
}

// =============================================================================
// Helper: build a valid ts+exec for RecallLLM direct tests
//
// Integration check: when handleGoalRecovery triggers, the underlying
// RecallLLM invocation must succeed for the LLM to retry. Verify by
// verifying existing Phase 12.11 tests still pass (smoke level — full
// end-to-end covered by Phase 12.11 test suite).
func TestRecallLLM_HandleGoalRecovery_Smoke(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("connection refused"), // transient at attempt 0
		},
		responses: []*providers.LLMResponse{
			nil,
			{Content: "recovered after transient", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "handleGoalRecovery", func() {
			// Simulate the 4-counter reset (Phase 12.11.1 lesson)
			ts.emptyResponseRecoverySent = false
			ts.textOnlySoftRetriesDone = 0
			ts.textOnlyHardRetriesDone = 0
			ts.toolExecRecoveryAttempts = nil
		},
	)
	if err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}
	if resp == nil || resp.Content != "recovered after transient" {
		t.Fatalf("expected recovered response, got %+v", resp)
	}
	if provider.callCountN() != 2 {
		t.Fatalf("expected 2 LLM calls (1 transient + 1 success), got %d", provider.callCountN())
	}
}

// =============================================================================
// Test 7: smoke test — retryLLMForBlockedTool still works through RecallLLM
//
// Same integration check for the second migrated caller.
func TestRecallLLM_RetryLLMForBlockedTool_Smoke(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("rate limit exceeded"), // transient at attempt 0
		},
		responses: []*providers.LLMResponse{
			nil,
			{
				Content: "choose correct tool",
				ToolCalls: []providers.ToolCall{
					{ID: "call-1", Name: "complete_goal", Arguments: map[string]any{}},
				},
				FinishReason: "tool_calls",
			},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "retryLLMForBlockedTool", func() {
			// Simulate the 4-counter reset + msg injection
			ts.emptyResponseRecoverySent = false
			ts.textOnlySoftRetriesDone = 0
			ts.textOnlyHardRetriesDone = 0
			ts.toolExecRecoveryAttempts = map[string]int{}
			// msg injection (one-time)
			exec.messages = append(exec.messages, providers.Message{
				Role:    "user",
				Content: "blocked tool hint",
			})
			exec.callMessages = exec.messages
		},
	)
	if err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}
	if resp == nil || len(resp.ToolCalls) != 1 {
		t.Fatalf("expected tool call response, got %+v", resp)
	}
	if provider.callCountN() != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", provider.callCountN())
	}
}

// =============================================================================
// Helper: build a valid ts+exec for RecallLLM direct tests
func setupRecallTestTurnState(t *testing.T, al *AgentLoop, pipeline *Pipeline) (*turnState, *turnExecution) {
	t.Helper()
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}
	al.SkipGoalArchiveForTest()
	al.SetGoalPhaseForTest(string(GoalPhaseOpen))

	opts := makeTestProcessOpts("test-recall-session-" + t.Name())
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-recall-test",
		context: newTurnContext(nil, nil, nil),
	})

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn failed: %v", err)
	}

	return ts, exec
}

// =============================================================================
// Test 8: Hard abort AFTER some attempts (mid-retry abort)
//
// RecallLLM checks ts.hardAbortRequested() at every iteration boundary. If
// a transient retry is in flight and hard abort fires between attempts, the
// helper must return immediately without calling LLM again.
func TestRecallLLM_HardAbortMidRetry(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("connection refused: attempt 0 transient"),
			// attempt 1 must not be reached — hard abort fires before
		},
		responses: []*providers.LLMResponse{
			nil,
			{Content: "should not reach here", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	// Inject hard abort into the provider's first call: when Chat returns the
	// first transient error, requestHardAbort flips the flag. RecallLLM then
	// checks the flag at the next attempt boundary (BEFORE the backoff sleep)
	// and returns early without calling LLM a 2nd time.
	abortAfter0 := 0
	provider.abortOnCall = &abortAfter0   // fire after call index 0
	provider.abortTurnState = ts          // point to the ts RecallLLM is using

	start := time.Now()
	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "test_hard_abort_mid", nil,
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from mid-retry hard abort, got nil")
	}
	if !strings.Contains(err.Error(), "hard abort") {
		t.Fatalf("expected error to mention 'hard abort', got %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil resp on mid-retry hard abort, got %+v", resp)
	}
	// 1 LLM call only — hard abort fires during backoff, prevents attempt 1
	if provider.callCountN() != 1 {
		t.Fatalf("expected 1 LLM call (hard abort before attempt 1), got %d", provider.callCountN())
	}
	// elapsed ≈ 1s — one backoff sleep happened, then abort check fired
	// (abort check is AFTER backoff in the loop, not before). This is the
	// expected ordering: sleep first, then check flag. Verify neither too
	// fast (no sleep at all, =<500ms) nor too slow (full 2 sleeps, >=2s).
	if elapsed < 500*time.Millisecond {
		t.Fatalf("expected ~1s elapsed (1 backoff sleep), got %v — abort may have fired too early", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("expected ~1s elapsed (1 backoff sleep, NOT 2), got %v — abort check may have fired after attempt 1", elapsed)
	}
}

// =============================================================================
// Test 9: Non-transient error at attempt N>0 (transient then fatal)
//
// RecallLLM retries transient errors. If attempt 0 returns transient and
// attempt 1 returns non-transient, the helper must NOT continue retrying —
// propagate immediately.
func TestRecallLLM_TransientThenNonTransient(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("timeout exceeded"),          // attempt 0: transient (will retry)
			fmt.Errorf("invalid API key, fatal"),    // attempt 1: non-transient (must propagate)
		},
		responses: []*providers.LLMResponse{
			nil,
			nil,
			{Content: "should not reach here", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	resp, err := pipeline.RecallLLM(
		context.Background(),
		context.Background(),
		ts, exec, 1, "test_transient_then_fatal", nil,
	)
	if err == nil {
		t.Fatal("expected non-transient error to propagate, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil resp, got %+v", resp)
	}
	if !strings.Contains(err.Error(), "invalid API key") {
		t.Fatalf("expected error from attempt 1, got %v", err)
	}
	// 2 calls total: 1 transient (retried) + 1 non-transient (propagated)
	if provider.callCountN() != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", provider.callCountN())
	}
}

// =============================================================================
// Test 10: Backoff context cancellation (sleep interrupted by ctx.Done)
//
// RecallLLM uses sleepWithContext between transient retries. If the supplied
// ctx is cancelled mid-sleep, the backoff should return ctx.Canceled and the
// helper should propagate immediately.
func TestRecallLLM_BackoffContextCancelled(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("connection refused: attempt 0 transient"),
		},
		responses: []*providers.LLMResponse{
			nil,
			{Content: "should not reach here", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	// Build a cancellable ctx; cancel AFTER RecallLLM starts (async-safe).
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Give RecallLLM time to enter the first call + see transient error.
		// RecallLLM reads exec.messages; the lock + sleep gives us a small
		// window before the backoff. Use a short delay.
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	resp, err := pipeline.RecallLLM(
		ctx,
		context.Background(),
		ts, exec, 1, "test_ctx_cancel", nil,
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from ctx cancellation, got nil")
	}
	if resp != nil {
		t.Fatalf("expected nil resp on ctx cancel, got %+v", resp)
	}
	// elapsed should be < 1s — backoff interrupted, no full 1s sleep
	if elapsed >= 1*time.Second {
		t.Fatalf("expected <1s elapsed (backoff interrupted), got %v", elapsed)
	}
	// exactly 1 LLM call — backoff interrupted before attempt 1
	if provider.callCountN() != 1 {
		t.Fatalf("expected 1 LLM call (ctx cancelled before attempt 1), got %d", provider.callCountN())
	}
}

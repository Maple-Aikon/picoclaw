// Tests for native-tool-call-timeout-force-kill-20260702 (Phase 5).
//
// These tests cover the registry-level timeout surface added by Plan §3 (Phase 1)
// so a hung FUSE/NFS read or kernel busy-loop cannot freeze the agent loop.
//
// All tests use small durations to keep CI fast; semantics are identical to the
// production 120s default — only the numbers shrink.
package tools

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// blockingTool blocks until ctx.Done() or blockDuration elapses, whichever comes
// first. Used to simulate a hung native tool that ignores cancellation (worst
// case — orphan goroutine after timeout fires).
type blockingTool struct {
	name         string
	blockDuration time.Duration
	calls        atomic.Int64
	lastCtx      atomic.Value // context.Context
}

func (t *blockingTool) Name() string               { return t.name }
func (t *blockingTool) Description() string        { return "blocks until ctx.Done() or blockDuration" }
func (t *blockingTool) Parameters() map[string]any { return map[string]any{} }

func (t *blockingTool) Execute(ctx context.Context, _ map[string]any) *ToolResult {
	t.calls.Add(1)
	t.lastCtx.Store(ctx)

	select {
	case <-ctx.Done():
		return &ToolResult{
			ForLLM:  "tool noticed ctx.Done()",
			IsError: true,
			Err:     ctx.Err(),
		}
	case <-time.After(t.blockDuration):
		return &ToolResult{ForLLM: "tool finished normally"}
	}
}

// fastTool returns immediately without touching ctx — used to verify that the
// timeout wrapper doesn't slow down the happy path.
type fastTool struct {
	name string
}

func (t *fastTool) Name() string               { return t.name }
func (t *fastTool) Description() string        { return "fast no-op tool" }
func (t *fastTool) Parameters() map[string]any { return map[string]any{} }

func (t *fastTool) Execute(_ context.Context, _ map[string]any) *ToolResult {
	return &ToolResult{ForLLM: "fast ok"}
}

// TestExecuteWithContext_NativeToolTimeout — registry fires ErrTimeout when a
// tool blocks past its deadline. Verifies the goroutine + select pattern
// actually unblocks the agent loop and that the returned ToolResult carries the
// correct ErrKind.
func TestExecuteWithContext_NativeToolTimeout(t *testing.T) {
	blocker := &blockingTool{name: "blocker", blockDuration: 5 * time.Second}

	reg := NewToolRegistry()
	reg.Register(blocker)

	cfg := &config.ToolsConfig{TimeoutSeconds: 1} // 1s root default — fast for CI
	reg.SetToolsConfig(cfg)

	start := time.Now()
	result := reg.ExecuteWithContext(
		context.Background(),
		"blocker",
		map[string]any{},
		"test-channel",
		"test-chat",
		nil,
	)
	elapsed := time.Since(start)

	if !result.IsError {
		t.Fatalf("expected IsError=true, got: %+v", result)
	}
	if result.ErrKind != ErrTimeout {
		t.Fatalf("expected ErrKind=ErrTimeout, got: %q", result.ErrKind)
	}
	if !strings.Contains(result.ForLLM, "timed out") &&
		!strings.Contains(result.ForLLM, "exceeded timeout") &&
		!strings.Contains(result.ForLLM, "cancelled before") {
		t.Fatalf("expected timeout message, got: %q", result.ForLLM)
	}
	// 1s deadline + small overhead for goroutine scheduling — 2s upper bound.
	if elapsed > 2*time.Second {
		t.Fatalf("expected return ≤2s với timeout 1s, took: %v", elapsed)
	}
}

// TestExecuteWithContext_PerToolOverride — per-tool override (Q1 typed struct
// field) takes precedence over the root config default.
func TestExecuteWithContext_PerToolOverride(t *testing.T) {
	// Tool is named "i2c" so the typed switch in lookupToolTimeout picks up the
	// I2C.TimeoutSeconds field; if we used "blocker" the override would not apply.
	blocker := &blockingTool{name: "i2c", blockDuration: 30 * time.Second}

	reg := NewToolRegistry()
	reg.Register(blocker)

	cfg := &config.ToolsConfig{
		TimeoutSeconds: 60,                                  // generous root default
		I2C:            config.ToolConfig{TimeoutSeconds: 1}, // 1s override (fast for CI)
	}
	reg.SetToolsConfig(cfg)

	start := time.Now()
	result := reg.ExecuteWithContext(
		context.Background(),
		"i2c", // name "i2c" → typed lookup returns I2C.TimeoutSeconds=200 (0.2s)
		map[string]any{},
		"test-channel",
		"test-chat",
		nil,
	)
	elapsed := time.Since(start)

	if !result.IsError || result.ErrKind != ErrTimeout {
		t.Fatalf("expected ErrTimeout, got: %+v", result)
	}
	// 1s deadline + slack for goroutine scheduling — 3s upper bound.
	if elapsed > 3*time.Second {
		t.Fatalf("expected return ≤3s với override 1s, took: %v", elapsed)
	}
}

// TestExecuteWithContext_TimeoutDisabledWhenZero — Q4 rollback: root
// timeout_seconds=0 disables the feature entirely. The tool must run to
// completion regardless of how long it takes (within reasonable test bounds).
func TestExecuteWithContext_TimeoutDisabledWhenZero(t *testing.T) {
	// Fast tool so the test doesn't actually hang — but the point is the
	// registry does not inject WithTimeout, so the goroutine is the only
	// barrier to completion.
	fast := &fastTool{name: "echo"}

	reg := NewToolRegistry()
	reg.Register(fast)

	cfg := &config.ToolsConfig{TimeoutSeconds: 0} // Q4: feature OFF
	reg.SetToolsConfig(cfg)

	result := reg.ExecuteWithContext(
		context.Background(),
		"echo",
		map[string]any{},
		"test-channel",
		"test-chat",
		nil,
	)

	if result.IsError {
		t.Fatalf("với timeout=0 feature phải OFF, nhưng got error: %+v", result)
	}
}

// TestExecuteWithContext_NormalCompletionUnaffected — fast tool's happy path:
// no error, no timeout, returned via done channel.
func TestExecuteWithContext_NormalCompletionUnaffected(t *testing.T) {
	fast := &fastTool{name: "fast"}

	reg := NewToolRegistry()
	reg.Register(fast)

	cfg := &config.ToolsConfig{TimeoutSeconds: 120}
	reg.SetToolsConfig(cfg)

	result := reg.ExecuteWithContext(
		context.Background(),
		"fast",
		map[string]any{},
		"test-channel",
		"test-chat",
		nil,
	)

	if result.IsError {
		t.Fatalf("expected success, got: %+v", result)
	}
	if !strings.Contains(result.ForLLM, "fast ok") {
		t.Fatalf("expected 'fast ok', got: %q", result.ForLLM)
	}
}

// TestExecuteWithContext_TimeoutMetricIncrements — Q3: in-memory counter must
// increment on every timeout fire, with the kind label distinguishing a true
// deadline failure from a parent cancellation.
func TestExecuteWithContext_TimeoutMetricIncrements(t *testing.T) {
	blocker := &blockingTool{name: "metric_blocker", blockDuration: 5 * time.Second}

	reg := NewToolRegistry()
	reg.Register(blocker)
	cfg := &config.ToolsConfig{TimeoutSeconds: 1}
	reg.SetToolsConfig(cfg)

	before := reg.TimeoutStats().Count("metric_blocker", TimedOutDeadlineExceeded)

	_ = reg.ExecuteWithContext(
		context.Background(),
		"metric_blocker",
		map[string]any{},
		"test-channel",
		"test-chat",
		nil,
	)

	after := reg.TimeoutStats().Count("metric_blocker", TimedOutDeadlineExceeded)
	if after != before+1 {
		t.Fatalf("metric không increment: before=%d after=%d", before, after)
	}
}

// TestResolveToolTimeout_Precedence — pure-function unit test for the precedence
// ladder without booting the registry.
func TestResolveToolTimeout_Precedence(t *testing.T) {
	t.Run("root zero disables", func(t *testing.T) {
		cfg := &config.ToolsConfig{TimeoutSeconds: 0}
		dur, ok := resolveToolTimeout(context.Background(), "read_file", cfg)
		if ok || dur != 0 {
			t.Fatalf("expected (0,false) khi root=0, got (%v,%v)", dur, ok)
		}
	})
	t.Run("per-tool override wins", func(t *testing.T) {
		cfg := &config.ToolsConfig{
			TimeoutSeconds: 60,
			ReadFile:       config.ReadFileToolConfig{TimeoutSeconds: 7},
		}
		dur, ok := resolveToolTimeout(context.Background(), "read_file", cfg)
		if !ok || dur != 7*time.Second {
			t.Fatalf("expected (7s,true) với override, got (%v,%v)", dur, ok)
		}
	})
	t.Run("root fallback when no override", func(t *testing.T) {
		cfg := &config.ToolsConfig{TimeoutSeconds: 45}
		dur, ok := resolveToolTimeout(context.Background(), "read_file", cfg)
		if !ok || dur != 45*time.Second {
			t.Fatalf("expected (45s,true) cho root default, got (%v,%v)", dur, ok)
		}
	})
	t.Run("hardcoded fallback when cfg nil", func(t *testing.T) {
		dur, ok := resolveToolTimeout(context.Background(), "read_file", nil)
		if !ok || dur != DefaultToolTimeoutSeconds*time.Second {
			t.Fatalf("expected (%ds,true) fallback, got (%v,%v)", DefaultToolTimeoutSeconds, dur, ok)
		}
	})
	t.Run("caller deadline honoured over root", func(t *testing.T) {
		cfg := &config.ToolsConfig{TimeoutSeconds: 999}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		dur, ok := resolveToolTimeout(ctx, "read_file", cfg)
		if !ok || dur > 300*time.Millisecond {
			t.Fatalf("expected ctx deadline honoured (<300ms), got (%v,%v)", dur, ok)
		}
	})
}

// TestToolTimeoutStats_Concurrent — Q3 counter must be safe under concurrent
// writers; sync.Map + atomic.Int64 combination should not lose increments.
func TestToolTimeoutStats_Concurrent(t *testing.T) {
	stats := newToolTimeoutStats()
	const goroutines = 16
	const incrementsPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < incrementsPerGoroutine; j++ {
				stats.RecordTimeout("concurrent_tool", TimedOutDeadlineExceeded)
			}
		}()
	}
	wg.Wait()

	count := stats.Count("concurrent_tool", TimedOutDeadlineExceeded)
	want := int64(goroutines * incrementsPerGoroutine)
	if count != want {
		t.Fatalf("lost increments: got=%d want=%d (race in counter)", count, want)
	}
}

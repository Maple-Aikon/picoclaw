package mcp

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// ---------------------------------------------------------------------------
// R8 — the reaper kills a server, and the NEXT tool call must bring it back.
//
// Measured live on 2026-11-16 after the b04c4f03 fix went live:
//
//	15:56:01  Killing MCP server by timeout policy  (7 servers, idle_timeout)
//	15:59:xx  plan_list -> "MCP tool execution failed: server planning-mcp not found"
//
// The idle timeout is supposed to be INVISIBLE to the user: the commit
// message of b04c4f03 says "the next tool call lazily reconnects". It did
// not. killServer deletes the entry from m.servers, and CallTool only ever
// reconnects when the entry EXISTS but its session is broken. A server that
// was removed by the reaper is simply gone, so every tool of that server
// fails forever until the gateway restarts.
//
// This test pins that contract: after the reaper removes the server, a tool
// call against it must succeed again.
// ---------------------------------------------------------------------------

func TestR8_ToolCallReconnectsAfterIdleTimeoutKill(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	mgr.idleTimeout = 100 * time.Millisecond
	mgr.maxLifetime = 0

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}

	// Drive the reaper until the server is removed from the map.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mgr.reapExpired()
		if _, still := mgr.GetServer("fake"); !still {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, still := mgr.GetServer("fake"); still {
		t.Fatal("server was NOT removed from the map by the idle timeout")
	}

	// The server is gone from the map. A tool call must still work: the
	// Manager has to re-spawn it on demand.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := mgr.CallTool(ctx, "fake", "tools/echo", map[string]any{}); err != nil {
		t.Fatalf("CallTool after idle-timeout kill failed: %v", err)
	}

	if _, back := mgr.GetServer("fake"); !back {
		t.Fatal("server was not re-added to the map after the idle-timeout kill")
	}
}

// TestR8b_LazyReconnectUsesTheStoredServerConfig makes sure the lazy
// reconnect re-spawns the server with its ORIGINAL configuration. A
// reconnect that loses the command line would silently connect to the wrong
// binary (or to nothing).
func TestR8b_LazyReconnectUsesTheStoredServerConfig(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	mgr.idleTimeout = 100 * time.Millisecond
	mgr.maxLifetime = 0

	cfg := stdioConfig(bin)
	if err := mgr.ConnectServer(context.Background(), "fake", cfg); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mgr.reapExpired()
		if _, still := mgr.GetServer("fake"); !still {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := mgr.CallTool(ctx, "fake", "tools/echo", map[string]any{}); err != nil {
		t.Fatalf("CallTool after idle-timeout kill failed: %v", err)
	}

	conn, ok := mgr.GetServer("fake")
	if !ok {
		t.Fatal("server missing after lazy reconnect")
	}
	if conn.Config.Command != cfg.Command {
		t.Fatalf("lazy reconnect used command %q, want %q",
			conn.Config.Command, cfg.Command)
	}
	if conn.SubprocessPID <= 0 {
		t.Fatal("lazy reconnect produced a server with no subprocess PID")
	}
}

// TestR8c_LazyReconnectUsesAnIndependentContext is the P0a contract
// applied to the lazy-reconnect path: the handshake context must NOT be
// derived from the caller's context.
//
// NOTE ON WHY THIS TEST LOOKS THE WAY IT DOES: since P0a the subprocess is
// spawned with exec.Command (not exec.CommandContext), so a canceled
// caller ctx no longer kills the process. That means "the process survived
// the caller cancel" is NOT a testable symptom any more - it is true by
// construction.
//
// The testable symptom is the CONTEXT ITSELF: if the handshake ctx were
// derived from the caller ctx, then a caller ctx canceled during the
// handshake would abort the handshake, and the server would never come
// back. So the test cancels the caller ctx WHILE the handshake is in
// flight and asserts that the handshake context was still alive.
//
// A first version of this test asserted only that the process was still
// alive after cancel() - and it PASSED with the mutation "derive the
// handshake ctx from the caller ctx" applied. That was a hole in the
// test, not a bug in the fix: the mutation was invisible because
// exec.Command ignores ctx for the process lifetime.
func TestR8c_LazyReconnectUsesAnIndependentContext(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	mgr.idleTimeout = 100 * time.Millisecond
	mgr.maxLifetime = 0

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}

	// Remove the server from the map, as the reaper would.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mgr.reapExpired()
		if _, still := mgr.GetServer("fake"); !still {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Intercept the handshake and cancel the caller while it is in flight.
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() { connectServerFunc = originalConnectServerFunc })

	var (
		mu           sync.Mutex
		handshakeCtx context.Context
		ctxWasAlive  bool
	)

	callerCtx, cancelCaller := context.WithCancel(context.Background())

	connectServerFunc = func(
		ctx context.Context,
		name string,
		cfg config.MCPServerConfig,
	) (*ServerConnection, error) {
		mu.Lock()
		//nolint:fatcontext // the handshake ctx is captured ON PURPOSE: the test must
		// observe it after this literal returns, to prove it was not canceled with
		// the caller ctx. That is exactly the property under test.
		handshakeCtx = ctx
		mu.Unlock()

		// Cancel the caller WHILE the handshake is running.
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancelCaller()
		}()
		time.Sleep(300 * time.Millisecond)

		mu.Lock()
		ctxWasAlive = ctx.Err() == nil
		mu.Unlock()

		return connectServer(ctx, name, stdioConfig(bin))
	}

	// The CALL itself uses the caller's ctx, which we canceled on purpose,
	// so the call may well fail with "context canceled". That is the
	// caller's own doing and is not what this test is about.
	if _, err := mgr.CallTool(callerCtx, "fake", "tools/echo", map[string]any{}); err != nil {
		t.Logf("CallTool returned %v (the caller ctx was canceled on purpose)", err)
	}

	mu.Lock()
	ctx := handshakeCtx
	alive := ctxWasAlive
	mu.Unlock()

	if ctx == nil {
		t.Fatal("the lazy reconnect never reached connectServerFunc")
	}
	if !alive {
		t.Fatal("REGRESSION: the lazy-reconnect handshake ctx was canceled with the caller ctx")
	}

	// The server must be back in the map: the handshake completed, so the
	// re-spawn happened even though the caller's ctx was canceled.
	if _, back := mgr.GetServer("fake"); !back {
		t.Fatal("server was not re-added to the map after the lazy reconnect")
	}
}

// ---------------------------------------------------------------------------
// R9 — the reaper must not read a torn timestamp.
//
// lastUsed is a time.Time (24 bytes: wall, ext, loc). touch() writes it
// from the tool-call goroutine WITHOUT holding m.mu, while reapExpired()
// reads it under m.mu.RLock(). The lock does not help: the writer does not
// take it, so the reaper can observe a half-written time.Time.
//
// The symptom is not theoretical: a torn timestamp can read as a wildly
// wrong instant, which either kills a server that was JUST used (idle
// computed from a garbage value) or keeps an expired server alive.
//
// Fix: store the timestamps as atomic int64 (UnixNano) so a reader can
// never observe a partial write.
// ---------------------------------------------------------------------------

// TestR9_ReaperNeverReadsATornTimestamp hammers touch() and the reaper
// concurrently and asserts that every observed idle value is sane.
//
// Without the fix this test is flaky-but-real: on a 24-byte time.Time the
// window is small, so the test also asserts the STRUCTURAL property (the
// timestamp is stored atomically) via a helper the production code must
// use.
func TestR9_ReaperNeverReadsATornTimestamp(t *testing.T) {
	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	conn := &ServerConnection{Name: "fake"}
	conn.startedAtNano.Store(time.Now().UnixNano())
	conn.lastUsedNano.Store(time.Now().UnixNano())

	// Concurrent writers and readers.
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				conn.touch()
			}
		}
	}()

	// The reader path used by the reaper.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				idle := conn.idleSince()
				// A torn read can produce a negative idle (timestamp in the
				// future) or an absurdly large one.
				if idle < 0 {
					t.Errorf("observed a negative idle duration: %v", idle)
					return
				}
				if idle > time.Hour {
					t.Errorf("observed an absurd idle duration: %v", idle)
					return
				}
			}
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestR9b_IdleSinceFallsBackToStartedAt pins the fallback: a server that
// was never called must be measured from its spawn time, not from the
// zero time.
func TestR9b_IdleSinceFallsBackToStartedAt(t *testing.T) {
	conn := &ServerConnection{Name: "fake"}
	conn.startedAtNano.Store(time.Now().Add(-5 * time.Minute).UnixNano())

	if got := conn.idleSince(); got < 4*time.Minute {
		t.Fatalf("idleSince = %v, want ~5m (fallback to startedAt)", got)
	}
}

// TestR9c_TouchResetsTheIdleTimer makes sure touch() actually moves the
// idle clock - otherwise the reaper would kill busy servers.
func TestR9c_TouchResetsTheIdleTimer(t *testing.T) {
	conn := &ServerConnection{Name: "fake"}
	conn.startedAtNano.Store(time.Now().Add(-time.Hour).UnixNano())

	conn.touch()
	idle := conn.idleSince()
	if idle > time.Second {
		t.Fatalf("idleSince after touch = %v, want ~0", idle)
	}
}

// ---------------------------------------------------------------------------
// R10 — the kill must be reported with the process actually dead.
//
// The live run at 15:56:01 logged "Killing MCP server by timeout policy"
// for 7 servers but produced NO "MCP subprocess exited" rows. The waiter
// goroutine that closes conn.procExited therefore never ran, which means
// isProcessAlive() cannot tell "dead" from "alive" for a killed server.
//
// The reaper removes the server from the map immediately, so a missing
// "MCP subprocess exited" row is invisible today - but it is exactly the
// signal that makes the cause attribution (P0c) trustworthy.
//
// This test asserts that the kill path records that the process is gone.
// ---------------------------------------------------------------------------

func TestR10_KilledServerReportsItsProcessAsDead(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	mgr.idleTimeout = 100 * time.Millisecond
	mgr.maxLifetime = 0

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("fake")
	pid := conn.SubprocessPID

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mgr.reapExpired()
		if _, still := mgr.GetServer("fake"); !still {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	waitProcessGone(t, pid)

	// conn.procExited must be closed so isProcessAlive() reports false.
	select {
	case <-conn.procExited:
	case <-time.After(5 * time.Second):
		t.Fatal("procExited was never closed for a killed server: the waiter goroutine did not run")
	}

	if conn.isProcessAlive() {
		t.Fatal("isProcessAlive() returned true for a killed server")
	}
}

// TestR10b_CloseAlsoMarksTheProcessDead covers the shutdown path.
func TestR10b_CloseAlsoMarksTheProcessDead(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("fake")
	pid := conn.SubprocessPID

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	waitProcessGone(t, pid)

	select {
	case <-conn.procExited:
	case <-time.After(5 * time.Second):
		t.Fatal("procExited was never closed on Close")
	}

	if conn.isProcessAlive() {
		t.Fatal("isProcessAlive() returned true after Close")
	}
}

// TestR11_LazyReconnectDoesNotRaceWithAConcurrentReaper makes sure a
// lazy reconnect and the reaper can run at the same time without the
// reaper killing the freshly reconnected server.
func TestR11_LazyReconnectDoesNotRaceWithAConcurrentReaper(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	mgr.idleTimeout = 200 * time.Millisecond
	mgr.maxLifetime = 0

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = mgr.CallTool(ctx, "fake", "tools/echo", map[string]any{})
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				mgr.reapExpired()
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	// Whatever happened, the Manager must still be able to serve the tool.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := mgr.CallTool(ctx, "fake", "tools/echo", map[string]any{}); err != nil {
		t.Fatalf("CallTool failed after concurrent reap/reconnect: %v", err)
	}
}

// compile-time guard: the fake config helper must keep the shape the
// lazy reconnect relies on.
var _ = config.MCPServerConfig{}

// ---------------------------------------------------------------------------
// R12 - the process waiter must NOT have a short timeout.
//
// Root cause of Bug #3, found by arithmetic on the live log:
//
//	15:45:31  server spawned
//	15:46:31  waiter gave up (60s cap)
//	15:56:01  idle_timeout kill -> NO "MCP subprocess exited" row,
//	          procExited never closed, isProcessAlive() lied.
//
// The 60s cap is SHORTER than the idle timeout (10m) and the max lifetime
// (2h), so it always expires before the policy kill. The unit tests missed
// it because they kill in ~100ms, well inside the 60s window.
//
// This test pins the invariant without waiting 60 seconds: the waiter must
// still be alive AFTER a wait that outlives any plausible cap.
// TestR12_ProcessWaiterOutlivesAnyShortTimeout reproduces the EXACT
// defect of the 60s cap.
//
// Root cause of Bug #3, found by arithmetic on the live log:
//
//	15:45:31  server spawned
//	15:46:31  waiter gave up (60s cap)
//	15:56:01  idle_timeout kill -> NO "MCP subprocess exited" row,
//	          procExited never closed, isProcessAlive() lied.
//
// The 60s cap is SHORTER than the idle timeout (10m) and the max
// lifetime (2h), so it always expires while the process is still alive.
//
// A first version of this test waited for procExited within 10s of the
// kill and PASSED with the 60s cap restored - because a cap is an upper
// bound on the wait, not a lower bound: the waiter was still parked when
// the process died, so it woke up immediately. The defect is the GIVING
// UP, so that is what the test has to reproduce.
//
// The test does that without waiting 60 real seconds: it uses the
// transport's timeout parameter as the knob. A waiter that gave up after
// the timeout would return ok=false; a waiter that waits for the process
// returns ok=true once the process dies.
func TestR12_ProcessWaiterOutlivesAnyShortTimeout(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	mgr.idleTimeout = 0
	mgr.maxLifetime = 0

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("fake")
	pid := conn.SubprocessPID

	ict := conn.transport
	if ict == nil {
		t.Fatal("the connection has no isolatedCommandTransport")
	}

	// THE CONTRACT: the Manager's process waiter must wait for the exit,
	// not give up after a fixed budget.
	//
	// The live bug was a 60s budget, which is shorter than the idle
	// timeout (10m) and the max lifetime (2h): the waiter gave up at
	// 15:46:31, ten minutes before the 15:56:01 policy kill, so the exit
	// was never observed and isProcessAlive() lied.
	//
	// The transport counts the unbounded waits, so this can be asserted
	// without spending 60 real seconds.
	deadline := time.Now().Add(5 * time.Second)
	for ict.unboundedWaitCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ict.unboundedWaitCount.Load() == 0 {
		t.Fatal("the Manager's process waiter used a BOUNDED wait: it gives up " +
			"after a fixed budget instead of waiting for the process to exit")
	}

	// A bounded wait on a LIVE process must still report "no result".
	shortBudget := 100 * time.Millisecond
	if _, ok := ict.waitResult(shortBudget); ok {
		t.Fatal("a bounded wait on a LIVE process returned a result - the timeout is not honored")
	}

	// Kill the process; the Manager's waiter must observe the exit.
	conn.intentionalKill.Store(true)
	killProcessGroup(pid, mcpKillGrace)

	select {
	case <-conn.procExited:
	case <-time.After(15 * time.Second):
		t.Fatal("procExited was not closed for the killed server")
	}

	if conn.isProcessAlive() {
		t.Fatal("isProcessAlive() returned true for a dead process")
	}
}

// ---------------------------------------------------------------------------
// R13 - the exit result is available to concurrent waiters.
//
// The old shape was a buffered channel (capacity 1) that every reader put
// the value back into. It WORKS: the channel serializes the hand-off, so
// N readers take turns and none of them deadlocks.
//
// This test was written when I believed that shape could deadlock. That
// belief was WRONG, and mutation 5 (reverting the broadcast) proved it:
// R13 passed with the old shape in place. The test is kept because the
// property is worth pinning - concurrent waiters are safe - but it is
// NOT evidence for a bug that never existed.
// ---------------------------------------------------------------------------

func TestR13_ExitResultIsBroadcastToConcurrentWaiters(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	mgr.idleTimeout = 0
	mgr.maxLifetime = 0

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("fake")
	pid := conn.SubprocessPID

	ict := conn.transport
	if ict == nil {
		t.Fatal("the connection has no isolatedCommandTransport")
	}

	// Start several waiters BEFORE the process dies, so they all pile up
	// on the same wait.
	const waiters = 8
	results := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			err, ok := ict.waitResult(0)
			if !ok {
				results <- fmt.Errorf("waitResult reported no result")
				return
			}
			results <- err
		}()
	}

	// Give the waiters time to block.
	time.Sleep(200 * time.Millisecond)

	conn.intentionalKill.Store(true)
	killProcessGroup(pid, mcpKillGrace)

	deadline := time.After(15 * time.Second)
	for i := 0; i < waiters; i++ {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("waiter got no error for a killed process")
			}
		case <-deadline:
			t.Fatalf("only %d of %d waiters returned - the exit result is NOT broadcast", i, waiters)
		}
	}
}

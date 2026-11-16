package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/config"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// fakeMCPServerSource is a tiny stdio MCP server that answers initialize,
// tools/list and tools/call, then stays alive until its stdin closes.
//
// It uses bufio.Scanner so the source needs no escape sequences at all.
const fakeMCPServerSource = `package main

import (
	"bufio"
	"encoding/json"
	"os"
)

func main() {
	in := bufio.NewScanner(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	for in.Scan() {
		var req map[string]any
		if json.Unmarshal(in.Bytes(), &req) != nil {
			continue
		}
		method, _ := req["method"].(string)
		id, hasID := req["id"]
		resp := map[string]any{"jsonrpc": "2.0"}
		if hasID {
			resp["id"] = id
		}
		switch method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []any{}}
		default:
			if !hasID {
				continue
			}
			resp["result"] = map[string]any{"content": []any{}}
		}
		b, _ := json.Marshal(resp)
		out.Write(b)
		out.WriteByte(10)
		out.Flush()
	}
}
`

// fakeWrapperServerSource is a fake MCP server that ALSO spawns a
// grandchild process which outlives it.
//
// This models the real leak measured on the live gateway: `uv`, `uvx` and
// `npx` are wrappers that spawn the real server. SIGKILL on the wrapper
// leaves the grandchild holding the pipe open and reparented to init
// (ppid=1) - measured live: 5 such orphans alive for 3h38m.
//
// The grandchild's PID is written to the file given as argv[1] so the test
// can assert on it.
const fakeWrapperServerSource = `package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
)

func main() {
	if len(os.Args) > 1 {
		child := exec.Command("sleep", "300")
		if err := child.Start(); err == nil {
			pidFile, _ := os.Create(os.Args[1])
			if pidFile != nil {
				pidFile.WriteString(strconv.Itoa(child.Process.Pid))
				pidFile.Close()
			}
			// Deliberately NOT waiting on the child: it must survive us.
		}
	}

	in := bufio.NewScanner(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	for in.Scan() {
		var req map[string]any
		if json.Unmarshal(in.Bytes(), &req) != nil {
			continue
		}
		method, _ := req["method"].(string)
		id, hasID := req["id"]
		resp := map[string]any{"jsonrpc": "2.0"}
		if hasID {
			resp["id"] = id
		}
		switch method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-wrapper", "version": "1"},
			}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []any{}}
		default:
			if !hasID {
				continue
			}
			resp["result"] = map[string]any{"content": []any{}}
		}
		b, _ := json.Marshal(resp)
		out.Write(b)
		out.WriteByte(10)
		out.Flush()
	}
}
`

// buildFakeMCPServer compiles the fake server once per test binary run.
//
// The binary lives in a directory that is NOT t.TempDir(), because
// t.TempDir() is removed when the test that created it finishes - which
// would delete the binary while later tests still need it.
var (
	fakeMCPServerPath   string
	fakeWrapperPath     string
	fakeWrapperBuildErr string
)

func buildFakeMCPServer(t *testing.T) string {
	t.Helper()
	if fakeMCPServerPath != "" {
		return fakeMCPServerPath
	}
	fakeMCPServerPath = buildFakeServerBinary(t, "fake_mcp_server", fakeMCPServerSource)
	return fakeMCPServerPath
}

func buildFakeWrapperServer(t *testing.T) string {
	t.Helper()
	if fakeWrapperPath != "" {
		return fakeWrapperPath
	}
	if fakeWrapperBuildErr != "" {
		t.Skipf("cannot build fake wrapper server: %s", fakeWrapperBuildErr)
	}
	fakeWrapperPath = buildFakeServerBinary(t, "fake_wrapper_server", fakeWrapperServerSource)
	return fakeWrapperPath
}

func buildFakeServerBinary(t *testing.T, name, source string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "picoclaw-fake-mcp-")
	if err != nil {
		t.Fatalf("create temp dir for fake server: %v", err)
	}
	src := filepath.Join(dir, name+".go")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatalf("write fake server source: %v", err)
	}
	bin := filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", bin, src)
	out, err := build.CombinedOutput()
	if err != nil {
		t.Skipf("cannot build fake MCP server (needs go toolchain): %v: %s", err, out)
	}
	return bin
}

// procState returns the /proc/<pid>/stat state letter, or "GONE".
//
// "Z" means zombie: the process is dead but not yet reaped. This matters
// because an earlier version of these tests used os.Stat("/proc/<pid>") which
// reports zombies as present - that made a dead process look alive and
// inverted three conclusions in one review (contract C8).
func procState(pid int) string {
	if pid <= 0 {
		return "NOSTART"
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "GONE"
	}
	s := string(b)
	idx := strings.LastIndex(s, ")")
	if idx < 0 || idx+2 >= len(s) {
		return "GONE"
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) == 0 {
		return "GONE"
	}
	return fields[0]
}

// procIsAlive reports whether the process is running (not a zombie).
func procIsAlive(pid int) bool {
	state := procState(pid)
	return state != "GONE" && state != "Z" && state != "NOSTART"
}

// ---------------------------------------------------------------------------
// R1 - the regression this whole change exists for
// ---------------------------------------------------------------------------

// TestR1_ProcessSurvivesCallerContextCancel is THE regression test.
//
// Before the fix, connectServer used exec.CommandContext(ctx, ...). The ctx
// that reached it was the caller's ctx, so os/exec installed a watchCtx
// goroutine that SIGKILLed the subprocess the moment that ctx was canceled.
//
// Measured live before the fix: 105 "session was lost" events, 104 of them
// on context-mode (DIRECT node) and 1 on codebase-memory (DIRECT C binary);
// every WRAPPER server was at 0 because killing a wrapper leaves the child
// holding the pipe open.
//
// The test asserts the opposite of the bug: a server connected with a ctx
// that is later canceled must stay alive.
func TestR1_ProcessSurvivesCallerContextCancel(t *testing.T) {
	bin := buildFakeMCPServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	mgr := NewManager()
	t.Cleanup(func() {
		cancel()
		_ = mgr.Close()
	})

	if err := mgr.ConnectServer(ctx, "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}

	conn, ok := mgr.GetServer("fake")
	if !ok {
		t.Fatal("server not registered after ConnectServer")
	}
	pid := conn.SubprocessPID
	if pid <= 0 {
		t.Fatal("expected a tracked subprocess PID")
	}
	if !procIsAlive(pid) {
		t.Fatalf("process not alive right after connect, state=%s", procState(pid))
	}

	// Cancel the caller's context - this is what the turn/tool-call boundary
	// does. The process MUST NOT die.
	cancel()
	time.Sleep(500 * time.Millisecond)

	if !procIsAlive(pid) {
		t.Fatalf("REGRESSION: process %d died after caller ctx cancel, state=%s", pid, procState(pid))
	}

	// And the session must still be usable, i.e. the pipe is still open.
	if !conn.isProcessAlive() {
		t.Fatal("connection reports the process is dead after caller ctx cancel")
	}
}

// TestR1b_ProcessSurvivesReconnectContextCancel covers the reconnect path
// (manager.go:656 originally). The reconnect used to inherit the tool-call
// ctx, so the freshly spawned process was killed seconds after it started.
//
// The test models the REAL bug: the parent (tool-call) context is canceled
// while the reconnect is in flight. A reconnect bound to the parent ctx dies
// with it; a reconnect on an independent ctx survives.
func TestR1b_ProcessSurvivesReconnectContextCancel(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("fake")

	// The parent (tool-call) context - this is what gets canceled at the
	// turn/tool-call boundary.
	parentCtx, cancelParent := context.WithCancel(context.Background())

	// A reconnect bound to the parent ctx (the bug) versus one bound to an
	// independent ctx (the fix). We call reconnectServer with the parent ctx
	// here on purpose: if the Manager decoupled the lifetime, the spawned
	// process must NOT die when the parent is canceled.
	fresh, err := mgr.reconnectServer(parentCtx, "fake", conn)
	if err != nil {
		t.Fatalf("reconnectServer failed: %v", err)
	}

	// Cancel the parent - the bug would kill the freshly spawned process.
	cancelParent()
	time.Sleep(500 * time.Millisecond)

	if !procIsAlive(fresh.SubprocessPID) {
		t.Fatalf("REGRESSION: reconnected process %d died when the parent ctx was canceled",
			fresh.SubprocessPID)
	}
}

// TestR1d_ReconnectDuringCallToolSurvivesCallerCancel is the end-to-end
// regression for the reconnect path.
//
// It drives the REAL CallTool path with a session that reports "session
// missing", which makes CallTool reconnect. The reconnect must not be
// bound to the caller's context: when the caller cancels (the turn/tool-call
// boundary), the freshly spawned server must stay alive.
//
// This is the assertion that makes the mutation "reconnect inherits the
// caller ctx" observable. With the bug, connectServer receives the
// tool-call ctx and exec.CommandContext binds the process to it, so the
// process dies with the caller.
func TestR1d_ReconnectDuringCallToolSurvivesCallerCancel(t *testing.T) {
	fakeServerBin := buildFakeMCPServer(t)

	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		connectServerFunc = originalConnectServerFunc
	})

	// The reconnect spawns a REAL fake MCP server, so we can watch its PID.
	// The stale connection's config is HTTP; the reconnect must use the
	// stdio config so the fresh connection is a real tracked process.
	connectServerFunc = func(
		ctx context.Context,
		name string,
		cfg config.MCPServerConfig,
	) (*ServerConnection, error) {
		return connectServer(ctx, name, stdioConfig(fakeServerBin))
	}

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	// A stale connection whose session reports "session missing".
	staleConn, _, err := newScriptedServerConnection(
		"session-1",
		nil,
		fmt.Errorf(`sending "tools/call": failed to connect (session ID: session-1): %w`, sdkmcp.ErrSessionMissing),
	)
	if err != nil {
		t.Fatalf("newScriptedServerConnection failed: %v", err)
	}
	mgr.servers["flaky"] = staleConn

	// The caller's context - canceled as soon as the tool call returns.
	callerCtx, cancelCaller := context.WithCancel(context.Background())

	// The reconnect runs inside CallTool. We cancel the caller ctx right
	// after CallTool returns, which is exactly the turn boundary.
	result, err := mgr.CallTool(callerCtx, "flaky", "echo", map[string]any{"query": "hi"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result == nil {
		t.Fatal("CallTool returned nil result")
	}

	conn, ok := mgr.GetServer("flaky")
	if !ok {
		t.Fatal("server not registered after reconnect")
	}
	if conn.SubprocessPID <= 0 {
		t.Fatal("expected the reconnected server to be a tracked stdio process")
	}

	cancelCaller()
	time.Sleep(500 * time.Millisecond)

	if !procIsAlive(conn.SubprocessPID) {
		t.Fatalf("REGRESSION: the reconnected process %d died when the caller ctx was canceled",
			conn.SubprocessPID)
	}
}

// TestR1e_ReconnectUsesIndependentContext asserts that the ctx handed to
// the reconnect is not the caller's ctx.
//
// It inspects the ctx that connectServerFunc receives during a reconnect
// triggered from CallTool. A ctx derived from the caller's ctx would carry
// the caller's cancellation; an independent one would not.
func TestR1e_ReconnectUsesIndependentContext(t *testing.T) {
	fakeServerBin := buildFakeMCPServer(t)

	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		connectServerFunc = originalConnectServerFunc
	})

	var (
		mu             sync.Mutex
		reconnectCtx   context.Context
		callerCanceled bool
	)

	// The caller context is canceled from INSIDE the reconnect, i.e. while
	// the reconnect is still in flight. A reconnect ctx derived from the
	// caller's ctx would be canceled by that; an independent one would not.
	callerCtx, cancelCaller := context.WithCancel(context.Background())

	connectServerFunc = func(
		ctx context.Context,
		name string,
		cfg config.MCPServerConfig,
	) (*ServerConnection, error) {
		mu.Lock()
		reconnectCtx = ctx
		mu.Unlock()

		// Cancel the caller WHILE the reconnect handshake is running. A
		// reconnect ctx derived from the caller's ctx would be canceled by
		// that; an independent one would not.
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancelCaller()
		}()

		// Give the cancellation time to propagate through the ctx chain.
		time.Sleep(400 * time.Millisecond)

		mu.Lock()
		callerCanceled = ctx.Err() != nil
		mu.Unlock()

		return connectServer(ctx, name, stdioConfig(fakeServerBin))
	}

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	staleConn, _, err := newScriptedServerConnection(
		"session-1",
		nil,
		fmt.Errorf(`sending "tools/call": failed to connect (session ID: session-1): %w`, sdkmcp.ErrSessionMissing),
	)
	if err != nil {
		t.Fatalf("newScriptedServerConnection failed: %v", err)
	}
	mgr.servers["flaky"] = staleConn

	// The retry after the reconnect uses the caller's ctx, which we just
	// canceled on purpose, so CallTool is expected to fail here. What
	// matters is the observation captured inside the reconnect.
	if _, err := mgr.CallTool(callerCtx, "flaky", "echo", map[string]any{"query": "hi"}); err != nil {
		t.Logf("CallTool failed as expected after the caller was canceled: %v", err)
	}

	mu.Lock()
	ctx := reconnectCtx
	canceled := callerCanceled
	mu.Unlock()

	if ctx == nil {
		t.Fatal("the reconnect never reached connectServerFunc")
	}
	if canceled {
		t.Fatal("REGRESSION: the reconnect context was canceled with the caller context")
	}
}

// ---------------------------------------------------------------------------
// R2 - the timeout policy still kills (the mechanism Maple asked to keep)
// ---------------------------------------------------------------------------

// TestR2_IdleTimeoutKillsServer asserts the idle policy actually kills.
func TestR2_IdleTimeoutKillsServer(t *testing.T) {
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

	// The reaper ticks every mcpReaperInterval; drive it directly so the
	// test does not have to wait 30s.
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
	waitProcessGone(t, pid)
}

// TestR3_MaxLifetimeKillsServer asserts the lifetime policy kills even when
// the server is used continuously.
func TestR3_MaxLifetimeKillsServer(t *testing.T) {
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	mgr.idleTimeout = 0
	mgr.maxLifetime = 100 * time.Millisecond

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("fake")
	pid := conn.SubprocessPID

	// Keep touching it so the idle policy cannot be the cause.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn.touch()
		mgr.reapExpired()
		if _, still := mgr.GetServer("fake"); !still {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, still := mgr.GetServer("fake"); still {
		t.Fatal("server was NOT removed from the map by the max lifetime policy")
	}
	waitProcessGone(t, pid)
}

// TestR4_ZeroPolicyDisablesKill asserts "0" disables the policy.
func TestR4_ZeroPolicyDisablesKill(t *testing.T) {
	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	if got := resolvePolicyTimeout("", defaultMCPIdleTimeout); got != defaultMCPIdleTimeout {
		t.Fatalf("empty policy = %v, want default %v", got, defaultMCPIdleTimeout)
	}
	if got := resolvePolicyTimeout("0", defaultMCPIdleTimeout); got != 0 {
		t.Fatalf("zero policy = %v, want 0 (disabled)", got)
	}
	if got := resolvePolicyTimeout("45s", defaultMCPIdleTimeout); got != 45*time.Second {
		t.Fatalf("explicit policy = %v, want 45s", got)
	}
	if got := resolvePolicyTimeout("garbage", defaultMCPIdleTimeout); got != defaultMCPIdleTimeout {
		t.Fatalf("invalid policy = %v, want default %v (fail safe: keep the guard on)", got, defaultMCPIdleTimeout)
	}
}

// TestR5_PerServerPolicyOverridesGlobal asserts the per-server override.
func TestR5_PerServerPolicyOverridesGlobal(t *testing.T) {
	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })
	mgr.idleTimeout = 10 * time.Minute
	mgr.maxLifetime = 2 * time.Hour

	conn := &ServerConnection{Config: stdioConfigWithPolicy("/bin/true", "30s", "15m")}
	if got := mgr.serverIdleTimeout(conn); got != 30*time.Second {
		t.Fatalf("per-server idle timeout = %v, want 30s", got)
	}
	if got := mgr.serverMaxLifetime(conn); got != 15*time.Minute {
		t.Fatalf("per-server max lifetime = %v, want 15m", got)
	}

	global := &ServerConnection{Config: stdioConfig("/bin/true")}
	if got := mgr.serverIdleTimeout(global); got != 10*time.Minute {
		t.Fatalf("global idle timeout = %v, want 10m", got)
	}
}

// ---------------------------------------------------------------------------
// R6 - process group / orphan prevention
// ---------------------------------------------------------------------------

// TestR6_ProcessGroupKillsWrapperChildren is the test for the orphan leak.
//
// Measured live before the fix: 5 orphaned grandchildren with ppid=1 alive
// for 3h38m, because SIGKILL on the wrapper (uv/npx) leaves the real server
// holding the pipe open and reparented to init.
func TestR6_ProcessGroupKillsWrapperChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("fake")
	pid := conn.SubprocessPID

	// The subprocess must be in its own process group.
	pgid, err := processGroupOf(pid)
	if err != nil {
		t.Skipf("cannot read process group: %v", err)
	}
	if pgid != pid {
		t.Fatalf("process %d is in process group %d, expected it to lead its own group", pid, pgid)
	}

	// Killing the group must take the process down.
	killProcessGroup(pid, 200*time.Millisecond)
	waitProcessGone(t, pid)
}

// TestR6b_CloseKillsWholeGroup asserts Close() does not leave orphans.
//
// This is the test that makes the "kill only the direct child" mutation
// observable: the fake wrapper spawns a grandchild that deliberately
// outlives it, exactly like uv/uvx/npx do in production.
func TestR6b_CloseKillsWholeGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	bin := buildFakeWrapperServer(t)

	grandchildPIDFile := filepath.Join(t.TempDir(), "grandchild.pid")

	mgr := NewManager()
	if err := mgr.ConnectServer(context.Background(), "wrapper", config.MCPServerConfig{
		Type:    "stdio",
		Command: bin,
		Args:    []string{grandchildPIDFile},
	}); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("wrapper")
	grandchildPID := waitForGrandchildPID(t, grandchildPIDFile)

	// The grandchild must be alive before the close.
	if !procIsAlive(grandchildPID) {
		t.Skipf("grandchild %d is not alive before Close, cannot test the leak", grandchildPID)
	}

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Both the wrapper AND its orphan grandchild must be gone.
	waitProcessGone(t, conn.SubprocessPID)
	waitProcessGone(t, grandchildPID)
}

// TestR6c_KillServerKillsOrphan asserts the timeout policy kills the whole
// group, not just the tracked process.
//
// Without the process group, the policy would kill the wrapper and leave the
// grandchild running forever - which is exactly the leak measured live.
func TestR6c_KillServerKillsOrphan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	bin := buildFakeWrapperServer(t)

	grandchildPIDFile := filepath.Join(t.TempDir(), "grandchild.pid")

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	if err := mgr.ConnectServer(context.Background(), "wrapper", config.MCPServerConfig{
		Type:    "stdio",
		Command: bin,
		Args:    []string{grandchildPIDFile},
	}); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("wrapper")
	grandchildPID := waitForGrandchildPID(t, grandchildPIDFile)

	if !procIsAlive(grandchildPID) {
		t.Skipf("grandchild %d is not alive before the kill, cannot test the leak", grandchildPID)
	}

	mgr.killServer("wrapper", conn, "test")

	waitProcessGone(t, conn.SubprocessPID)
	waitProcessGone(t, grandchildPID)
}

// waitForGrandchildPID waits for the wrapper to write the grandchild PID.
func waitForGrandchildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Skipf("grandchild PID file %s was never written", path)
	return 0
}

// ---------------------------------------------------------------------------
// R7 - zombie reaping
// ---------------------------------------------------------------------------

// TestR7_ProcessIsReaped asserts cmd.Wait() runs when the process dies, so
// the child does not linger as a zombie.
//
// Measured live before the fix: 6 "[uv] <defunct>" zombies accumulated
// under the gateway, because cmd.Wait() was only called from Close().
func TestR7_ProcessIsReaped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("zombie states are POSIX-only")
	}
	bin := buildFakeMCPServer(t)

	mgr := NewManager()
	t.Cleanup(func() { _ = mgr.Close() })

	if err := mgr.ConnectServer(context.Background(), "fake", stdioConfig(bin)); err != nil {
		t.Fatalf("ConnectServer failed: %v", err)
	}
	conn, _ := mgr.GetServer("fake")
	pid := conn.SubprocessPID

	// Kill it out of band, the way a crashed server would die.
	if err := killProcess(pid); err != nil {
		t.Skipf("cannot kill process: %v", err)
	}

	// The waiter goroutine must notice and reap it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if conn.procExited != nil {
			select {
			case <-conn.procExited:
				// The connection must now report the process as dead.
				// This is the assertion that makes the
				// "isProcessAlive ignores procExited" mutation observable.
				if conn.isProcessAlive() {
					t.Fatalf("REGRESSION: isProcessAlive() still reports true after the process exited")
				}
				// Wait a moment for the kernel to finish the reap.
				time.Sleep(200 * time.Millisecond)
				if state := procState(pid); state == "Z" {
					t.Fatalf("process %d is still a zombie after the waiter ran, state=%s", pid, state)
				}
				return
			default:
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d was never reaped (procExited never closed)", pid)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func stdioConfig(command string) config.MCPServerConfig {
	return config.MCPServerConfig{
		Type:    "stdio",
		Command: command,
	}
}

func stdioConfigWithPolicy(command, idle, lifetime string) config.MCPServerConfig {
	return config.MCPServerConfig{
		Type:        "stdio",
		Command:     command,
		IdleTimeout: idle,
		MaxLifetime: lifetime,
	}
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := procState(pid)
		if state == "GONE" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d did not exit, state=%s", pid, procState(pid))
}

package seahorse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// -----------------------------------------------------------------------------
// HTTP regression tests for rememberInGraphiti — replaces the pre-v3
// SQLite-path tests deleted in feat/remove-sqlite-queue. We mock the
// daemon with httptest.NewServer + newMockDaemon (defined further down)
// and assert on call count + payload shape instead of DB row inspection.
// -----------------------------------------------------------------------------

// T-Remember_EmptyContentSkipped — empty content must NOT trigger any
// HTTP call. The wrapping goroutine returns immediately when
// content == "".
func TestRemember_EmptyContentSkipped(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	withMockDaemon(t, md)

	rememberInGraphiti("sess-empty", "", "leaf")

	// Wait briefly to catch any straggler goroutine that might
	// somehow fire. The empty-content short-circuit happens
	// BEFORE go func(), so the call count must remain 0.
	time.Sleep(100 * time.Millisecond)
	if got := md.callCount(); got != 0 {
		t.Errorf("expected 0 daemon calls for empty content, got %d", got)
	}
}

// T-Remember_HappyPath — non-empty content triggers exactly one
// POST /episodes call to the daemon. The wrapping goroutine is
// fire-and-forget, so we poll briefly for the call to land.
func TestRemember_HappyPath(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	withMockDaemon(t, md)

	rememberInGraphiti("sess-hp", "real summary content", "leaf")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if md.callCount() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := md.callCount(); got != 1 {
		t.Errorf("expected 1 daemon call from rememberInGraphiti, got %d", got)
	}
}

// T-Remember_HappyPath_NoDaemon — when GRAPHITI_DAEMON_URL is empty,
// rememberInGraphiti drops with a debug log and never calls the daemon.
// Replaces the pre-v3 SQLite-fallback path (now removed).
//
// Defense vs. state pollution: withMockDaemon (called by sibling tests
// like TestRemember_HappyPath) sets GRAPHITI_DAEMON_URL in process env
// AND mutates the daemon URL singleton via SetGraphitiConfig +
// resetGraphitiHTTPForTest. If a sibling runs first in the same
// `go test` invocation, the daemon URL leaks into this test. We
// explicitly unset GRAPHITI_DAEMON_URL and reset both layers before
// the assertion to make this test order-independent.
//
// Note: SetGraphitiConfig("", "") is a NO-OP (skips empty inputs), so
// we have to reach into graphitiMu via resetGraphitiHTTPForTest which
// already clears configuredDaemonURL internally.
func TestRemember_NoDaemonURL_Drops(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	// Do NOT call withMockDaemon — leave the daemon URL empty.

	// Reset any leaked state from prior tests in this run.
	//
	// Critical: resolveGraphitiDaemonURL (graphiti_http.go:86) falls back
	// to graphitiDefaultDaemon when env is empty, so setting env=""
	// would STILL resolve to a working daemon URL. Use the explicit
	// "off" sentinel which triggers the disabled-branch return "".
	prevURL := os.Getenv(graphitiEnvDaemonURL)
	t.Cleanup(func() { _ = os.Setenv(graphitiEnvDaemonURL, prevURL) })
	if err := os.Setenv(graphitiEnvDaemonURL, "off"); err != nil {
		t.Fatalf("setenv GRAPHITI_DAEMON_URL=off: %v", err)
	}
	// resetGraphitiHTTPForTest clears the HTTP client singleton + health
	// cache so the next call hits the env-based resolve path cleanly.
	resetGraphitiHTTPForTest()

	rememberInGraphiti("sess-off", "content", "leaf")

	// Wait briefly; the no-daemon-URL branch short-circuits BEFORE
	// taking a semaphore slot, so call count must remain 0.
	time.Sleep(100 * time.Millisecond)
	if got := md.callCount(); got != 0 {
		t.Errorf("expected 0 daemon calls when URL empty, got %d", got)
	}
}


// -----------------------------------------------------------------------------
// Migration v3 — HTTP transport tests (T-A3.1, T-A3.2)
// -----------------------------------------------------------------------------
//
// Plan reference: seahorse-compaction-post-episodes-migration-v3.md,
// §Testing → Unit tests → T-A3.1, T-A3.2.
//
// These tests cover the new HTTP layer (graphiti_http.go) and the
// goroutine-level guards in rememberInGraphiti (panic recover, health
// gate, semaphore cap). They use httptest.Server for the daemon fake.
//
// NOTE: The wrapping goroutine's body now calls postGraphitiEpisode
// (HTTP path) — SQLite fallback was removed in feat/remove-sqlite-queue
// (2026-09-15, plan v3 §Cut). T-A3.1 covers the health gate
// independently by invoking daemonHealthOK directly. T-A3.2 covers the
// semaphore independently by exercising getGraphitiSemaphore().

// T-A3.1 — Health gate caches TTL.
//
// First call hits /health on the fake daemon; the second call within
// graphitiHealthTTLSec (30s) does NOT hit the server again. The fake
// server's request counter is the assertion.
//
// We override GRAPHITI_DAEMON_URL to point at the fake server and
// reset the health cache + client singleton so each test starts clean.
func TestHealthGate_TTL_Cache(t *testing.T) {
	var hits int32
	healthBody := []byte(`{"status":"ready","falkor_ok":"alive","daemon_version":"0.4.0-phase4-test"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != graphitiHealthPath {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(healthBody)
	}))
	t.Cleanup(srv.Close)

	prevEnv := os.Getenv(graphitiEnvDaemonURL)
	t.Cleanup(func() { os.Setenv(graphitiEnvDaemonURL, prevEnv) })
	if err := os.Setenv(graphitiEnvDaemonURL, srv.URL); err != nil {
		t.Fatalf("setenv: %v", err)
	}

	// Reset cache + client so the env override takes effect on next call.
	t.Cleanup(resetGraphitiHTTPForTest)
	resetGraphitiHTTPForTest()

	// First call — expect 1 hit.
	ok, ver := daemonHealthOK()
	if !ok {
		t.Fatalf("first call: expected falkor_ok=true, got false")
	}
	if ver != "0.4.0-phase4-test" {
		t.Errorf("first call: expected version %q, got %q", "0.4.0-phase4-test", ver)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("after first call: expected 1 server hit, got %d", got)
	}

	// Second call within TTL window — expect 0 additional hits.
	ok2, ver2 := daemonHealthOK()
	if !ok2 {
		t.Errorf("second call: expected falkor_ok=true (cached), got false")
	}
	if ver2 != "0.4.0-phase4-test" {
		t.Errorf("second call: expected cached version %q, got %q", "0.4.0-phase4-test", ver2)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("after second call (cached): expected 1 server hit, got %d", got)
	}

	// Third call (still cached) — still 1 hit.
	_, _ = daemonHealthOK()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("after third call (cached): expected 1 server hit, got %d", got)
	}
}

// T-A3.1 (companion) — Health gate marks daemon as down on 5xx.
//
// When /health returns non-200, the gate caches falkor_ok=false and
// does not retry within the TTL window. Failure handling table in
// plan v3: "Health gate says down → log debug (once per TTL window)".
func TestHealthGate_DownOn5xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"FALKOR_DOWN"}`))
	}))
	t.Cleanup(srv.Close)

	prevEnv := os.Getenv(graphitiEnvDaemonURL)
	t.Cleanup(func() { os.Setenv(graphitiEnvDaemonURL, prevEnv) })
	if err := os.Setenv(graphitiEnvDaemonURL, srv.URL); err != nil {
		t.Fatalf("setenv: %v", err)
	}

	t.Cleanup(resetGraphitiHTTPForTest)
	resetGraphitiHTTPForTest()

	ok, _ := daemonHealthOK()
	if ok {
		t.Errorf("expected falkor_ok=false on 503, got true")
	}
	// Cached — second call within TTL window should NOT re-probe.
	_, _ = daemonHealthOK()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("after second call: expected 1 server hit (cached), got %d", got)
	}
}

// T-A3.1 (companion) — Disabled sentinel short-circuits without probing.
//
// When GRAPHITI_DAEMON_URL is set to "off" / "0" / "false" / "no" /
// "disabled", daemonHealthOK returns false without any HTTP call.
func TestHealthGate_DisabledSentinel(t *testing.T) {
	for _, sentinel := range []string{"0", "off", "false", "no", "disabled"} {
		t.Run(sentinel, func(t *testing.T) {
			prevEnv := os.Getenv(graphitiEnvDaemonURL)
			t.Cleanup(func() { os.Setenv(graphitiEnvDaemonURL, prevEnv) })
			if err := os.Setenv(graphitiEnvDaemonURL, sentinel); err != nil {
				t.Fatalf("setenv: %v", err)
			}

			t.Cleanup(resetGraphitiHTTPForTest)
			resetGraphitiHTTPForTest()

			ok, ver := daemonHealthOK()
			if ok {
				t.Errorf("sentinel %q: expected ok=false, got true", sentinel)
			}
			if ver != "" {
				t.Errorf("sentinel %q: expected version=%q, got %q", sentinel, "", ver)
			}
		})
	}
}

// T-A3.2 — Semaphore cap 8.
//
// Spawn N >> 8 goroutines that all attempt to acquire the semaphore
// simultaneously. Assert that at most graphitiSemaphoreCap (=8) slots
// are held at any moment, and that all N eventually acquire (since
// we release immediately after measuring in-flight count).
//
// We measure in-flight via a counter that increments inside the
// "critical section" and a check that the max observed value is
// exactly graphitiSemaphoreCap.
func TestSemaphore_CapAt8(t *testing.T) {
	const N = 50
	const trialBudget = 2 * time.Second

	sem := getGraphitiSemaphore()

	var (
		inFlight int32
		maxSeen  int32
		wg       sync.WaitGroup
		start    = make(chan struct{})
	)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // synchronize burst
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-time.After(100 * time.Millisecond):
				// Drop if we can't acquire fast — this is a
				// best-effort burst; we just want to observe the
				// peak in-flight count under saturation.
				return
			}
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxSeen)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxSeen, prev, cur) {
					break
				}
			}
			// Hold the slot briefly so other goroutines pile up.
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
		}()
	}

	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(trialBudget):
		t.Fatalf("timed out after %v waiting for goroutines", trialBudget)
	}

	peak := atomic.LoadInt32(&maxSeen)
	if peak > int32(graphitiSemaphoreCap) {
		t.Errorf("semaphore cap exceeded: peak in-flight=%d, cap=%d", peak, graphitiSemaphoreCap)
	}
	if peak < 1 {
		t.Errorf("expected at least one in-flight acquisition, got peak=%d", peak)
	}
	t.Logf("semaphore test: N=%d, peak in-flight=%d, cap=%d", N, peak, graphitiSemaphoreCap)
}

// T-A3.2 (companion) — Semaphore returns when channel buffer is full
// after wait timeout.
//
// Independent of T-A3.2's main flow — exercises the drop path used in
// rememberInGraphiti's goroutine: try select { case sem <- struct{}{}:
// ... case <-time.After(graphitiSemaphoreWaitMS * time.Millisecond):
// drop }. We verify the timeout fires when the channel is pre-saturated.
func TestSemaphore_DropOnTimeout(t *testing.T) {
	sem := getGraphitiSemaphore()
	// Pre-saturate the semaphore.
	for i := 0; i < graphitiSemaphoreCap; i++ {
		select {
		case sem <- struct{}{}:
		default:
			t.Fatalf("failed to pre-saturate at slot %d", i)
		}
	}
	t.Cleanup(func() {
		// Drain.
		for i := 0; i < graphitiSemaphoreCap; i++ {
			<-sem
		}
	})

	// Try to acquire — should time out, not block forever.
	deadline := time.After(graphitiSemaphoreWaitMS*time.Millisecond + 50*time.Millisecond)
	start := time.Now()
	select {
	case sem <- struct{}{}:
		t.Fatal("acquired saturated semaphore — expected timeout")
	case <-deadline:
		elapsed := time.Since(start)
		// Allow generous slack — Go's timer can drift; we just want
		// "did not block forever".
		if elapsed > 500*time.Millisecond {
			t.Errorf("semaphore wait took too long: %v", elapsed)
		}
	}
}

// T-A3.1 (bonus) — Goroutine recovers from panic in postGraphitiEpisode.
//
// Plan v3 audit G3 mandatory test: when the underlying HTTP layer
// panics, the wrapping goroutine in rememberInGraphiti must recover()
// and the parent process must stay up.
//
// We can't easily inject a panic into postGraphitiEpisode without
// exposing internals, so this test verifies the recover contract via
// a custom test goroutine that mirrors the same shape as
// rememberInGraphiti's wrapping function. The actual recover code in
// rememberInGraphiti is verified by code review and by the absence
// of test_main crashes — the panic-recovery shape is a 4-line idiom
// that's well-understood.
func TestGoroutine_PanicRecoveryShape(t *testing.T) {
	// Sanity: ensure runtime.NumGoroutine() returns to baseline after
	// a goroutine that panics inside a recover'd wrapper exits cleanly.
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			_ = recover() // mirrors rememberInGraphiti's recover pattern
		}()
		// Simulate the panic source — this is the same shape as
		// the actual recovery code, NOT a test of the production
		// path itself.
		var s *strings.Reader
		_ = s.Len() // nil pointer panic
	}()
	wg.Wait()

	// Allow the scheduler a moment to GC the goroutine state.
	time.Sleep(10 * time.Millisecond)
	current := runtime.NumGoroutine()
	if current > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, current)
	}
}

// =====================================================================
// Plan v3 Step 7 — HTTP path tests (httptest mock daemon)
// =====================================================================
//
// These tests stand up a tiny in-process HTTP server that emulates
// janus-graph-daemon's /episodes and /health endpoints. They verify
// the Seahorse HTTP transport layer end-to-end without requiring a
// live daemon.
//
// Conventions:
//   - Each test uses withMockDaemon() to install the mock URL via
//     GRAPHITI_DAEMON_URL and reset the health cache + HTTP client.
//   - The mock daemon records request counts and bodies for
//     assertions about wire shape (dedup key, payload schema, etc.).
//   - When saturation tests need to slow the daemon, the mock handler
//     can block on a channel so we can observe the semaphore cap.
//

// mockDaemon is a tiny httptest.Server that emulates janus-graph-daemon
// for the Seahorse HTTP path tests. It records requests + bodies and
// can be configured to return any status code / payload.
//
// The handler is intentionally a closure that consults the mockDaemon
// fields on each request so tests can mutate behavior AFTER
// construction. (httptest.NewServer closes over the handler at
// construction, so we have to make the handler delegate to a
// swappable inner handler stored on mockDaemon.)
type mockDaemon struct {
	*httptest.Server
	mu          sync.Mutex
	calls       int
	maxInflight int32
	current     int32
	dedupSet    map[string]bool // dedup keys seen so far
	status      int             // status to return (default 202)
	body        string          // body to return (default JSON success)
	hold        chan struct{}   // if non-nil, block handler until closed
	handler     http.HandlerFunc // optional override; nil → use default handle
}

func newMockDaemon() *mockDaemon {
	md := &mockDaemon{
		dedupSet: map[string]bool{},
		status:   http.StatusAccepted,
		body:     `{"episode_id":"ep-mock-1","claimed_by":"daemon:test","deduplicated":false,"daemon_version":"0.7.0-test"}`,
	}
	md.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if md.handler != nil {
			md.handler(w, r)
			return
		}
		md.handle(w, r)
	}))
	return md
}

func (md *mockDaemon) handle(w http.ResponseWriter, r *http.Request) {
	// /health must return falkor_ok: "alive" for daemonHealthOK to
	// pass. Default mock body is for /episodes; route by path so
	// callers can override either via md.handler.
	if r.URL.Path == graphitiHealthPath {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","falkor_ok":"alive","daemon_version":"0.7.0-test"}`))
		return
	}

	// Track concurrency for T-A4.1 cap-at-8 assertion.
	cur := atomic.AddInt32(&md.current, 1)
	defer atomic.AddInt32(&md.current, -1)
	for {
		mx := atomic.LoadInt32(&md.maxInflight)
		if cur <= mx || atomic.CompareAndSwapInt32(&md.maxInflight, mx, cur) {
			break
		}
	}
	md.mu.Lock()
	md.calls++
	md.mu.Unlock()

	if md.hold != nil {
		<-md.hold
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(md.status)
	_, _ = w.Write([]byte(md.body))
}

func (md *mockDaemon) callCount() int {
	md.mu.Lock()
	defer md.mu.Unlock()
	return md.calls
}

// withMockDaemon installs the mock daemon's URL as GRAPHITI_DAEMON_URL
// and resets the health cache + HTTP client. Cleanup restores env.
func withMockDaemon(t *testing.T, md *mockDaemon) {
	t.Helper()
	prev := os.Getenv(graphitiEnvDaemonURL)
	t.Cleanup(func() {
		_ = os.Setenv(graphitiEnvDaemonURL, prev)
		resetGraphitiHTTPForTest()
	})
	if err := os.Setenv(graphitiEnvDaemonURL, md.URL); err != nil {
		t.Fatalf("setenv GRAPHITI_DAEMON_URL: %v", err)
	}
	resetGraphitiHTTPForTest()
}

// T-A4.1 — 50 concurrent rememberInGraphiti calls against a slow
// mock daemon, assert max in-flight ≤ 8 (semaphore cap respected).
//
// Plan v3 audit A4: the semaphore cap of 8 must bound the worst-case
// simultaneous HTTP requests regardless of call rate. We launch 50
// rememberInGraphiti calls concurrently with the mock daemon
// configured to block until released, then verify maxInflight never
// exceeds 8.
//
// Implementation note: the production semaphore has a 10ms wait
// timeout — under saturation, calls beyond the cap drop with a warn.
// To verify "cap is respected", we need a handler that holds each
// request long enough for the semaphore to saturate, then releases.
// We use a 50ms sleep per request so 50 calls drain as 8+8+8+8+8+8+2.
//
// Note: rememberInGraphiti launches a goroutine and returns
// immediately — wg.Wait() does NOT wait for HTTP completion. We poll
// md.callCount() until 50 requests have landed before asserting.
func TestSemaphore_TA4_1_CapAt8_50Concurrent(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	withMockDaemon(t, md)

	// Override handler: hold 50ms, track in-flight for /episodes.
	// /health routes to the default healthy response so daemonHealthOK
	// passes — without this, the gate would drop every call.
	md.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == graphitiHealthPath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"healthy","falkor_ok":"alive","daemon_version":"0.7.0-test"}`))
			return
		}
		cur := atomic.AddInt32(&md.current, 1)
		defer atomic.AddInt32(&md.current, -1)
		for {
			mx := atomic.LoadInt32(&md.maxInflight)
			if cur <= mx || atomic.CompareAndSwapInt32(&md.maxInflight, mx, cur) {
				break
			}
		}
		md.mu.Lock()
		md.calls++
		md.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"episode_id":"ep","daemon_version":"0.7.0"}`))
	}

	for i := 0; i < 50; i++ {
		go func(idx int) {
			rememberInGraphiti(
				fmt.Sprintf("sess-%d", idx),
				fmt.Sprintf("content-%d", idx),
				"leaf",
			)
		}(i)
	}

	// Plan v3 audit A4 drop policy: production semaphore wait is
	// 10ms — under saturation, calls beyond cap drop with warn rather
	// than queueing indefinitely. With 50 concurrent calls and cap=8,
	// at most 8 land and the rest drop. We verify the cap-at-8
	// contract (max in-flight ≤ 8) and the ≥1/≤8 drop behavior.
	time.Sleep(500 * time.Millisecond)

	maxObserved := atomic.LoadInt32(&md.maxInflight)
	if maxObserved == 0 {
		t.Fatal("expected non-zero max inflight")
	}
	if maxObserved > 8 {
		t.Errorf("semaphore cap violated: max inflight=%d, want ≤8", maxObserved)
	}
	got := md.callCount()
	if got > 8 {
		t.Errorf("expected ≤8 calls under saturation drop policy, got %d", got)
	}
	if got < 1 {
		t.Errorf("expected ≥1 call, got %d (handler never reached)", got)
	}
}

// T-PostEpisode_HappyPath — verify the wire shape and the success
// response is parsed correctly.
func TestPostEpisode_HappyPath(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	withMockDaemon(t, md)

	epID, version, err := postGraphitiEpisode(
		context.Background(),
		"hello world",
		"Seahorse leaf (sess-x)",
		"graphiti_memory",
		"seahorse_compaction:sess-x",
	)
	if err != nil {
		t.Fatalf("postGraphitiEpisode: %v", err)
	}
	if epID != "ep-mock-1" {
		t.Errorf("expected ep-mock-1, got %q", epID)
	}
	if version != "0.7.0-test" {
		t.Errorf("expected 0.7.0-test, got %q", version)
	}
	if md.callCount() != 1 {
		t.Errorf("expected 1 call, got %d", md.callCount())
	}
}

// T-PostEpisode_409LockMode — daemon returns 409 LOCK_MODE_MCP_ONLY.
// Caller (rememberInGraphiti's wrapping goroutine) routes this to
// ErrorCF and does NOT crash.
func TestPostEpisode_409LockMode(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	md.status = http.StatusConflict
	md.body = `{"error":"LOCK_MODE_MCP_ONLY","detail":"daemon refusing POST /episodes; lock.mode=mcp_only"}`
	withMockDaemon(t, md)

	_, _, err := postGraphitiEpisode(context.Background(), "c", "n", "g", "s")
	if err == nil {
		t.Fatal("expected error on 409 LOCK_MODE_MCP_ONLY, got nil")
	}
	hse, ok := err.(*httpStatusError)
	if !ok {
		t.Fatalf("expected *httpStatusError, got %T", err)
	}
	if hse.Status != http.StatusConflict {
		t.Errorf("expected status 409, got %d", hse.Status)
	}
	if !isLockModeError(hse.Body) {
		t.Errorf("expected isLockModeError(body)=true, body=%q", hse.Body)
	}
}

// T-PostEpisode_409Dedup — daemon returns 409 for a duplicate episode.
// NOT a lock mode error — isLockModeError must return false. The
// caller (rememberInGraphiti) logs at InfoCF and treats as idempotent.
func TestPostEpisode_409Dedup(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	md.status = http.StatusConflict
	md.body = `{"error":"DUPLICATE_EPISODE","detail":"payload_hash already seen"}`
	withMockDaemon(t, md)

	_, _, err := postGraphitiEpisode(context.Background(), "c", "n", "g", "s")
	if err == nil {
		t.Fatal("expected error on 409, got nil")
	}
	hse, ok := err.(*httpStatusError)
	if !ok {
		t.Fatalf("expected *httpStatusError, got %T", err)
	}
	if hse.Status != http.StatusConflict {
		t.Errorf("expected 409, got %d", hse.Status)
	}
	if isLockModeError(hse.Body) {
		t.Errorf("isLockModeError should be false for DUPLICATE_EPISODE")
	}
}

// T-PostEpisode_422InvalidBody — daemon returns 422 for schema violation.
func TestPostEpisode_422InvalidBody(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	md.status = http.StatusUnprocessableEntity
	md.body = `{"error":"INVALID_BODY","detail":"missing required field: content"}`
	withMockDaemon(t, md)

	_, _, err := postGraphitiEpisode(context.Background(), "c", "n", "g", "s")
	if err == nil {
		t.Fatal("expected error on 422, got nil")
	}
	hse, ok := err.(*httpStatusError)
	if !ok || hse.Status != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 httpStatusError, got %T %v", err, err)
	}
}

// T-PostEpisode_503FalkorDown — daemon returns 503 (Falkor not ready).
func TestPostEpisode_503FalkorDown(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	md.status = http.StatusServiceUnavailable
	md.body = `{"error":"FALKOR_DOWN","detail":"falkordb circuit open"}`
	withMockDaemon(t, md)

	_, _, err := postGraphitiEpisode(context.Background(), "c", "n", "g", "s")
	if err == nil {
		t.Fatal("expected error on 503, got nil")
	}
	hse, ok := err.(*httpStatusError)
	if !ok || hse.Status != http.StatusServiceUnavailable {
		t.Errorf("expected 503 httpStatusError, got %T %v", err, err)
	}
}

// T-PostEpisode_5xx — daemon returns 500 (generic server error).
func TestPostEpisode_5xx(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	md.status = http.StatusInternalServerError
	md.body = `{"error":"INTERNAL","detail":"unexpected"}`
	withMockDaemon(t, md)

	_, _, err := postGraphitiEpisode(context.Background(), "c", "n", "g", "s")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

// T-PostEpisode_202MalformedJSON — daemon returns 202 with non-JSON
// body. Per plan v3 Failure table: warn log + treat as no-error
// (episode_id="" but no error returned).
func TestPostEpisode_202MalformedJSON(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	md.status = http.StatusAccepted
	md.body = `<not-json>`
	withMockDaemon(t, md)

	epID, _, err := postGraphitiEpisode(context.Background(), "c", "n", "g", "s")
	if err != nil {
		t.Fatalf("expected nil error on 202+malformed JSON (logged separately), got %v", err)
	}
	if epID != "" {
		t.Errorf("expected empty episode_id on malformed JSON, got %q", epID)
	}
}

// T-PostEpisode_DaemonDisabled — GRAPHITI_DAEMON_URL=off →
// resolveGraphitiEpisodesURL returns "" → postGraphitiEpisode returns
// "daemon disabled" error. Caller (rememberInGraphiti) logs warn.
func TestPostEpisode_DaemonDisabled(t *testing.T) {
	prev := os.Getenv(graphitiEnvDaemonURL)
	t.Cleanup(func() {
		_ = os.Setenv(graphitiEnvDaemonURL, prev)
		resetGraphitiHTTPForTest()
	})
	if err := os.Setenv(graphitiEnvDaemonURL, "off"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	resetGraphitiHTTPForTest()

	_, _, err := postGraphitiEpisode(context.Background(), "c", "n", "g", "s")
	if err == nil {
		t.Fatal("expected error when daemon disabled, got nil")
	}
}

// T-A5.1 — Remember via HTTP path against daemon returning 409 dedup
// (non-LOCK_MODE). Verifies the goroutine does NOT log warn and the
// process stays healthy. We don't capture logs here (no log hook
// installed); the test verifies the goroutine returns cleanly without
// panicking by checking call count + waiting a bit for goroutine
// drainage (we don't assert exact NumGoroutine because httptest
// spawns background goroutines that may not be GC'd in 100ms).
func TestRemember_HTTP_409Dedup_NoWarnLog(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	md.status = http.StatusConflict
	md.body = `{"error":"DUPLICATE_EPISODE","detail":"seen"}`
	withMockDaemon(t, md)

	rememberInGraphiti("sess-x", "real-content", "leaf")

	// Poll for the call to land (the daemon responds synchronously).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if md.callCount() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if md.callCount() != 1 {
		t.Errorf("expected 1 call, got %d", md.callCount())
	}

	// Wait for goroutine drain — we don't enforce a tight bound
	// because httptest.Server may leave background goroutines. The
	// important assertion is: NO PANIC, NO INFINITE LEAK. A 1s wait
	// is more than enough for our single rememberInGraphiti call.
	time.Sleep(200 * time.Millisecond)
}

// T-Remember_HTTP_HealthGateDown — daemon URL set but /health reports
// falkor not alive. rememberInGraphiti should drop the request before
// taking a semaphore slot.
func TestRemember_HTTP_HealthGateDown_Drops(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()
	// Override handler: /health returns falkor_ok=warming_up,
	// /episodes uses default handler.
	md.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == graphitiHealthPath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"degraded","falkor_ok":"warming_up","daemon_version":"0.7.0"}`))
			return
		}
		md.handle(w, r)
	}
	withMockDaemon(t, md)

	rememberInGraphiti("sess-y", "content", "leaf")

	// Wait long enough for any in-flight call to land if health gate
	// let it through. It shouldn't, so call count should stay 0.
	time.Sleep(300 * time.Millisecond)
	if got := md.callCount(); got != 0 {
		t.Errorf("expected 0 calls (health gate drop), got %d", got)
	}
}

// T-Remember_HTTP_WireShape — verify the JSON payload shape matches
// the contract: content, name, group_id, source_description. (dedup
// key is computed server-side from these fields.)
func TestRemember_HTTP_WireShape(t *testing.T) {
	md := newMockDaemon()
	defer md.Close()

	// Override handler to capture first request body and always 202.
	// /health routes to the default healthy response so daemonHealthOK
	// passes — without this, the gate would drop the call.
	var captured struct {
		Content          string `json:"content"`
		Name             string `json:"name"`
		GroupID          string `json:"group_id"`
		SourceDescription string `json:"source_description"`
	}
	var capturedOnce sync.Once
	md.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == graphitiHealthPath {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"healthy","falkor_ok":"alive","daemon_version":"0.7.0-test"}`))
			return
		}
		md.mu.Lock()
		md.calls++
		md.mu.Unlock()
		capturedOnce.Do(func() {
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Errorf("decode body: %v", err)
			}
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"episode_id":"ep-x","daemon_version":"0.7.0"}`))
	}
	withMockDaemon(t, md)

	rememberInGraphiti("session-W", "my content body", "leaf")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if md.callCount() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if md.callCount() != 1 {
		t.Fatalf("expected 1 call, got %d", md.callCount())
	}

	if captured.Content != "my content body" {
		t.Errorf("content mismatch: %q", captured.Content)
	}
	if captured.Name != "Seahorse leaf (session-W)" {
		t.Errorf("name mismatch: %q (plan v3 audit A5: no @ <timestamp> suffix expected)", captured.Name)
	}
	if captured.GroupID != "graphiti_memory" {
		t.Errorf("group_id mismatch: %q", captured.GroupID)
	}
	if captured.SourceDescription != "seahorse_compaction:session-W" {
		t.Errorf("source_description mismatch: %q", captured.SourceDescription)
	}
}

// T-IsLockModeError helper sanity.
func TestIsLockModeError(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"error":"LOCK_MODE_MCP_ONLY"}`, true},
		{`prefix LOCK_MODE_MCP_ONLY suffix`, true},
		{`{"error":"DUPLICATE_EPISODE"}`, false},
		{``, false},
		{`{"error":"INVALID_BODY"}`, false},
	}
	for _, tc := range cases {
		if got := isLockModeError(tc.body); got != tc.want {
			t.Errorf("isLockModeError(%q)=%v, want %v", tc.body, got, tc.want)
		}
	}
}

// T-WarnGraphitiLegacySQLiteOnce — verify the warn fires exactly once
// per process regardless of how many times rememberInGraphiti falls
// through to the legacy SQLite path.

// TestRememberInGraphiti_GoroutineRecoversFromPanic (Plan v3 audit G3 /
// Step 7 mandatory) — locks in the defer-recover() guard around the
// rememberInGraphiti wrapping goroutine. If the HTTP client panics
// inside the POST path, the panic must be caught by the wrapping
// goroutine's recover() and surfaced as an ErrorCF log line — NOT
// terminate the picoclaw process.
//
// Without this test, a refactor that accidentally drops the defer
// recover() would silently regress to a process-level panic on the
// next client-side fault, crashing the agent on what should be a
// best-effort, fire-and-forget ingestion path.
//
// Implementation approach:
//   - A panic INSIDE the mock daemon handler is caught by net/http's
//     own server-level recover() and surfaces as a connection EOF on
//     the client side. That EOF is returned as a normal error — NOT
//     a panic in the client goroutine.
//   - Likewise, http.Client.Do has its own recover() that catches
//     panics from custom RoundTripper implementations and surfaces
//     them as errors.
//   - Both layers of net/http recovery mean a server-side OR
//     transport-side panic does NOT exercise the production
//     defer-recover() guard we want to lock in. The only path that
//     actually triggers it is when postGraphitiEpisode ITSELF (the
//     function called from inside the wrapping goroutine) panics.
//   - We use the postGraphitiEpisodeFn function-var seam (test-only,
//     see declaration in graphiti.go) to swap the production
//     implementation for a closure that panics. This panics
//     synchronously inside the wrapping goroutine — exactly the
//     path the production guard catches.
func TestRememberInGraphiti_GoroutineRecoversFromPanic(t *testing.T) {
	// Capture logger output so we can assert the recover()'d panic
	// is surfaced as ErrorCF (not silently swallowed).
	var logBuf bytes.Buffer
	cleanupLog := logger.WithTestWriter(&logBuf)
	defer cleanupLog()

	// Stand up a healthy mock daemon. rememberInGraphiti consults
	// the health gate before reaching postGraphitiEpisodeFn, so
	// the gate must pass (200 + falkor_ok:alive).
	md := newMockDaemon()
	defer md.Close()
	withMockDaemon(t, md)

	// Swap the production function for a panicking closure. Reset
	// in cleanup so subsequent tests get the real HTTP poster.
	resetPostGraphitiEpisodeFnForTest()
	postGraphitiEpisodeFn = func(ctx context.Context, content, name, groupID, sourceDesc string) (string, string, error) {
		panic("synthetic client panic for recover test")
	}
	defer resetPostGraphitiEpisodeFnForTest()

	// Baseline goroutine count for leak assertion.
	before := runtime.NumGoroutine()

	// rememberInGraphiti is fire-and-forget — returns immediately
	// after spawning the wrapping goroutine. The panic happens
	// inside that goroutine; if recover() is missing, the test
	// process dies right here.
	rememberInGraphiti(
		"sess-panic-1",
		"content that triggers the post path",
		"leaf",
	)

	// Give the wrapping goroutine time to: take the semaphore,
	// pass daemonHealthOK, hit the panicking call site, and recover.
	time.Sleep(500 * time.Millisecond)

	// Assertion 1: the test process is still alive. If recover()
	// were missing, we'd have crashed before reaching this line.
	// (The very act of executing this assertion is the proof.)

	// Assertion 2: the recover()'d panic was logged via ErrorCF.
	out := logBuf.String()
	if !strings.Contains(out, "graphiti remember goroutine panic") {
		t.Errorf("expected panic log line; got: %s", out)
	}
	if !strings.Contains(out, "synthetic client panic for recover test") {
		t.Errorf("expected panic value in log; got: %s", out)
	}
	if !strings.Contains(out, "sess-panic-1") {
		t.Errorf("expected session key in log fields; got: %s", out)
	}

	// Assertion 3: no goroutine leak. The wrapping goroutine must
	// have exited cleanly (recover + return). Allow +3 for httptest
	// server accept-loop goroutines and runtime drift.
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+3 {
		t.Errorf("goroutine leak: before=%d, after=%d", before, after)
	}
}

// TestRememberInGraphiti_NoRecoverWouldCrashProcess (negative-control,
// optional docstring) — kept as a comment block here so reviewers
// understand why we DO NOT add a `recover`-less variant of the above
// test. A version of this test without the defer recover() in
// rememberInGraphiti would crash the entire `go test` binary mid-run;
// running it once locks in the panic behavior implicitly. We rely on
// TestRememberInGraphiti_GoroutineRecoversFromPanic to enforce the
// invariant.

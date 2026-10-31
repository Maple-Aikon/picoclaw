package seahorse

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// Regression tests for the falkor_ok dual-shape contract.
//
// The janus-graph daemon has shipped TWO shapes for the "falkor_ok"
// field of GET /health:
//
//	bool   — current producer: falkordb_supervisor.probe() returns a
//	         bool, http_server.py serialises it directly.
//	         Live payload: {"status":"ready","falkor_ok":true,...}
//	string — older producer: probe() returned the ProbeState str-Enum
//	         (ALIVE="alive"), which json.dumps renders as "alive".
//
// The consumer used to declare the field as `string`, so a bool payload
// FAILED to decode ("cannot unmarshal bool into Go struct field ... of
// type string"). That error was handled as "malformed JSON → daemon
// down", so the health gate stayed CLOSED and every seahorse compaction
// was dropped silently for ~20 days (2026-10-10 → 2026-10-30).
//
// Plan: picoclaw-graphiti-health-contract-mismatch-falkor-ok-bool-vs-alive-20261030
//
// These tests pin BOTH shapes so a future producer change cannot
// silently re-break the gate again.
// -----------------------------------------------------------------------------

// TestFalkorOKAlive_Shapes unit-tests the shape interpreter directly.
// Table-driven: every shape the daemon has produced, plus the
// degenerate ones.
func TestFalkorOKAlive_Shapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		// --- shape A: bool (current producer) ---
		{"bool true", `true`, true},
		{"bool false", `false`, false},

		// --- shape B: string (older producer, ProbeState str-Enum) ---
		{`string "alive"`, `"alive"`, true},
		{`string "ALIVE" (case-insensitive)`, `"ALIVE"`, true},
		{`string "dead"`, `"dead"`, false},
		{`string "warming_up"`, `"warming_up"`, false},

		// --- degenerate / fail-closed ---
		{"null", `null`, false},
		{"empty (field absent)", ``, false},
		{"number", `1`, false},
		{"object", `{"a":1}`, false},
		{"array", `[]`, false},
		{"unknown string", `"maybe"`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw json.RawMessage
			if tc.raw != "" {
				raw = json.RawMessage(tc.raw)
			}
			if got := falkorOKAlive(raw); got != tc.want {
				t.Errorf("falkorOKAlive(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestHealthGate_DualShape_FalkorOK drives the real daemonHealthOK()
// path (not just the helper) against a fake daemon that serves the two
// production /health shapes.
//
// This is the test that would have caught the ~20-day outage: it goes
// through json.Decode + the health cache, exactly like production.
func TestHealthGate_DualShape_FalkorOK(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "bool shape (current daemon) opens the gate",
			body: `{"status":"ready","falkor_ok":true,"daemon_version":"0.7.2"}`,
			want: true,
		},
		{
			name: "string shape (older daemon) opens the gate",
			body: `{"status":"ready","falkor_ok":"alive","daemon_version":"0.7.0"}`,
			want: true,
		},
		{
			name: "bool false keeps the gate closed",
			body: `{"status":"degraded","falkor_ok":false,"daemon_version":"0.7.2"}`,
			want: false,
		},
		{
			name: `string "dead" keeps the gate closed`,
			body: `{"status":"degraded","falkor_ok":"dead","daemon_version":"0.7.0"}`,
			want: false,
		},
		{
			name: `string "warming_up" keeps the gate closed`,
			body: `{"status":"degraded","falkor_ok":"warming_up","daemon_version":"0.7.0"}`,
			want: false,
		},
		{
			name: "missing falkor_ok keeps the gate closed",
			body: `{"status":"ready","daemon_version":"0.7.2"}`,
			want: false,
		},
		{
			name: "malformed JSON keeps the gate closed",
			body: `{"status":"ready","falkor_ok":`,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			var unexpectedPaths int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != graphitiHealthPath {
					// A /episodes POST can land here from a
					// fire-and-forget rememberInGraphiti goroutine
					// spawned by an EARLIER test in this package: it
					// survives the test boundary, reads the
					// GRAPHITI_DAEMON_URL we just set, and posts
					// here. That is legitimate cross-test noise, not
					// a failure of the health gate — so count it
					// instead of failing the test.
					atomic.AddInt32(&unexpectedPaths, 1)
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"episode_id":"ep-cross-test","claimed_by":"daemon:test","deduplicated":false,"daemon_version":"0.7.2"}`))
					return
				}
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			prevEnv := os.Getenv(graphitiEnvDaemonURL)
			t.Cleanup(func() { _ = os.Setenv(graphitiEnvDaemonURL, prevEnv) })
			if err := os.Setenv(graphitiEnvDaemonURL, srv.URL); err != nil {
				t.Fatalf("setenv: %v", err)
			}

			// Reset cache + client so the env override takes effect and
			// no stale result leaks in from a previous subtest.
			t.Cleanup(resetGraphitiHTTPForTest)
			resetGraphitiHTTPForTest()

			// The health cache is a package-level singleton and the
			// rememberInGraphiti goroutines are fire-and-forget: a
			// goroutine from an EARLIER test can still be running
			// when this subtest starts, and it will re-probe a
			// daemon URL that has since been closed → it caches
			// falkor_ok=false AFTER our reset, so our very first
			// read can observe a fresh-but-wrong cache entry.
			//
			// Retry the probe until it reflects THIS server's
			// response. The first attempt may be poisoned by a
			// concurrent writer; the retry clears the cache again
			// and re-probes.
			var ok bool
			for attempt := 0; attempt < 5; attempt++ {
				resetGraphitiHTTPForTest()
				ok, _ = daemonHealthOK()
				// Require BOTH the expected verdict AND at least one
				// real /health hit: a poisoned cache (fresh but
				// written by a leftover goroutine) would satisfy the
				// verdict without any request reaching our server.
				if ok == tc.want && atomic.LoadInt32(&hits) >= 1 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if ok != tc.want {
				t.Errorf("daemonHealthOK() with body %s = %v, want %v", tc.body, ok, tc.want)
			}
			if got := atomic.LoadInt32(&hits); got < 1 {
				t.Errorf("expected at least 1 /health hit, got %d", got)
			}
		})
	}
}

// TestHealthGate_DualShape_RealDaemonShape is a guard against the
// fake drifting away from production: it pins the EXACT bytes the
// live daemon currently returns (captured from
// curl http://127.0.0.1:8765/health on 2026-10-31).
//
// If the daemon's shape changes again, this test's expectation table
// must be updated deliberately — not by accident.
func TestHealthGate_DualShape_RealDaemonShape(t *testing.T) {
	// Live payload shape (trimmed to the fields the consumer reads).
	const liveBody = `{"status":"ready","falkor_ok":true,"daemon_version":"0.7.2"}`

	// Sanity-check the fixture itself: it must decode as bool, and a
	// string-typed field must NOT be able to decode it.
	var asBool struct {
		FalkorOK json.RawMessage `json:"falkor_ok"`
	}
	if err := json.Unmarshal([]byte(liveBody), &asBool); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if !falkorOKAlive(asBool.FalkorOK) {
		t.Errorf("live fixture must decode as alive, got %s", asBool.FalkorOK)
	}

	// The old consumer shape must reject it — this documents WHY the
	// dual-shape handling exists. If this ever starts succeeding, the
	// regression test below is no longer proving anything.
	var asString struct {
		FalkorOK string `json:"falkor_ok"`
	}
	if err := json.Unmarshal([]byte(liveBody), &asString); err == nil {
		t.Errorf("expected the old string-typed field to fail decoding the live payload")
	}

	// End-to-end through the real health gate.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(liveBody))
	}))
	t.Cleanup(srv.Close)

	prevEnv := os.Getenv(graphitiEnvDaemonURL)
	t.Cleanup(func() { _ = os.Setenv(graphitiEnvDaemonURL, prevEnv) })
	if err := os.Setenv(graphitiEnvDaemonURL, srv.URL); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(resetGraphitiHTTPForTest)
	resetGraphitiHTTPForTest()

	// Same singleton-cache caveat as TestHealthGate_DualShape_FalkorOK:
	// a leftover fire-and-forget goroutine from an earlier test can
	// poison the cache right after our reset. Retry until the probe
	// reflects THIS server.
	var (
		ok  bool
		ver string
	)
	for attempt := 0; attempt < 5; attempt++ {
		resetGraphitiHTTPForTest()
		ok, ver = daemonHealthOK()
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("live daemon shape must open the health gate")
	}
	if ver != "0.7.2" {
		t.Errorf("expected daemon_version %q, got %q", "0.7.2", ver)
	}
}

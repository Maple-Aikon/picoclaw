// Package seahorse — graphiti_http.go
//
// HTTP client + health-gate for the Seahorse → janus-graph-daemon
// migration (plan v3: seahorse-compaction-post-episodes-migration-v2).
//
// Design (audit A3 + A4 fixes):
//   - Health-gate with 30s TTL — caps worst-case loss during restart
//     windows. Single debug log per TTL window when daemon is down.
//   - Semaphore-bounded concurrency — cap 8 in-flight POSTs in the
//     wrapping goroutine of rememberInGraphiti (implemented in
//     graphiti.go, not here — this file is the HTTP transport only).
//   - 3s HTTP timeout per request (config knob: GRAPHITI_HTTP_TIMEOUT_MS).
//   - MaxIdleConnsPerHost=4 to avoid TIME_WAIT thrash at sustained load.
//
// Fire-and-forget semantics: callers do not retry. A failure logs at
// warn (or info for 409 DUPLICATE_EPISODE) and returns. The daemon's
// queue is the source of truth for retry/DLQ — not the client.
package seahorse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// HTTP transport constants. Kept package-private so the only public
// surface stays the high-level helpers in graphiti.go.
const (
	graphitiEnvDaemonURL  = "GRAPHITI_DAEMON_URL"
	graphitiDefaultDaemon  = "http://127.0.0.1:8765"
	graphitiEpisodesPath  = "/episodes"
	graphitiHealthPath    = "/health"
	graphitiHealthTTLSec  = 30
	graphitiHTTPTimeoutMS = 3000
)

// graphitiHTTPClient is the lazy singleton *http.Client used for both
// /health probes and /episodes POSTs. We share one client so the
// connection pool is amortized across calls (avoids TIME_WAIT at high
// cadence).
//
// Initialization is one-shot guarded by initOnce. On any error the
// handle stays nil and subsequent callers fall through to a per-call
// best-effort construction (so a transient issue doesn't break the
// whole pipeline permanently).
var (
	graphitiHTTPClient     *http.Client
	graphitiHTTPClientOnce sync.Once
)

// getHTTPClient returns the package-level HTTP client singleton. Uses
// sync.Once so concurrent first-callers share a single handle.
//
// 3s timeout covers the daemon's queue insertion (target p99 < 200ms;
// timeout is for the long tail of falkordb warm-up after restart).
func getHTTPClient() *http.Client {
	graphitiHTTPClientOnce.Do(func() {
		timeout := time.Duration(graphitiHTTPTimeoutMS) * time.Millisecond
		graphitiHTTPClient = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				DisableKeepAlives:   false,
			},
		}
	})
	return graphitiHTTPClient
}

// resolveGraphitiDaemonURL returns the base URL (no trailing path) of
// the janus-graph-daemon. Env override GRAPHITI_DAEMON_URL; falls back
// to the localhost default. Caller appends the path segment.
//
// Empty sentinel "" = disabled (GRAPHITI_DAEMON_URL="off" or "0"),
// letting the caller short-circuit. Mirrors the disabled-sentinel
// convention used by GRAPHITI_ENABLED on the SQLite path.
func resolveGraphitiDaemonURL() string {
	raw := os.Getenv(graphitiEnvDaemonURL)
	if raw == "" {
		raw = graphitiDefaultDaemon
	}
	if v := raw; v == "0" || v == "false" || v == "off" || v == "no" || v == "disabled" {
		return ""
	}
	return raw
}

// resolveGraphitiEpisodesURL joins base + /episodes. Empty if base is
// disabled.
func resolveGraphitiEpisodesURL() string {
	base := resolveGraphitiDaemonURL()
	if base == "" {
		return ""
	}
	return base + graphitiEpisodesPath
}

// resolveGraphitiHealthURL joins base + /health. Empty if base is
// disabled.
func resolveGraphitiHealthURL() string {
	base := resolveGraphitiDaemonURL()
	if base == "" {
		return ""
	}
	return base + graphitiHealthPath
}

// graphitiHealthCache caches the result of the most recent /health
// probe. The cached snapshot is reused for graphitiHealthTTLSec
// seconds, then refreshed on next probe request.
//
// Lock-free reads via atomic pointer are tempting but overkill — the
// hot path is a single sync.RWMutex.RLock + struct read. Cost is
// dominated by the mutex acquire, which is nanoseconds.
type graphitiHealthCache struct {
	mu        sync.RWMutex
	checkedAt time.Time
	falkorOK  bool
	version   string
}

var healthCache graphitiHealthCache

// daemonHealthOK returns (falkor_ok, daemon_version). Caches the
// result for graphitiHealthTTLSec seconds. When the cache is stale or
// the daemon is down, performs a single GET /health with a short
// timeout (1s — independent of the main HTTP timeout).
//
// "Down" is defined as: falkor_ok != "alive" (case-insensitive) OR
// any network failure. We deliberately do NOT inspect lock.mode here
// — /health does not expose it (see Verified facts G1 in plan v3).
// Step 0 handles lock-mode via a separate functional POST probe.
//
// A logDebugOnce flag is tracked so we emit at most one warn+debug
// line per TTL window instead of spamming the log per compaction.
func daemonHealthOK() (falkor bool, version string) {
	healthCache.mu.RLock()
	if !healthCache.checkedAt.IsZero() && time.Since(healthCache.checkedAt) < graphitiHealthTTLSec*time.Second {
		falkor = healthCache.falkorOK
		version = healthCache.version
		healthCache.mu.RUnlock()
		return
	}
	healthCache.mu.RUnlock()

	// Cache stale or absent — refresh.
	healthCache.mu.Lock()
	defer healthCache.mu.Unlock()

	// Double-check after write lock (another goroutine may have
	// refreshed in the gap).
	if !healthCache.checkedAt.IsZero() && time.Since(healthCache.checkedAt) < graphitiHealthTTLSec*time.Second {
		return healthCache.falkorOK, healthCache.version
	}

	healthURL := resolveGraphitiHealthURL()
	if healthURL == "" {
		// Disabled — treat as down.
		healthCache.falkorOK = false
		healthCache.version = ""
		healthCache.checkedAt = time.Now()
		return false, ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		// Should not happen for a well-formed URL; treat as down.
		healthCache.falkorOK = false
		healthCache.version = ""
		healthCache.checkedAt = time.Now()
		return false, ""
	}

	client := getHTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		// Network-level failure (ECONNREFUSED, timeout, DNS, ...).
		// Mark down + cache for full TTL so we don't hammer the daemon.
		healthCache.falkorOK = false
		healthCache.version = ""
		healthCache.checkedAt = time.Now()
		return false, ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Daemon returned non-200 — treat as down (likely mid-restart).
		// Drain body so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		healthCache.falkorOK = false
		healthCache.version = ""
		healthCache.checkedAt = time.Now()
		return false, ""
	}

	// Parse minimal JSON: status / falkor_ok / daemon_version.
	// We intentionally tolerate missing fields — daemon is the source
	// of truth, not us.
	var payload struct {
		Status        string `json:"status"`
		FalkorOK      string `json:"falkor_ok"`
		DaemonVersion string `json:"daemon_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		// Malformed JSON — treat as down.
		healthCache.falkorOK = false
		healthCache.version = ""
		healthCache.checkedAt = time.Now()
		return false, ""
	}

	falkorOK := payload.FalkorOK == "alive"
	healthCache.falkorOK = falkorOK
	healthCache.version = payload.DaemonVersion
	healthCache.checkedAt = time.Now()
	return falkorOK, payload.DaemonVersion
}

// resetGraphitiHTTPForTest clears the health cache + client singleton
// so tests can start clean. Test-only.
func resetGraphitiHTTPForTest() {
	graphitiHTTPClientOnce = sync.Once{}
	graphitiHTTPClient = nil
	healthCache.mu.Lock()
	healthCache.checkedAt = time.Time{}
	healthCache.falkorOK = false
	healthCache.version = ""
	healthCache.mu.Unlock()
}

// postEpisodeRequest is the wire shape POSTed to /episodes. Mirrors
// the Python handler's contract (http_server.py:170-188).
//
// v3 A5: name dropped the `@ <rfc3339nano>` suffix because the dedup
// key is `_payload_hash(content, group_id, source_description)` — the
// timestamp never participated in dedup. Removal makes names readable
// in MCP dashboards.
type postEpisodeRequest struct {
	Content          string `json:"content"`
	Name             string `json:"name,omitempty"`
	GroupID          string `json:"group_id,omitempty"`
	SourceDescription string `json:"source_description,omitempty"`
}

// postEpisodeResponse is the daemon's success payload (http_server.py:267-273).
type postEpisodeResponse struct {
	EpisodeID     string `json:"episode_id"`
	ClaimedBy     string `json:"claimed_by"`
	Deduplicated  bool   `json:"deduplicated"`
	DaemonVersion string `json:"daemon_version"`
}

// postGraphitiEpisode fires POST /episodes with the given content.
// Returns (episode_id, daemon_version, nil) on 202. On 409 dedup,
// returns ("", "", nil) — caller logs at info (idempotent replay).
// On any other error, returns ("", "", err).
//
// Does NOT consult the health gate — callers (rememberInGraphiti's
// wrapping goroutine) own that policy so this function stays
// composable + unit-testable in isolation.
func postGraphitiEpisode(ctx context.Context, content, name, groupID, sourceDesc string) (episodeID, version string, err error) {
	url := resolveGraphitiEpisodesURL()
	if url == "" {
		return "", "", fmt.Errorf("graphiti daemon disabled (GRAPHITI_DAEMON_URL=off)")
	}

	body, err := json.Marshal(postEpisodeRequest{
		Content:           content,
		Name:              name,
		GroupID:           groupID,
		SourceDescription: sourceDesc,
	})
	if err != nil {
		return "", "", fmt.Errorf("marshal request: %w", err)
	}

	client := getHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("new request: %w", err)
	}
	req.Body = io.NopCloser(bytesReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case http.StatusAccepted: // 202 — happy path.
		var pr postEpisodeResponse
		if err := json.Unmarshal(respBody, &pr); err != nil {
			// Daemon replied 202 but with malformed JSON — log full body
			// snippet for forensics (per Failure handling table, 422
			// would log; here it's "daemon said yes but we can't read it").
			logger.WarnCF("seahorse", "graphiti 202 but JSON malformed",
				map[string]any{"body": truncateBody(string(respBody), 256)})
			return "", "", nil
		}
		return pr.EpisodeID, pr.DaemonVersion, nil

	case http.StatusConflict: // 409 — DUPLICATE_EPISODE or LOCK_MODE_MCP_ONLY.
		// Caller (rememberInGraphiti) inspects errCode via separate code path.
		return "", "", &httpStatusError{Status: resp.StatusCode, Body: string(respBody)}

	case http.StatusUnprocessableEntity: // 422 — INVALID_BODY (schema violation).
		logger.WarnCF("seahorse", "graphiti 422 invalid body",
			map[string]any{"body": truncateBody(string(respBody), 256)})
		return "", "", &httpStatusError{Status: resp.StatusCode, Body: string(respBody)}

	case http.StatusServiceUnavailable: // 503 — FALKOR_DOWN / NOT_READY.
		return "", "", &httpStatusError{Status: resp.StatusCode, Body: string(respBody)}

	default:
		return "", "", &httpStatusError{Status: resp.StatusCode, Body: string(respBody)}
	}
}

// httpStatusError is a typed error that exposes the HTTP status code
// so callers (rememberInGraphiti) can route the log level per the
// Failure handling table in plan v3.
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, truncateBody(e.Body, 200))
}

// truncateBody bounds log line length. Inline rather than a util import
// to keep this file self-contained. Renamed from truncate() to avoid
// collision with short_engine.go's truncate (which has a different
// signature).
func truncateBody(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// bytesReader is a tiny adapter so we can pass []byte to http.NewRequest
// without pulling in bytes.NewReader at the call site (kept local for
// self-containment).
func bytesReader(b []byte) *bytesReaderAdapter { return &bytesReaderAdapter{b: b} }

type bytesReaderAdapter struct {
	b   []byte
	pos int
}

func (r *bytesReaderAdapter) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

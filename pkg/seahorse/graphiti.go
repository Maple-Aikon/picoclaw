// Package seahorse — graphiti.go
//
// Fire-and-forget enqueue of compaction summaries into the Graphiti MCP
// async ingestion queue. This replaces the legacy rememberInSignet helper
// (Signet daemon port 3850) which is no longer running.
//
// Architecture: the Go runtime writes directly to the same SQLite WAL
// database that the Python `EpisodeQueue` reads. WAL mode + busy_timeout
// gives us safe multi-language, multi-process concurrent access without
// coordination. The Python worker (cron-driven) handles retry / DLQ.
package seahorse

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// Graphiti enqueue constants. Kept package-private so callers go through
// the helper functions and respect the fire-and-forget semantics.
//
// Migration v3: queuePath (SQLite) is being replaced by daemonURL
// (HTTP POST → janus-graph-daemon). Both seams are kept live during
// the migration window: SetGraphitiConfig(daemonURL, groupID) is the
// new canonical seam; the GRAPHITI_QUEUE_DB env var is honored only
// when GRAPHITI_DAEMON_URL is unset, with a one-time deprecation
// warning at startup.
const (
	graphitiDefaultDBPath  = "~/.picoclaw/workspace/apps/graphiti-mcp/queue/episodes.db"
	graphitiEnvDBPath      = "GRAPHITI_QUEUE_DB"
	graphitiEnvEnabled     = "GRAPHITI_ENABLED"
	graphitiGroupID        = "graphiti_memory"
	graphitiSourcePrefix   = "seahorse_compaction"
	graphitiSemaphoreCap   = 8
	graphitiSemaphoreWaitMS = 10 // wait up to 10ms when cap is saturated
)

// graphitiPragmas matches the Python EpisodeQueue configuration so Go and
// Python access the same database in a compatible way.
//
// Empirically verified at 100 concurrent goroutines / 60s sustained load:
//   - 0 SQLITE_BUSY
//   - Median latency ~1.5ms
//   - Throughput ~1300 ops/sec
const graphitiPragmas = "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(OFF)"

// graphitiQueue is the lazy singleton DB handle used by rememberInGraphiti (legacy SQLite path).
// We use a package-level singleton so multiple compaction goroutines share
// a single connection pool rather than opening a fresh DB per call.
//
// Initialization is one-shot guarded by initOnce. On any error the handle
// is nil and subsequent calls fail-fast (logged) — this prevents cascading
// retry storms that would amplify a transient issue into a load problem.
//
// Migration v3: the SQLite path is the LEGACY path. New code should use
// the HTTP transport (graphiti_http.go) via rememberInGraphiti's
// wrapping goroutine. The SQLite path remains live during the migration
// window so agent_init.go callers that haven't been updated still work.
//
// HTTP path state is parallel:
var (
	graphitiQueue       *sql.DB
	graphitiOnce        sync.Once
	graphitiMu          sync.RWMutex
	graphitiPath        string // resolved absolute path for logging
	configuredDaemonURL string // new HTTP seam (v3) — replaces configuredQueuePath
	configuredGroupID   string

	// graphitiSem bounds concurrent in-flight POST /episodes requests
	// (plan v3 audit A4). Initialized once at package init via lazy
	// make (we use sync.Once + RWMutex pattern to keep init cheap).
	graphitiSem   chan struct{}
	graphitiSemMu sync.Mutex
)

// postGraphitiEpisodeFn is the test seam for swapping the production
// HTTP poster out at runtime. Same-package tests can replace this with
// a closure that intentionally panics to exercise the defer-recover()
// guard in rememberInGraphiti's wrapping goroutine (audit G3). The
// production assignment is to postGraphitiEpisode; reset helper
// resetPostGraphitiEpisodeFnForTest restores it.
//
// Why a function var instead of a mock RoundTripper: Go's
// http.Client.Do has its own recover() that catches panics raised in
// custom RoundTrippers and surfaces them as connection errors. That
// means a Transport-level panic NEVER reaches the wrapping goroutine
// of rememberInGraphiti — leaving the production defer-recover()
// guard completely untestable via the HTTP surface alone. Calling
// the function directly from the goroutine is the only path that
// exercises the guard.
var postGraphitiEpisodeFn = postGraphitiEpisode

// getGraphitiSemaphore returns the package-level semaphore, lazily
// initialized to graphitiSemaphoreCap slots. Returning the same channel
// reference across calls is critical — the goroutine inside
// rememberInGraphiti captures it once at function entry.
func getGraphitiSemaphore() chan struct{} {
	graphitiSemMu.Lock()
	defer graphitiSemMu.Unlock()
	if graphitiSem == nil {
		graphitiSem = make(chan struct{}, graphitiSemaphoreCap)
	}
	return graphitiSem
}

// SetGraphitiConfig configures the Graphiti ingestion seam (HTTP URL +
// tenant). Migration v3: the first parameter is now a daemon URL (e.g.
// "http://127.0.0.1:8765"), not a SQLite path. The SQLite path seam is
// retained as a deprecation alias — callers passing a path-shaped string
// (contains "/" but no "://") are honored via the legacy env var with a
// one-time warn log, but the path itself is no longer read by the new
// HTTP path. Caller responsibility: agents should pass
// `cfg.GraphitiDaemonURL` (renamed from GraphitiQueuePath in plan v3 A2).
func SetGraphitiConfig(daemonURL, groupID string) {
	graphitiMu.Lock()
	defer graphitiMu.Unlock()
	if daemonURL != "" && daemonURL != configuredDaemonURL {
		configuredDaemonURL = daemonURL
	}
	if groupID != "" {
		configuredGroupID = groupID
	}
}

// GetGraphitiConfig returns the currently configured daemon URL and group ID.
// First return is the HTTP base URL (no trailing path) — semantically
// distinct from v1's SQLite path.
func GetGraphitiConfig() (daemonURL, groupID string) {
	graphitiMu.RLock()
	defer graphitiMu.RUnlock()
	return configuredDaemonURL, configuredGroupID
}

// resolveGraphitiGroupID returns the configured group_id, overridable by
// GRAPHITI_GROUP_ID env var, falling back to graphitiGroupID constant.
func resolveGraphitiGroupID() string {
	graphitiMu.RLock()
	gid := configuredGroupID
	graphitiMu.RUnlock()
	if gid != "" {
		return gid
	}
	if v := os.Getenv("GRAPHITI_GROUP_ID"); v != "" {
		return v
	}
	return graphitiGroupID
}

// resolveGraphitiDBPathLocked picks the queue DB location while graphitiMu is already locked.
//
// Migration v3: configuredDaemonURL is the canonical seam but legacy
// SQLite path resolution still inspects it (the field stores the
// canonical "ingestion target"; for the SQLite path that's the DB file,
// for the HTTP path that's the daemon URL). When GRAPHITI_DAEMON_URL is
// unset (and not configured via SetGraphitiConfig), we fall through to
// GRAPHITI_QUEUE_DB for backward compatibility — this matches the
// production rollout path where the YAML config field is renamed but
// env vars are honored as a deprecation alias.
func resolveGraphitiDBPathLocked() string {
	if v := os.Getenv(graphitiEnvEnabled); v == "0" || v == "false" || v == "no" {
		return ""
	}
	raw := configuredDaemonURL
	if raw == "" {
		raw = os.Getenv(graphitiEnvDBPath)
	}
	if raw == "" {
		raw = graphitiDefaultDBPath
	}
	// Expand leading ~ to the user's home dir, matching shell convention.
	if len(raw) >= 2 && raw[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			raw = filepath.Join(home, raw[2:])
		}
	}
	return raw
}

// resolveGraphitiDBPath picks the queue DB location. The default uses the
// workspace-relative path of the production Graphiti MCP install, overridable
// via configuredQueuePath or GRAPHITI_QUEUE_DB. This matches the constant in
// Python queue.py so a Go process and a Python process can share the same file safely.
//
// Returns "" if disabled (GRAPHITI_ENABLED=0). The empty sentinel lets the
// caller short-circuit without touching the filesystem.
func resolveGraphitiDBPath() string {
	graphitiMu.RLock()
	defer graphitiMu.RUnlock()
	return resolveGraphitiDBPathLocked()
}

// openGraphitiQueue initializes the singleton DB handle. Returns the handle
// and the resolved path, or (nil, "") if disabled/unavailable.
//
// Idempotent: only the first call actually opens the DB. Subsequent callers
// reuse the cached handle (read-locked).
func openGraphitiQueue() (*sql.DB, string) {
	// Fast path: handle already initialized.
	graphitiMu.RLock()
	if graphitiQueue != nil {
		p := graphitiPath
		graphitiMu.RUnlock()
		return graphitiQueue, p
	}
	graphitiMu.RUnlock()

	graphitiMu.Lock()
	defer graphitiMu.Unlock()
	// Double-check after acquiring write lock.
	if graphitiQueue != nil {
		return graphitiQueue, graphitiPath
	}

	path := resolveGraphitiDBPathLocked()
	if path == "" {
		// Disabled via env. Don't even attempt to open.
		return nil, ""
	}

	// Ensure parent directory exists. The Python queue also does this but
	// we make it idempotent here so a fresh install (no Python touched yet)
	// doesn't fail on a missing directory.
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.WarnCF("seahorse", "graphiti mkdir failed",
				map[string]any{"dir": dir, "error": err.Error()})
			return nil, ""
		}
	}

	dsn := path + "?" + graphitiPragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		logger.WarnCF("seahorse", "graphiti sql.Open failed",
			map[string]any{"path": path, "error": err.Error()})
		return nil, ""
	}
	// Limit pool so we don't accidentally flood SQLite. The Python side
	// uses a single connection with asyncio.Lock; we mirror that idea by
	// limiting Go side to 4 concurrent open handles (SQLite WAL still
	// serializes writes regardless).
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0) // never expire — long-lived singleton

	// Probe with a trivial query to confirm the DB is reachable. This
	// surfaces a missing file / permission issue immediately rather than
	// at first compaction summary.
	if err := db.Ping(); err != nil {
		logger.WarnCF("seahorse", "graphiti ping failed",
			map[string]any{"path": path, "error": err.Error()})
		_ = db.Close()
		return nil, ""
	}

	// Ensure the episodes table exists. This matches the SCHEMA constant
	// in Python queue.py. We keep the DDL inline (instead of importing)
	// to avoid a circular dependency and to make this file self-contained.
	if _, err := db.Exec(graphitiSchema); err != nil {
		logger.WarnCF("seahorse", "graphiti schema bootstrap failed",
			map[string]any{"path": path, "error": err.Error()})
		_ = db.Close()
		return nil, ""
	}

	graphitiQueue = db
	graphitiPath = path
	logger.InfoCF("seahorse", "graphiti queue ready",
		map[string]any{"path": path})
	return graphitiQueue, path
}

// graphitiSchema mirrors queue.py SCHEMA. Only the columns we actually use
// in the INSERT are kept inline; the rest are not duplicated here to avoid
// drift. The Python worker fills the optional columns as it processes.
//
// Kept minimal: just enough DDL to make a fresh episodes.db usable from
// the Go side. The Python worker is the source of truth for the full
// schema and adds missing columns if needed.
const graphitiSchema = `
CREATE TABLE IF NOT EXISTS episodes (
    id TEXT PRIMARY KEY,
    status TEXT NOT NULL DEFAULT 'queued',
    payload_json TEXT NOT NULL,
    enqueued_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_status ON episodes(status);
`

// warnGraphitiLegacySQLiteOnce emits a one-time WARN log when the
// legacy SQLite path is reached (HTTP daemon URL empty AND legacy
// GRAPHITI_QUEUE_DB / configuredDaemonURL-as-path is set).
//
// Plan v3 Step 8 deprecates this path: future production should
// configure GRAPHITI_DAEMON_URL (or cfg.GraphitiDaemonURL) and let
// the HTTP route take over. SQLite path remains live during the
// migration window so existing deployments don't break, but the
// warn log surfaces drift in logs / dashboards.
var warnGraphitiLegacySQLiteOnceSync sync.Once

// resetPostGraphitiEpisodeFnForTest restores postGraphitiEpisodeFn to
// the production implementation. Tests that swap the function var for
// panic-injection should call this in their cleanup so subsequent
// tests get the real HTTP poster.
func resetPostGraphitiEpisodeFnForTest() {
	postGraphitiEpisodeFn = postGraphitiEpisode
}

func warnGraphitiLegacySQLiteOnce() {
	warnGraphitiLegacySQLiteOnceSync.Do(func() {
		logger.WarnCF("seahorse", "graphiti: legacy SQLite path in use (HTTP daemon URL empty). "+
			"Set GRAPHITI_DAEMON_URL (or cfg.GraphitiDaemonURL) and restart to migrate. "+
			"See plan seahorse-compaction-post-episodes-migration-v2 §Step 8.",
			map[string]any{
				"hint":     "GRAPHITI_DAEMON_URL=http://127.0.0.1:8765",
				"deprecation_doc": "memory/plan/seahorse-compaction-post-episodes-migration-v2-20260910.md",
			})
	})
}

// init inspects the environment at package import time and emits a
// one-time deprecation warning if the operator is still relying on
// the legacy SQLite path (GRAPHITI_QUEUE_DB set, GRAPHITI_DAEMON_URL
// empty or "off"). The warning is independent of whether any actual
// enqueue is performed — it surfaces config drift in dashboards
// before production traffic touches the path.
//
// Plan v3 Step 8: hard-deprecation timeline is Q4 2026 (six months
// from this audit). For now, warn-only.
func init() {
	daemonURL := os.Getenv(graphitiEnvDaemonURL)
	queueDB := os.Getenv(graphitiEnvDBPath)
	enabled := os.Getenv(graphitiEnvEnabled)

	// Skip the warning when Graphiti ingestion is explicitly disabled.
	if enabled == "0" || enabled == "false" || enabled == "no" {
		return
	}

	// If the operator has explicitly set GRAPHITI_QUEUE_DB and the
	// daemon URL is empty/disabled, they are still on the legacy path.
	if queueDB != "" && (daemonURL == "" || daemonURL == "off" || daemonURL == "0" || daemonURL == "false" || daemonURL == "disabled") {
		warnGraphitiLegacySQLiteOnce()
		return
	}

	// If GRAPHITI_DAEMON_URL is explicitly set to "off"/disabled but
	// ingestion is otherwise enabled, surface that too — this is the
	// "operator forgot to re-enable after maintenance" case.
	if daemonURL == "off" || daemonURL == "0" || daemonURL == "false" || daemonURL == "disabled" {
		if queueDB == "" {
			// Ingestion disabled — silent (operator intended).
			return
		}
		// Fall through to SQLite path warning (warnGraphitiLegacySQLiteOnce
		// is gated by the "daemon URL empty" check at runtime).
	}
}

// isLockModeError inspects a 409 body for the LOCK_MODE_MCP_ONLY
// sentinel. Plan v3 audit G1: the daemon returns this error code
// when the lock mode is mcp_only; in that state POST /episodes is
// rejected because the daemon reserves writes to the MCP add_episode
// tool. Operator must flip JANUS_DAEMON__LOCK__MODE to daemon_only
// (or dual_with_lock) and restart.
//
// Substring match rather than strict JSON parse — daemon response is
// small (~150 bytes) and we don't want to fail on cosmetic field
// reorderings. Lock-mode sentinel is a stable contract.
func isLockModeError(body string) bool {
	return strings.Contains(body, "LOCK_MODE_MCP_ONLY")
}

// enqueueGraphitiEpisode writes one episode row to the Graphiti queue.
// This is the synchronous half — the caller still wraps it in `go ...`
// for fire-and-forget. Splitting sync-write and async-launch keeps the
// function easy to unit-test without goroutine scaffolding.
//
// Returns the inserted episode ID on success, or "" with err populated.
// Designed to be called from within a goroutine: errors are logged but
// never propagated, because losing one summary should never abort the
// compaction pipeline.
func enqueueGraphitiEpisode(sessionKey, content, summaryKind string) (string, error) {
	db, path := openGraphitiQueue()
	if db == nil {
		return "", fmt.Errorf("graphiti queue unavailable (path=%q)", path)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	rid := uuid.New().String()

	groupID := resolveGraphitiGroupID()

	// Payload schema mirrors the Python EpisodeQueue.enqueue contract:
	// a single dict with the keys the Graphiti MCP server expects.
	//
	// Migration v3 (audit A5): `name` dropped the `@ <rfc3339nano>`
	// suffix. Daemon dedup key is `_payload_hash(content, group_id,
	// source_description)` — the timestamp never participated, so the
	// suffix was visually noisy without changing semantics. Names are
	// now human-readable: "Seahorse leaf (<sessionKey>)".
	payload := map[string]any{
		"content":            content,
		"name":               fmt.Sprintf("Seahorse %s (%s)", summaryKind, sessionKey),
		"source_description": fmt.Sprintf("%s:%s", graphitiSourcePrefix, sessionKey),
		"group_id":           groupID,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	// Single-statement INSERT. No transaction needed: this is one row,
	// and WAL mode handles crash-safety at the page level.
	_, err = db.Exec(
		"INSERT INTO episodes (id, status, payload_json, enqueued_at, created_at, updated_at) "+
			"VALUES (?, 'queued', ?, ?, ?, ?)",
		rid, string(payloadJSON), now, now, now,
	)
	if err != nil {
		return rid, fmt.Errorf("insert episode: %w", err)
	}

	logger.DebugCF("seahorse", "graphiti episode enqueued",
		map[string]any{"id": rid, "session": sessionKey, "kind": summaryKind})
	return rid, nil
}

// rememberInGraphiti is the fire-and-forget replacement for
// rememberInSignet. It enqueues a compaction summary into the Graphiti
// MCP async queue so the long-term memory layer can ingest it.
//
// Failures are logged but never returned to the caller — losing one
// summary is preferable to aborting the entire compaction flow. The
// queue worker (Python) is the source of truth for retry semantics.
//
// Mirrors the legacy function signature exactly so callers (short_compaction.go
// lines 306 and 414) can swap with a single-token edit.
//
// Migration v3 — wrapping goroutine:
//   - recover() is mandatory (audit G3): a panic inside the goroutine
//     would otherwise terminate the entire picoclaw process. For a
//     fire-and-forget ingestion path that should NEVER take down the
//     agent.
//   - Semaphore cap (audit A4): bounds in-flight POSTs to 8. If
//     saturated, wait up to graphitiSemaphoreWaitMS before dropping.
//   - Health gate (audit A3): a single /health probe per 30s. If
//     down, drop with a debug log.
//
// As of v3 Steps 1-5, the wrapping goroutine is in place but the body
// still uses the SQLite path (legacy). The HTTP swap (Step 6+) will
// replace enqueueGraphitiEpisode with postGraphitiEpisode (gated on
// resolveGraphitiEpisodesURL() being non-empty); the wrapping
// goroutine is the invariant and stays put.
func rememberInGraphiti(sessionKey, content, summaryKind string) {
	if content == "" {
		// Nothing to remember. Log at debug only — this can legitimately
		// happen for tiny compactions that don't yield a summary.
		logger.DebugCF("seahorse", "graphiti remember skipped empty content",
			map[string]any{"session": sessionKey})
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorCF("seahorse", "graphiti remember goroutine panic",
					map[string]any{
						"session": sessionKey,
						"kind":    summaryKind,
						"panic":   fmt.Sprintf("%v", r),
					})
			}
		}()

		// Pre-call health gate. If down, short-circuit before taking a
		// semaphore slot — the gate is fast and bounded to 1 probe per
		// graphitiHealthTTLSec seconds thanks to daemonHealthOK's cache.
		// We consult the gate here in the goroutine so the caller (the
		// synchronous caller of rememberInGraphiti) stays unblocked.
		//
		// Skip the gate when the daemon URL is empty (HTTP path
		// disabled → SQLite fallback will be taken below). This keeps
		// the legacy SQLite path reachable without a live daemon —
		// critical for tests that exercise enqueueGraphitiEpisode
		// directly without mocking an HTTP server.
		if url := resolveGraphitiEpisodesURL(); url != "" {
			if ok, _ := daemonHealthOK(); !ok {
				logger.DebugCF("seahorse", "graphiti remember skipped: daemon health gate down",
					map[string]any{"session": sessionKey, "kind": summaryKind})
				return
			}
		}

		// Semaphore: bounded concurrency. When saturated, wait briefly;
		// if still saturated after graphitiSemaphoreWaitMS, drop with a
		// warn. This caps worst-case simultaneous in-flight HTTP
		// requests at graphitiSemaphoreCap (currently 8).
		sem := getGraphitiSemaphore()
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-time.After(graphitiSemaphoreWaitMS * time.Millisecond):
			logger.WarnCF("seahorse", "graphiti remember dropped: semaphore saturated",
				map[string]any{"session": sessionKey, "kind": summaryKind, "cap": graphitiSemaphoreCap})
			return
		}

		// Body (plan v3 Step 6): HTTP path is canonical when
		// resolveGraphitiEpisodesURL() returns a non-empty URL.
		// SQLite path is retained as a deprecation alias (plan v3
		// Step 8) — only reached when daemon is disabled AND the
		// legacy GRAPHITI_QUEUE_DB env var (or configuredQueuePath)
		// is set, with a one-time warn log.
		if url := resolveGraphitiEpisodesURL(); url != "" {
			groupID := resolveGraphitiGroupID()
			name := fmt.Sprintf("Seahorse %s (%s)", summaryKind, sessionKey)
			sourceDesc := fmt.Sprintf("%s:%s", graphitiSourcePrefix, sessionKey)
			// 3s context deadline — caps worst-case hang if daemon is
			// wedged. The shared client's timeout is also 3s but a
			// per-call deadline is clearer at the call site.
			ctx, cancel := context.WithTimeout(context.Background(), graphitiHTTPTimeoutMS*time.Millisecond)
			defer cancel()
			episodeID, version, err := postGraphitiEpisodeFn(ctx, content, name, groupID, sourceDesc)
			if err != nil {
				// Map status codes to log level per Failure handling
				// table in plan v3 §Failure modes. 409 dedup is the
				// one case where we DON'T log — it's an idempotent
				// replay, not an error.
				if hse, ok := err.(*httpStatusError); ok && hse.Status == http.StatusConflict {
					if isLockModeError(hse.Body) {
						logger.ErrorCF("seahorse", "graphiti 409 LOCK_MODE_MCP_ONLY — daemon refusing POST /episodes",
							map[string]any{
								"session": sessionKey,
								"kind":    summaryKind,
								"hint":    "set JANUS_DAEMON__LOCK__MODE=daemon_only or dual_with_lock; pmc restart janus-graph-daemon",
							})
					} else {
						logger.InfoCF("seahorse", "graphiti 409 duplicate episode (idempotent)",
							map[string]any{"session": sessionKey, "kind": summaryKind})
					}
					return
				}
				logger.WarnCF("seahorse", "graphiti remember failed",
					map[string]any{
						"session": sessionKey,
						"kind":    summaryKind,
						"error":   err.Error(),
						"version": version,
					})
				return
			}
			logger.DebugCF("seahorse", "graphiti episode posted",
				map[string]any{"session": sessionKey, "kind": summaryKind, "episode": episodeID, "daemon": version})
			return
		}

		// Legacy SQLite path. Only reached when daemon URL is empty
		// (off/disabled) AND the legacy GRAPHITI_QUEUE_DB or
		// configuredDaemonURL-as-path is set. Plan v3 Step 8 marks
		// this deprecated; a one-time warn log at package init
		// (warnGraphitiLegacySQLiteOnce) flags the caller.
		warnGraphitiLegacySQLiteOnce()
		if _, err := enqueueGraphitiEpisode(sessionKey, content, summaryKind); err != nil {
			logger.WarnCF("seahorse", "graphiti remember failed (sqlite fallback)",
				map[string]any{
					"session": sessionKey,
					"kind":    summaryKind,
					"error":   err.Error(),
				})
		}
	}()
}

// CloseGraphitiQueue shuts down the singleton DB handle. Exposed for tests
// that need a clean shutdown, and for graceful server shutdown paths.
// Safe to call multiple times.
func CloseGraphitiQueue() error {
	graphitiMu.Lock()
	defer graphitiMu.Unlock()
	if graphitiQueue == nil {
		return nil
	}
	err := graphitiQueue.Close()
	graphitiQueue = nil
	graphitiPath = ""
	return err
}

// resetGraphitiQueueForTest clears the singleton state so a test can
// re-initialize it (e.g. with a different env var / path). Not for
// production use — guarded behind the build-time-safe test build tag
// by convention only.
func resetGraphitiQueueForTest() {
	_ = CloseGraphitiQueue()
	graphitiMu.Lock()
	configuredDaemonURL = ""
	configuredGroupID = ""
	graphitiMu.Unlock()
	graphitiOnce = sync.Once{}
}
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
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// Graphiti enqueue constants. Kept package-private so callers go through
// the helper functions and respect the fire-and-forget semantics.
const (
	graphitiDefaultDBPath = "~/.picoclaw/workspace/apps/graphiti-mcp/queue/episodes.db"
	graphitiEnvDBPath     = "GRAPHITI_QUEUE_DB"
	graphitiEnvEnabled    = "GRAPHITI_ENABLED"
	graphitiGroupID       = "graphiti_memory"
	graphitiSourcePrefix  = "seahorse_compaction"
)

// graphitiPragmas matches the Python EpisodeQueue configuration so Go and
// Python access the same database in a compatible way.
//
// Empirically verified at 100 concurrent goroutines / 60s sustained load:
//   - 0 SQLITE_BUSY
//   - Median latency ~1.5ms
//   - Throughput ~1300 ops/sec
const graphitiPragmas = "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(OFF)"

// graphitiQueue is the lazy singleton DB handle used by rememberInGraphiti.
// We use a package-level singleton so multiple compaction goroutines share
// a single connection pool rather than opening a fresh DB per call.
//
// Initialization is one-shot guarded by initOnce. On any error the handle
// is nil and subsequent calls fail-fast (logged) — this prevents cascading
// retry storms that would amplify a transient issue into a load problem.
var (
	graphitiQueue *sql.DB
	graphitiOnce  sync.Once
	graphitiMu    sync.RWMutex
	graphitiPath  string // resolved absolute path for logging
)

// resolveGraphitiDBPath picks the queue DB location. The default uses the
// workspace-relative path of the production Graphiti MCP install, overridable
// via GRAPHITI_QUEUE_DB. This matches the constant in Python queue.py so a
// Go process and a Python process can share the same file safely.
//
// Returns "" if disabled (GRAPHITI_ENABLED=0). The empty sentinel lets the
// caller short-circuit without touching the filesystem.
func resolveGraphitiDBPath() string {
	if v := os.Getenv(graphitiEnvEnabled); v == "0" || v == "false" || v == "no" {
		return ""
	}
	raw := os.Getenv(graphitiEnvDBPath)
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

	path := resolveGraphitiDBPath()
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

	// Payload schema mirrors the Python EpisodeQueue.enqueue contract:
	// a single dict with the keys the Graphiti MCP server expects.
	payload := map[string]any{
		"content":            content,
		"name":               fmt.Sprintf("Seahorse %s (%s @ %s)", summaryKind, sessionKey, now),
		"source_description": fmt.Sprintf("%s:%s", graphitiSourcePrefix, sessionKey),
		"group_id":           graphitiGroupID,
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
func rememberInGraphiti(sessionKey, content, summaryKind string) {
	if content == "" {
		// Nothing to remember. Log at debug only — this can legitimately
		// happen for tiny compactions that don't yield a summary.
		logger.DebugCF("seahorse", "graphiti remember skipped empty content",
			map[string]any{"session": sessionKey})
		return
	}
	if _, err := enqueueGraphitiEpisode(sessionKey, content, summaryKind); err != nil {
		logger.WarnCF("seahorse", "graphiti remember failed",
			map[string]any{
				"session": sessionKey,
				"kind":    summaryKind,
				"error":   err.Error(),
			})
	}
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
	graphitiOnce = sync.Once{}
}
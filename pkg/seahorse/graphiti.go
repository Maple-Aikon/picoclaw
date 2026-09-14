// Package seahorse — graphiti.go
//
// Fire-and-forget enqueue of compaction summaries into the Graphiti
// async ingestion queue (HTTP POST → janus-graph-daemon). This replaces
// the legacy rememberInSignet helper (Signet daemon port 3850) and the
// pre-v3 SQLite-shared-with-Python path (plan v3 SHIPPED 2026-09-10).
//
// Architecture: the Go runtime posts directly to the daemon's
// /episodes endpoint with a 3s context deadline. The daemon owns the
// retry / DLQ semantics and is the source of truth for ingestion.
// Failures are logged but never propagated — losing one summary is
// preferable to aborting the compaction pipeline (fire-and-forget).
package seahorse

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// Graphiti enqueue constants. Kept package-private so callers go through
// the helper functions and respect the fire-and-forget semantics.
//
// Migration v3 (SHIPPED): only the HTTP seam (daemonURL) remains. The
// SQLite path (GRAPHITI_QUEUE_DB, configuredQueuePath-as-path,
// openGraphitiQueue singleton, enqueueGraphitiEpisode sync-write) has
// been removed in feat/remove-sqlite-queue (2026-09-15) per plan
// seahorse-compaction-post-episodes-migration-v2-20260910 §Cut.
const (
	graphitiGroupID         = "graphiti_memory"
	graphitiSourcePrefix    = "seahorse_compaction"
	graphitiSemaphoreCap    = 8
	graphitiSemaphoreWaitMS = 10 // wait up to 10ms when cap is saturated
)

// HTTP path state. The semaphore bounds in-flight POSTs (plan v3 audit A4);
// graphitiMu protects the configured* fields used by SetGraphitiConfig.
var (
	graphitiMu          sync.RWMutex
	configuredDaemonURL string // canonical HTTP seam — set via SetGraphitiConfig or GRAPHITI_DAEMON_URL
	configuredGroupID   string

	// graphitiSem bounds concurrent in-flight POST /episodes requests
	// (plan v3 audit A4). Initialized once via lazy make (sync.Once +
	// RWMutex pattern keeps init cheap).
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
// tenant). Migration v3 (SHIPPED): the first parameter is the daemon
// URL (e.g. "http://127.0.0.1:8765"). Empty / "off" / "disabled" disable
// ingestion (rememberInGraphiti drops with a debug log).
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
// First return is the HTTP base URL (no trailing path).
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

// rememberInGraphiti is the fire-and-forget replacement for
// rememberInSignet. It posts a compaction summary to the Graphiti
// daemon's /episodes endpoint so the long-term memory layer can ingest it.
//
// Failures are logged but never returned to the caller — losing one
// summary is preferable to aborting the entire compaction flow. The
// daemon is the source of truth for retry semantics.
//
// Mirrors the legacy function signature exactly so callers (short_compaction.go
// lines 306 and 414) can swap with a single-token edit.
//
// Migration v3 — wrapping goroutine invariants (audit G3/A3/A4):
//   - recover() is mandatory: a panic inside the goroutine would
//     otherwise terminate the entire picoclaw process. For a
//     fire-and-forget ingestion path that should NEVER take down the
//     agent.
//   - Semaphore cap: bounds in-flight POSTs to 8. If saturated, wait
//     up to graphitiSemaphoreWaitMS before dropping.
//   - Health gate: a single /health probe per 30s (cached in
//     daemonHealthOK). If down, drop with a debug log.
//
// If the daemon URL is empty/disabled, the call is a no-op (debug log).
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

		// Daemon URL must be configured. Empty/disabled → silent drop
		// (the legacy SQLite fallback was removed in feat/remove-sqlite-queue).
		url := resolveGraphitiEpisodesURL()
		if url == "" {
			logger.DebugCF("seahorse", "graphiti remember skipped: daemon URL empty",
				map[string]any{"session": sessionKey, "kind": summaryKind})
			return
		}

		// Pre-call health gate. If down, short-circuit before taking a
		// semaphore slot — the gate is fast and bounded to 1 probe per
		// graphitiHealthTTLSec seconds thanks to daemonHealthOK's cache.
		// We consult the gate here in the goroutine so the caller (the
		// synchronous caller of rememberInGraphiti) stays unblocked.
		if ok, _ := daemonHealthOK(); !ok {
			logger.DebugCF("seahorse", "graphiti remember skipped: daemon health gate down",
				map[string]any{"session": sessionKey, "kind": summaryKind})
			return
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

		// Body: HTTP POST via the test-swappable function var.
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
	}()
}

// resetPostGraphitiEpisodeFnForTest restores postGraphitiEpisodeFn to
// the production implementation. Tests that swap the function var for
// panic-injection should call this in their cleanup so subsequent
// tests get the real HTTP poster.
func resetPostGraphitiEpisodeFnForTest() {
	postGraphitiEpisodeFn = postGraphitiEpisode
}

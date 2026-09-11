# Seahorse Compaction — Configuration Reference

This document covers the configuration seams for the Seahorse compaction
engine (`pkg/seahorse`) and its integration with the janus-graph-daemon
HTTP transport.

**Plan reference:** [`memory/plan/seahorse-compaction-post-episodes-migration-v2-20260910.md`](../../memory/plan/seahorse-compaction-post-episodes-migration-v2-20260910.md)

## Migration Status (2026-09-11)

- **v2 audit (8 findings)**: completed 2026-09-11. See plan v3 §"v3 delta from v2" for the full delta.
- **HTTP path canonical**: when `GRAPHITI_DAEMON_URL` (or YAML `graphiti_daemon_url`) is set, Seahorse POSTs episodes to `janus-graph-daemon` instead of writing to a local SQLite WAL.
- **SQLite path deprecated**: legacy `GRAPHITI_QUEUE_DB` is honored as a deprecation alias only — a one-time warn log fires at package init when detected.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GRAPHITI_DAEMON_URL` | `http://127.0.0.1:8765` | Base URL of `janus-graph-daemon`. Seahorse appends `/episodes` for ingestion and `/health` for the pre-call health gate. Set to `off`, `0`, `false`, `no`, or `disabled` to disable the HTTP path (fallback to SQLite only). |
| `GRAPHITI_QUEUE_DB` | `~/.picoclaw/workspace/apps/graphiti-mcp/queue/episodes.db` | Legacy SQLite WAL path. **Deprecated** — the HTTP path is the canonical ingestion route. Honored only when `GRAPHITI_DAEMON_URL` is unset/disabled. |
| `GRAPHITI_ENABLED` | `1` (enabled) | Master kill-switch for the entire Graphiti ingestion path. Set to `0`/`false`/`no` to disable both HTTP and SQLite paths. |
| `GRAPHITI_GROUP_ID` | `graphiti_memory` | Graphiti group_id (tenant) for all episodes enqueued from this process. |

## YAML Configuration (preferred over env vars)

The `config.yaml` schema exposes two fields under `agents.defaults`:

```yaml
agents:
  defaults:
    # Canonical seam (HTTP). New deployments should set this.
    graphiti_daemon_url: "http://127.0.0.1:8765"
    # Legacy seam (SQLite WAL). Honored only if graphiti_daemon_url is unset.
    graphiti_queue_path: "~/.picoclaw/workspace/apps/graphiti-mcp/queue/episodes.db"
```

Resolution order (highest priority first):
1. `agents.defaults.graphiti_daemon_url` (HTTP)
2. Top-level `graphiti_daemon_url` (HTTP)
3. `agents.defaults.graphiti_queue_path` (SQLite, legacy)
4. Top-level `graphiti_queue_path` (SQLite, legacy)
5. `GRAPHITI_DAEMON_URL` env var (HTTP)
6. `GRAPHITI_QUEUE_DB` env var (SQLite, legacy)
7. Built-in default (`http://127.0.0.1:8765`)

## janus-graph-daemon (downstream of Seahorse)

These env vars configure the daemon's behavior when handling
`POST /episodes` requests from Seahorse:

| Variable | Default | Description |
|----------|---------|-------------|
| `JANUS_DAEMON__LOCK__MODE` | `mcp_only` | Lock mode for the daemon's queue. **Critical**: Seahorse's HTTP path requires `daemon_only` or `dual_with_lock` — `mcp_only` returns 409 `LOCK_MODE_MCP_ONLY` and silently drops every ingestion attempt. See Step 0 of the plan for the functional probe. |
| `JANUS_PIPELINE__MAX_ATTEMPTS` | `3` | Maximum retry attempts for an episode before it lands in the DLQ. Seahorse's HTTP path is fire-and-forget; the daemon owns retry/DLQ semantics. |
| `JANUS_GRAPHITI__GROUP_ID` | `graphiti_memory` | Daemon-side group_id (must match Seahorse's `GRAPHITI_GROUP_ID` for episodes to land in the same tenant). |

## Operational Checklist

When deploying Seahorse with the HTTP path:

- [ ] `GRAPHITI_DAEMON_URL` set in the picoclaw process environment (or YAML `graphiti_daemon_url`).
- [ ] `JANUS_DAEMON__LOCK__MODE` set to `daemon_only` or `dual_with_lock` (not `mcp_only`).
- [ ] Functional probe (Step 0): `curl -s -X POST http://127.0.0.1:8765/episodes -H 'Content-Type: application/json' -d '{"content":"smoke","name":"smoke"}'` returns `202` (not `409 LOCK_MODE_MCP_ONLY`).
- [ ] `pmc restart janus-graph-daemon` to apply lock-mode change.
- [ ] Watch daemon log for `Background saving terminated with success` lines to confirm FalkorDB writes are landing.
- [ ] After 24h, check `queue_stats` via `mcp_janus-graph_graphiti_health` — `done` should grow, `dlq` should stay near zero.

## Deprecation Timeline

- **2026-09-11**: SQLite path marked deprecated, warn log fires on first enqueue + at package init when `GRAPHITI_QUEUE_DB` is set without `GRAPHITI_DAEMON_URL`.
- **Q4 2026 (six-month window)**: SQLite path will be removed. Production deployments must migrate to the HTTP path before then.

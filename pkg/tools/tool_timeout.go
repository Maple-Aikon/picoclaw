// Package tools — native tool timeout helpers (Phase 3, native-tool-call-timeout-force-kill-20260702).
//
// Goal: every native Go tool call runs through a deadline-bounded context so that
// a hung FUSE/NFS read, kernel busy-loop, or hardware bus block cannot freeze the
// agent loop indefinitely. Go cannot force-kill a goroutine; the underlying
// operation may keep running after the LLM loop has moved on, but the user no
// longer has to `kill -9` picoclaw to recover.
//
// Precedence (Q1 final decisions, 2026-07-02):
//   1. Per-tool override via typed struct field (ToolsConfig.<tool>.TimeoutSeconds)
//   2. turnCtx deadline (caller-set, e.g. agent loop)
//   3. ToolsConfig.TimeoutSeconds root default (120s)
//   4. Fallback 120s when cfg is nil (khi config chưa load)
//
// hasTimeout=false chỉ khi root timeout_seconds == 0 → feature off (Q4).
package tools

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// DefaultToolTimeoutSeconds is the fallback applied when no config is loaded.
// Kept as a constant — the canonical default lives in code so non-bootstrapped
// paths (tests, callers without config injection) still get a sane deadline.
const DefaultToolTimeoutSeconds = 120

// resolveToolTimeout determines the effective timeout for a tool call.
//
// Order:
//  1. Root disable guard — ToolsConfig.TimeoutSeconds == 0 returns (0, false) so
//     the caller (registry) skips WithTimeout entirely (Q4 rollback).
//  2. Per-tool override (ToolsConfig.<tool>.TimeoutSeconds — typed switch).
//  3. turnCtx deadline if caller already set one (e.g. agent loop's outer timer).
//  4. Root default ToolsConfig.TimeoutSeconds (non-zero — already checked in step 1).
//  5. Hard-coded DefaultToolTimeoutSeconds when cfg is nil.
func resolveToolTimeout(ctx context.Context, name string, cfg *config.ToolsConfig) (time.Duration, bool) {
	// Step 0/4: feature disabled if config says so.
	if cfg != nil && cfg.TimeoutSeconds == 0 {
		return 0, false
	}

	// Step 1: per-tool override.
	if cfg != nil {
		if override := lookupToolTimeout(cfg, name); override > 0 {
			return time.Duration(override) * time.Second, true
		}
	}

	// Step 2: caller already set a deadline.
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline), true
	}

	// Step 3/5: root config or hard-coded fallback.
	if cfg != nil {
		return time.Duration(cfg.TimeoutSeconds) * time.Second, true
	}
	return DefaultToolTimeoutSeconds * time.Second, true
}

// lookupToolTimeout returns the per-tool override in seconds, or 0 if unset.
//
// Adding a new tool requires (a) a `TimeoutSeconds int` field on the relevant
// ToolsConfig struct and (b) one entry in this switch. The LLM-readable JSON
// key is the same lowercase name used here (e.g. `append_file`).
//
// Typed switch chosen over map[string]ToolTimeoutConfig because PicoClaw already
// uses typed fields per tool — keeps static type checking at boot and avoids
// `json.Unmarshal` silently dropping typos.
func lookupToolTimeout(cfg *config.ToolsConfig, name string) int {
	switch name {
	case "append_file":
		return cfg.AppendFile.TimeoutSeconds
	case "edit_file":
		return cfg.EditFile.TimeoutSeconds
	case "find_skills":
		return cfg.FindSkills.TimeoutSeconds
	case "i2c":
		return cfg.I2C.TimeoutSeconds
	case "install_skill":
		return cfg.InstallSkill.TimeoutSeconds
	case "list_dir":
		return cfg.ListDir.TimeoutSeconds
	case "load_image":
		return cfg.LoadImage.TimeoutSeconds
	case "read_file":
		return cfg.ReadFile.TimeoutSeconds
	case "serial":
		return cfg.Serial.TimeoutSeconds
	case "send_file":
		return cfg.SendFile.TimeoutSeconds
	case "send_tts":
		return cfg.SendTTS.TimeoutSeconds
	case "spawn":
		return cfg.Spawn.TimeoutSeconds
	case "spawn_status":
		return cfg.SpawnStatus.TimeoutSeconds
	case "spi":
		return cfg.SPI.TimeoutSeconds
	case "extend_turn_iteration":
		return cfg.ExtendTurnIteration.TimeoutSeconds
	case "message":
		return cfg.Message.TimeoutSeconds // promoted from embedded ToolConfig
	case "web", "web_search":
		return cfg.Web.TimeoutSeconds // promoted from embedded ToolConfig
	default:
		return 0
	}
}

// TimedOutKind names the failure mode surfaced by the timeout path. Stored as
// a metric label (Q3) and used in log warnings so operators can distinguish a
// true deadline failure from a parent cancellation race.
type TimedOutKind string

const (
	TimedOutDeadlineExceeded TimedOutKind = "deadline_exceeded"
	TimedOutParentCancelled TimedOutKind = "parent_cancelled"
)

// ToolTimeoutStats tracks timeout counts per tool+kind. Storage is a sync.Map
// of *atomic.Int64 keyed by "<tool>|<kind>" so reads during shutdown don't
// block writers. Cheap, lock-free, observable via /debug/tool-stats.
type ToolTimeoutStats struct {
	counters sync.Map // map[string]*atomic.Int64
}

func newToolTimeoutStats() *ToolTimeoutStats {
	return &ToolTimeoutStats{}
}

func (s *ToolTimeoutStats) timeoutKey(tool string, kind TimedOutKind) string {
	return tool + "|" + string(kind)
}

// RecordTimeout increments the counter for (tool, kind).
func (s *ToolTimeoutStats) RecordTimeout(tool string, kind TimedOutKind) {
	key := s.timeoutKey(tool, kind)
	if v, ok := s.counters.Load(key); ok {
		v.(*atomic.Int64).Add(1)
		return
	}
	counter := new(atomic.Int64)
	counter.Add(1)
	// LoadOrStore discards our counter only if another writer raced us; that
	// counter still has 1 from its own Inc so we don't double-count.
	actual, _ := s.counters.LoadOrStore(key, counter)
	if actual.(*atomic.Int64) != counter {
		actual.(*atomic.Int64).Add(1)
	}
}

// Count returns the current timeout count for (tool, kind), or 0.
func (s *ToolTimeoutStats) Count(tool string, kind TimedOutKind) int64 {
	key := s.timeoutKey(tool, kind)
	if v, ok := s.counters.Load(key); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// Snapshot returns all current (tool, kind, count) tuples for /debug/tool-stats.
func (s *ToolTimeoutStats) Snapshot() []TimeoutStat {
	out := make([]TimeoutStat, 0, 16)
	s.counters.Range(func(k, v any) bool {
		keyStr, _ := k.(string)
		count := v.(*atomic.Int64).Load()
		var tool, kind string
		for i := 0; i < len(keyStr); i++ {
			if keyStr[i] == '|' {
				tool = keyStr[:i]
				kind = keyStr[i+1:]
				break
			}
		}
		out = append(out, TimeoutStat{Tool: tool, Kind: kind, Count: count})
		return true
	})
	return out
}

// TimeoutStat is one row in the timeout stats snapshot.
type TimeoutStat struct {
	Tool  string `json:"tool"`
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

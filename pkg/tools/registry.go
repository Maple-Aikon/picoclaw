package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type ToolEntry struct {
	Tool   Tool
	IsCore bool
	TTL    int
}

type ToolRegistry struct {
	tools          map[string]*ToolEntry
	sigTrackers    map[string]*SignatureFailureTracker // key: composite (channel:chatID); see getOrCreateSigTracker
	mu             sync.RWMutex
	sigTrackerMu   sync.Mutex // serializes lazy allocation of per-session SignatureFailureTrackers
	version        atomic.Uint64 // incremented on Register/RegisterHidden for cache invalidation
	mediaStore      media.MediaStore
	allowlist       map[string]struct{}
	phase           string // active goal phase for per-phase allowlist semantics (Phase 12.5); "" = no phase info
	projectionFrozen bool  // when true, ToProviderDefs projects all tools to keep prompt cache prefix 100% frozen
	cfg             *config.ToolsConfig // optional; nil → fallback DefaultToolTimeoutSeconds
	timeoutStats    *ToolTimeoutStats   // Q3 metric; nil-safe via lazy init
	knowledgeStore  *ToolKnowledgeStore // optional persistent "lessons learned" per tool; nil = feature off

	// seenFirstSuccess tracks (channel:chatID:tool:errKind) keys for which we
	// have already appended the "consider saving" soft prompt at the execution
	// site. Per-registry because the prompt fires within a single turn and is
	// reset by ResetSignatureFailures at the turn boundary (Phase 3).
	seenFirstSuccessMu sync.Mutex
	seenFirstSuccess   map[string]struct{}
}

// SetToolKnowledgeStore attaches (or clears, with nil) the persistent
// "lessons learned" store. When nil, the tool_knowledge tool Execute
// returns an explanatory ErrDependencyDown rather than silently failing.
//
// Plan: tool-knowledge-experiential-memory-for-tool-failures-3-phases-20260718
func (r *ToolRegistry) SetToolKnowledgeStore(s *ToolKnowledgeStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.knowledgeStore = s
}

// ToolKnowledgeStore returns the configured store, or nil when unconfigured.
func (r *ToolRegistry) ToolKnowledgeStore() *ToolKnowledgeStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.knowledgeStore
}

// toolKnowledgeFor loads the lesson body for the given tool if a store is
// configured. Returns "" when unconfigured or no knowledge exists. Used by
// the signature-tracker escalation wire (Phase 2) so the registry does not
// need to inline the nil-check at every callsite.
//
// Safe for concurrent use — delegates to ToolKnowledgeStore.LoadForEscalation
// which holds the per-tool mutex internally.
func (r *ToolRegistry) toolKnowledgeFor(tool string) string {
	ks := r.ToolKnowledgeStore()
	if ks == nil {
		return ""
	}
	return ks.LoadForEscalation(tool)
}

type mediaStoreAware interface {
	SetMediaStore(store media.MediaStore)
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:            make(map[string]*ToolEntry),
		sigTrackers:      make(map[string]*SignatureFailureTracker),
		timeoutStats:     newToolTimeoutStats(),
		seenFirstSuccess: make(map[string]struct{}),
	}
}

// SetProjectionFrozen controls whether ToProviderDefs() emits all registered tools
// regardless of active goal phase (for KV cache prefix stability).
func (r *ToolRegistry) SetProjectionFrozen(frozen bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projectionFrozen = frozen
}

// ProjectionFrozen returns whether frozen tool schema projection is active.
func (r *ToolRegistry) ProjectionFrozen() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.projectionFrozen
}

// SetToolsConfig attaches the loaded ToolsConfig so that resolveToolTimeout can
// honour per-tool and root TimeoutSeconds. Safe to call nil; cleared by passing nil.
func (r *ToolRegistry) SetToolsConfig(cfg *config.ToolsConfig) {
	r.cfg = cfg
}

// TimeoutStats returns the metric collector (Q3). Always non-nil after
// NewToolRegistry; nil-safe if SetToolsConfig was used to swap registries.
func (r *ToolRegistry) TimeoutStats() *ToolTimeoutStats {
	if r.timeoutStats == nil {
		r.timeoutStats = newToolTimeoutStats()
	}
	return r.timeoutStats
}

// SetAllowlist restricts registrations to the provided runtime tool names.
// A nil slice means "allow all". An empty-but-non-nil slice means "allow none".
// Phase 12.5: also clears any previously-set active goal phase (call SetPhase
// separately to re-enable per-phase discovery exemption behavior).
func (r *ToolRegistry) SetAllowlist(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if names == nil {
		r.allowlist = nil
		r.phase = ""
		return
	}

	allowlist := make(map[string]struct{}, len(names))
	for _, name := range names {
		trimmed := strings.ToLower(strings.TrimSpace(name))
		if trimmed == "" {
			continue
		}
		allowlist[trimmed] = struct{}{}
	}
	r.allowlist = allowlist
	r.phase = ""
}

// SetPhase records the active goal phase ("set" / "open" / "checkpoint" /
// "final" / "") so per-phase allowlist rules can take effect inside
// toolAllowedLocked. Pass "" to clear. Phase 12.5: in "set" or "final"
// phases the unconditional discovery-tool exemption is suppressed, so
// tool_search_tool_bm25 / tool_search_tool_regex must appear in the
// allowlist to be visible. Other phases ("open", "checkpoint", "") keep
// the legacy behavior of letting discovery tools bypass.
func (r *ToolRegistry) SetPhase(phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phase = strings.ToLower(strings.TrimSpace(phase))
}

func (r *ToolRegistry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerLocked(tool, true)
}

// GetAllowlist returns a sorted copy of the current runtime allowlist.
// A nil slice means "no allowlist filter active (all registered tools
// pass through)". An empty-but-non-nil slice means "allow none".
// Callers must not mutate the returned slice.
func (r *ToolRegistry) GetAllowlist() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.allowlist == nil {
		return nil
	}
	out := make([]string, 0, len(r.allowlist))
	for name := range r.allowlist {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// RegisterHidden saves hidden tools (visible only via TTL)
func (r *ToolRegistry) RegisterHidden(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerLocked(tool, false)
}

// registerLocked adds a tool under the registry's lock. The caller must hold
// r.mu (write). isCore distinguishes core tools (always available) from hidden
// tools (only reachable through TTL lookup).
func (r *ToolRegistry) registerLocked(tool Tool, isCore bool) {
	kind := "hidden"
	logPrefix := "Hidden"
	if isCore {
		kind = "core"
		logPrefix = "core"
	}
	name := tool.Name()
	if !r.toolAllowedLocked(name) {
		logger.DebugCF(
			"tools",
			"Skipped "+kind+" tool registration by agent allowlist",
			map[string]any{"name": name},
		)
		return
	}
	if _, exists := r.tools[name]; exists {
		logger.WarnCF(
			"tools",
			logPrefix+" tool registration overwrites existing tool",
			map[string]any{"name": name},
		)
	}
	r.tools[name] = &ToolEntry{
		Tool:   tool,
		IsCore: isCore,
		TTL:    0, // Core tools do not use TTL
	}
	if aware, ok := tool.(mediaStoreAware); ok && r.mediaStore != nil {
		aware.SetMediaStore(r.mediaStore)
	}
	r.version.Add(1)
	logger.DebugCF("tools", "Registered "+kind+" tool", map[string]any{"name": name})
}

// SetMediaStore injects a MediaStore into all registered tools that can
// consume it, and remembers it for future registrations.
func (r *ToolRegistry) SetMediaStore(store media.MediaStore) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mediaStore = store
	for _, entry := range r.tools {
		if aware, ok := entry.Tool.(mediaStoreAware); ok {
			aware.SetMediaStore(store)
		}
	}
}

// PromoteTools atomically sets the TTL for multiple non-core tools.
// This prevents a concurrent TickTTL from decrementing between promotions.
func (r *ToolRegistry) PromoteTools(names []string, ttl int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	promoted := 0
	for _, name := range names {
		if entry, exists := r.tools[name]; exists {
			if !entry.IsCore {
				entry.TTL = ttl
				promoted++
			}
		}
	}
	logger.DebugCF(
		"tools",
		"PromoteTools completed",
		map[string]any{"requested": len(names), "promoted": promoted, "ttl": ttl},
	)
}

// TickTTL decreases TTL only for non-core tools
func (r *ToolRegistry) TickTTL() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.tools {
		if !entry.IsCore && entry.TTL > 0 {
			entry.TTL--
		}
	}
}

// Version returns the current registry version (atomically).
func (r *ToolRegistry) Version() uint64 {
	return r.version.Load()
}

// IsAllowed reports whether a tool name passes the runtime allowlist check.
// Returns true when no allowlist is active (allowlist == nil) or when the
// name is in the allowlist or is a discovery tool (exempt). Safe for callers
// that do not already hold r.mu — takes the read lock internally.
func (r *ToolRegistry) IsAllowed(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.toolAllowedLocked(name)
}

// isLifecycleToolAllowed — thin shim to IsLifecycleToolAllowed (Plan §3.3
// site 2 rewrite, T4). Kept for in-package callers that do not want to
// re-route through the public helper. New code should call
// IsLifecycleToolAllowed directly.
func isLifecycleToolAllowed(toolName, phase string) bool {
	return IsLifecycleToolAllowed(toolName, phase)
}

func (r *ToolRegistry) toolAllowedLocked(name string) bool {
	// Phase 12.47 (E2): POST-FINAL has NO tools at all — the LLM only emits
	// the final user-facing report text. Early-return at the VERY TOP (before
	// both the lifecycle gate and the allowlist==nil check) so every tool —
	// non-lifecycle, lifecycle, discovery — is blocked even when
	// SetAllowlist(nil) (allow-all) was called. Replaces the
	// discovery-suppression list edit + lifecycle-gate case edit (both
	// redundant once this fires first).
	if r.phase == "post_final" {
		return false
	}
	// Phase 12.31: lifecycle-tool phase gate. Runs FIRST so the gate fires
	// regardless of allowlist state. The allowlist filter alone is insufficient
	// for the no-`tools:`-field case where SetAllowlist(nil) makes the allowlist
	// empty, leaving all 85 tools visible at OPEN phase — including set_goal and
	// goal_progress which should be restricted by phase.
	if !isLifecycleToolAllowed(name, r.phase) {
		return false
	}
	if r.allowlist == nil {
		return true
	}
	if isToolDiscoveryToolName(name) {
		// Discovery tools are part of the MCP control plane: they must remain
		// available whenever configured so deferred MCP tools can still be
		// unlocked. Per-agent allowlists still apply to the hidden MCP tools
		// themselves during RegisterHidden.
		//
		// Phase 12.5 exception: at strict lifecycle phases (GoalPhaseSet /
		// GoalPhaseFinal) the allowlist is intentionally reduced to a single
		// tool (set_goal or complete_goal) so the LLM commits to a single
		// forward path. Letting the LLM BM25-search for hidden MCP tools at
		// GoalPhaseSet defeats the gate entirely — the user reported on
		// 2026-07-23 16:18 ICT that iter 1 returned [set_goal,
		// tool_search_tool_bm25] instead of just [set_goal]. Suppress the
		// discovery exemption in those phases so the LLM sees exactly the
		// tools in the allowlist and nothing else.
		//
		// Phase 12.48 plan §3.3 site 3 rewrite: discovery suppression now
		// data-driven via ToolPolicyForPhase.DiscoveryVisible. Hardcoded
		// phase list `{set, final, checkpoint}` → policy row (single source
		// of truth). Fail-CLOSED for unknown non-empty phase (R6-F1, L2):
		// null policy means we cannot prove the exemption is safe — block
		// the discovery tool rather than leak it. The post_final hard guard
		// at the top of toolAllowedLocked remains (R4-F2 HIGH, 2 reviewer) —
		// this rewrite changes the suppression list edit, not the hard guard.
		//
		// Empty phase ("") is an explicit backward-compat path: agents that
		// only ever call SetAllowlist (never SetPhase, e.g., instance.go:113)
		// keep discovery visible. SetPhase→"" via SetAllowlist clears phase
		// (TestToolRegistry_SetAllowlistClearsPhase) — that path expects the
		// legacy "discovery exempt at all phases" behavior.
		p := ToolPolicyForPhase(r.phase)
		if p == nil && r.phase != "" {
			return false // fail-CLOSED (R6-F1) — non-empty unknown phase
		}
		if p == nil {
			// phase == "" → backward-compat path. Pre-Phase 12.5 registry
			// did not know about phase at all; discovery always exempt.
			return true
		}
		if p.DiscoveryVisible {
			return true
		}
		// DiscoveryVisible=false → fall through to allowlist membership
		// check below (BM25/Regex either in the allowlist or not).
	}
	_, ok := r.allowlist[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// HasRegistered reports whether a tool name is present in the registry,
// including hidden tools whose TTL is currently zero.
func (r *ToolRegistry) HasRegistered(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

// HiddenToolSnapshot holds a consistent snapshot of hidden tools and the
// registry version at which it was taken. Used by BM25SearchTool cache.
type HiddenToolSnapshot struct {
	Docs    []HiddenToolDoc
	Version uint64
}

// HiddenToolDoc is a lightweight representation of a hidden tool for search indexing.
type HiddenToolDoc struct {
	Name        string
	Description string
}

// SnapshotHiddenTools returns all non-core tools and the current registry
// version under a single read-lock, guaranteeing consistency between the
// two values.
func (r *ToolRegistry) SnapshotHiddenTools() HiddenToolSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	docs := make([]HiddenToolDoc, 0, len(r.tools))
	for name, entry := range r.tools {
		if !entry.IsCore {
			docs = append(docs, HiddenToolDoc{
				Name:        name,
				Description: entry.Tool.Description(),
			})
		}
	}
	return HiddenToolSnapshot{
		Docs:    docs,
		Version: r.version.Load(),
	}
}

func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	// Hidden tools with expired TTL are not callable.
	if !entry.IsCore && entry.TTL <= 0 {
		return nil, false
	}
	return entry.Tool, true
}

func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]any) *ToolResult {
	return r.ExecuteWithContext(ctx, name, args, "", "", nil)
}

// getOrCreateSigTracker returns the SignatureFailureTracker scoped to the
// (channel, chatID) session, allocating a fresh tracker on first use.
// Counter scope is per-session; Reset() is called at turn boundaries by the
// caller (see pkg/agent/turn_coord.go runTurn start-of-turn path).
//
// The sigTrackerMu lock protects the sigTrackers map; the returned tracker
// has its own internal mutex for concurrent EscalateIfNeeded / MarkSuccess /
// Reset calls (which are exercised from tool dispatch goroutines).
func (r *ToolRegistry) getOrCreateSigTracker(channel, chatID string) *SignatureFailureTracker {
	key := channel + ":" + chatID

	r.sigTrackerMu.Lock()
	if tr, ok := r.sigTrackers[key]; ok {
		r.sigTrackerMu.Unlock()
		return tr
	}
	tr := NewSignatureFailureTracker(0) // 0 → defaultSigThreshold
	r.sigTrackers[key] = tr
	r.sigTrackerMu.Unlock()
	return tr
}

// ResetSignatureFailures clears all failure counters in the per-session
// SignatureFailureTracker and the per-(session,tool) "first success seen"
// flags surfaced by Phase 3 soft prompts. Called at turn boundaries so a
// new turn starts with a fresh slate. No-op if no tracker exists yet for
// the session.
//
// Plan: tool-knowledge-experiential-memory-for-tool-failures-3-phases-20260718
func (r *ToolRegistry) ResetSignatureFailures(channel, chatID string) {
	key := channel + ":" + chatID

	r.sigTrackerMu.Lock()
	tr, ok := r.sigTrackers[key]
	r.sigTrackerMu.Unlock()
	if ok {
		tr.Reset()
	}

	// Clear the seen-first-success flags for this session only. We keep
	// the map small by filtering on the session prefix; entries for other
	// sessions stay intact (registry is long-lived across turns).
	sessionPrefix := channel + "|" + chatID + "|"
	r.seenFirstSuccessMu.Lock()
	for k := range r.seenFirstSuccess {
		if strings.HasPrefix(k, sessionPrefix) {
			delete(r.seenFirstSuccess, k)
		}
	}
	r.seenFirstSuccessMu.Unlock()
}

// seenFirstSuccessKey builds the canonical key used by the Phase 3 dedup map.
// Format: "<channel>|<chatID>|<tool>" — no normalization beyond pipe-safety
// because channel/chatID come from internal callers (not the LLM) and tool
// names are sanitized by the knowledge store path.
func seenFirstSuccessKey(channel, chatID, tool string) string {
	return channel + "|" + chatID + "|" + tool
}

// markFirstSuccess records that we have already appended SoftPromptFirstSuccess
// for this (session, tool) in the current turn. Subsequent successes on the
// same key are deduped until ResetSignatureFailures clears the entry.
func (r *ToolRegistry) markFirstSuccess(channel, chatID, tool string) {
	if channel == "" && chatID == "" {
		return // anon namespace — soft prompt is meaningless without a session
	}
	r.seenFirstSuccessMu.Lock()
	r.seenFirstSuccess[seenFirstSuccessKey(channel, chatID, tool)] = struct{}{}
	r.seenFirstSuccessMu.Unlock()
}

// seenFirstSuccessBefore returns true iff markFirstSuccess has been called
// for this (session, tool) since the last ResetSignatureFailures. The call
// is the gating check that prevents SoftPromptFirstSuccess from spamming
// the prompt on every successful call within the same turn.
func (r *ToolRegistry) seenFirstSuccessBefore(channel, chatID, tool string) bool {
	if channel == "" && chatID == "" {
		return false // anon namespace — always eligible to receive the prompt
	}
	r.seenFirstSuccessMu.Lock()
	_, ok := r.seenFirstSuccess[seenFirstSuccessKey(channel, chatID, tool)]
	r.seenFirstSuccessMu.Unlock()
	return ok
}

// sigTrackerFor returns the SignatureFailureTracker scoped to (channel,
// chatID) or nil if none has been created yet. Use this when you need
// read/reset access without forcing lazy allocation — e.g. the success
// path that only wants to clear stale counters when a tracker already
// exists (avoiding the alloc + map insert for tools that never failed).
func (r *ToolRegistry) sigTrackerFor(channel, chatID string) *SignatureFailureTracker {
	key := channel + ":" + chatID
	r.sigTrackerMu.Lock()
	tr, ok := r.sigTrackers[key]
	r.sigTrackerMu.Unlock()
	if !ok {
		return nil
	}
	return tr
}


func (r *ToolRegistry) ExecuteWithContext(
	ctx context.Context,
	name string,
	args map[string]any,
	channel, chatID string,
	asyncCallback AsyncCallback,
) *ToolResult {
	// Phase 12.3 fix: enforce the runtime allowlist at the EXECUTION gate.
	// Phase 12.2 fixed the projection gate (ToProviderDefs honours
	// toolAllowedLocked) but execution was still a gap — LLM could call
	// tools outside the 4-phase goal allowlist (set/open/checkpoint/final)
	// if it knew their names from prior turns (signet-memory recall).
	// This check happens BEFORE r.Get() so disallowed tools cannot execute
	// even if they remain registered. Discovery tools are exempt via
	// toolAllowedLocked (MCP control plane). See plan
	// ~/.picoclaw/workspace/memory/plan/picoclaw-phase12.3-execution-gate-allowlist-prompt-20260723.md
	if !r.IsAllowed(name) {
		allowed := r.GetAllowlist()
		logger.WarnCF("tool", "Tool execution blocked by runtime allowlist",
			map[string]any{
				"tool":    name,
				"allowed": allowed,
				"reason":  "toolAllowedLocked returned false",
			})
		return ErrorResult(
			fmt.Sprintf("tool %q is temporarily unavailable (allowed tools: %v)", name, allowed),
		).WithError(fmt.Errorf("tool %q not in runtime allowlist", name))
	}

	logger.InfoCF("tool", "Tool execution started",
		map[string]any{
			"tool": name,
			"args": args,
		})

	tool, ok := r.Get(name)
	if !ok {
		logger.ErrorCF("tool", "Tool not found",
			map[string]any{
				"tool": name,
			})
		return ErrorResult(
			fmt.Sprintf("tool %q not found", name),
		).WithError(fmt.Errorf("tool not found"))
	}


	// Validate arguments against the tool's declared schema.
	if err := validateToolArgs(tool.Parameters(), args); err != nil {
		logger.WarnCF("tool", "Tool argument validation failed",
			map[string]any{"tool": name, "error": err.Error()})

		// Record validation error against circuit breaker. Per
		// circuit-breaker-3-tier-errkind-semantics-toolfeedback-20260717
		// Tier 3 semantics, ErrInvalidInput NEVER trips the breaker — a bad
		// argument is the LLM's mistake, not a tool fault. RecordResult
		// returns StatusValidationError with the validation hint; we
		// surface it in ForLLM but DO NOT emit a breaker event (the
		// JustTripped flag is guaranteed false for this tier, so the
		// event guard is belt-and-braces).
		res := ErrorResult(fmt.Sprintf("invalid arguments for tool %q: %s", name, err)).
			WithErrorKind(ErrInvalidInput).
			WithError(fmt.Errorf("argument validation failed: %w", err))

		if res.ErrKind == ErrInvalidInput || res.ErrKind == ErrTransient || res.ErrKind == ErrTimeout {
			if tracker := r.getOrCreateSigTracker(channel, chatID); tracker != nil {
				if hint := tracker.EscalateIfNeeded(SignatureKey{
					Tool:    name,
					ErrKind: res.ErrKind,
					ArgSig:  "",
				}, res.ForLLM, r.toolKnowledgeFor(name)); hint != "" {
					res = appendEscalatorHint(res, hint)
				}
			}
		}
		return res
	}

	// Inject channel/chatID into ctx so tools read them via ToolChannel(ctx)/ToolChatID(ctx).
	// Always inject — tools validate what they require.
	ctx = WithToolContext(ctx, channel, chatID)

	// Inject per-tool timeout (Phase 1 + Phase 3, native-tool-call-timeout-force-kill-20260702).
	// Precedence: per-tool override → caller's ctx deadline → root config default → 120s fallback.
	// hasTimeout=false means Q4 rollback (`tools.timeout_seconds: 0`).
	timeout, hasTimeout := resolveToolTimeout(ctx, name, r.cfg)
	var cancel context.CancelFunc
	if hasTimeout {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// If tool implements AsyncExecutor and callback is provided, use ExecuteAsync.
	// The callback is a call parameter, not mutable state on the tool instance.
	var result *ToolResult
	start := time.Now()

	// Run tool execution in a separate goroutine so a hung FUSE/NFS read or
	// busy-looped syscalls cannot block the agent loop forever. Go cannot
	// force-kill a goroutine — if `tool.Execute` ignores context cancellation
	// (e.g. it called a C library), the goroutine leaks until the underlying
	// syscall eventually returns. Accepted MVP trade-off (Q2): the LLM loop is
	// unblocked within the configured deadline regardless.
	done := make(chan *ToolResult, 1)
	go func() {
		defer func() {
			if re := recover(); re != nil {
				logger.RecoverPanicNoExit(re)
				errMsg := fmt.Sprintf("Tool '%s' crashed with panic: %v", name, re)
				logger.ErrorCF("tool", "Tool execution panic recovered",
					map[string]any{
						"tool":  name,
						"panic": fmt.Sprintf("%v", re),
					})
				done <- &ToolResult{
					ForLLM:  errMsg,
					ForUser: errMsg,
					IsError: true,
					ErrKind: ErrTransient,
					Err:     fmt.Errorf("panic: %v", re),
				}
			}
		}()

		var execResult *ToolResult
		if asyncExec, ok := tool.(AsyncExecutor); ok && asyncCallback != nil {
			logger.DebugCF("tool", "Executing async tool via ExecuteAsync",
				map[string]any{
					"tool": name,
				})
			execResult = asyncExec.ExecuteAsync(ctx, args, asyncCallback)
		} else {
			execResult = tool.Execute(ctx, args)
		}
		done <- execResult
	}()

	if hasTimeout {
		select {
		case result = <-done:
			// Normal completion (or panic recovered).
		case <-ctx.Done():
			// Timeout or parent cancellation fired before tool returned.
			timedOutKind := TimedOutParentCancelled
			errMsg := fmt.Sprintf("Tool '%s' was cancelled before it could complete (%v). The underlying operation may still be running but the agent loop has moved on.", name, ctx.Err())
			deadlineExceeded := errors.Is(ctx.Err(), context.DeadlineExceeded)
			if deadlineExceeded {
				timedOutKind = TimedOutDeadlineExceeded
				errMsg = fmt.Sprintf("Tool '%s' exceeded timeout (%v) and was cancelled. The underlying operation may still be running but the agent loop has moved on.", name, timeout)
			}
			logger.WarnCF("tool", "Tool execution timeout (orphan goroutine)",
				map[string]any{
					"tool":              name,
					"timeout_seconds":   timeout.Seconds(),
					"deadline_exceeded": deadlineExceeded,
					"parent_cancelled":  !deadlineExceeded && ctx.Err() != nil,
				})
			// Q3: increment in-memory counter before the result is built so even
			// the timeout-failure path yields the metric.
			r.TimeoutStats().RecordTimeout(name, timedOutKind)
			result = &ToolResult{
				ForLLM:  errMsg,
				ForUser: fmt.Sprintf("Tool '%s' timed out", name),
				IsError: true,
				ErrKind: ErrTimeout,
				Err:     ctx.Err(),
			}
		}
	} else {
		// Q4 rollback: feature off — wait indefinitely for the goroutine.
		result = <-done
	}

	// Handle nil result (should not happen, but defensive)
	if result == nil {
		result = &ToolResult{
			ForLLM:  fmt.Sprintf("Tool '%s' returned nil result unexpectedly", name),
			ForUser: fmt.Sprintf("Tool '%s' returned nil result unexpectedly", name),
			IsError: true,
			ErrKind: ErrTransient,
			Err:     fmt.Errorf("nil result from tool"),
		}
	}

	// Phase 12.55 (T11): tool-error feedback no longer flows through the
	// circuit breaker. Status is computed directly from the typed ErrKind:
	//   statusOK         → !result.IsError
	//   statusTransient  → ErrTransient / ErrTimeout (intentionally NOT
	//                      ErrDependencyDown — upstream errors are handled
	//                      by the agent's same-iter recovery layer).
	statusOK := !result.IsError
	statusTransient := result.IsError && (result.ErrKind == ErrTransient || result.ErrKind == ErrTimeout ||
		// W1 fix (2026-08-07): untyped errors (legacy executors, MCP
		// tool-level errors without ErrKind) fall back to the shared
		// transient classifier so the escalator / soft prompts still fire
		// for e.g. SearXNG 403 — mirrors checkToolExecErrorRecovery's
		// empty-kind fallback in the agent layer.
		(result.ErrKind == "" && IsTransientErrorText(result.ForLLM)))

	// SignatureFailureTracker escalation — same gate as the old
	// StatusTransient path. The hint is appended directly to ForLLM after
	// the soft prompts (writer-without-reader lesson, Phase 10.1: the hint
	// must not die with the breaker's fb.Message channel).
	var escalatorHint string
	if statusTransient {
		if tracker := r.getOrCreateSigTracker(channel, chatID); tracker != nil {
			if hint := tracker.EscalateIfNeeded(SignatureKey{
				Tool:    name,
				ErrKind: result.ErrKind,
				ArgSig:  "",
			}, result.ForLLM, r.toolKnowledgeFor(name)); hint != "" {
				escalatorHint = hint
			}
		}
	}

	// Normalize FIRST so soft prompts append to a sanitized message.
	// Phase 3 (tool-knowledge-...-20260718): previously normalize ran
	// after the soft prompt block, but looksLikeLargeBase64Payload
	// requires a very high base64-like ratio - appending SoftPromptFirstSuccess
	// (~280 chars of plain English) before sanitize dropped the ratio
	// below threshold and let huge base64 payloads leak through. Running
	// normalize first keeps the ratio check honest. Phase 12.55: normalize
	// now runs unconditionally (it was previously gated behind the breaker).
	result = normalizeToolResult(result, name, r.mediaStore, channel, chatID)

	// Phase 3 — soft prompts (tool-knowledge-...-20260718):
	//
	//   - On success: clear the signature counter (so the next failure
	//     starts fresh from count=1) and append SoftPromptFirstSuccess
	//     at most once per (session, tool) per turn.
	//   - On transient failure below threshold: when the count is in
	//     [2, threshold-1] (i.e. the LLM has retried the same approach
	//     once and is on the brink of escalation), append
	//     SoftPromptRepeatedFailure to nudge it toward saving a lesson.
	if statusOK {
		if tr := r.sigTrackerFor(channel, chatID); tr != nil {
			tr.MarkSuccess(SignatureKey{
				Tool:    name,
				ErrKind: result.ErrKind,
				ArgSig:  "",
			})
		}
		if !r.seenFirstSuccessBefore(channel, chatID, name) {
			r.markFirstSuccess(channel, chatID, name)
			result.ForLLM += SoftPromptFirstSuccess
		}
	} else if statusTransient {
		// Count was incremented inside EscalateIfNeeded; if it now
		// sits in [2, threshold-1] the LLM has retried once but
		// has not yet been told "stop". SoftPromptRepeatedFailure
		// bridges that gap with a save-knowledge nudge.
		if tr := r.sigTrackerFor(channel, chatID); tr != nil {
			key := SignatureKey{Tool: name, ErrKind: result.ErrKind, ArgSig: ""}
			c := tr.Count(key)
			if c >= 2 && c < tr.Threshold() {
				result.ForLLM += SoftPromptRepeatedFailure
			}
		}
	}

	// Escalator hint LAST (exactly once) — the tracker's directive is
	// intentionally stronger than the soft-prompt nudge.
	result = appendEscalatorHint(result, escalatorHint)

	duration := time.Since(start)

	// Log based on result type
	if result.IsError {
		logger.ErrorCF("tool", "Tool execution failed",
			map[string]any{
				"tool":     name,
				"duration": duration.Milliseconds(),
				"error":    result.ForLLM,
			})
	} else if result.Async {
		logger.InfoCF("tool", "Tool started (async)",
			map[string]any{
				"tool":     name,
				"duration": duration.Milliseconds(),
			})
	} else {
		logger.InfoCF("tool", "Tool execution completed",
			map[string]any{
				"tool":          name,
				"duration_ms":   duration.Milliseconds(),
				"result_length": len(result.ContentForLLM()),
			})
	}

	return result
}

// sortedToolNames returns tool names in sorted order for deterministic iteration.
// This is critical for KV cache stability: non-deterministic map iteration would
// produce different system prompts and tool definitions on each call, invalidating
// the LLM's prefix cache even when no tools have changed.
func (r *ToolRegistry) sortedToolNames() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *ToolRegistry) GetDefinitions() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sorted := r.sortedToolNames()
	definitions := make([]map[string]any, 0, len(sorted))
	for _, name := range sorted {
		entry := r.tools[name]

		if !entry.IsCore && entry.TTL <= 0 {
			continue
		}

		definitions = append(definitions, ToolToSchema(r.tools[name].Tool))
	}
	return definitions
}

// ToProviderDefs converts tool definitions to provider-compatible format.
// This is the format expected by LLM provider APIs.
func (r *ToolRegistry) ToProviderDefs() []providers.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sorted := r.sortedToolNames()
	definitions := make([]providers.ToolDefinition, 0, len(sorted))
	for _, name := range sorted {
		// Phase 11 fix: honour the runtime allowlist when projecting tool
		// definitions to the provider. In frozen projection mode (for KV prompt caching),
		// we bypass this filter and project all core/active tools while relying on runtime
		// gating (isLifecycleToolAllowed/Get) to block execution.
		if !r.projectionFrozen && !r.toolAllowedLocked(name) {
			continue
		}

		entry := r.tools[name]

		if !entry.IsCore && entry.TTL <= 0 {
			continue
		}

		schema := ToolToSchema(entry.Tool)

		// Safely extract nested values with type checks
		fn, ok := schema["function"].(map[string]any)
		if !ok {
			continue
		}

		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		params, _ := fn["parameters"].(map[string]any)
		metadata := promptMetadataForTool(entry.Tool)

		definitions = append(definitions, providers.ToolDefinition{
			Type: "function",
			Function: providers.ToolFunctionDefinition{
				Name:        name,
				Description: desc,
				Parameters:  params,
			},
			PromptLayer:  metadata.Layer,
			PromptSlot:   metadata.Slot,
			PromptSource: metadata.Source,
		})
	}
	return definitions
}

func promptMetadataForTool(tool Tool) PromptMetadata {
	metadata := PromptMetadata{
		Layer:  ToolPromptLayerCapability,
		Slot:   ToolPromptSlotTooling,
		Source: ToolPromptSourceRegistry,
	}
	if provider, ok := tool.(PromptMetadataProvider); ok {
		provided := provider.PromptMetadata()
		if provided.Layer != "" {
			metadata.Layer = provided.Layer
		}
		if provided.Slot != "" {
			metadata.Slot = provided.Slot
		}
		if provided.Source != "" {
			metadata.Source = provided.Source
		}
	}
	return metadata
}

// List returns a list of all registered tool names.
func (r *ToolRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.sortedToolNames()
}

// Clone creates an independent copy of the registry containing the same tool
// entries (shallow copy of each ToolEntry). This is used to give subagents a
// snapshot of the parent agent's tools without sharing the same registry —
// tools registered on the parent after cloning (e.g. spawn, spawn_status)
// will NOT be visible to the clone, preventing recursive subagent spawning.
// The version counter is reset to 0 in the clone as it's a new independent registry.
//
// Breaker state is intentionally not inherited: the clone starts with an empty
// breakers map, so the first tool execution on the subagent will lazily
// allocate a fresh Closed-state breaker for its (channel, chatID, tool)
// tuple. This matches the original design intent ("subagent = breaker mới")
// and, with per-session keys, also prevents a subagent from observing — or
// being observed by — the parent's transient failure state.
func (r *ToolRegistry) Clone() *ToolRegistry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	clone := &ToolRegistry{
		tools:    make(map[string]*ToolEntry, len(r.tools)),
		mediaStore: r.mediaStore,
	}
	if r.allowlist != nil {
		clone.allowlist = make(map[string]struct{}, len(r.allowlist))
		for name := range r.allowlist {
			clone.allowlist[name] = struct{}{}
		}
	}
	for name, entry := range r.tools {
		clone.tools[name] = &ToolEntry{
			Tool:   entry.Tool,
			IsCore: entry.IsCore,
			TTL:    entry.TTL,
		}
	}
	return clone
}

// Count returns the number of registered tools.
func (r *ToolRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// GetSummaries returns human-readable summaries of all registered tools.
// Returns a slice of "name - description" strings.
func (r *ToolRegistry) GetSummaries() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sorted := r.sortedToolNames()
	summaries := make([]string, 0, len(sorted))
	for _, name := range sorted {
		entry := r.tools[name]

		if !entry.IsCore && entry.TTL <= 0 {
			continue
		}

		summaries = append(
			summaries,
			fmt.Sprintf("- `%s` - %s", entry.Tool.Name(), entry.Tool.Description()),
		)
	}
	return summaries
}

// appendEscalatorHint appends the signature-tracker escalation directive to
// a tool result's ForLLM exactly once, when non-empty. Phase 12.55 (T11):
// the escalator hint used to ride on the circuit breaker's fb.Message
// channel; with the breaker deleted it must be appended directly here or it
// dies silently (writer-without-reader lesson, Phase 10.1).
func appendEscalatorHint(result *ToolResult, hintMsg string) *ToolResult {
	if hintMsg == "" {
		return result
	}
	result.ForLLM += "\n\n" + hintMsg
	return result
}

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
	breakers       map[string]*CircuitBreaker // key: composite (channel:chatID:name); see getCircuitBreaker
	sigTrackers    map[string]*SignatureFailureTracker // key: composite (channel:chatID); see getOrCreateSigTracker
	mu             sync.RWMutex
	cbMu           sync.Mutex // serializes lazy allocation of per-session breakers
	sigTrackerMu   sync.Mutex // serializes lazy allocation of per-session SignatureFailureTrackers
	version        atomic.Uint64 // incremented on Register/RegisterHidden for cache invalidation
	mediaStore     media.MediaStore
	allowlist      map[string]struct{}
	cfg            *config.ToolsConfig // optional; nil → fallback DefaultToolTimeoutSeconds
	timeoutStats   *ToolTimeoutStats   // Q3 metric; nil-safe via lazy init
	eventPublisher ToolEventPublisher  // optional bridge to runtime event bus (pkg/agent); nil = silent
}

// ToolBreakerEvent is the primitive metadata about a circuit breaker
// transition. Defined in pkg/tools (not pkg/agent) so the registry can
// publish without importing the agent package, and the agent's adapter
// wraps it into the runtime event envelope.
//
// Plan: circuit-breaker-3-tier-errkind-semantics-toolfeedback-20260717
type ToolBreakerEvent struct {
	Channel      string
	ChatID       string
	Tool         string
	LastErrorKind ErrorKind
	Failures     int
}

// ToolEventPublisher is the hook a ToolRegistry uses to surface circuit
// breaker transitions to the broader agent runtime (events, logs,
// dashboards). Nil-safe: when unset, breaker transitions are silent — only
// the in-tool hint message + ToolHealthContributor surface the change to
// the LLM. Set via SetEventPublisher.
//
// Implementations live in pkg/agent (e.g. AgentLoop.PublishToolBreakerTripped).
// The interface is intentionally tiny (single method, primitive types
// only) so it can be satisfied by test doubles without dragging the runtime
// event bus into the registry's dependency graph.
type ToolEventPublisher interface {
	PublishToolBreakerTripped(ToolBreakerEvent)
}

// SetEventPublisher wires (or clears, with nil) the publisher that the
// registry invokes when a circuit breaker transitions to Open. Safe to
// call concurrently with ExecuteWithContext — readers of eventPublisher
// take r.mu.RLock to avoid racing with this setter.
func (r *ToolRegistry) SetEventPublisher(p ToolEventPublisher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventPublisher = p
}

type mediaStoreAware interface {
	SetMediaStore(store media.MediaStore)
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:       make(map[string]*ToolEntry),
		breakers:    make(map[string]*CircuitBreaker),
		sigTrackers: make(map[string]*SignatureFailureTracker),
		timeoutStats: newToolTimeoutStats(),
	}
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
func (r *ToolRegistry) SetAllowlist(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if names == nil {
		r.allowlist = nil
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
}

func (r *ToolRegistry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registerLocked(tool, true)
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
	// Breakers are created lazily by getCircuitBreaker on first use, scoped by
	// (channel, chatID, name). We no longer pre-allocate a per-tool breaker
	// here; doing so would defeat the per-session isolation that lives in
	// the registry's breaker map.
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

func (r *ToolRegistry) toolAllowedLocked(name string) bool {
	if r.allowlist == nil {
		return true
	}
	if isToolDiscoveryToolName(name) {
		// Discovery tools are part of the MCP control plane: they must remain
		// available whenever configured so deferred MCP tools can still be
		// unlocked. Per-agent allowlists still apply to the hidden MCP tools
		// themselves during RegisterHidden.
		return true
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

// getCircuitBreaker returns the breaker scoped to the (channel, chatID, name)
// tuple, allocating a fresh Closed-state breaker on first use. Callers that
// pass empty channel/chatID land in the shared "_anon" namespace so that
// legacy code paths (e.g. Execute without context) cannot trip a real
// session's breaker. The cbMu lock protects the breakers map; the returned
// breaker has its own internal mutex for state transitions.
func (r *ToolRegistry) getCircuitBreaker(channel, chatID, name string) *CircuitBreaker {
	key := breakerKey(channel, chatID, name)

	r.cbMu.Lock()
	if cb, ok := r.breakers[key]; ok {
		r.cbMu.Unlock()
		return cb
	}
	cb := NewCircuitBreakerWithName(name)
	r.breakers[key] = cb
	r.cbMu.Unlock()
	return cb
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
	key := breakerKey(channel, chatID, "")

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
// SignatureFailureTracker. Called at turn boundaries so a new turn starts
// with a fresh slate. No-op if no tracker exists yet for the session.
func (r *ToolRegistry) ResetSignatureFailures(channel, chatID string) {
	key := breakerKey(channel, chatID, "")

	r.sigTrackerMu.Lock()
	tr, ok := r.sigTrackers[key]
	r.sigTrackerMu.Unlock()
	if ok {
		tr.Reset()
	}
}

// OpenToolInfo describes a tool whose circuit breaker is currently open.
// Returned by ToolRegistry.OpenTools() to drive the ToolHealthContributor
// self-correction directive in the LLM prompt.
//
// LastErrorKind lets the prompt contributor surface "transient/network"
// vs "dependency down" so the LLM can pick a different retry strategy
// (e.g. "wait and retry" vs "fall back to a different tool"). Empty
// when the breaker is Closed or was opened before this field was added.
//
// Plan: circuit-breaker-3-tier-errkind-semantics-toolfeedback-20260717
type OpenToolInfo struct {
	Name          string
	OpenedAt      time.Time
	Failures      int
	LastErrorKind ErrorKind
}

// OpenTools returns aggregated info for all tools with an open circuit
// breaker across all session scopes (channel:chatID:name tuples). A tool
// open in multiple sessions appears once with the earliest OpenedAt and
// the failures count of that earliest-opened breaker. Result is sorted by
// OpenedAt (oldest first) so the prompt can highlight the longest outage.
func (r *ToolRegistry) OpenTools() []OpenToolInfo {
	r.cbMu.Lock()
	breakers := make([]*CircuitBreaker, 0, len(r.breakers))
	for _, cb := range r.breakers {
		breakers = append(breakers, cb)
	}
	r.cbMu.Unlock()

	byName := make(map[string]OpenToolInfo)
	for _, cb := range breakers {
		name := cb.Name()
		if name == "" {
			continue
		}
		state, openedAt, failures, lastErrKind := cb.Snapshot()
		if state != StateOpen {
			continue
		}
		if existing, ok := byName[name]; !ok || openedAt.Before(existing.OpenedAt) {
			byName[name] = OpenToolInfo{
				Name:          name,
				OpenedAt:      openedAt,
				Failures:      failures,
				LastErrorKind: lastErrKind,
			}
		}
	}

	out := make([]OpenToolInfo, 0, len(byName))
	for _, info := range byName {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].OpenedAt.Before(out[j].OpenedAt)
	})
	return out
}

// ExecuteWithContext executes a tool with channel/chatID context and optional async callback.
// If the tool implements AsyncExecutor and a non-nil callback is provided,
// ExecuteAsync is called instead of Execute — the callback is a parameter,
// never stored as mutable state on the tool.
func (r *ToolRegistry) ExecuteWithContext(
	ctx context.Context,
	name string,
	args map[string]any,
	channel, chatID string,
	asyncCallback AsyncCallback,
) *ToolResult {
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

	// Circuit Breaker check
	cb := r.getCircuitBreaker(channel, chatID, name)
	if cb != nil && !cb.Allow() {
		logger.WarnCF("tool", "Tool execution blocked by circuit breaker",
			map[string]any{"tool": name})
		// Pick the hint by last-error-kind: ErrDependencyDown means the
		// upstream is dead (no recovery window matters), ErrTransient /
		// ErrTimeout means we'll auto-retry after recoveryTimeout. This
		// matches the canonical messages appended by RecordResult on the
		// hot path so the LLM sees consistent wording whether it hit the
		// breaker on the way in (Allow==false) or on the way out
		// (RecordResult returned StatusBlocked after a JustTripped
		// transition).
		blockedHint := escalationHint(name)
		if cb.LastErrorKind() == ErrDependencyDown {
			blockedHint = dependencyDownHint(name)
		}
		return ErrorResult(blockedHint).
			WithErrorKind(ErrDependencyDown).
			WithError(fmt.Errorf("circuit breaker open for tool %q", name))
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

		if cb != nil {
			fb := cb.RecordResult(name, true, res.ErrKind)
			// Phase 2: SignatureFailureTracker escalation — after threshold
			// repeated failures of the same (tool, errKind) signature, swap
			// the canonical hint for a stronger "stop retrying" directive so
			// the LLM does not burn the rest of the turn budget on minor
			// variations of the same failing approach. Only fires for
			// StatusValidationError (Tier 3) and StatusTransient (still
			// below breaker threshold). StatusBlocked is handled by the
			// breaker hot path above (and TryRecover via Change 4).
			if fb.Status == StatusValidationError || fb.Status == StatusTransient {
				if tracker := r.getOrCreateSigTracker(channel, chatID); tracker != nil {
					if hint := tracker.EscalateIfNeeded(SignatureKey{
						Tool:    name,
						ErrKind: res.ErrKind,
						ArgSig:  "",
					}, res.ForLLM); hint != "" {
						fb.Message = hint
					}
				}
			}
			if fb.Message != "" {
				res.ForLLM += "\n\n" + fb.Message
			}
			// fb.JustTripped is always false for ErrInvalidInput (Tier 3
			// never trips), so no event emission here. Defensive guard
			// for future tier changes.
			if fb.JustTripped && r.eventPublisher != nil {
				r.mu.RLock()
				publisher := r.eventPublisher
				r.mu.RUnlock()
				if publisher != nil {
					publisher.PublishToolBreakerTripped(ToolBreakerEvent{
						Channel:      channel,
						ChatID:       chatID,
						Tool:         name,
						LastErrorKind: cb.LastErrorKind(),
						Failures:     cb.Failures(),
					})
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

	if cb != nil {
		// Only record synchronous tool executions, async results are handled later/elsewhere
		// but for now we'll just track if the initial sync execution failed.
		fb := cb.RecordResult(name, result.IsError, result.ErrKind)

		// Phase 2: SignatureFailureTracker escalation — same as the
		// validation path above. Fires for transient/timeout errors that
		// are still below breaker threshold (StatusTransient) so the LLM
		// sees a stronger "stop retrying" directive before the breaker
		// itself trips (which is handled by the StatusBlocked path above
		// via dependencyDownHint/escalationHint).
		if fb.Status == StatusTransient {
			if tracker := r.getOrCreateSigTracker(channel, chatID); tracker != nil {
				if hint := tracker.EscalateIfNeeded(SignatureKey{
					Tool:    name,
					ErrKind: result.ErrKind,
					ArgSig:  "",
				}, result.ForLLM); hint != "" {
					fb.Message = hint
				}
			}
		}
		// Append the canonical hint produced by RecordResult (transientHint,
		// escalationHint, dependencyDownHint, validationHint). Append for
		// ANY non-empty Message so the LLM sees a consistent directive —
		// even on the transient (below-threshold) case where the previous
		// inline "Note/Warning" appender lived. JustTripped flag is the
		// ONLY signal we trust to fire the runtime event — duplicate
		// RecordResult calls during the same Open period must not re-emit.
		if fb.Message != "" {
			result.ForLLM += "\n\n" + fb.Message
		}
		if fb.JustTripped && r.eventPublisher != nil {
			r.mu.RLock()
			publisher := r.eventPublisher
			r.mu.RUnlock()
			if publisher != nil {
				publisher.PublishToolBreakerTripped(ToolBreakerEvent{
					Channel:      channel,
					ChatID:       chatID,
					Tool:         name,
					LastErrorKind: cb.LastErrorKind(),
					Failures:     cb.Failures(),
				})
			}
		}
	}

	result = normalizeToolResult(result, name, r.mediaStore, channel, chatID)

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
		breakers: make(map[string]*CircuitBreaker),
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

// GetAll returns all registered tools (both core and non-core with TTL > 0).
// Used by SubTurn to inherit parent's tool set.
func (r *ToolRegistry) GetAll() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sorted := r.sortedToolNames()
	tools := make([]Tool, 0, len(sorted))
	for _, name := range sorted {
		entry := r.tools[name]

		// Include core tools and non-core tools with active TTL
		if entry.IsCore || entry.TTL > 0 {
			tools = append(tools, entry.Tool)
		}
	}
	return tools
}

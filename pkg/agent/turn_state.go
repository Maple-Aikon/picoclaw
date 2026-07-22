// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/routing"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// =============================================================================
// TurnPhase - represents the current phase of a turn
// =============================================================================

type TurnPhase string

const (
	TurnPhaseSetup      TurnPhase = "setup"
	TurnPhaseRunning    TurnPhase = "running"
	TurnPhaseTools      TurnPhase = "tools"
	TurnPhaseFinalizing TurnPhase = "finalizing"
	TurnPhaseCompleted  TurnPhase = "completed"
	TurnPhaseAborted    TurnPhase = "aborted"
)

// =============================================================================
// Control signals - returned from Pipeline methods to drive runTurn's coordinator loop
// =============================================================================

type Control int

const (
	// ControlContinue tells the coordinator to jump back to the top of the turn loop
	// (equivalent to the original "goto turnLoop").
	ControlContinue Control = iota
	// ControlBreak tells the coordinator to exit the turn loop and proceed to Finalize.
	ControlBreak
	// ControlToolLoop tells the coordinator to execute the tool loop.
	ControlToolLoop
)

// ToolControl signals returned from ExecuteTools to drive tool loop iteration.
type ToolControl int

const (
	// ToolControlContinue tells the tool loop to jump to the next iteration
	// (pendingMessages arrived, SubTurn results, etc.).
	ToolControlContinue ToolControl = iota
	// ToolControlBreak tells the tool loop to exit and return to the coordinator.
	ToolControlBreak
	// ToolControlFinalize tells the coordinator that all tool responses were
	// handled and the turn should finalize without another LLM call.
	ToolControlFinalize
)

// LLMPhase indicates which phase the turn is executing in.
type LLMPhase int

const (
	LLMPhaseSetup LLMPhase = iota
	LLMPhasePreLLM
	LLMPhaseLLMCall
	LLMPhaseProcessing
	LLMPhaseToolLoop
	LLMPhaseTools
	LLMPhaseFinalizing
	LLMPhaseCompleted
	LLMPhaseAborted
)

// =============================================================================
// turnResult - returned from runTurn
// =============================================================================

type turnResult struct {
	finalContent string
	modelName    string
	status       TurnEndStatus
	followUps    []bus.InboundMessage
}

// =============================================================================
// ActiveTurnInfo - public info about an active turn
// =============================================================================

type ActiveTurnInfo struct {
	TurnID       string
	AgentID      string
	SessionKey   string
	Channel      string
	ChatID       string
	UserMessage  string
	Phase        TurnPhase
	Iteration    int
	StartedAt    time.Time
	Depth        int
	ParentTurnID string
	ChildTurnIDs []string
}

// =============================================================================
// turnExecution - mutable state that persists across turn loop iterations
// =============================================================================

type turnExecution struct {
	// Core message state (accumulates throughout the turn)
	messages         []providers.Message // built from ContextBuilder, grows per-iteration
	pendingMessages  []providers.Message // steering/SubTurn messages awaiting injection
	history          []providers.Message // from ContextManager.Assemble
	summary          string
	taskSummaryChan  chan string
	currentTurnStart int

	// injectedTaskSummary tracks whether a task reminder has been injected
	// this turn. Empty = not yet injected; non-empty = the exact summary text
	// that was injected. Guards against duplicate injection at the 50%
	// threshold when steering already placed a reminder into the messages.
	injectedTaskSummary string

	// Turn output
	finalContent string

	// Iteration tracking
	iteration int

	// Per-iteration state set by Pipeline.PreLLM
	activeCandidates  []providers.FallbackCandidate
	activeModel       string
	activeModelConfig *config.ModelConfig
	activeProvider    providers.LLMProvider
	tier              routing.Tier
	usedLight         bool

	// LLM call per-iteration state
	response            *providers.LLMResponse
	normalizedToolCalls []providers.ToolCall
	allResponsesHandled bool
	streamingPublisher  *streamingChunkPublisher
	streamingFallback   bool
	suppressReasoning   bool
	callMessages        []providers.Message
	providerToolDefs    []providers.ToolDefinition
	llmModel            string
	llmModelName        string
	llmOpts             map[string]any
	gracefulTerminal    bool
	useNativeSearch     bool

	// Phase tracking
	phase LLMPhase

	// Error recovery: set when prior turn failed and we pre-extract task context
	isErrorRecovery bool

	// Abort signaling for coordinator (set by Pipeline methods)
	abortedByHardAbort bool // true when hard abort triggered during LLM/tools
	abortedByHook      bool // true when HookActionAbortTurn triggered

	// taskExtractCancel cancels the in-flight background task extraction goroutine.
	// Set by SetupTurn, called by runTurn when steering messages arrive mid-turn
	// to prevent stale summaries from overwriting the steering result.
	taskExtractCancel context.CancelFunc
}

// newTurnExecution creates a turnExecution initialized from turnState and options.
func newTurnExecution(
	agent *AgentInstance,
	opts processOptions,
	history []providers.Message,
	summary string,
	messages []providers.Message,
) *turnExecution {
	return &turnExecution{
		history:          history,
		summary:          summary,
		messages:         messages,
		pendingMessages:  append([]providers.Message(nil), opts.InitialSteeringMessages...),
		taskSummaryChan:  make(chan string, 1),
		currentTurnStart: len(messages),
		iteration:        0,
		phase:            LLMPhaseSetup,
	}
}

// =============================================================================
// turnState - the full state for a turn, constructed once per turn
// =============================================================================

type turnState struct {
	mu sync.RWMutex

	agent   *AgentInstance
	opts    processOptions
	profile config.EffectiveTurnProfile
	scope   turnEventScope

	turnID            string
	agentID           string
	sessionKey        string
	activeSkills      []string
	attemptedSkills   []string
	skillContextTrace []SkillContextSnapshot
	toolKinds         []string
	toolExecutions    []ToolExecutionRecord
	turnCtx           *TurnContext

	channel     string
	chatID      string
	workspace   string
	userMessage string
	media       []string

	phase                 TurnPhase
	iteration             int
	assistantText         string // Phase 11: LLM text output from most recent iteration; used by complete_goal to decide final reply (assistantText vs summary param).
	iterationCap          int    // (Phase 10.1 restored: goal_progress can self-extend up to agent.MaxIterationsCap)
	maxIterationsCap      int    // (Phase 10.1) absolute ceiling for iterationCap; set from agent.MaxIterationsCap at turn start (0 = unbounded)
	lastExtensionReason   string // Phase 10.1: reason string from the most recent ExtendIterationCap call (for audit/diagnostics)
	lastExtensionAtIter   int    // Phase 10.1: iteration number when ExtendIterationCap last fired (0 = never extended)
	goalFinalized         bool   // Phase 11: set true after complete_goal tool call so the loop breaks immediately.

	// Replay counter: bound AfterLLM hook replay attempts within a single iteration.
	// Replay attempts are recovery retries (e.g. malformed tool-call recovery)
	// that shouldn't consume an iteration slot in iterationCap. See plan
	// same-iteration-replay-loop-with-reusable-boundedretry-primitive-20260717.
	replayCount           int    // bumps per replay attempt (resets each iteration)
	replayCap             int    // hard cap; defaults to agent.MaxReplayAttempts (or defaultRetryMaxAttempts)

	// Goal-lifecycle retry counters (Phase 5): bound same-iteration recovery retries
	// per-iteration so they don't consume iterationCap slots. Each field resets
	// at iteration boundary. See plan §5.2 + §5.3 in
	// picoclaw-goal-lifecycle-long-running-task-với-setviewcomplete-goal-goal-phase-tool-allowlist-20260719.
	textOnlyStreak           int      // consecutive iterations with text-only LLM response (no tool calls). Goal Phase 1 only.
	emptyResponseRecoverySent bool    // once per iteration: have we injected EMPTY_FINAL_RESPONSE_MESSAGE yet?
	toolExecRecoveryAttempts map[string]int // per-tool execution error retry count (not signature). Same iteration.

	// Goal-lifecycle recovery side-effects (Phase 5): set by applyRecoveryAction.
	// Consumed at the start of the next iteration (ControlContinue path) to
	// inject the recovery message into the conversation, strip non-goal tools,
	// or finalize the goal. See plan §5.2 + §8.3.
	pendingRecoveryMessage  string // message to inject before next LLM call (empty = no injection)
	goalArchiveRequested    bool   // if true, caller must call finalizeGoalOnTurnEnd (Phase 6 hook)

	startedAt             time.Time
	finalContent          string

	followUps []bus.InboundMessage

	gracefulInterrupt     bool
	gracefulInterruptHint string
	gracefulTerminalUsed  bool
	hardAbort             bool
	providerCancel        context.CancelFunc
	turnCancel            context.CancelFunc

	restorePointHistory []providers.Message
	restorePointSummary string
	persistedMessages   []providers.Message

	// SubTurn support (from HEAD)
	depth                int                    // SubTurn depth (0 for root turn)
	parentTurnID         string                 // Parent turn ID (empty for root turn)
	childTurnIDs         []string               // Child turn IDs
	pendingResults       chan *tools.ToolResult // Channel for SubTurn results
	concurrencySem       chan struct{}          // Semaphore for limiting concurrent SubTurns
	isFinished           atomic.Bool            // Whether this turn has finished
	session              session.SessionStore   // Session store reference
	initialHistoryLength int                    // Snapshot of history length at turn start

	// Additional SubTurn fields
	ctx             context.Context    // Context for this turn
	cancelFunc      context.CancelFunc // Cancel function for this turn's context
	critical        bool               // Whether this SubTurn should continue after parent ends
	parentTurnState *turnState         // Reference to parent turnState
	parentEnded     atomic.Bool        // Whether parent has ended
	closeOnce       sync.Once          // Ensures pendingResults channel is closed once
	finishedChan    chan struct{}      // Closed when turn finishes

	// Token budget tracking
	tokenBudget      *atomic.Int64        // Shared token budget counter
	lastFinishReason string               // Last LLM finish_reason
	lastUsage        *providers.UsageInfo // Last LLM usage info

	// Back-reference to the owning AgentLoop (set for SubTurns only, used for hard abort cascade)
	al *AgentLoop
}

// =============================================================================
// turnState constructors and active turn management
// =============================================================================

func newTurnState(agent *AgentInstance, opts processOptions, scope turnEventScope) *turnState {
	ts := &turnState{
		agent:        agent,
		opts:         opts,
		profile:      opts.TurnProfile,
		scope:        scope,
		turnID:       scope.turnID,
		agentID:      agent.ID,
		sessionKey:   opts.Dispatch.SessionKey,
		activeSkills: activeSkillNames(agent, opts),
		turnCtx:      cloneTurnContext(scope.context),
		channel:      opts.Dispatch.Channel(),
		chatID:       opts.Dispatch.ChatID(),
		workspace:    agent.Workspace,
		userMessage:  opts.Dispatch.UserMessage,
		media:        append([]string(nil), opts.Dispatch.Media...),
		phase:        TurnPhaseSetup,
		startedAt:    time.Now(),
	}

	// Initialize iterationCap to the agent's MaxIterations. Phase 10.1
	// restored the ExtendIterationCap mechanism so goal_progress (and any
	// future internal recovery code) can self-extend iterationCap up to
	// agent.MaxIterationsCap. The user-facing /extend command was removed
	// in Phase 10; only programmatic internal callers may extend.
	ts.iterationCap = agent.MaxIterations
	ts.maxIterationsCap = agent.MaxIterationsCap

	// Initialize replayCap to agent.MaxReplayAttempts (defaultRetryMaxAttempts
	// when unset). The cap bounds how many same-iteration LLM replays a hook
	// can request via HookActionReplay before the pipeline degrades to a
	// ControlContinue with a warning event (LLMReplayExhaustedPayload).
	ts.replayCap = agent.MaxReplayAttempts
	if ts.replayCap <= 0 {
		ts.replayCap = defaultRetryMaxAttempts
	}

	// Bind session store and capture initial history length for rollback logic
	if agent != nil && agent.Sessions != nil {
		ts.session = agent.Sessions
		history := agent.Sessions.GetHistory(opts.Dispatch.SessionKey)
		ts.initialHistoryLength = len(history)
		ts.restorePointHistory = append([]providers.Message(nil), history...)
		ts.restorePointSummary = agent.Sessions.GetSummary(opts.Dispatch.SessionKey)
	}

	return ts
}

func (al *AgentLoop) registerActiveTurn(ts *turnState) {
	al.activeTurnStates.Store(ts.sessionKey, ts)
}

func (al *AgentLoop) clearActiveTurn(ts *turnState) {
	al.releaseSessionTurnState(ts.sessionKey, ts)
}

func (al *AgentLoop) releaseSessionTurnState(sessionKey string, expected *turnState) {
	if expected == nil {
		al.activeTurnStates.Delete(sessionKey)
		return
	}
	if actual, ok := al.activeTurnStates.Load(sessionKey); ok && actual == expected {
		al.activeTurnStates.Delete(sessionKey)
	}
}

func (al *AgentLoop) getActiveTurnState(sessionKey string) *turnState {
	if val, ok := al.activeTurnStates.Load(sessionKey); ok {
		if ts, ok := val.(*turnState); ok {
			return ts
		}
		// Unexpected non-*turnState value — treat as "no active turn" to avoid
		// panics. This should not happen under normal operation.
	}
	return nil
}

// getAnyActiveTurnState returns any active turn state (for backward compatibility)
func (al *AgentLoop) getAnyActiveTurnState() *turnState {
	var firstTS *turnState
	al.activeTurnStates.Range(func(key, value any) bool {
		if ts, ok := value.(*turnState); ok {
			firstTS = ts
			return false
		}
		return true
	})
	return firstTS
}

func (al *AgentLoop) GetActiveTurn() *ActiveTurnInfo {
	// For backward compatibility, return the first active turn found
	// In the new architecture, there can be multiple concurrent turns
	var firstTS *turnState
	al.activeTurnStates.Range(func(key, value any) bool {
		if ts, ok := value.(*turnState); ok {
			firstTS = ts
			return false
		}
		return true
	})
	if firstTS == nil {
		return nil
	}
	info := firstTS.snapshot()
	return &info
}

func (al *AgentLoop) GetActiveTurnBySession(sessionKey string) *ActiveTurnInfo {
	ts := al.getActiveTurnState(sessionKey)
	if ts == nil {
		return nil
	}
	info := ts.snapshot()
	return &info
}

// =============================================================================
// turnState - getters and setters
// =============================================================================

func (ts *turnState) snapshot() ActiveTurnInfo {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	return ActiveTurnInfo{
		TurnID:       ts.turnID,
		AgentID:      ts.agentID,
		SessionKey:   ts.sessionKey,
		Channel:      ts.channel,
		ChatID:       ts.chatID,
		UserMessage:  ts.userMessage,
		Phase:        ts.phase,
		Iteration:    ts.iteration,
		StartedAt:    ts.startedAt,
		Depth:        ts.depth,
		ParentTurnID: ts.parentTurnID,
		ChildTurnIDs: append([]string(nil), ts.childTurnIDs...),
	}
}

func (ts *turnState) setPhase(phase TurnPhase) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.phase = phase
}

func (ts *turnState) setIteration(iteration int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.iteration = iteration
}

func (ts *turnState) currentIteration() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.iteration
}

// resetReplayCount zeroes the per-iteration replay counter. Called by the
// coordinator at the top of each iteration so that replay attempts don't
// carry over to the next iteration's budget.
//
// See plan same-iteration-replay-loop-with-reusable-boundedretry-primitive-20260717.
func (ts *turnState) resetReplayCount() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.replayCount = 0
}

// currentReplayCount returns the current replay attempt count (for observability).
func (ts *turnState) currentReplayCount() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.replayCount
}

// currentReplayCap returns the configured replay cap (for observability).
func (ts *turnState) currentReplayCap() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.replayCap <= 0 {
		return defaultRetryMaxAttempts
	}
	return ts.replayCap
}

// RemainingIterations returns the number of tool iterations remaining before the
// turn's hard cap is reached. Clamped to zero if the cap has already been exceeded.
func (ts *turnState) RemainingIterations() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	remaining := ts.iterationCap - ts.iteration
	if remaining < 0 {
		return 0
	}
	return remaining
}

// CurrentIteration returns the turn's current iteration count (1-based).
func (ts *turnState) CurrentIteration() int {
	return ts.currentIteration()
}

// IterationCap returns the turn's iteration cap. The cap is set at turn start
// from agent.MaxIterations and may be extended during the turn by
// ExtendIterationCap (Phase 10.1 restored for goal_progress self-extend).
func (ts *turnState) IterationCap() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.iterationCap
}

// MaxIterationsCap returns the absolute ceiling for iterationCap (set from
// agent.MaxIterationsCap at turn start). ExtendIterationCap refuses any
// extension that would push iterationCap past this value.
func (ts *turnState) MaxIterationsCap() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.maxIterationsCap
}

// ExtendIterationCap programmatically raises iterationCap by n additional
// iterations, clamped to the absolute ceiling agent.MaxIterationsCap. Returns
// the new iterationCap value and the delta actually applied (0 if n == 0,
// negative if the cap was already at the ceiling and could not be extended
// further).
//
// Phase 10.1: restored from Phase 10 removal. The only caller is the
// goal_progress tool handler, which uses this to keep the iterationCap above
// the current iteration when remaining_steps > 0, so the agent can keep
// making progress within a single turn. Other internal recovery logic may
// also call it in future phases.
//
// Replaces the user-facing extend_turn_iteration tool that Phase 10 removed.
// Tool integration via WithIterationExtender is no longer required; this
// method is now called directly from goal_progress via the turnState passed
// through the tool's exec context.
func (ts *turnState) ExtendIterationCap(n int, reason string) (newCap int, delta int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if n == 0 {
		return ts.iterationCap, 0
	}
	if n < 0 {
		return ts.iterationCap, 0
	}
	if ts.maxIterationsCap <= 0 {
		// No ceiling configured: unbounded but conservative fallback (cap = +n).
		ts.iterationCap += n
		ts.lastExtensionReason = reason
		ts.lastExtensionAtIter = ts.iteration
		return ts.iterationCap, n
	}
	ceiling := ts.maxIterationsCap
	proposed := ts.iterationCap + n
	if proposed > ceiling {
		// Clamp to ceiling; delta reflects what was actually granted.
		delta = ceiling - ts.iterationCap
		ts.iterationCap = ceiling
	} else {
		delta = n
		ts.iterationCap = proposed
	}
	ts.lastExtensionReason = reason
	ts.lastExtensionAtIter = ts.iteration
	return ts.iterationCap, delta
}

// LastExtensionInfo returns the reason and iteration number from the most
// recent ExtendIterationCap call. Both zero values mean no extension has
// happened this turn (used for diagnostics + @debugcf logging).
func (ts *turnState) LastExtensionInfo() (reason string, atIter int) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.lastExtensionReason, ts.lastExtensionAtIter
}

// CanExtendIterationCap reports whether iterationCap is below the absolute
// ceiling agent.MaxIterationsCap (i.e. there is room for at least one more
// extension). Callers should check this before calling ExtendIterationCap to
// avoid redundant extension events in @debugcf logs.
func (ts *turnState) CanExtendIterationCap() bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.maxIterationsCap <= 0 {
		return true
	}
	return ts.iterationCap < ts.maxIterationsCap
}

// MarkGoalFinalized is the Phase 11 hook that complete_goal calls to
// short-circuit the per-turn loop. Once set, currentGoalPhase() returns
// GoalPhaseFinal and the runtime breaks out of the iteration loop after
// the tool result is processed. Idempotent — repeated calls are a no-op.
func (ts *turnState) MarkGoalFinalized() {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.goalFinalized = true
}

// IsGoalFinalized reports whether MarkGoalFinalized has been called this
// turn. Used by the runTurn coordinator's top-of-loop check to break
// out of the per-turn tool loop after complete_goal fired.
//
// Implements goal.GoalTurnState interface (Phase 11). Read-only — only
// the turn state itself can change the underlying flag.
func (ts *turnState) IsGoalFinalized() bool {
	if ts == nil {
		return false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.goalFinalized
}

// MaxIterationsPerCheckpoint returns the per-checkpoint iteration budget
// (default = agent.MaxIterations, e.g. 20). Implements the
// goal.IterationExtender interface so goal_progress can pick the
// ExtendIterationCap amount without importing pkg/agent.
func (ts *turnState) MaxIterationsPerCheckpoint() int {
	if ts == nil || ts.agent == nil {
		return 0
	}
	return ts.agent.MaxIterations
}

func (ts *turnState) setFinalContent(content string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.finalContent = content
}

func (ts *turnState) finalContentLen() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.finalContent)
}

func (ts *turnState) finalContentSnapshot() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.finalContent
}

func (ts *turnState) recordToolKind(tool string) {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	for _, existing := range ts.toolKinds {
		if existing == tool {
			return
		}
	}
	ts.toolKinds = append(ts.toolKinds, tool)
}

func (ts *turnState) toolKindsSnapshot() []string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return append([]string(nil), ts.toolKinds...)
}

func (ts *turnState) recordToolExecution(tool string, success bool, errorSummary string, skillNames []string) {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return
	}

	ts.recordToolKind(tool)

	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.toolExecutions = append(ts.toolExecutions, ToolExecutionRecord{
		Name:         tool,
		Success:      success,
		ErrorSummary: strings.TrimSpace(errorSummary),
		SkillNames:   append([]string(nil), skillNames...),
	})
}

func (ts *turnState) toolExecutionsSnapshot() []ToolExecutionRecord {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if len(ts.toolExecutions) == 0 {
		return nil
	}

	out := make([]ToolExecutionRecord, 0, len(ts.toolExecutions))
	for _, exec := range ts.toolExecutions {
		out = append(out, ToolExecutionRecord{
			Name:         exec.Name,
			Success:      exec.Success,
			ErrorSummary: exec.ErrorSummary,
			SkillNames:   append([]string(nil), exec.SkillNames...),
		})
	}
	return out
}

func (ts *turnState) recordAttemptedSkills(skillNames []string) {
	if len(skillNames) == 0 {
		return
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	for _, skillName := range skillNames {
		skillName = strings.TrimSpace(skillName)
		if skillName == "" {
			continue
		}
		seen := false
		for _, existing := range ts.attemptedSkills {
			if existing == skillName {
				seen = true
				break
			}
		}
		if seen {
			continue
		}
		ts.attemptedSkills = append(ts.attemptedSkills, skillName)
	}
}

func (ts *turnState) attemptedSkillsSnapshot() []string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return append([]string(nil), ts.attemptedSkills...)
}

func (ts *turnState) recordSkillContextSnapshot(trigger string, skillNames []string) {
	if len(skillNames) == 0 {
		return
	}

	filtered := make([]string, 0, len(skillNames))
	for _, skillName := range skillNames {
		skillName = strings.TrimSpace(skillName)
		if skillName == "" {
			continue
		}
		filtered = append(filtered, skillName)
	}
	if len(filtered) == 0 {
		return
	}

	ts.recordAttemptedSkills(filtered)

	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.skillContextTrace = append(ts.skillContextTrace, SkillContextSnapshot{
		Sequence:   len(ts.skillContextTrace) + 1,
		Trigger:    trigger,
		SkillNames: append([]string(nil), filtered...),
	})
}

func (ts *turnState) latestSkillContextSnapshot() []string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if len(ts.skillContextTrace) == 0 {
		return nil
	}
	return append([]string(nil), ts.skillContextTrace[len(ts.skillContextTrace)-1].SkillNames...)
}

func (ts *turnState) skillContextSnapshotsSnapshot() []SkillContextSnapshot {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if len(ts.skillContextTrace) == 0 {
		return nil
	}

	snapshots := make([]SkillContextSnapshot, 0, len(ts.skillContextTrace))
	for _, snapshot := range ts.skillContextTrace {
		snapshots = append(snapshots, SkillContextSnapshot{
			Sequence:   snapshot.Sequence,
			Trigger:    snapshot.Trigger,
			SkillNames: append([]string(nil), snapshot.SkillNames...),
		})
	}
	return snapshots
}

func (ts *turnState) setTurnCancel(cancel context.CancelFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.turnCancel = cancel
}

func (ts *turnState) setProviderCancel(cancel context.CancelFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.providerCancel = cancel
}

func (ts *turnState) clearProviderCancel(_ context.CancelFunc) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.providerCancel = nil
}

func (ts *turnState) requestGracefulInterrupt(hint string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.hardAbort {
		return false
	}
	ts.gracefulInterrupt = true
	ts.gracefulInterruptHint = hint
	return true
}

func (ts *turnState) gracefulInterruptRequested() (bool, string) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.gracefulInterrupt && !ts.gracefulTerminalUsed, ts.gracefulInterruptHint
}

func (ts *turnState) markGracefulTerminalUsed() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.gracefulTerminalUsed = true
}

func (ts *turnState) requestHardAbort() bool {
	ts.mu.Lock()
	if ts.hardAbort {
		ts.mu.Unlock()
		return false
	}
	ts.hardAbort = true
	turnCancel := ts.turnCancel
	providerCancel := ts.providerCancel
	ts.mu.Unlock()

	if providerCancel != nil {
		providerCancel()
	}
	if turnCancel != nil {
		turnCancel()
	}
	return true
}

func (ts *turnState) hardAbortRequested() bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.hardAbort
}

func (ts *turnState) eventMeta(source, tracePath string) HookMeta {
	snap := ts.snapshot()
	return HookMeta{
		AgentID:     snap.AgentID,
		TurnID:      snap.TurnID,
		SessionKey:  snap.SessionKey,
		Iteration:   snap.Iteration,
		Source:      source,
		TracePath:   tracePath,
		turnContext: cloneTurnContext(ts.turnCtx),
	}
}

func (ts *turnState) captureRestorePoint(history []providers.Message, summary string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.restorePointHistory = append([]providers.Message(nil), history...)
	ts.restorePointSummary = summary
}

func (ts *turnState) recordPersistedMessage(msg providers.Message) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.persistedMessages = append(ts.persistedMessages, msg)
}

func (ts *turnState) persistedMessagesSnapshot() []providers.Message {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return append([]providers.Message(nil), ts.persistedMessages...)
}

func (ts *turnState) refreshRestorePointFromSession(agent *AgentInstance) {
	history := agent.Sessions.GetHistory(ts.sessionKey)
	summary := agent.Sessions.GetSummary(ts.sessionKey)

	persisted := ts.persistedMessagesSnapshot()

	if matched := matchingTurnMessageTail(history, persisted); matched > 0 {
		history = append([]providers.Message(nil), history[:len(history)-matched]...)
	}

	ts.captureRestorePoint(history, summary)
}

// ingestMessage calls the ContextManager's Ingest method for a persisted message.
// Errors are logged but never block the turn.
func (ts *turnState) ingestMessage(ctx context.Context, al *AgentLoop, msg providers.Message) {
	if al.contextManager == nil {
		return
	}
	if err := al.contextManager.Ingest(ctx, &IngestRequest{
		SessionKey: ts.sessionKey,
		Message:    msg,
	}); err != nil {
		logger.WarnCF("agent", "Context manager ingest failed", map[string]any{
			"session_key": ts.sessionKey,
			"error":       err.Error(),
		})
	}
}

func (ts *turnState) restoreSession(agent *AgentInstance) error {
	ts.mu.RLock()
	history := append([]providers.Message(nil), ts.restorePointHistory...)
	summary := ts.restorePointSummary
	ts.mu.RUnlock()

	agent.Sessions.SetHistory(ts.sessionKey, history)
	agent.Sessions.SetSummary(ts.sessionKey, summary)
	return agent.Sessions.Save(ts.sessionKey)
}

func matchingTurnMessageTail(history, persisted []providers.Message) int {
	maxMatch := min(len(history), len(persisted))
	for size := maxMatch; size > 0; size-- {
		if messageSlicesEquivalent(history[len(history)-size:], persisted[len(persisted)-size:]) {
			return size
		}
	}
	return 0
}

func splitHistoryForActiveTurn(
	history []providers.Message,
	persisted []providers.Message,
) ([]providers.Message, []providers.Message) {
	matched := matchingTurnMessageTail(history, persisted)
	if matched <= 0 {
		return append([]providers.Message(nil), history...), nil
	}

	stable := append([]providers.Message(nil), history[:len(history)-matched]...)
	protected := append([]providers.Message(nil), history[len(history)-matched:]...)
	return stable, protected
}

func messageSlicesEquivalent(a, b []providers.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !messagesEquivalent(a[i], b[i]) {
			return false
		}
	}
	return true
}

func messagesEquivalent(a, b providers.Message) bool {
	return reflect.DeepEqual(normalizeMessageForComparison(a), normalizeMessageForComparison(b))
}

func normalizeMessageForComparison(msg providers.Message) providers.Message {
	msg.PromptLayer = ""
	msg.PromptSlot = ""
	msg.PromptSource = ""

	if len(msg.Media) == 0 {
		msg.Media = nil
	}
	if len(msg.Attachments) == 0 {
		msg.Attachments = nil
	}
	if len(msg.SystemParts) == 0 {
		msg.SystemParts = nil
	} else {
		msg.SystemParts = append([]providers.ContentBlock(nil), msg.SystemParts...)
		for i := range msg.SystemParts {
			msg.SystemParts[i].PromptLayer = ""
			msg.SystemParts[i].PromptSlot = ""
			msg.SystemParts[i].PromptSource = ""
		}
	}
	if len(msg.ToolCalls) == 0 {
		msg.ToolCalls = nil
	} else {
		msg.ToolCalls = append([]providers.ToolCall(nil), msg.ToolCalls...)
		for i := range msg.ToolCalls {
			msg.ToolCalls[i].Name = ""
			msg.ToolCalls[i].Arguments = nil
			msg.ToolCalls[i].ThoughtSignature = ""
			if msg.ToolCalls[i].Function != nil {
				fn := *msg.ToolCalls[i].Function
				fn.ThoughtSignature = ""
				msg.ToolCalls[i].Function = &fn
			}
		}
	}

	return msg
}

func (ts *turnState) interruptHintMessage() providers.Message {
	_, hint := ts.gracefulInterruptRequested()
	content := "Interrupt requested. Stop scheduling tools and provide a short final summary."
	if hint != "" {
		content += "\n\nInterrupt hint: " + hint
	}
	return interruptPromptMessage(content)
}

func (ts *turnState) toolLimitHintMessage() providers.Message {
	content := "SYSTEM DIRECTIVE: Tool call limit reached. CEASE ALL TOOL CALLS IMMEDIATELY. " +
		"YOU MUST NOW PROVIDE A FINAL STATUS REPORT ON THE ASSIGNED MISSION, SUMMARIZING COMPLETED ACTIONS AND OUTLINING THE REMAINING STEPS TO COMPLETION."
	return providers.Message{
		Role:    "user",
		Content: content,
	}
}

// recoveryHintMessage returns a provider.Message that injects the
// pendingRecoveryMessage (if any) into the next LLM call, then clears
// the field so subsequent calls in the same iteration don't repeat it.
//
// This is wired in pipeline_llm.go immediately after the interrupt/tool-limit
// hint blocks. Without this consumer (Phase 11.1 fix), pendingRecoveryMessage
// was set by recovery_goal.go (empty response, text-only streak, tool exec
// error) but never reached the LLM context — counters still bumped, retries
// still fired, but the LLM saw the same messages without guidance.
func (ts *turnState) recoveryHintMessage() providers.Message {
	content := ts.pendingRecoveryMessage
	ts.pendingRecoveryMessage = ""
	if strings.TrimSpace(content) == "" {
		// Return a zero-value message; caller should check Content == ""
		// before appending to avoid empty user-role messages.
		return providers.Message{}
	}
	return recoveryPromptMessage(content)
}

// =============================================================================
// SubTurn-related methods
// =============================================================================

// Finish marks the turn as finished and closes the pendingResults channel
func (ts *turnState) Finish(isHardAbort bool) {
	ts.isFinished.Store(true)

	// Close pendingResults channel exactly once
	ts.closeOnce.Do(func() {
		if ts.pendingResults != nil {
			close(ts.pendingResults)
		}
		ts.mu.Lock()
		if ts.finishedChan == nil {
			ts.finishedChan = make(chan struct{})
		}
		close(ts.finishedChan)
		ts.mu.Unlock()
	})

	// Any graceful finish must signal direct children so nested SubTurns can
	// observe parent completion and decide whether to stop or continue.
	if !isHardAbort {
		ts.parentEnded.Store(true)
	}

	// Cancel the turn context
	if ts.cancelFunc != nil {
		ts.cancelFunc()
	}

	// Hard abort cascades to all child turns
	if isHardAbort && ts.al != nil {
		ts.mu.RLock()
		children := append([]string(nil), ts.childTurnIDs...)
		ts.mu.RUnlock()
		for _, childID := range children {
			if val, ok := ts.al.activeTurnStates.Load(childID); ok {
				if child, ok := val.(*turnState); ok {
					child.Finish(true)
				}
			}
		}
	}
}

// Finished returns whether the turn has finished
func (ts *turnState) Finished() chan struct{} {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.finishedChan == nil {
		ts.finishedChan = make(chan struct{})
	}
	return ts.finishedChan
}

// IsParentEnded checks if the parent turn has ended
func (ts *turnState) IsParentEnded() bool {
	if ts.parentTurnState == nil {
		return false
	}
	return ts.parentTurnState.parentEnded.Load()
}

// GetLastFinishReason returns the last LLM finish_reason
func (ts *turnState) GetLastFinishReason() string {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.lastFinishReason
}

// SetLastFinishReason sets the last LLM finish_reason
func (ts *turnState) SetLastFinishReason(reason string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastFinishReason = reason
}

// GetLastUsage returns the last LLM usage info
func (ts *turnState) GetLastUsage() *providers.UsageInfo {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.lastUsage
}

// SetLastUsage sets the last LLM usage info
func (ts *turnState) SetLastUsage(usage *providers.UsageInfo) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastUsage = usage
}

// =============================================================================
// Context helper functions for turnState
// =============================================================================

type turnStateKeyType struct{}

var turnStateKey = turnStateKeyType{}

func withTurnState(ctx context.Context, ts *turnState) context.Context {
	return context.WithValue(ctx, turnStateKey, ts)
}

func turnStateFromContext(ctx context.Context) *turnState {
	ts, _ := ctx.Value(turnStateKey).(*turnState)
	return ts
}

// TurnStateFromContext retrieves turnState from context (exported for tools)
func TurnStateFromContext(ctx context.Context) *turnState {
	return turnStateFromContext(ctx)
}

// AsExtender returns the turnState wrapped as goal.IterationExtender so
// packages that cannot import the private turnState type (e.g. pkg/agent/goal)
// can still call the extension methods. The returned interface is declared
// in pkg/agent/goal (the sole consumer) to avoid an import cycle.
func (ts *turnState) AsExtender() goal.IterationExtender {
	return ts
}

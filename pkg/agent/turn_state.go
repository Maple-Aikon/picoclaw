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

	// toolCallSeq — monotonic per-turn counter cho layer-1 id allocation
	// (Phase 12.61). In-memory per turn; nguồn uniqueness THẬT của id mới
	// `call_<turnID>_<unixNano>_<seq>` (unixNano chỉ là namespace cross-restart).
	toolCallSeq int64
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

	postCompleteGoalReportSent bool // Phase 12.7: emit the final-report hint once after complete_goal; resets to true after the post-final-report iter runs.
	pendingFinalReportIter     bool // Phase 12.9: transient signal set at top of body if this iter is the post-complete_goal final-report iter; consumed + cleared at end of body.

	// Phase 12.35: goal_progress defers iterationCap extension to end of iter
	// (after ExecuteTools returns, before continue). Setting the flag here
	// keeps phase stable within the iter — iterationCap does NOT bump until
	// the post-iter hook fires, so phase resolvers reading `iter >= iterCap`
	// continue to see CHECKPOINT for the rest of the iter.
	willExtendIterCap       bool   // true if a goal_progress call set this during ExecuteTools
	willExtendIterCapAmount int    // amount to extend (0 = use MaxIterationsPerCheckpoint default)
	willExtendIterCapReason string // audit reason for the deferred extend

	// Phase 12.35: when ExecuteTools pre-checks IsAllowed and finds the tool
	// is blocked by the phase/lifecycle gate, it sets this on the blocked
	// tool result and skips ExecuteWithContext. The post-ExecuteTools recovery
	// path then routes to retryLLMForBlockedTool (Phase 12.22) instead of the
	// tool-exec-error recovery path (Phase 12.28) — wire must distinguish
	// BLOCKED-by-gate from EXEC-failed.

	// Phase 12.33: tracks the GoalPhase value that was current when
	// messages[0] (system prompt) was last built. Used by
	// maybeRebuildPromptForPhaseChange to detect when the goal phase
	// changed between iterations (e.g., Open → Checkpoint at iter=MaxIter)
	// and force a system prompt rebuild so the LLM sees the phase-
	// appropriate hint text instead of the stale iter-0 prompt.
	// Empty string = no rebuild has happened yet (initial iter).
	lastBuiltPromptPhase string

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
	emptyResponseRecoveryCount int    // Phase 12.37: count of same-iter empty-response recovery injections (was bool one-shot; now 3-shot cap per spec 9)
	toolExecRecoveryAttempts map[string]int // per-tool execution error retry count (not signature). Same iteration.

	// Phase 12: per-iteration escalation counters for text-only recovery.
	// Soft retry fires first (gentle prompt), hard retry fires second (firm
	// prompt); if LLM still produces text-only after hard, archive the goal.
	// Both reset at iteration boundary (when ts.iteration bumps). Streak
	// is left intact across same-iteration retries because textOnlyStreak
	// tracks cross-iteration cadence — these counters track within-iteration.
	textOnlySoftRetriesDone int // how many soft prompts fired in current iteration
	textOnlyHardRetriesDone int // how many hard prompts fired in current iteration

	// Phase 12.13: per-iteration phase-stuck counters. Reset at iteration
	// boundary. Each counter tracks consecutive same-iteration failures of
	// the phase-specific lifecycle tool. When the counter reaches the
	// stuck threshold (>= 2) and the goal is archived, the abort_reason
	// is set to the phase-specific value (goal_set_stuck,
	// goal_checkpoint_stuck, goal_final_stuck) so applyFallbackForEmptyResponse
	// can return a user-facing message that names the phase.
	setGoalAttemptCount         int  // consecutive set_goal invalid arguments in Set phase
	goalProgressAttemptCount    int  // consecutive goal_progress invalid arguments in Checkpoint phase
	completeGoalAttemptCount    int  // consecutive complete_goal invalid arguments in Final phase
	// Phase 12.52a (R10 F01 split): archive flags separate the "was the goal
	// archived at this phase" signal (bool) from the real per-failure attempt
	// count above. Before the split, the counters were dual-purpose: the
	// archive path ratcheted them to 2 so the abort-reason threshold fired,
	// which inflated the user-facing "failed N times" display. Now the flag
	// carries the archive signal and the count stays honest.
	setGoalArchiveFlag          bool
	goalProgressArchiveFlag     bool
	completeGoalArchiveFlag     bool

	// lastPhaseStuckError (Phase 12.13) — last error message from the
	// phase-specific lifecycle tool. Set whenever a phase-stuck counter
	// increments. Used by phaseStuckFallbackMessage to format the
	// user-facing error message with the actual failure cause.
	lastPhaseStuckError      string

	// Goal-lifecycle recovery side-effects (Phase 5): set by applyRecoveryAction.
	// Consumed at the start of the next iteration (ControlContinue path) to
	// inject the recovery message into the conversation, strip non-goal tools,
	// or finalize the goal. See plan §5.2 + §8.3.
	pendingRecoveryMessage  string // message to inject before next LLM call (empty = no injection)
	goalArchiveRequested    bool   // if true, caller must call finalizeGoalOnTurnEnd (Phase 6 hook)

	// Phase 12.28.3: last tool execution result, populated at pipeline_execute.go
	// tool-exec-end (when `hookResult *tools.ToolResult` is in scope). Read by
	// checkToolExecErrorRecovery to determine ErrKind-based recovery gate. nil
	// means no tool has executed yet this iteration OR a legacy test fixture
	// that doesn't populate this field — recovery falls back to prefix heuristic
	// in those cases. Always reset at iter boundary via the per-iter reset block
	// at turn_coord.go:163-164 (alongside the other recovery counters).
	lastToolResult *tools.ToolResult

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

// RequestExtendIterationCap is the deferred-extend counterpart of
// ExtendIterationCap. It stages a request and the agent loop applies it at
// end of iter via FlushPendingExtend. The cap is NOT bumped immediately, so
// phase resolvers reading `iter >= iterationCap` see the same value for the
// rest of the iter (Phase 12.35 fix: phase no longer flips mid-iter when
// goal_progress succeeds at CHECKPOINT).
//
// Returns true if the request was accepted. A request is rejected if there
// is no room to extend (cap is at the absolute ceiling); the caller
// (goal_progress) handles rejection by not showing a success message.
func (ts *turnState) RequestExtendIterationCap(n int, reason string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	// Reject if at ceiling (same edge case ExtendIterationCap clamps to).
	if ts.maxIterationsCap > 0 && ts.iterationCap >= ts.maxIterationsCap {
		return false
	}
	// Reject if n <= 0 (defensive — goal_progress should never pass 0/negative).
	if n <= 0 {
		return false
	}
	ts.willExtendIterCap = true
	if ts.willExtendIterCapAmount == 0 {
		ts.willExtendIterCapAmount = n
	} else {
		// Sum multiple requests within the same iter (defensive).
		ts.willExtendIterCapAmount += n
	}
	if reason != "" {
		ts.willExtendIterCapReason = reason
	}
	return true
}

// FlushPendingExtend applies any staged RequestExtendIterationCap request.
// Returns applied=true with the new cap and delta if a request was staged.
// Called by the agent loop at end of body, after ExecuteTools returns and
// before the loop top-of-body check re-evaluates `iter < iterationCap`.
//
// Phase 12.35: this is the only place iterationCap actually bumps for a
// goal_progress self-extend; the synchronous ExtendIterationCap call from
// goal_progress tool handler was removed to keep phase stable within iter.
func (ts *turnState) FlushPendingExtend() (bool, int, int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if !ts.willExtendIterCap {
		return false, ts.iterationCap, 0
	}
	// Apply the staged amount. Clamp to ceiling the same way ExtendIterationCap
	// does (defensive: someone might have set the ceiling lower since).
	amount := ts.willExtendIterCapAmount
	reason := ts.willExtendIterCapReason
	// Reset BEFORE calling, so any re-entrant RequestExtend sees a clean state.
	ts.willExtendIterCap = false
	ts.willExtendIterCapAmount = 0
	ts.willExtendIterCapReason = ""

	if amount <= 0 {
		return false, ts.iterationCap, 0
	}
	// Reuse the existing logic by calling ExtendIterationCap under the held
	// lock. We can't simply call the public method because it also takes
	// ts.mu (re-entrant deadlock on sync.Mutex). Inline the clamp here.
	oldCap := ts.iterationCap
	if ts.maxIterationsCap <= 0 {
		ts.iterationCap = oldCap + amount
	} else {
		proposed := oldCap + amount
		if proposed > ts.maxIterationsCap {
			ts.iterationCap = ts.maxIterationsCap
		} else {
			ts.iterationCap = proposed
		}
	}
	delta := ts.iterationCap - oldCap
	ts.lastExtensionReason = reason
	ts.lastExtensionAtIter = ts.iteration
	return true, ts.iterationCap, delta
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

// PublishToUser implements goal.TurnStateAccess (Phase 12.44): publishes
// text directly to the user's channel. Void + best-effort (F14 fold) —
// matches PublishResponseIfNeeded convention: the system has NO detectable
// fail path for outbound publish (no outbox, no ack). Retry publish does
// not exist by design; the final-report iter is the safety net.
//
// Audit note (2026-08-03): ts.al is set for SubTurns at subturn.go:412 AND
// for main turns at runTurn (turn_coord.go) — the plan §4 assumption
// "al field có sẵn" was only half-true (SubTurn-only before Phase 12.44).
func (ts *turnState) PublishToUser(ctx context.Context, text string) {
	if ts == nil || text == "" {
		return
	}
	if ts.al == nil {
		// No AgentLoop back-ref (e.g. unit fixtures) — cannot publish.
		return
	}
	if ts.channel == "" || ts.chatID == "" {
		// CLI/test runs have no channel — skip (T5 contract).
		return
	}
	ts.al.PublishResponseIfNeeded(ctx, ts.channel, ts.chatID, ts.sessionKey, text)
}

// PublishGoalSummary implements goal.TurnStateAccess (Phase 12.55.2):
// publishes the complete_goal outcome as a user-facing message with the
// iteration/phase header + summary (header + ": summary"). The LLM's
// content text (explanation) is intentionally NOT published here — the
// tool-feedback explanation path (Phase 12.58.3) already delivers it.
// Void + best-effort, mirrors PublishToUser.
//
// Phase 12.55.3: phase is the execute-time snapshot passed by the caller
// (complete_goal reads it from ctx via goal.ToolCallPhaseFromContext).
// Never re-resolve via currentGoalPhase() here — by the time this runs
// the goal file has been archived, so hasGoal()=false → GoalPhaseSet.
// Empty phase falls back to live resolution (unit-fixture compat).
func (ts *turnState) PublishGoalSummary(ctx context.Context, phase, summary string) {
	if ts == nil {
		return
	}
	if ts.al == nil {
		return
	}
	if ts.channel == "" || ts.chatID == "" {
		return
	}
	if phase == "" {
		phase = string(ts.currentGoalPhase())
	}
	header := toolFeedbackIterContextFor(ts, GoalPhase(phase))
	ts.al.PublishResponseIfNeeded(ctx, ts.channel, ts.chatID, ts.sessionKey, header+": summary\n\n"+summary)
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

// shouldEmitPostCompleteGoalReport reports whether the post-complete_goal
// final-report iter should fire (Phase 12.7). True exactly once per turn:
// when complete_goal just ran and we have not already emitted the final-
// report hint. The caller (turn_coord.go) sets postCompleteGoalReportSent
// to true after the iter to suppress further fires.
//
// Owner decision (2026-07-24 08:50 ICT, anh Maple): always fire this iter
// after complete_goal, even if the LLM already emitted text in the same
// iteration. The LLM may add additional info or simply skip — either is
// fine. We give the LLM one last chance regardless.
func (ts *turnState) shouldEmitPostCompleteGoalReport() bool {
	if ts == nil {
		return false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.goalFinalized && !ts.postCompleteGoalReportSent
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

// toolLimitHintMessage removed in Phase 12.8 — the cap-hit case is now
// owned by the goal-phase machinery (Phase 11) and the Phase 12 text-only
// recovery chain. Keeping the constant string here for the historical
// record / grep trail:
//   "SYSTEM DIRECTIVE: Tool call limit reached. CEASE ALL TOOL CALLS
//    IMMEDIATELY. YOU MUST NOW PROVIDE A FINAL STATUS REPORT ON THE
//    ASSIGNED MISSION, SUMMARIZING COMPLETED ACTIONS AND OUTLINING THE
//    REMAINING STEPS TO COMPLETION."

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

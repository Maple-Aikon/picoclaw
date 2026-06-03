package agent

import (
	"time"

	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
)

// HookMeta contains correlation fields shared by agent hook requests and
// runtime events emitted from turn processing.
type HookMeta struct {
	AgentID      string
	TurnID       string
	ParentTurnID string
	SessionKey   string
	Iteration    int
	TracePath    string
	Source       string
	turnContext  *TurnContext
}

// EventKind is the legacy in-agent event kind alias kept for tests and
// compatibility shims on top of the runtime event bus.
type EventKind = runtimeevents.Kind

const (
	EventKindTurnStart              EventKind = runtimeevents.KindAgentTurnStart
	EventKindTurnEnd                EventKind = runtimeevents.KindAgentTurnEnd
	EventKindLLMRequest             EventKind = runtimeevents.KindAgentLLMRequest
	EventKindLLMDelta               EventKind = runtimeevents.KindAgentLLMDelta
	EventKindLLMResponse            EventKind = runtimeevents.KindAgentLLMResponse
	EventKindLLMRetry               EventKind = runtimeevents.KindAgentLLMRetry
	EventKindContextCompress        EventKind = runtimeevents.KindAgentContextCompress
	EventKindSessionSummarize       EventKind = runtimeevents.KindAgentSessionSummarize
	EventKindToolExecStart          EventKind = runtimeevents.KindAgentToolExecStart
	EventKindToolExecEnd            EventKind = runtimeevents.KindAgentToolExecEnd
	EventKindToolExecSkipped        EventKind = runtimeevents.KindAgentToolExecSkipped
	EventKindSteeringInjected       EventKind = runtimeevents.KindAgentSteeringInjected
	EventKindFollowUpQueued         EventKind = runtimeevents.KindAgentFollowUpQueued
	EventKindInterruptReceived      EventKind = runtimeevents.KindAgentInterruptReceived
	EventKindSubTurnSpawn           EventKind = runtimeevents.KindAgentSubTurnSpawn
	EventKindSubTurnEnd             EventKind = runtimeevents.KindAgentSubTurnEnd
	EventKindSubTurnResultDelivered EventKind = runtimeevents.KindAgentSubTurnResultDelivered
	EventKindSubTurnOrphan          EventKind = runtimeevents.KindAgentSubTurnOrphan
	EventKindError                  EventKind = runtimeevents.KindAgentError
)

// TurnStartPayload describes the start of a turn.
type TurnStartPayload struct {
	UserMessage string
	MediaCount  int
}

// TurnEndPayload describes the completion of a turn.
type TurnEndPayload struct {
	Status          TurnEndStatus
	Iterations      int
	Duration        time.Duration
	FinalContentLen int
}

// LLMRequestPayload describes an outbound LLM request.
type LLMRequestPayload struct {
	Model         string
	MessagesCount int
	ToolsCount    int
	MaxTokens     int
	Temperature   float64
}

// LLMResponsePayload describes an inbound LLM response.
type LLMResponsePayload struct {
	ContentLen   int
	ToolCalls    int
	HasReasoning bool
}

// LLMDeltaPayload describes a streamed LLM delta.
type LLMDeltaPayload struct {
	ContentDeltaLen   int
	ReasoningDeltaLen int
}

// LLMRetryPayload describes a retry of an LLM request.
type LLMRetryPayload struct {
	Attempt    int
	MaxRetries int
	Reason     string
	Error      string
	Backoff    time.Duration
}

// ContextCompressReason identifies why emergency compression ran.
type ContextCompressReason string

const (
	// ContextCompressReasonProactive indicates compression before the first LLM call.
	ContextCompressReasonProactive ContextCompressReason = "proactive_budget"
	// ContextCompressReasonRetry indicates compression during context-error retry handling.
	ContextCompressReasonRetry ContextCompressReason = "llm_retry"
	// ContextCompressReasonSummarize indicates post-turn async summarization.
	ContextCompressReasonSummarize ContextCompressReason = "summarize"
)

// CompactHookRequest is sent to hook.before_compact to notify hooks
// that context compaction is about to occur. The hook receives the
// session key, compaction reason, and context budget for observability.
type CompactHookRequest struct {
	SessionKey string                `json:"session_key"`
	Reason     ContextCompressReason `json:"reason"`
	Budget     int                   `json:"budget"`
}

// Clone returns a shallow copy of the CompactHookRequest.
func (r *CompactHookRequest) Clone() *CompactHookRequest {
	if r == nil {
		return nil
	}
	return &CompactHookRequest{
		SessionKey: r.SessionKey,
		Reason:     r.Reason,
		Budget:     r.Budget,
	}
}

// ContextCompressPayload describes a forced history compression.
type ContextCompressPayload struct {
	Reason            ContextCompressReason
	DroppedMessages   int
	RemainingMessages int
}

// SessionSummarizePayload describes a completed async session summarization.
type SessionSummarizePayload struct {
	SummarizedMessages int
	KeptMessages       int
	SummaryLen         int
	OmittedOversized   bool
}

// ToolExecStartPayload describes a tool execution request.
type ToolExecStartPayload struct {
	Tool      string
	Arguments map[string]any
}

// ToolExecEndPayload describes the outcome of a tool execution.
type ToolExecEndPayload struct {
	Tool       string
	Duration   time.Duration
	ForLLMLen  int
	ForUserLen int
	IsError    bool
	Async      bool
}

// ToolExecSkippedPayload describes a skipped tool call.
type ToolExecSkippedPayload struct {
	Tool   string
	Reason string
}

// SteeringInjectedPayload describes steering messages appended before the next LLM call.
type SteeringInjectedPayload struct {
	Count           int
	TotalContentLen int
}

// FollowUpQueuedPayload describes an async follow-up queued back into the inbound bus.
type FollowUpQueuedPayload struct {
	SourceTool string
	ContentLen int
}

type InterruptKind string

const (
	InterruptKindSteering InterruptKind = "steering"
	InterruptKindGraceful InterruptKind = "graceful"
	InterruptKindHard     InterruptKind = "hard_abort"
)

// InterruptReceivedPayload describes accepted turn-control input.
type InterruptReceivedPayload struct {
	Kind       InterruptKind
	Role       string
	ContentLen int
	QueueDepth int
	HintLen    int
}

// SubTurnSpawnPayload describes the creation of a child turn.
type SubTurnSpawnPayload struct {
	AgentID      string
	Label        string
	ParentTurnID string
}

// SubTurnEndPayload describes the completion of a child turn.
type SubTurnEndPayload struct {
	AgentID string
	Status  string
}

// SubTurnResultDeliveredPayload describes delivery of a sub-turn result.
type SubTurnResultDeliveredPayload struct {
	TargetChannel string
	TargetChatID  string
	ContentLen    int
}

// SubTurnOrphanPayload describes a sub-turn result that could not be delivered.
type SubTurnOrphanPayload struct {
	ParentTurnID string
	ChildTurnID  string
	Reason       string
}

// ErrorPayload describes an execution error inside the agent loop.
type ErrorPayload struct {
	Stage   string
	Message string
}

// EventMeta is the legacy name for hook metadata.
type EventMeta = HookMeta

// Event is the legacy agent event envelope exposed by SubscribeEvents and a
// handful of tests. Runtime code publishes pkg/events.Event internally.
type Event struct {
	Kind    EventKind
	Time    time.Time
	Meta    EventMeta
	Context *TurnContext
	Payload any
}

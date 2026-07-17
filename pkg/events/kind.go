package events

const (
	// KindAgentTurnStart is emitted when an agent turn starts.
	KindAgentTurnStart Kind = "agent.turn.start"
	// KindAgentTurnEnd is emitted when an agent turn ends.
	KindAgentTurnEnd Kind = "agent.turn.end"

	// KindAgentLLMRequest is emitted before an LLM request.
	KindAgentLLMRequest Kind = "agent.llm.request"
	// KindAgentLLMDelta is emitted for streaming LLM deltas.
	KindAgentLLMDelta Kind = "agent.llm.delta"
	// KindAgentLLMResponse is emitted after an LLM response.
	KindAgentLLMResponse Kind = "agent.llm.response"
	// KindAgentLLMRetry is emitted before retrying an LLM request.
	KindAgentLLMRetry Kind = "agent.llm.retry"
	// KindAgentLLMReplayAttempt is emitted before retrying an LLM call due to
	// an AfterLLM hook requesting HookActionReplay (Step 2 auto-recovery).
	// Distinct from KindAgentLLMRetry which is for transient network/HTTP errors.
	KindAgentLLMReplayAttempt Kind = "agent.llm.replay.attempt"
	// KindAgentLLMReplayExhausted is emitted when the per-iteration replay
	// budget (MaxReplayAttempts) is hit. Subsequent iterations may retry.
	KindAgentLLMReplayExhausted Kind = "agent.llm.replay.exhausted"

	// KindAgentContextCompress is emitted when agent context is compressed.
	KindAgentContextCompress Kind = "agent.context.compress"
	// KindAgentSessionSummarize is emitted when session summarization completes.
	KindAgentSessionSummarize Kind = "agent.session.summarize"

	// KindAgentToolExecStart is emitted before a tool executes.
	KindAgentToolExecStart Kind = "agent.tool.exec_start"
	// KindAgentToolExecEnd is emitted after a tool finishes.
	KindAgentToolExecEnd Kind = "agent.tool.exec_end"
	// KindAgentToolExecSkipped is emitted when a tool call is skipped.
	KindAgentToolExecSkipped Kind = "agent.tool.exec_skipped"

	// KindAgentSteeringInjected is emitted when steering is injected into context.
	KindAgentSteeringInjected Kind = "agent.steering.injected"
	// KindAgentFollowUpQueued is emitted when async follow-up input is queued.
	KindAgentFollowUpQueued Kind = "agent.follow_up.queued"
	// KindAgentInterruptReceived is emitted when a turn interrupt is accepted.
	KindAgentInterruptReceived Kind = "agent.interrupt.received"

	// KindAgentSubTurnSpawn is emitted when a sub-turn is spawned.
	KindAgentSubTurnSpawn Kind = "agent.subturn.spawn"
	// KindAgentSubTurnEnd is emitted when a sub-turn ends.
	KindAgentSubTurnEnd Kind = "agent.subturn.end"
	// KindAgentSubTurnResultDelivered is emitted when a sub-turn result is delivered.
	KindAgentSubTurnResultDelivered Kind = "agent.subturn.result_delivered"
	// KindAgentSubTurnOrphan is emitted when a sub-turn result cannot be delivered.
	KindAgentSubTurnOrphan Kind = "agent.subturn.orphan"
	// KindAgentError is emitted when agent execution reports an error.
	KindAgentError Kind = "agent.error"

	// KindChannelLifecycleStarted is emitted when a channel starts.
	KindChannelLifecycleStarted Kind = "channel.lifecycle.started"
	// KindChannelLifecycleInitialized is emitted when a channel is initialized.
	KindChannelLifecycleInitialized Kind = "channel.lifecycle.initialized"
	// KindChannelLifecycleStartFailed is emitted when a channel fails to start.
	KindChannelLifecycleStartFailed Kind = "channel.lifecycle.start_failed"
	// KindChannelLifecycleStopped is emitted when a channel stops.
	KindChannelLifecycleStopped Kind = "channel.lifecycle.stopped"
	// KindChannelWebhookRegistered is emitted when a channel webhook is registered.
	KindChannelWebhookRegistered Kind = "channel.webhook.registered"
	// KindChannelWebhookUnregistered is emitted when a channel webhook is unregistered.
	KindChannelWebhookUnregistered Kind = "channel.webhook.unregistered"
	// KindChannelMessageOutboundQueued is emitted when an outbound message is queued.
	KindChannelMessageOutboundQueued Kind = "channel.message.outbound_queued"
	// KindChannelMessageOutboundSent is emitted when an outbound channel message is sent.
	KindChannelMessageOutboundSent Kind = "channel.message.outbound_sent"
	// KindChannelMessageOutboundFailed is emitted when an outbound channel message fails.
	KindChannelMessageOutboundFailed Kind = "channel.message.outbound_failed"
	// KindChannelRateLimited is emitted when channel rate limiting blocks delivery.
	KindChannelRateLimited Kind = "channel.rate_limited"

	// KindBusPublishFailed is emitted when message bus publish fails.
	KindBusPublishFailed Kind = "bus.publish.failed"
	// KindBusMessageDropped is emitted when a message is dropped due to
	// backpressure (channel full for longer than the drop budget).
	KindBusMessageDropped Kind = "bus.message.dropped"
	// KindBusCloseStarted is emitted when message bus close starts.
	KindBusCloseStarted Kind = "bus.close.started"
	// KindBusCloseCompleted is emitted when message bus close completes.
	KindBusCloseCompleted Kind = "bus.close.completed"
	// KindBusCloseDrained is emitted when message bus close drains buffered messages.
	KindBusCloseDrained Kind = "bus.close.drained"

	// KindGatewayStart is emitted when gateway startup reaches runtime bootstrap.
	KindGatewayStart Kind = "gateway.start"
	// KindGatewayReady is emitted when gateway services are started and ready.
	KindGatewayReady Kind = "gateway.ready"
	// KindGatewayShutdown is emitted when gateway shutdown starts.
	KindGatewayShutdown Kind = "gateway.shutdown"
	// KindGatewayReloadStarted is emitted when gateway reload starts.
	KindGatewayReloadStarted Kind = "gateway.reload.started"
	// KindGatewayReloadCompleted is emitted when gateway reload completes.
	KindGatewayReloadCompleted Kind = "gateway.reload.completed"
	// KindGatewayReloadFailed is emitted when gateway reload fails.
	KindGatewayReloadFailed Kind = "gateway.reload.failed"

	// KindMCPServerConnected is emitted when an MCP server connects.
	KindMCPServerConnected Kind = "mcp.server.connected"
	// KindMCPServerConnecting is emitted before connecting to an MCP server.
	KindMCPServerConnecting Kind = "mcp.server.connecting"
	// KindMCPServerFailed is emitted when an MCP server fails.
	KindMCPServerFailed Kind = "mcp.server.failed"
	// KindMCPToolDiscovered is emitted when an MCP tool is discovered.
	KindMCPToolDiscovered Kind = "mcp.tool.discovered"
	// KindMCPToolCallStart is emitted when an MCP tool call starts.
	KindMCPToolCallStart Kind = "mcp.tool.call.start"
	// KindMCPToolCallEnd is emitted when an MCP tool call ends.
	KindMCPToolCallEnd Kind = "mcp.tool.call.end"

	// KindAgentToolBreakerTripped is emitted exactly once when a tool's
	// circuit breaker transitions from Closed/HalfOpen → Open. The registry
	// guarantees idempotency via ToolFeedback.JustTripped. The kind sits in
	// the agent namespace because circuit-breaker state is a per-agent-loop
	// concern; the breaker primitive itself lives in pkg/tools and stays
	// event-bus agnostic.
	//
	// Plan: circuit-breaker-3-tier-errkind-semantics-toolfeedback-20260717
	KindAgentToolBreakerTripped Kind = "agent.tool.breaker_tripped"
	// KindAgentToolBreakerRecovered is emitted when a circuit breaker
	// transitions Open → Closed (via TryRecover probe success). Optional;
	// emitted only when the registry actively probes recovery — silent
	// recoveries (e.g. Allow() flipping after recoveryTimeout elapses) do
	// NOT emit.
	KindAgentToolBreakerRecovered Kind = "agent.tool.breaker_recovered"
)

var knownKinds = []Kind{
	KindAgentTurnStart,
	KindAgentTurnEnd,
	KindAgentLLMRequest,
	KindAgentLLMDelta,
	KindAgentLLMResponse,
	KindAgentLLMRetry,
	KindAgentContextCompress,
	KindAgentSessionSummarize,
	KindAgentToolExecStart,
	KindAgentToolExecEnd,
	KindAgentToolExecSkipped,
	KindAgentSteeringInjected,
	KindAgentFollowUpQueued,
	KindAgentInterruptReceived,
	KindAgentSubTurnSpawn,
	KindAgentSubTurnEnd,
	KindAgentSubTurnResultDelivered,
	KindAgentSubTurnOrphan,
	KindAgentError,
	KindChannelLifecycleStarted,
	KindChannelLifecycleInitialized,
	KindChannelLifecycleStartFailed,
	KindChannelLifecycleStopped,
	KindChannelWebhookRegistered,
	KindChannelWebhookUnregistered,
	KindChannelMessageOutboundQueued,
	KindChannelMessageOutboundSent,
	KindChannelMessageOutboundFailed,
	KindChannelRateLimited,
	KindBusPublishFailed,
	KindBusMessageDropped,
	KindBusCloseStarted,
	KindBusCloseCompleted,
	KindBusCloseDrained,
	KindGatewayStart,
	KindGatewayReady,
	KindGatewayShutdown,
	KindGatewayReloadStarted,
	KindGatewayReloadCompleted,
	KindGatewayReloadFailed,
	KindMCPServerConnected,
	KindMCPServerConnecting,
	KindMCPServerFailed,
	KindMCPToolDiscovered,
	KindMCPToolCallStart,
	KindMCPToolCallEnd,
	KindAgentLLMReplayAttempt,
	KindAgentLLMReplayExhausted,
	KindAgentToolBreakerTripped,
	KindAgentToolBreakerRecovered,
}

// KnownKinds returns the runtime event kinds declared by this package.
func KnownKinds() []Kind {
	return append([]Kind(nil), knownKinds...)
}

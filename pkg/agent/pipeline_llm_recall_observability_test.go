// Package agent — Phase 12.45 observability tests for RecallLLM.
//
// RecallLLM is the single choke point for recovery/retry LLM calls
// (handleGoalRecovery + retryExecuteToolChain). It used to be fully
// invisible in gateway.log (no runtime events) and prompt_history.log
// (no hooks run for recall calls). These tests verify it emits runtime
// events with tracePath "turn.llm.replay.recall" — distinct from the
// main path "turn.llm.request"/"turn.llm.response" — plus drop-warning
// on subscriber-full (A-F05/B-F05).
package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// drainEventsAfter collects events until the channel goes quiet (300ms).
// Safe after synchronous work: PublishNonBlocking enqueues into the
// subscriber buffer before returning.
func drainEventsAfter(ch <-chan runtimeevents.Event) []runtimeevents.Event {
	var out []runtimeevents.Event
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, evt)
		case <-time.After(300 * time.Millisecond):
			return out
		}
	}
}

func mustReplayRequestPayload(t *testing.T, evt runtimeevents.Event) LLMRequestPayload {
	t.Helper()
	p, ok := evt.Payload.(LLMRequestPayload)
	if !ok {
		t.Fatalf("expected LLMRequestPayload, got %T", evt.Payload)
	}
	return p
}

func mustReplayResponsePayload(t *testing.T, evt runtimeevents.Event) LLMResponsePayload {
	t.Helper()
	p, ok := evt.Payload.(LLMResponsePayload)
	if !ok {
		t.Fatalf("expected LLMResponsePayload, got %T", evt.Payload)
	}
	return p
}

func mustReplayRetryPayload(t *testing.T, evt runtimeevents.Event) LLMRetryPayload {
	t.Helper()
	p, ok := evt.Payload.(LLMRetryPayload)
	if !ok {
		t.Fatalf("expected LLMRetryPayload, got %T", evt.Payload)
	}
	return p
}

// =============================================================================
// T1 — request event: kind, payload, tracePath
func TestRecallLLM_EmitsRequestEvent(t *testing.T) {
	provider := &recallTestProvider{
		responses: []*providers.LLMResponse{{Content: "ok", FinishReason: "stop"}},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)
	ts.setIteration(3) // eventMeta reads iteration from ts state (main-path contract)

	ch, closeFn := subscribeRuntimeEventsForTest(t, al, 16, runtimeevents.KindAgentLLMRequest)
	defer closeFn()

	_, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 3, "test_helper", nil)
	if err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}

	events := drainEventsAfter(ch)
	if len(events) != 1 {
		t.Fatalf("expected 1 request event, got %d", len(events))
	}
	evt := events[0]
	if evt.Kind != runtimeevents.KindAgentLLMRequest {
		t.Fatalf("expected KindAgentLLMRequest, got %s", evt.Kind)
	}
	if got := evt.Correlation.TraceID; got != replayTracePath {
		t.Fatalf("expected TraceID %q, got %q", replayTracePath, got)
	}
	if attrs, ok := evt.Attrs["agent_source"]; !ok || attrs != "runTurn" {
		t.Fatalf("expected agent_source=runTurn attr, got %v", evt.Attrs)
	}
	if attrs, ok := evt.Attrs["iteration"]; !ok || attrs != 3 {
		t.Fatalf("expected iteration=3 attr, got %v", evt.Attrs)
	}

	p := mustReplayRequestPayload(t, evt)
	if p.Model != exec.llmModel {
		t.Errorf("expected Model %q, got %q", exec.llmModel, p.Model)
	}
	if p.MessagesCount != len(exec.callMessages) {
		t.Errorf("expected MessagesCount %d, got %d", len(exec.callMessages), p.MessagesCount)
	}
	if p.ToolsCount != len(exec.providerToolDefs) {
		t.Errorf("expected ToolsCount %d, got %d", len(exec.providerToolDefs), p.ToolsCount)
	}
	if p.MaxTokens != ts.agent.MaxTokens {
		t.Errorf("expected MaxTokens %d, got %d", ts.agent.MaxTokens, p.MaxTokens)
	}
	if p.Temperature != ts.agent.Temperature {
		t.Errorf("expected Temperature %v, got %v", ts.agent.Temperature, p.Temperature)
	}
}

// =============================================================================
// T2 — response event: payload matches real response (emit at callErr==nil)
func TestRecallLLM_EmitsResponseEvent(t *testing.T) {
	resp := &providers.LLMResponse{
		Content:    "hello world",
		FinishReason: "stop",
		ToolCalls: []providers.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "/tmp/x"}},
		},
	}
	provider := &recallTestProvider{responses: []*providers.LLMResponse{resp}}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	ch, closeFn := subscribeRuntimeEventsForTest(t, al, 16, runtimeevents.KindAgentLLMResponse)
	defer closeFn()

	got, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "test_helper", nil)
	if err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}

	events := drainEventsAfter(ch)
	if len(events) != 1 {
		t.Fatalf("expected 1 response event, got %d", len(events))
	}
	evt := events[0]
	if got := evt.Correlation.TraceID; got != replayTracePath {
		t.Fatalf("expected TraceID %q, got %q", replayTracePath, got)
	}
	p := mustReplayResponsePayload(t, evt)
	if p.ContentLen != len(got.Content) {
		t.Errorf("expected ContentLen %d, got %d", len(got.Content), p.ContentLen)
	}
	if p.ToolCalls != 1 {
		t.Errorf("expected ToolCalls 1, got %d", p.ToolCalls)
	}
	if p.HasReasoning {
		t.Error("expected HasReasoning false for plain response")
	}
}

func TestRecallLLM_EmitsResponseEvent_HasReasoning(t *testing.T) {
	provider := &recallTestProvider{
		responses: []*providers.LLMResponse{{Content: "x", ReasoningContent: "thinking...", FinishReason: "stop"}},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	ch, closeFn := subscribeRuntimeEventsForTest(t, al, 16, runtimeevents.KindAgentLLMResponse)
	defer closeFn()

	if _, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "test_helper", nil); err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}

	events := drainEventsAfter(ch)
	if len(events) != 1 {
		t.Fatalf("expected 1 response event, got %d", len(events))
	}
	if !mustReplayResponsePayload(t, events[0]).HasReasoning {
		t.Error("expected HasReasoning true when ReasoningContent present")
	}
}

// =============================================================================
// T3 — transient retries: ordered request→retry→request→retry→request→response
func TestRecallLLM_TransientRetries_EmitsRetryAndPerAttempt(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("connection refused: backend down"), // attempt 0: transient
			fmt.Errorf("timeout exceeded"),                 // attempt 1: transient
			nil,                                             // attempt 2: success
		},
		responses: []*providers.LLMResponse{
			nil, nil,
			{Content: "success after retries", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	ch, closeFn := subscribeRuntimeEventsForTest(t, al, 32,
		runtimeevents.KindAgentLLMRequest, runtimeevents.KindAgentLLMResponse, runtimeevents.KindAgentLLMRetry)
	defer closeFn()

	_, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "test_transient", nil)
	if err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}

	events := drainEventsAfter(ch)

	// Expected order: req, retry, req, retry, req, resp
	var kinds []runtimeevents.Kind
	for _, evt := range events {
		kinds = append(kinds, evt.Kind)
	}
	want := []runtimeevents.Kind{
		runtimeevents.KindAgentLLMRequest,
		runtimeevents.KindAgentLLMRetry,
		runtimeevents.KindAgentLLMRequest,
		runtimeevents.KindAgentLLMRetry,
		runtimeevents.KindAgentLLMRequest,
		runtimeevents.KindAgentLLMResponse,
	}
	if len(kinds) != len(want) {
		t.Fatalf("expected %d events in order, got %d: %v", len(want), len(kinds), kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event %d: expected %s, got %s (full: %v)", i, want[i], kinds[i], kinds)
		}
	}

	// Retry payloads: Attempt 1 then 2, MaxRetries=3, Backoff=1s
	replayTrace := replayTracePath
	for _, evt := range events {
		if evt.Correlation.TraceID != replayTrace {
			t.Fatalf("expected TraceID %q on all replay events, got %q", replayTrace, evt.Correlation.TraceID)
		}
	}
	retries := []LLMRetryPayload{
		mustReplayRetryPayload(t, events[1]),
		mustReplayRetryPayload(t, events[3]),
	}
	if retries[0].Attempt != 1 || retries[1].Attempt != 2 {
		t.Errorf("expected Attempt 1,2 got %d,%d", retries[0].Attempt, retries[1].Attempt)
	}
	if retries[0].MaxRetries != 3 || retries[1].MaxRetries != 3 {
		t.Errorf("expected MaxRetries 3, got %d,%d", retries[0].MaxRetries, retries[1].MaxRetries)
	}
	if retries[0].Reason == "" || retries[1].Reason == "" {
		t.Error("expected non-empty retry Reason")
	}
	if retries[0].Error == "" || retries[1].Error == "" {
		t.Error("expected non-empty retry Error")
	}
	if retries[0].Backoff != time.Second || retries[1].Backoff != time.Second {
		t.Errorf("expected Backoff 1s, got %v,%v", retries[0].Backoff, retries[1].Backoff)
	}
}

// =============================================================================
// T4 — non-transient error: 1 request, 0 response, 0 retry
func TestRecallLLM_NonTransientError_NoResponseEvent(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{errors.New("invalid API key, schema fail")},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	ch, closeFn := subscribeRuntimeEventsForTest(t, al, 16,
		runtimeevents.KindAgentLLMRequest, runtimeevents.KindAgentLLMResponse, runtimeevents.KindAgentLLMRetry)
	defer closeFn()

	_, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "test_non_transient", nil)
	if err == nil {
		t.Fatal("expected error to propagate")
	}

	events := drainEventsAfter(ch)
	var requests, responses, retries int
	for _, evt := range events {
		switch evt.Kind {
		case runtimeevents.KindAgentLLMRequest:
			requests++
		case runtimeevents.KindAgentLLMResponse:
			responses++
		case runtimeevents.KindAgentLLMRetry:
			retries++
		}
	}
	if requests != 1 {
		t.Errorf("expected 1 request event, got %d", requests)
	}
	if responses != 0 {
		t.Errorf("expected 0 response events, got %d", responses)
	}
	if retries != 0 {
		t.Errorf("expected 0 retry events, got %d", retries)
	}
}

// =============================================================================
// T5 — hard abort before attempt: 0 events
func TestRecallLLM_HardAbort_NoEvents(t *testing.T) {
	provider := &recallTestProvider{
		responses: []*providers.LLMResponse{{Content: "should not reach", FinishReason: "stop"}},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	ch, closeFn := subscribeRuntimeEventsForTest(t, al, 16,
		runtimeevents.KindAgentLLMRequest, runtimeevents.KindAgentLLMResponse, runtimeevents.KindAgentLLMRetry)
	defer closeFn()

	_ = ts.requestHardAbort()

	_, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "test_abort", nil)
	if err == nil {
		t.Fatal("expected hard-abort error")
	}
	if provider.callCountN() != 0 {
		t.Fatalf("expected 0 LLM calls, got %d", provider.callCountN())
	}

	events := drainEventsAfter(ch)
	if len(events) != 0 {
		t.Fatalf("expected 0 events on hard abort, got %d", len(events))
	}
}

// =============================================================================
// T10 — dropped events (subscriber buffer full) → drop warning fires
func TestRecallLLM_DroppedEvents_Warns(t *testing.T) {
	provider := &recallTestProvider{
		errors: []error{
			fmt.Errorf("connection refused: attempt 0 transient"),
			nil, // attempt 1: success
		},
		responses: []*providers.LLMResponse{
			nil,
			{Content: "recovered", FinishReason: "stop"},
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	// Buffer 1 with NO drain → second publish must drop.
	ch, closeFn := subscribeRuntimeEventsForTest(t, al, 1, runtimeevents.KindAgentLLMRequest)
	defer closeFn()

	before := replayDropWarnTotal.Load()

	if _, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "test_drop", nil); err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}

	// 2 request publishes into a buffer of 1 → 1 delivered, 1 dropped.
	events := drainEventsAfter(ch)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 delivered request event (buffer full), got %d", len(events))
	}
	if after := replayDropWarnTotal.Load(); after <= before {
		t.Fatalf("expected replayDropWarnTotal to increase (before=%d after=%d)", before, after)
	}
}

// =============================================================================
// T8 — wire: full turn with empty response → main + replay events distinct
func TestTurnWithEmptyResponse_EmitsReplayEvents(t *testing.T) {
	provider := &recallTestProvider{
		responses: []*providers.LLMResponse{
			{Content: "", FinishReason: "stop"},                          // main path → empty → recovery
			{Content: "recovered after recall", FinishReason: "stop"},    // RecallLLM attempt 0
			{Content: "final answer", FinishReason: "stop"},              // main path next iter
		},
	}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	t.Cleanup(cleanup)

	al.SkipGoalArchiveForTest()
	al.SetGoalPhaseForTest(string(GoalPhaseOpen))

	pipeline := NewPipeline(al)
	ws := t.TempDir()
	agent.Workspace = ws

	ts := newTurnState(agent, makeTestProcessOpts("phase-12-45-session"), turnEventScope{
		turnID:  "turn-12-45",
		context: newTurnContext(nil, nil, nil),
	})
	ts.iterationCap = 5
	ts.setIteration(1)

	// Seed an active goal so recovery fires (F10).
	now := time.Now().UTC()
	activeGoal := &goal.Goal{
		Name: "phase-12-45-test",
		Description: goal.Description{
			Objective:       "test recall observability on the wire",
			SuccessCriteria: []string{"replay events are visible in the bus"},
			Cadence:         "as_needed",
		},
		Status:    goal.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := goal.NewStore(ws).Write("phase-12-45-session", activeGoal); err != nil {
		t.Fatalf("Write goal: %v", err)
	}

	exec, err := pipeline.SetupTurn(context.Background(), ts)
	if err != nil {
		t.Fatalf("SetupTurn: %v", err)
	}
	_ = exec

	ch, closeFn := subscribeRuntimeEventsForTest(t, al, 16, runtimeevents.KindAgentLLMRequest)
	defer closeFn()

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	t.Logf("T8: turn status=%v", result.status)

	events := drainEventsAfter(ch)
	var mainReq, recallReq int
	for _, evt := range events {
		switch evt.Correlation.TraceID {
		case "turn.llm.request":
			mainReq++
		case replayTracePath:
			recallReq++
		}
	}
	if mainReq < 1 {
		t.Fatalf("expected ≥1 main request event (turn.llm.request), got %d", mainReq)
	}
	if recallReq < 1 {
		t.Fatalf("expected ≥1 replay recall request event (%s), got %d", replayTracePath, recallReq)
	}
	if provider.callCountN() < 2 {
		t.Fatalf("expected ≥2 LLM calls (main + recall), got %d", provider.callCountN())
	}
}

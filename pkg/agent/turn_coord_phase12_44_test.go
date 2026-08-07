// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors
//
// Phase 12.44 turn-level wire tests (T6, T7, T9, T10, T11, T15, T16, T19):
//   T6  bumpCapForFinalReportIter helper gate + math
//   T7  final-report iter runs after complete_goal (text reaches user)
//   T9  final-report iter strips ALL tool calls (never executes)
//   T10 final-report iter empty LLM text → silent skip (no fallback replay)
//   T15 applyFallbackForEmptyResponse guard (postCompleteGoalReportSent)
//   T16 static call-site count for bumpCapForFinalReportIter (Phase 12.36 mirror)
//   T19 e2e: user receives FULL summary at the transport boundary before turn end

package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// phase1244Provider scripts: set_goal → complete_goal → final-report iter
// (text, empty, or a tool call depending on the test).
type phase1244Provider struct {
	calls        int
	finalContent string // what the final-report iter returns ("" = use finalToolName)
	finalTool    string // non-empty: emit a tool call on the final-report iter
	summary      string // complete_goal summary (call 2)
}

func (p *phase1244Provider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	if isTaskExtractionCall(messages, toolDefs, opts) {
		return &providers.LLMResponse{Content: taskExtractionResponse(messages)}, nil
	}
	p.calls++
	switch p.calls {
	case 1:
		return &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{
				{
					ID:   "call-set",
					Name: "set_goal",
					Arguments: map[string]any{
						"name":             "phase1244-goal",
						"objective":        "Phase 12.44 wire test",
						"success_criteria": []any{"summary published"},
					},
				},
			},
		}, nil
	case 2:
		return &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{
				{
					ID:        "call-complete",
					Name:      "complete_goal",
					Arguments: map[string]any{"summary": p.summary},
				},
			},
		}, nil
	default:
		if p.finalTool != "" {
			return &providers.LLMResponse{
				ToolCalls: []providers.ToolCall{
					{ID: "call-strip", Name: p.finalTool, Arguments: map[string]any{}},
				},
			}, nil
		}
		return &providers.LLMResponse{Content: p.finalContent}, nil
	}
}

func (p *phase1244Provider) GetDefaultModel() string { return "phase1244-model" }

func newPhase1244Turn(t *testing.T, p *phase1244Provider) (*AgentLoop, *turnState, *Pipeline, func()) {
	t.Helper()
	al, agent, cleanup := newTurnCoordTestLoop(t, p)
	pipeline := NewPipeline(al)
	opts := makeTestProcessOpts("sk_v1_phase1244")
	ts := newTurnState(agent, opts, turnEventScope{
		turnID:  "turn-12.44",
		context: newTurnContext(nil, nil, nil),
	})
	// Phase 8.2 gotcha: Dispatch.InboundContext drives ts.channel/chatID —
	// makeTestProcessOpts does NOT populate it. PublishToUser requires both.
	ts.channel = "cli"
	ts.chatID = "test-chat"
	return al, ts, pipeline, cleanup
}

// T6 — bumpCapForFinalReportIter helper gate + cap math.
func TestBumpCapForFinalReportIter_Helper(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	opts := makeTestProcessOpts("sk_v1_bump")
	ts := newTurnState(agent, opts, turnEventScope{turnID: "t", context: newTurnContext(nil, nil, nil)})

	// 1. goalFinalized + !sent + iter==cap → bump to iter+1.
	ts.goalFinalized = true
	ts.postCompleteGoalReportSent = false
	ts.setIteration(5)
	ts.iterationCap = 5
	al.bumpCapForFinalReportIter(ts)
	if ts.iterationCap != 6 {
		t.Errorf("iter==cap: expected cap 6, got %d", ts.iterationCap)
	}

	// 2. sent=true → no bump.
	ts.iterationCap = 5
	ts.postCompleteGoalReportSent = true
	al.bumpCapForFinalReportIter(ts)
	if ts.iterationCap != 5 {
		t.Errorf("sent=true: expected cap unchanged (5), got %d", ts.iterationCap)
	}
	ts.postCompleteGoalReportSent = false

	// 3. goalFinalized=false → no bump.
	ts.goalFinalized = false
	al.bumpCapForFinalReportIter(ts)
	if ts.iterationCap != 5 {
		t.Errorf("not finalized: expected cap unchanged (5), got %d", ts.iterationCap)
	}

	// 4. iter below cap → no bump (cap already >= iter+1).
	ts.goalFinalized = true
	ts.setIteration(3)
	al.bumpCapForFinalReportIter(ts)
	if ts.iterationCap != 5 {
		t.Errorf("iter<cap: expected cap unchanged (5), got %d", ts.iterationCap)
	}
}

// T16 — static call-site count: bumpCapForFinalReportIter must be wired at
// BOTH ControlContinue exits (retry path + outer tool-run path), mirroring
// the Phase 12.36 applyDeferredExtend per-site hook pattern.
func TestBumpCapForFinalReportIter_StaticCallSites(t *testing.T) {
	src, err := os.ReadFile("turn_coord.go")
	if err != nil {
		t.Fatalf("read turn_coord.go: %v", err)
	}
	n := strings.Count(string(src), "al.bumpCapForFinalReportIter(ts)")
	if n != 2 {
		t.Errorf("bumpCapForFinalReportIter must have exactly 2 call sites (retry path + tool-run path), found %d", n)
	}
}

// T7 — final-report iter runs after complete_goal and the LLM text is the
// turn's final content.
func TestFinalReportIter_RunsAfterCompleteGoal(t *testing.T) {
	p := &phase1244Provider{summary: "done", finalContent: "Đã hoàn tất báo cáo cuối."}
	al, ts, pipeline, cleanup := newPhase1244Turn(t, p)
	defer cleanup()

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Errorf("expected Completed, got %v", result.status)
	}
	if !strings.Contains(result.finalContent, "Đã hoàn tất báo cáo cuối.") {
		t.Errorf("final report text must be the turn's final content, got %q", result.finalContent)
	}
	if !ts.postCompleteGoalReportSent {
		t.Error("postCompleteGoalReportSent must be true after the final-report iter")
	}
}

// T9 — final-report iter strips ALL tool calls (never executed).
func TestFinalReportIter_ToolCallsStripped(t *testing.T) {
	// A tool that does not exist: if the strip failed, ExecuteTools would
	// error and the turn would end Error, not Completed.
	p := &phase1244Provider{summary: "done", finalTool: "definitely_not_a_tool"}
	al, ts, pipeline, cleanup := newPhase1244Turn(t, p)
	defer cleanup()

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Errorf("tool call on final-report iter must be stripped (turn Completed, not Error); got %v", result.status)
	}
	if !ts.postCompleteGoalReportSent {
		t.Error("postCompleteGoalReportSent must be true after the final-report iter")
	}
}

// T10 — final-report iter with EMPTY LLM text: silent skip — no
// DefaultResponse, no goal.Summary replay, no toolLimitResponse.
func TestFinalReportIter_EmptyText_SilentSkip(t *testing.T) {
	p := &phase1244Provider{summary: "done", finalContent: ""}
	al, ts, pipeline, cleanup := newPhase1244Turn(t, p)
	defer cleanup()

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Errorf("expected Completed, got %v", result.status)
	}
	if result.finalContent != "" {
		t.Errorf("empty final-report text must stay empty (silent skip), got %q", result.finalContent)
	}
	if strings.Contains(result.finalContent, "I couldn't process") {
		t.Error("DefaultResponse must NOT leak into the final-report iter")
	}
	if !ts.postCompleteGoalReportSent {
		t.Error("postCompleteGoalReportSent must be true after the final-report iter")
	}
}

// T11 — LLM text on the final-report iter is carried in finalContent (the
// user-facing result of the turn).
func TestFinalReportIter_LLMTextReachesUser(t *testing.T) {
	p := &phase1244Provider{summary: "done", finalContent: "Final report to user."}
	al, ts, pipeline, cleanup := newPhase1244Turn(t, p)
	defer cleanup()

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Errorf("expected Completed, got %v", result.status)
	}
	if !strings.Contains(result.finalContent, "Final report to user.") {
		t.Errorf("finalContent must carry the final-report text, got %q", result.finalContent)
	}
}

// T15 — applyFallbackForEmptyResponse guard: postCompleteGoalReportSent=true
// → "" (silent skip), exercising the SAME helper both call sites use.
func TestFallbackGuard_BothCallSites(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()
	opts := makeTestProcessOpts("sk_v1_fallback")
	ts := newTurnState(agent, opts, turnEventScope{turnID: "t", context: newTurnContext(nil, nil, nil)})

	// Guard active → empty (silent skip).
	ts.postCompleteGoalReportSent = true
	if got := al.applyFallbackForEmptyResponse(ts); got != "" {
		t.Errorf("guard must return empty string, got %q", got)
	}
	// Guard inactive → normal fallback chain (DefaultResponse base).
	ts.postCompleteGoalReportSent = false
	ts.setIteration(1)
	ts.iterationCap = 5
	if got := al.applyFallbackForEmptyResponse(ts); got != opts.DefaultResponse {
		t.Errorf("without guard, expected DefaultResponse %q, got %q", opts.DefaultResponse, got)
	}
}

// T19 — e2e turn-level (F21B/F25/F26): fake channel = the REAL transport
// boundary (bus.OutboundChan). complete_goal with a 1200-rune summary →
// the FULL summary arrives at the boundary BEFORE turn end, exactly once,
// while the turn still completes (final-report iter preserved).
func TestTurnEnd_UserReceivesFullSummary_EndToEnd(t *testing.T) {
	full := buildLongSummary(1200)
	p := &phase1244Provider{summary: full, finalContent: "final-report text"}
	al, ts, pipeline, cleanup := newPhase1244Turn(t, p)
	defer cleanup()

	result, err := al.runTurn(context.Background(), ts, pipeline)
	if err != nil {
		t.Fatalf("runTurn failed: %v", err)
	}
	if result.status != TurnEndStatusCompleted {
		t.Errorf("expected Completed, got %v", result.status)
	}

	// Drain the outbound bus (transport boundary — where channelManager reads).
	mb, ok := al.bus.(*bus.MessageBus)
	if !ok {
		t.Fatal("test fixture must wire a *bus.MessageBus")
	}
	var outbounds []bus.OutboundMessage
	for {
		select {
		case m := <-mb.OutboundChan():
			outbounds = append(outbounds, m)
		default:
			goto drained
		}
	}
drained:
	if len(outbounds) != 1 {
		t.Fatalf("expected exactly 1 outbound message (the summary), got %d", len(outbounds))
	}
	got := outbounds[0].Content
	// Phase 12.55.2: summary now carries the iteration/phase header
	// ("📊 <turnID> (#iter/cap) Goal-<phase>: summary\n") — the summary
	// body itself must stay verbatim at the transport boundary.
	if !strings.HasPrefix(got, "📊 ") {
		t.Errorf("summary message must carry the iteration/phase header, got %q", got)
	}
	// Phase 12.55.3: header phase must be the phase AT EXECUTE TIME
	// (complete_goal ran at iter 2 in phase open), NOT re-resolved after
	// archive (which removes the active goal file → hasGoal()=false → Set).
	if !strings.Contains(got, "Goal-Open") {
		t.Errorf("summary header must say Goal-Open (phase at execute), got %q", got)
	}
	if !strings.Contains(got, ": summary\n") {
		t.Errorf("summary message must carry the ': summary' suffix on the header line, got %q", got)
	}
	if !strings.HasSuffix(got, full) {
		t.Error("summary at the transport boundary must end byte-identical to the LLM-supplied summary (F22A)")
	}
	if utf8.RuneCountInString(got) <= utf8.RuneCountInString(full) {
		t.Errorf("full summary must be present verbatim; got %d runes (summary alone is %d)", utf8.RuneCountInString(got), utf8.RuneCountInString(full))
	}
	if outbounds[0].Context.Channel != "cli" || outbounds[0].Context.ChatID != "test-chat" {
		t.Errorf("outbound must target the turn's channel/chat, got %q/%q", outbounds[0].Context.Channel, outbounds[0].Context.ChatID)
	}
	// Archived copy truncated to 1000 runes (owner decision 2026-08-03).
	st := goal.NewStore(ts.agent.Workspace)
	g, err := st.ReadAny("sk_v1_phase1244")
	if err != nil {
		t.Fatalf("ReadAny failed: %v", err)
	}
	if utf8.RuneCountInString(g.Summary) != 1000 {
		t.Errorf("archived summary must be truncated to 1000 runes, got %d", utf8.RuneCountInString(g.Summary))
	}
}

func buildLongSummary(n int) string {
	// "Phòng đẹp!" = 10 runes, ends in a non-whitespace char (PublishResponseIfNeeded
	// trims trailing whitespace — the summary must survive verbatim, so the test
	// payload must not end in whitespace).
	const chunk = "Phòng đẹp!"
	var b strings.Builder
	for utf8.RuneCountInString(b.String()) < n {
		b.WriteString(chunk)
	}
	s := b.String()
	for utf8.RuneCountInString(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

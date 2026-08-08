// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors
//
// Phase 12.44: complete_goal self-publish + truncate-archived-summary wire tests.
// TDD micro-loop — these tests FAIL against the Phase 12.43 binary (no
// PublishToUser on TurnStateAccess, no truncateRunes, >500 rejects).

package goal

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// publishSpy is a test double for TurnStateAccess that records publishes
// and the goalFinalized state at publish time (T14 ordering check).
type publishSpy struct {
	published          []string
	phases             []string // phase snapshot passed to PublishGoalSummary
	finalizedAtPublish []bool   // finalized flag value AT the moment each publish fired
	finalized          bool
}

func (s *publishSpy) MarkGoalFinalized() { s.finalized = true }
func (s *publishSpy) PublishToUser(_ context.Context, text string) {
	s.published = append(s.published, text)
	s.finalizedAtPublish = append(s.finalizedAtPublish, s.finalized)
}
func (s *publishSpy) PublishGoalSummary(_ context.Context, phase, summary string) {
	s.phases = append(s.phases, phase)
	s.published = append(s.published, summary)
	s.finalizedAtPublish = append(s.finalizedAtPublish, s.finalized)
}

func (s *publishSpy) publishedCount() int { return len(s.published) }

func newPublishTestCtx(sessionKey string, spy *publishSpy) context.Context {
	ctx := ctxWithSession(sessionKey, "agent")
	if spy != nil {
		ctx = WithTurnState(ctx, spy)
	}
	return ctx
}

// T1 — success path publishes the FULL summary (no truncate), at most once.
func TestCompleteGoalTool_PublishesFullSummaryOnSuccess(t *testing.T) {
	ws := tempWorkspace(t)
	spy := &publishSpy{}
	ctx := newPublishTestCtx("sess-pub-1", spy)
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": "Hoàn tất — đã kiểm tra toàn bộ 12 SOP và chốt 2 ứng viên.",
	})
	if res.IsError {
		t.Fatalf("complete_goal returned error: %s", res.ForLLM)
	}
	if spy.publishedCount() != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", spy.publishedCount())
	}
	want := "Hoàn tất — đã kiểm tra toàn bộ 12 SOP và chốt 2 ứng viên."
	if spy.published[0] != want {
		t.Errorf("published summary = %q, want FULL summary %q", spy.published[0], want)
	}
}

// T2 — archived copy truncated at 1000 runes; the FULL original still reaches the channel.
func TestCompleteGoalTool_TruncatesArchivedSummaryAt1000Runes(t *testing.T) {
	ws := tempWorkspace(t)
	spy := &publishSpy{}
	ctx := newPublishTestCtx("sess-pub-2", spy)
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	full := vnRunes(2000) // 2000 VN runes (~2800+ bytes)
	if utf8.RuneCountInString(full) != 2000 {
		t.Fatalf("test setup error: vnRunes(2000) produced %d runes", utf8.RuneCountInString(full))
	}
	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": full})
	if res.IsError {
		t.Fatalf("2000-rune summary must NOT be rejected (truncate, not reject); got isErr=%v msg=%q", res.IsError, res.ForLLM)
	}
	// Channel got the FULL original.
	if spy.publishedCount() != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", spy.publishedCount())
	}
	if utf8.RuneCountInString(spy.published[0]) != 2000 {
		t.Errorf("published summary must be the FULL 2000 runes (no truncate), got %d runes", utf8.RuneCountInString(spy.published[0]))
	}
	// Archive got exactly 1000 runes.
	store := newStoreFromCtx(ctx, ws)
	g, err := store.ReadAny("sess-pub-2")
	if err != nil {
		t.Fatalf("ReadAny failed: %v", err)
	}
	if utf8.RuneCountInString(g.Summary) != 1000 {
		t.Errorf("archived summary must be truncated to 1000 runes, got %d", utf8.RuneCountInString(g.Summary))
	}
	if !strings.HasPrefix(full, g.Summary) {
		t.Errorf("archived summary must be a prefix of the original")
	}
}

// T3 — tool does NOT set ResponseHandled (Phase 12.7 final-report iter preserved).
func TestCompleteGoalTool_DoesNotSetResponseHandled(t *testing.T) {
	ws := tempWorkspace(t)
	spy := &publishSpy{}
	ctx := newPublishTestCtx("sess-pub-3", spy)
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "done"})
	if res.IsError {
		t.Fatalf("complete_goal returned error: %s", res.ForLLM)
	}
	if res.ResponseHandled {
		t.Error("complete_goal must NOT set ResponseHandled — loop must continue to the final-report iter")
	}
}

// T4 — empty summary still rejected (required-field guard preserved).
func TestCompleteGoalTool_EmptySummaryStillRejected(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := newPublishTestCtx("sess-pub-4", nil)
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": ""})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("empty summary should hit required-field check, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
}

// T5 — no TurnStateAccess on ctx → publish skipped, no panic.
func TestCompleteGoalTool_PublishSkippedWhenNoTurnState(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-pub-5", "agent") // NO WithTurnState
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "done"})
	if res.IsError {
		t.Fatalf("complete_goal returned error: %s", res.ForLLM)
	}
	// No panic + goal still archived is the pass condition (nothing to spy).
}

// T8 — nil ctx guard: no panic, clean error.
func TestCompleteGoalTool_NilContext_NoPanic(t *testing.T) {
	ws := tempWorkspace(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("complete_goal panicked on nil-ish ctx: %v", r)
		}
	}()
	_ = NewCompleteGoalTool(ws).Execute(nil, map[string]any{"summary": "x"})
}

// T12 — exactly 1000 runes: byte-identical, no truncate, publish fires.
func TestCompleteGoalTool_SummaryExact1000Runes(t *testing.T) {
	ws := tempWorkspace(t)
	spy := &publishSpy{}
	ctx := newPublishTestCtx("sess-pub-12", spy)
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	summary := vnRunes(1000)
	if utf8.RuneCountInString(summary) != 1000 {
		t.Fatalf("test setup error: vnRunes(1000) produced %d runes", utf8.RuneCountInString(summary))
	}
	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": summary})
	if res.IsError {
		t.Fatalf("1000-rune summary should succeed; got isErr=%v msg=%q", res.IsError, res.ForLLM)
	}
	store := newStoreFromCtx(ctx, ws)
	g, err := store.ReadAny("sess-pub-12")
	if err != nil {
		t.Fatalf("ReadAny failed: %v", err)
	}
	if g.Summary != summary {
		t.Error("1000-rune summary must be stored byte-identical (no truncate at exact boundary)")
	}
	if spy.publishedCount() != 1 || spy.published[0] != summary {
		t.Errorf("publish must carry the exact 1000-rune summary; got %d publishes", spy.publishedCount())
	}
}

// T14 — ordering: channel receives the summary BEFORE goalFinalized flips.
func TestCompleteGoalTool_PublishBeforeMarkGoalFinalized(t *testing.T) {
	ws := tempWorkspace(t)
	spy := &publishSpy{}
	ctx := newPublishTestCtx("sess-pub-14", spy)
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "final summary"})
	if res.IsError {
		t.Fatalf("complete_goal returned error: %s", res.ForLLM)
	}
	if !spy.finalized {
		t.Error("MarkGoalFinalized must have been called by the end of Execute")
	}
	if spy.publishedCount() != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", spy.publishedCount())
	}
	if spy.finalizedAtPublish[0] {
		t.Error("publish must fire BEFORE MarkGoalFinalized (publish-then-flag ordering, F14/F15)")
	}
}

// T18 — 5000-rune summary arrives VERBATIM at the PublishToUser boundary
// (PublishToUser must never cut/truncate — split is outbound responsibility, F22A).
func TestCompleteGoalTool_PublishesFullSummaryUncut(t *testing.T) {
	ws := tempWorkspace(t)
	spy := &publishSpy{}
	ctx := newPublishTestCtx("sess-pub-18", spy)
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	full := vnRunes(5000)
	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": full})
	if res.IsError {
		t.Fatalf("5000-rune summary must be accepted (truncate archived only); got isErr=%v msg=%q", res.IsError, res.ForLLM)
	}
	if spy.publishedCount() != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", spy.publishedCount())
	}
	if spy.published[0] != full {
		t.Errorf("PublishToUser must receive the FULL 5000-rune summary verbatim; got %d runes (want %d)",
			utf8.RuneCountInString(spy.published[0]), utf8.RuneCountInString(full))
	}
}

// T19 — Phase 12.58.4 (Option B): complete_goal no longer publishes the
// LLM's content text (explanation) — the tool-feedback explanation path
// (Phase 12.58.3) delivers it. Only the summary is published (1 message).
// The phase snapshot (execute-time, from ctx) flows through verbatim.
func TestCompleteGoalTool_ExplanationNotPublishedSummaryOnly(t *testing.T) {
	ws := tempWorkspace(t)
	spy := &publishSpy{}
	ctx := newPublishTestCtx("sess-pub-19", spy)
	ctx = WithToolCallPhase(ctx, "open")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": "Hoàn tất — đã kiểm tra toàn bộ 12 SOP.",
	})
	if res.IsError {
		t.Fatalf("complete_goal error: %s", res.ContentForLLM())
	}
	if got, want := spy.publishedCount(), 1; got != want {
		t.Fatalf("publish count = %d, want %d (summary only)", got, want)
	}
	if got, want := spy.published[0], "Hoàn tất — đã kiểm tra toàn bộ 12 SOP."; got != want {
		t.Fatalf("published[0] = %q, want %q (summary)", got, want)
	}
	if len(spy.phases) != 1 || spy.phases[0] != "open" {
		t.Fatalf("PublishGoalSummary must receive the execute-time phase snapshot \"open\", got %v", spy.phases)
	}
}

// T20 — no LLM content text → only the summary is published (1 message).
func TestCompleteGoalTool_NoExplanationPublishesSummaryOnly(t *testing.T) {
	ws := tempWorkspace(t)
	spy := &publishSpy{}
	ctx := newPublishTestCtx("sess-pub-20", spy)
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": "Chỉ có summary, không có content text.",
	})
	if res.IsError {
		t.Fatalf("complete_goal error: %s", res.ContentForLLM())
	}
	if got, want := spy.publishedCount(), 1; got != want {
		t.Fatalf("publish count = %d, want %d (summary only)", got, want)
	}
	if got, want := spy.published[0], "Chỉ có summary, không có content text."; got != want {
		t.Fatalf("published[0] = %q, want %q", got, want)
	}
}

// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"strings"
	"testing"
	"time"
)

func TestRenderHeader_IncludesAllDescriptionFields(t *testing.T) {
	g := &Goal{
		Name: "demo",
		Description: Description{
			Objective:       "Ship the goal lifecycle MVP",
			SuccessCriteria: []string{"Phase 1 ships", "Phase 2 ships", "All tests pass"},
			InScope:         []string{"pkg/agent/goal", "agent_init wiring"},
			OutOfScope:      []string{"legacy task summary"},
			Cadence:         "weekly",
		},
		Status:    StatusActive,
		CreatedAt: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC),
	}
	header := g.RenderHeader()

	want := []string{
		"## Goal: demo",
		"**Status:** active",
		"**Created:** 2026-07-20T00:00:00Z",
		"**Updated:** 2026-07-20T01:00:00Z",
		"**Objective:** Ship the goal lifecycle MVP",
		"- Phase 1 ships",
		"- Phase 2 ships",
		"- All tests pass",
		"**Scope (in):** pkg/agent/goal, agent_init wiring",
		"**Scope (out):** legacy task summary",
		"**Cadence:** weekly",
	}
	for _, s := range want {
		if !strings.Contains(header, s) {
			t.Errorf("header missing %q\nheader was:\n%s", s, header)
		}
	}
}

func TestRenderHeader_OmitsOptionalFieldsWhenEmpty(t *testing.T) {
	g := &Goal{
		Name: "minimal",
		Description: Description{
			Objective:       "One thing",
			SuccessCriteria: []string{"Done"},
		},
		Status:    StatusActive,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	header := g.RenderHeader()
	for _, absent := range []string{"Scope (in)", "Scope (out)", "Cadence"} {
		if strings.Contains(header, absent) {
			t.Errorf("header should not mention %q when empty\nheader was:\n%s", absent, header)
		}
	}
}

func TestRenderHeader_NilGoalReturnsEmpty(t *testing.T) {
	var g *Goal
	if got := g.RenderHeader(); got != "" {
		t.Errorf("nil.RenderHeader = %q, want empty", got)
	}
}

func TestRenderProgress_EmptyBody(t *testing.T) {
	g := &Goal{Name: "fresh"}
	body, total, more := g.RenderProgress(0, 0)
	if !strings.Contains(body, "<no progress entries yet>") {
		t.Errorf("expected placeholder, got %q", body)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if more {
		t.Error("hasMore should be false for empty body")
	}
}

func TestRenderProgress_NumberedAndFullByDefault(t *testing.T) {
	g := &Goal{}
	g.AppendProgress(ProgressEntry{
		CompletedSteps: []string{"step1"},
		RemainingSteps: []string{"step2"},
		NextAction:     "review",
	})
	g.AppendProgress(ProgressEntry{
		Blockers:       []string{"waiting on CI"},
		RemainingSteps: []string{"step3"},
		NextAction:     "deploy",
	})

	body, total, more := g.RenderProgress(0, 0)
	if more {
		t.Error("hasMore should be false when maxLines is 0")
	}
	if total == 0 {
		t.Error("total should be > 0 for non-empty progress")
	}
	if !strings.Contains(body, "1: ") {
		t.Errorf("body should be 1-indexed, got:\n%s", body)
	}
	for _, want := range []string{"Completed: step1", "Blockers: waiting on CI", "Next action: deploy"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody was:\n%s", want, body)
		}
	}
}

func TestRenderProgress_Pagination(t *testing.T) {
	g := &Goal{}
	for i := 0; i < 10; i++ {
		g.AppendProgress(ProgressEntry{
			NextAction: "step" + string(rune('A'+i)),
		})
	}

	_, total, _ := g.RenderProgress(0, 0)
	if total < 10 {
		t.Fatalf("sanity: total should be >= 10, got %d", total)
	}

	first, fullTotal, firstMore := g.RenderProgress(0, 3)
	if !firstMore {
		t.Error("after first window of 3 lines, hasMore should be true")
	}
	if fullTotal != total {
		t.Errorf("total mismatch: first call %d vs full call %d", fullTotal, total)
	}
	lines := strings.Split(strings.TrimRight(first, "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("first window: got %d lines, want 3\nfirst:\n%s", len(lines), first)
	}

	second, _, secondMore := g.RenderProgress(3, 3)
	lines2 := strings.Split(strings.TrimRight(second, "\n"), "\n")
	if len(lines2) != 3 {
		t.Errorf("second window: got %d lines, want 3\nsecond:\n%s", len(lines2), second)
	}
	if first == second {
		t.Errorf("windows should differ: first=%q second=%q", first, second)
	}
	if !secondMore {
		t.Error("second window should still have more (assuming > 6 lines total)")
	}

	_, _, lastMore := g.RenderProgress(total-2, 10)
	if lastMore {
		t.Error("last window should report no more (within full total)")
	}
}

func TestRenderProgress_StartPastEOF(t *testing.T) {
	g := &Goal{}
	g.AppendProgress(ProgressEntry{NextAction: "only"})

	body, total, more := g.RenderProgress(100, 5)
	if body != "" {
		t.Errorf("body should be empty, got %q", body)
	}
	if total == 0 {
		t.Error("total should be > 0 for non-empty progress")
	}
	if more {
		t.Error("hasMore should be false past EOF")
	}
}

func TestRenderProgress_NegativeStartLineNormalizesToZero(t *testing.T) {
	g := &Goal{}
	g.AppendProgress(ProgressEntry{NextAction: "x"})

	body, total, _ := g.RenderProgress(-5, 1)
	if total == 0 {
		t.Fatal("total should be > 0")
	}
	if !strings.Contains(body, "1: ") {
		t.Errorf("negative startLine should normalize to 0, body:\n%s", body)
	}
}

func TestRenderProgress_MaxLinesZeroMeansAll(t *testing.T) {
	g := &Goal{}
	for i := 0; i < 5; i++ {
		g.AppendProgress(ProgressEntry{NextAction: "x"})
	}
	_, total, more := g.RenderProgress(0, 0)
	if more {
		t.Error("maxLines=0 should never report hasMore")
	}
	if total == 0 {
		t.Error("total should be > 0")
	}
}

func TestRenderProgress_DriftFlagRendersWhenTrue(t *testing.T) {
	g := &Goal{}
	g.AppendProgress(ProgressEntry{
		DriftDetected: true,
		NextAction:    "reset",
	})
	body, _, _ := g.RenderProgress(0, 0)
	if !strings.Contains(body, "Drift: true") {
		t.Errorf("expected \"Drift: true\", body:\n%s", body)
	}
}

func TestRenderProgress_DriftFlagAbsentWhenFalse(t *testing.T) {
	g := &Goal{}
	g.AppendProgress(ProgressEntry{NextAction: "ok"})
	body, _, _ := g.RenderProgress(0, 0)
	if strings.Contains(body, "Drift:") {
		t.Errorf("Drift should not render when false, body:\n%s", body)
	}
}

func TestRenderProgress_NilGoalSentinel(t *testing.T) {
	var g *Goal
	body, total, _ := g.RenderProgress(0, 0)
	if !strings.Contains(body, "<no goal loaded>") {
		t.Errorf("nil goal should render sentinel, got %q", body)
	}
	if total != 1 {
		t.Errorf("total should be 1 for sentinel, got %d", total)
	}
}

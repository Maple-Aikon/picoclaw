// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"strings"
	"testing"
)

func TestRenderGoalSnapshot_Initial_English(t *testing.T) {
	g := &Goal{
		Name: "n",
		Description: Description{
			Objective:        "Ship Phase 8.1",
			SuccessCriteria:  []string{"tests pass", "PR merged"},
			InScope:          []string{"write snapshot renderer", "wire into set_goal"},
			OutOfScope:       []string{"remove extraction fns"},
		},
	}
	snap := RenderGoalSnapshot(g, nil)
	want := "Goal: Ship Phase 8.1. Next: write snapshot renderer."
	if snap != want {
		t.Errorf("got %q\nwant %q", snap, want)
	}
}

func TestRenderGoalSnapshot_Initial_VietnamesePreserved(t *testing.T) {
	g := &Goal{
		Name: "n",
		Description: Description{
			Objective:       "Hoàn thành Phase 8.1",
			SuccessCriteria: []string{"tests pass"},
			InScope:         []string{"viết snapshot renderer"},
		},
	}
	snap := RenderGoalSnapshot(g, nil)
	// Vietnamese diacritics must round-trip verbatim — no normalization, no
	// ASCII transliteration. The render is byte-faithful to the input.
	if !strings.HasPrefix(snap, "Goal: Hoàn thành Phase 8.1. Next: viết snapshot renderer.") {
		t.Errorf("vn diacritics dropped: %q", snap)
	}
}

func TestRenderGoalSnapshot_Initial_OnlySuccessCriteria(t *testing.T) {
	g := &Goal{
		Name: "n",
		Description: Description{
			Objective:       "deploy",
			SuccessCriteria: []string{"run smoke tests"},
		},
	}
	snap := RenderGoalSnapshot(g, nil)
	want := "Goal: deploy. Next: run smoke tests."
	if snap != want {
		t.Errorf("got %q\nwant %q", snap, want)
	}
}

func TestRenderGoalSnapshot_Initial_EmptyEverything(t *testing.T) {
	g := &Goal{Name: "n", Description: Description{Objective: ""}}
	if snap := RenderGoalSnapshot(g, nil); snap != "" {
		t.Errorf("empty objective should yield empty snapshot, got %q", snap)
	}
}

func TestRenderGoalSnapshot_Progress_NextActionWins(t *testing.T) {
	g := &Goal{
		Name:        "n",
		Description: Description{Objective: "obj", InScope: []string{"a", "b"}},
	}
	entry := &ProgressEntry{
		CompletedSteps: []string{"x"},
		RemainingSteps: []string{"still left"},
		NextAction:     "do Y now",
	}
	snap := RenderGoalSnapshot(g, entry)
	want := "Goal: obj. Next: do Y now."
	if snap != want {
		t.Errorf("got %q\nwant %q", snap, want)
	}
}

func TestRenderGoalSnapshot_Progress_FallsBackToRemaining(t *testing.T) {
	g := &Goal{Description: Description{Objective: "obj"}}
	entry := &ProgressEntry{
		RemainingSteps: []string{"first remaining", "second remaining"},
	}
	snap := RenderGoalSnapshot(g, entry)
	want := "Goal: obj. Next: first remaining."
	if snap != want {
		t.Errorf("got %q\nwant %q", snap, want)
	}
}

func TestRenderGoalSnapshot_Progress_DriftPrefix(t *testing.T) {
	g := &Goal{Description: Description{Objective: "ship X"}}
	entry := &ProgressEntry{
		NextAction:    "replan",
		DriftDetected: true,
	}
	snap := RenderGoalSnapshot(g, entry)
	if !strings.HasPrefix(snap, "⚠ DRIFT: ") {
		t.Errorf("drift prefix missing: %q", snap)
	}
	if !strings.Contains(snap, "Next: replan.") {
		t.Errorf("next_action missing from drift snapshot: %q", snap)
	}
}

func TestRenderGoalSnapshot_Progress_NoForwardSignal(t *testing.T) {
	g := &Goal{
		Description: Description{
			Objective:       "ship",
			SuccessCriteria: []string{"ship"},
		},
	}
	entry := &ProgressEntry{CompletedSteps: []string{"something"}}
	snap := RenderGoalSnapshot(g, entry)
	// Falls back to first success_criterion.
	want := "Goal: ship. Next: ship."
	if snap != want {
		t.Errorf("got %q\nwant %q", snap, want)
	}
}

func TestRenderGoalSnapshot_CharCap_TrimsWithEllipsis(t *testing.T) {
	// Build a Goal whose initial render naturally exceeds 180 chars.
	longObj := strings.Repeat("very long objective ", 20) // ~400 chars
	g := &Goal{Description: Description{Objective: longObj}}
	snap := RenderGoalSnapshot(g, nil)
	if len(snap) > snapshotMaxLen {
		t.Errorf("snapshot %d chars exceeds cap %d: %q", len(snap), snapshotMaxLen, snap)
	}
	if !strings.HasSuffix(snap, "…") {
		t.Errorf("over-cap snapshot must end with ellipsis: %q", snap)
	}
	// Boundary: a render of exactly snapshotMaxLen should NOT be trimmed.
	// Template overhead is "Goal: o. Next: " (16 bytes) + trailing "." (1
	// byte) = 17 bytes; the success_criterion text can therefore be at
	// most snapshotMaxLen-17 bytes to fit under the cap.
	exact := strings.Repeat("a", snapshotMaxLen-17) // 163 chars of "a"
	exactG := &Goal{Description: Description{Objective: "o", SuccessCriteria: []string{exact}}}
	exactSnap := RenderGoalSnapshot(exactG, nil)
	if strings.HasSuffix(exactSnap, "…") {
		t.Errorf("under-cap snapshot was incorrectly trimmed: len=%d, snap=%q", len(exactSnap), exactSnap)
	}
}

func TestRenderGoalSnapshot_NilGoal(t *testing.T) {
	if snap := RenderGoalSnapshot(nil, nil); snap != "" {
		t.Errorf("nil goal should yield empty, got %q", snap)
	}
}

func TestRenderGoalSnapshot_TrimsTrailingPunctuation(t *testing.T) {
	g := &Goal{Description: Description{
		Objective: "deploy;",
		InScope:   []string{"run tests:"},
	}}
	snap := RenderGoalSnapshot(g, nil)
	// The trailing ; and : should be stripped so the template can append ".".
	if strings.Contains(snap, "deploy;.") || strings.Contains(snap, "run tests:.") {
		t.Errorf("trailing punctuation not stripped: %q", snap)
	}
	if !strings.HasSuffix(snap, "Next: run tests.") {
		t.Errorf("got %q", snap)
	}
}

func TestIsVietnameseText(t *testing.T) {
	cases := map[string]bool{
		"hello world":  false,
		"":             false,
		"viết":         true,
		"Hoàn thành":   true,
		"cafe":         false, // no Vietnamese diacritics
		"cà phê":       true,
	}
	for in, want := range cases {
		if got := isVietnameseText(in); got != want {
			t.Errorf("isVietnameseText(%q) = %v, want %v", in, got, want)
		}
	}
}

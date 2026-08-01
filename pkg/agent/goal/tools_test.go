// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// ctxWithSession returns a context with the standard tool session key/agent
// ID injected (mimicking what pipeline_execute.go does at run-time).
func ctxWithSession(sessionKey, agentID string) context.Context {
	ctx := toolshared.WithToolContext(context.Background(), "telegram", "chat1")
	ctx = toolshared.WithToolSessionContext(ctx, agentID, sessionKey, nil)
	return ctx
}

func tempWorkspace(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// set_goal
// ---------------------------------------------------------------------------

func TestSetGoalTool_CreatesNewGoal(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent-main")

	args := map[string]any{
		"name":             "ship-goal-tools",
		"objective":        "All 4 goal tools are wired and tested",
		"success_criteria": []string{"TestSetGoal passes", "TestViewGoal passes", "Tools registered"},
		"in_scope":         []string{"pkg/agent/goal/tools.go"},
		"out_of_scope":     []string{"Phase 3 dynamic allowlist"},
		"cadence":          "ship Phase 2 this session",
	}
	res := NewSetGoalTool(ws).Execute(ctx, args)
	if res.IsError {
		t.Fatalf("expected success, got error: %s (errKind=%s)", res.Err, res.ErrKind)
	}
	if !strings.Contains(res.ForLLM, "created") {
		t.Errorf("expected 'created' in summary, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "## Goal: ship-goal-tools") {
		t.Errorf("expected header render in ForLLM, got:\n%s", res.ForLLM)
	}

	st := NewStore(ws)
	g, err := st.Read("sess-A")
	if err != nil || g == nil {
		t.Fatalf("expected goal persisted, got: %v %v", g, err)
	}
	if g.Status != StatusActive {
		t.Errorf("expected status=active, got %q", g.Status)
	}
	if len(g.Description.SuccessCriteria) != 3 {
		t.Errorf("expected 3 success criteria, got %d", len(g.Description.SuccessCriteria))
	}
}

func TestSetGoalTool_ReplacesExistingGoal(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent-main")

	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "first",
		"objective":        "initial objective",
		"success_criteria": []string{"c1"},
	})

	res := NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "first",
		"objective":        "new objective",
		"success_criteria": []string{"c1", "c2"},
	})
	if !strings.Contains(res.ForLLM, "replaced") {
		t.Errorf("expected 'replaced' in summary, got: %s", res.ForLLM)
	}

	g, _ := NewStore(ws).Read("sess-A")
	if g.Description.Objective != "new objective" {
		t.Errorf("objective not replaced: %q", g.Description.Objective)
	}
	if len(g.Description.SuccessCriteria) != 2 {
		t.Errorf("expected 2 criteria after replace, got %d", len(g.Description.SuccessCriteria))
	}
}

func TestSetGoalTool_RejectsMissingName(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent-main")
	res := NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"objective":        "x",
		"success_criteria": []string{"y"},
	})
	if !res.IsError {
		t.Fatal("expected error for missing name")
	}
	if res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected ErrInvalidInput, got %q", res.ErrKind)
	}
}

func TestSetGoalTool_RejectsEmptySuccessCriteria(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent-main")
	res := NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "x",
		"success_criteria": []string{},
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input on empty criteria, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
}

func TestSetGoalTool_RejectsEmptySessionKey(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := context.Background() // no session injected
	res := NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"x"},
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input without session, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
}

func TestSetGoalTool_PreservesCreatedAtOnReplace(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent-main")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c1"},
	})
	first, _ := NewStore(ws).Read("sess-A")
	original := first.CreatedAt

	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o2",
		"success_criteria": []string{"c1", "c2"},
	})
	second, _ := NewStore(ws).Read("sess-A")
	if !second.CreatedAt.Equal(original) {
		t.Errorf("CreatedAt should be preserved on replace: was %v, now %v", original, second.CreatedAt)
	}
}

// ---------------------------------------------------------------------------
// view_goal
// ---------------------------------------------------------------------------

func TestViewGoalTool_NoGoalSentinel(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("nope", "agent")
	res := NewViewGoalTool(ws).Execute(ctx, nil)
	if res.IsError {
		t.Fatalf("missing goal is not an error, got %v", res)
	}
	if !strings.Contains(res.ForLLM, "<no goal set") {
		t.Errorf("expected sentinel, got: %s", res.ForLLM)
	}
}

func TestViewGoalTool_ReturnsHeaderAlways(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "ship-x",
		"objective":        "ship the X thing",
		"success_criteria": []string{"criterion alpha"},
	})

	res := NewViewGoalTool(ws).Execute(ctx, map[string]any{"max_lines": 0})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	for _, want := range []string{"## Goal: ship-x", "**Objective:** ship the X thing", "## Progress log"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("missing %q in response", want)
		}
	}
}

func TestViewGoalTool_PaginationHonored(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	for i := 0; i < 5; i++ {
		NewGoalProgressTool(ws).Execute(ctx, map[string]any{
			"completed_steps": []string{"step" + string(rune('A'+i))},
			"remaining_steps": []string{"continue iteration " + string(rune('A'+i))},
		})
	}

	first := NewViewGoalTool(ws).Execute(ctx, map[string]any{"start_line": 0, "max_lines": 3})
	last := NewViewGoalTool(ws).Execute(ctx, map[string]any{"start_line": 0, "max_lines": 0})

	if first.ForLLM == last.ForLLM {
		t.Error("paginating by start_line should yield different windows")
	}
	if !strings.Contains(first.ForLLM, "has_more=true") {
		t.Errorf("first window should report has_more=true, got:\n%s", first.ForLLM)
	}
	if !strings.Contains(last.ForLLM, "has_more=false") {
		t.Errorf("full-window call should report has_more=false, got:\n%s", last.ForLLM)
	}
}

func TestViewGoalTool_StartPastEOFReturnsHeaderOnly(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	res := NewViewGoalTool(ws).Execute(ctx, map[string]any{"start_line": 99999})
	if res.IsError {
		t.Fatalf("expected non-error sentinel for past-EOF, got %v", res.Err)
	}
	if !strings.Contains(res.ForLLM, "<start_line 99999 is past the end") {
		t.Errorf("expected past-EOF sentinel, got:\n%s", res.ForLLM)
	}
}

// ---------------------------------------------------------------------------
// goal_progress
// ---------------------------------------------------------------------------

func TestGoalProgressTool_AppendsAndPersists(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"remaining_steps": []string{"run tests"},
		"next_action":     "run tests",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}

	g, _ := NewStore(ws).Read("sess-A")
	if len(g.Progress) != 1 {
		t.Fatalf("expected 1 progress entry, got %d", len(g.Progress))
	}
	if g.Progress[0].NextAction != "run tests" {
		t.Errorf("expected next_action saved, got %q", g.Progress[0].NextAction)
	}
	if !strings.Contains(res.ForLLM, "Logged progress entry #1") {
		t.Errorf("expected entry index in summary, got: %s", res.ForLLM)
	}
}

func TestGoalProgressTool_RejectsEmptyEntry(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input on empty entry, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
}

func TestGoalProgressTool_DriftRequiresNextAction(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"x"},
		"drift_detected":  true,
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input when drift=true without next_action, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
}

func TestGoalProgressTool_RequiresExistingGoal(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-no-goal", "agent")
	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"x"},
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input without prior goal, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
}

func TestGoalProgressTool_RejectsAfterCompletion(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	// Phase 11: complete_goal requires a `summary` arg (1-500 chars).
	NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": "first summary",
	})

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"x"},
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input on completed goal, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
}

// ---------------------------------------------------------------------------
// complete_goal
// ---------------------------------------------------------------------------

func TestCompleteGoalTool_ArchivesFile(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"x"},
	})

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": "all done",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !strings.Contains(res.ForLLM, "marked completed") {
		t.Errorf("expected confirmation, got: %s", res.ForLLM)
	}

	st := NewStore(ws)
	if g, _ := st.Read("sess-A"); g != nil {
		t.Error("expected active file to be moved (Read should now report nil, nil)")
	}

	archiveDir := filepath.Join(ws, "memory", "goal", "archive")
	entries, _ := readDirNames(archiveDir)
	if len(entries) != 1 {
		t.Errorf("expected 1 archived file, got %d", len(entries))
	}
}

func TestCompleteGoalTool_NoGoalSentinel(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	res := NewCompleteGoalTool(ws).Execute(ctx, nil)
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input without goal, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
}

// Phase 12.20.1 fix (C): 2nd complete_goal call now returns IDEMPOTENT
// SUCCESS (was error pre-12.20.1). Goal archive in Phase 11 moved active
// file to archive/, so a 2nd complete_goal call at iter N+1 (final-report
// iter) previously returned "already completed (archived)" — but LLM
// training data may retry-complete_goal on errors, looping wastefully
// until cap. Now returns success with explicit "you may output your final
// report" signal so LLM switches to text-only mode at iter N+1.
func TestCompleteGoalTool_IdempotentSuccess(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "first"})

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "second"})
	if res.IsError {
		t.Errorf("expected success (not error) on 2nd complete_goal, got isErr=true err_kind=%q msg=%q",
			res.ErrKind, res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "already completed") {
		t.Errorf("expected 'already completed' message in success reply, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "no-op") {
		t.Errorf("expected 'no-op' marker in success reply (LLM signal to switch to text-only), got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Do NOT call any more tools") {
		t.Errorf("expected 'Do NOT call any more tools' directive, got: %s", res.ForLLM)
	}
}

// Phase 12.20.1 fix (C) negative regression test: ensure the "no goal
// set" branch still returns error (only the "already completed" branch
// became idempotent success). Two different semantic branches must not be
// conflated.
func TestCompleteGoalTool_NoGoalSentinel_StillError(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-no-goal", "agent")
	// No set_goal call — store has no goal at all.
	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "x"})
	if !res.IsError {
		t.Errorf("expected error when no goal exists, got isErr=false msg=%q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "no goal set") && !strings.Contains(res.ForLLM, "set_goal") {
		t.Errorf("expected 'no goal set / set_goal' directive, got: %s", res.ForLLM)
	}
}

// TestCompleteGoalTool_RequiresSummary verifies Phase 11: complete_goal
// must be called with a `summary` arg (1-500 chars). Empty / missing
// summary returns invalid_input so the LLM retries in the same
// iteration. The runtime cannot fabricate a final reply on the LLM's
// behalf — that would defeat the audit trail.
func TestCompleteGoalTool_RequiresSummary(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-summary", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	// Missing summary arg
	res := NewCompleteGoalTool(ws).Execute(ctx, nil)
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("missing summary: want invalid_input, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
	if !strings.Contains(res.ForLLM, "summary") {
		t.Errorf("missing summary: error message should mention 'summary', got: %s", res.ForLLM)
	}

	// Empty summary arg
	res = NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": ""})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("empty summary: want invalid_input, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}

	// Whitespace-only summary
	res = NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "   "})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("whitespace summary: want invalid_input, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}

	// Verify goal was NOT archived (still active after 3 invalid attempts).
	st := NewStore(ws)
	if g, _ := st.Read("sess-summary"); g == nil {
		t.Fatalf("expected goal to still be active after invalid summary attempts")
	}
	if g, _ := st.Read("sess-summary"); g != nil && g.Status != "active" {
		t.Errorf("expected status=active after invalid summary, got %q", g.Status)
	}
}

// TestCompleteGoalTool_PersistsSummary verifies the LLM-supplied
// `summary` is persisted in the archive file's YAML frontmatter so
// operators can read it post-hoc.
func TestCompleteGoalTool_PersistsSummary(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-persist", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "all done — foo and bar"})

	st := NewStore(ws)
	post, err := st.ReadAny("sess-persist")
	if err != nil {
		t.Fatalf("ReadAny: %v", err)
	}
	if post == nil {
		t.Fatalf("expected archive to be readable via ReadAny, got nil")
	}
	if post.Summary != "all done — foo and bar" {
		t.Errorf("Summary = %q, want %q", post.Summary, "all done — foo and bar")
	}
	if post.Status != "completed" {
		t.Errorf("Status = %q, want completed", post.Status)
	}
}

// ---------------------------------------------------------------------------
// Name/Description/Parameters shape.
// ---------------------------------------------------------------------------

func TestToolShapes(t *testing.T) {
	ws := tempWorkspace(t)
	cases := []struct {
		new  func() toolshared.Tool
		name string
	}{
		{func() toolshared.Tool { return NewSetGoalTool(ws) }, "set_goal"},
		{func() toolshared.Tool { return NewViewGoalTool(ws) }, "view_goal"},
		{func() toolshared.Tool { return NewGoalProgressTool(ws) }, "goal_progress"},
		{func() toolshared.Tool { return NewCompleteGoalTool(ws) }, "complete_goal"},
	}
	for _, c := range cases {
		tl := c.new()
		if tl.Name() != c.name {
			t.Errorf("Name(): got %q, want %q", tl.Name(), c.name)
		}
		if tl.Description() == "" {
			t.Errorf("%s: Description must be non-empty", c.name)
		}
		p := tl.Parameters()
		if p["type"] != "object" {
			t.Errorf("%s: Parameters should be {type: object,...}, got %v", c.name, p)
		}
	}
}

// TestSetGoalTool_RejectsInvalidNameChars guards Fix 4: schema says name must match
// ^[A-Za-z0-9_-]{1,64}$. Anything else (path traversal, spaces, unicode) must reject.
func TestSetGoalTool_RejectsInvalidNameChars(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent-main")
	cases := map[string]string{
		"path_traversal":   "../../etc/passwd",
		"space":            "has space",
		"unicode":          "café",
		"too_long":         strings.Repeat("a", 65),
		"slash":            "foo/bar",
		"colon":            "foo:bar",
		"empty_after_trim": "   ",
	}
	for label, bad := range cases {
		t.Run(label, func(t *testing.T) {
			res := NewSetGoalTool(ws).Execute(ctx, map[string]any{
				"name":             bad,
				"objective":        "x",
				"success_criteria": []string{"y"},
			})
			if !res.IsError {
				t.Fatalf("expected error for name=%q, got OK", bad)
			}
			if res.ErrKind != toolshared.ErrInvalidInput {
				t.Errorf("expected ErrInvalidInput, got %q", res.ErrKind)
			}
		})
	}
}

// TestStringSliceArg_WhitespaceOnlyDrops: after Fix 1, ["" ] or ["   "] (whitespace-only)
// passed as []any should be treated identically to []string — both drop whitespace-only
// entries. Goal stored with all-whitespace criteria must be rejected (no real criterion).
func TestStringSliceArg_WhitespaceOnlyDrops(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent-main")
	res := NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "ws",
		"objective":        "x",
		"success_criteria": []any{"", "   ", "\t\n"},
	})
	if !res.IsError {
		t.Fatalf("expected error for all-whitespace criteria, got OK")
	}
	if res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected ErrInvalidInput, got %q", res.ErrKind)
	}
}

// ---------------------------------------------------------------------------

// fakeExtender is a minimal IterationExtender used to verify the
// goal_progress → ExtendIterationCap wire contract without needing a real
// turnState. Tracks call count and last reason.
type fakeExtender struct {
	mu             sync.Mutex
	remaining      int
	canExtend      bool
	iterCap        int
	maxPerCheck    int
	maxCap         int
	extendCalls    int
	lastReason     string
	lastN          int
	// Phase 12.35: deferred-extend semantics.
	requestCalls       int // times RequestExtendIterationCap was invoked
	pendingFlushAmount int // amount staged by RequestExtendIterationCap
	pendingFlushReason string
	flushedApplied     int // times FlushPendingExtend returned applied=true
}

func (f *fakeExtender) RemainingIterations() int { return f.remaining }
func (f *fakeExtender) CanExtendIterationCap() bool { return f.canExtend }
func (f *fakeExtender) IterationCap() int {
	if f.iterCap == 0 {
		return 50
	}
	return f.iterCap
}
func (f *fakeExtender) MaxIterationsPerCheckpoint() int {
	if f.maxPerCheck == 0 {
		return 20
	}
	return f.maxPerCheck
}
func (f *fakeExtender) MaxIterationsCap() int {
	if f.maxCap == 0 {
		return 200
	}
	return f.maxCap
}
func (f *fakeExtender) ExtendIterationCap(n int, reason string) (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extendCalls++
	f.lastReason = reason
	f.lastN = n
	return f.IterationCap() + n, n
}
// RequestExtendIterationCap stages a deferred extend. Returns true unless
// at ceiling (matches real *turnState behavior). The fake's CanExtendIterationCap
// flag is the gate; if false, the request is rejected.
func (f *fakeExtender) RequestExtendIterationCap(n int, reason string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestCalls++
	if !f.canExtend {
		return false
	}
	if n <= 0 {
		return false
	}
	if f.maxCap > 0 && f.iterCap >= f.maxCap {
		return false
	}
	f.pendingFlushAmount += n
	if reason != "" {
		f.pendingFlushReason = reason
	}
	return true
}
// FlushPendingExtend applies any staged request (mimics the real agent loop
// end-of-iter hook). Returns applied=true if a request was staged.
func (f *fakeExtender) FlushPendingExtend() (bool, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pendingFlushAmount <= 0 {
		return false, f.iterCap, 0
	}
	delta := f.pendingFlushAmount
	f.iterCap += delta
	f.lastReason = f.pendingFlushReason
	f.lastN = delta
	f.extendCalls++
	f.flushedApplied++
	f.pendingFlushAmount = 0
	f.pendingFlushReason = ""
	return true, f.iterCap, delta
}
func (f *fakeExtender) iterationCap() int { return 50 }
func (f *fakeExtender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.extendCalls
}
func (f *fakeExtender) recorded() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReason, f.lastN
}

func TestGoalProgressTool_ExtendsIterationCap_WhenRemainingSteps_HasRoom(t *testing.T) {
	// Phase 10.1: pre-Phase 12.8, Tier 3 force-wrap-up stripped all tools
	// when RemainingIterations()==0, so the LLM could not call goal_progress
	// AT cap. Wire instead fires when still has iteration slots, proactively
	// adding room for the next iteration.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-extend", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 1, canExtend: true} // has slot, ceiling available
	ctx = WithIterationExtender(ctx, ext)

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"remaining_steps": []string{"run tests"},
		"next_action":     "run tests",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	// Phase 12.35: goal_progress defers ExtendIterationCap to end-of-iter via
	// FlushPendingExtend. Immediate post-Execute: callCount is still 0, only
	// requestCalls is incremented. The agent loop calls FlushPendingExtend
	// after ExecuteTools returns, before continuing the loop.
	if ext.callCount() != 0 {
		t.Errorf("expected 0 immediate ExtendIterationCap calls (deferred), got %d", ext.callCount())
	}
	// Simulate agent loop end-of-iter hook: extend should now apply.
	applied, _, delta := ext.FlushPendingExtend()
	if !applied {
		t.Errorf("expected FlushPendingExtend to apply the staged request, got applied=false")
	}
	if ext.callCount() != 1 {
		t.Errorf("expected 1 total ExtendIterationCap after flush, got %d", ext.callCount())
	}
	reason, n := ext.recorded()
	if !strings.Contains(reason, "goal_progress") {
		t.Errorf("expected reason to mention goal_progress, got %q", reason)
	}
	// Phase 11: extend amount = MaxIterationsPerCheckpoint (default 20),
	// not n=1 as in Phase 10.1. A single iteration is too small to be
	// useful for multi-step goals; per-checkpoint budget matches the
	// budget the runtime grants at Open → Checkpoint transition.
	if n != ext.MaxIterationsPerCheckpoint() {
		t.Errorf("expected n=%d (MaxIterationsPerCheckpoint), got %d", ext.MaxIterationsPerCheckpoint(), n)
	}
	_ = delta
}

func TestGoalProgressTool_NoExtend_WhenNoCanExtend(t *testing.T) {
	// Remaining>0 but ceiling reached (CanExtend==false) — wire must not fire.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-noextend", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 5, canExtend: false} // ceiling reached
	ctx = WithIterationExtender(ctx, ext)

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"remaining_steps": []string{"run tests"},
		"next_action":     "run tests",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	// Phase 12.35: deferred semantics — should not even have staged a request
	// when CanExtend==false. flushApplied stays 0.
	applied, _, _ := ext.FlushPendingExtend()
	if applied {
		t.Errorf("expected FlushPendingExtend to not apply when CanExtend=false, got applied=true")
	}
	if ext.callCount() != 0 {
		t.Errorf("expected 0 ExtendIterationCap calls when ceiling reached, got %d", ext.callCount())
	}
}

func TestGoalProgressTool_ExtendsIterationCap_AtCheckpointCap(t *testing.T) {
	// Phase 12.23: at GoalPhaseCheckpoint (iter == iterationCap), goal_progress
	// IS callable (Phase 12.8 deleted Tier 3 force-wrap, so tools are not
	// stripped at cap). The LLM calls goal_progress to lift the cap so it can
	// keep working through remaining_steps on subsequent iterations. Without
	// this wire, the goal_progress tool output is "burned" by the loop exit
	// and the user sees canned toolLimitResponse ("I've reached
	// max_tool_iterations...") instead of the goal progress summary.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-checkpoint", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 0, canExtend: true} // at cap, ceiling available
	ctx = WithIterationExtender(ctx, ext)

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"remaining_steps": []string{"run tests"},
		"next_action":     "run tests",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	// Phase 12.35: deferred extend. Immediately after Execute, callCount=0;
	// after FlushPendingExtend (simulating end-of-iter agent loop hook), it's 1.
	if ext.callCount() != 0 {
		t.Errorf("expected 0 immediate extend calls (deferred), got %d", ext.callCount())
	}
	applied, _, _ := ext.FlushPendingExtend()
	if !applied {
		t.Errorf("expected flush to apply, got applied=false")
	}
	if ext.callCount() != 1 {
		t.Errorf("expected 1 ExtendIterationCap call after flush (Checkpoint extend), got %d", ext.callCount())
	}
	reason, n := ext.recorded()
	if !strings.Contains(reason, "goal_progress") {
		t.Errorf("expected reason to mention goal_progress, got %q", reason)
	}
	if n != ext.MaxIterationsPerCheckpoint() {
		t.Errorf("expected n=%d (MaxIterationsPerCheckpoint), got %d", ext.MaxIterationsPerCheckpoint(), n)
	}
}

// Phase 12.24e (anh Maple decision, 2026-07-27): the previous ceiling-bound
// threshold (gap ≤ 3) was removed in favor of a simpler model — goal_progress
// at Checkpoint ALWAYS extends by MaxIterationsPerCheckpoint(), and
// ExtendIterationCap clamps to MaxIterationsCap internally (turn_state.go:591-594).
// These tests verify the new single-path behavior: extend is always called once
// with amount = MaxIterationsPerCheckpoint, regardless of gap size.

// helper: invoke goal_progress with given iterCap/maxCap and verify extend fired once with maxPerCheck amount
func assertCheckpointExtend(t *testing.T, ws string, iterCap, maxCap, maxPerCheck int) {
	t.Helper()
	ctx := ctxWithSession(fmt.Sprintf("sess-checkpoint-ext-%d-%d", iterCap, maxCap), "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 0, canExtend: true, iterCap: iterCap, maxCap: maxCap, maxPerCheck: maxPerCheck}
	ctx = WithIterationExtender(ctx, ext)

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"remaining_steps": []string{"wrap up"},
		"next_action":     "wrap up",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	// Phase 12.35: deferred extend semantics — callCount is 0 immediately
	// after Execute; only FlushPendingExtend triggers the actual extend.
	if ext.callCount() != 0 {
		t.Fatalf("expected 0 immediate ExtendIterationCap calls (deferred), got %d", ext.callCount())
	}
	applied, _, _ := ext.FlushPendingExtend()
	if !applied {
		t.Fatalf("expected FlushPendingExtend to apply, got applied=false")
	}
	if ext.callCount() != 1 {
		t.Fatalf("expected 1 ExtendIterationCap call after flush, got %d", ext.callCount())
	}
	reason, n := ext.recorded()
	if !strings.Contains(reason, "checkpoint extend") {
		t.Errorf("expected reason to mention 'checkpoint extend', got %q", reason)
	}
	if n != maxPerCheck {
		t.Errorf("expected n=%d (MaxIterationsPerCheckpoint request), got %d", maxPerCheck, n)
	}
}

func TestGoalProgressTool_CheckpointExtend_NormalGap(t *testing.T) {
	// iterCap=100, maxCap=200, maxPerCheck=20: large gap, full amount requested.
	ws := tempWorkspace(t)
	assertCheckpointExtend(t, ws, 100, 200, 20)
}

func TestGoalProgressTool_CheckpointExtend_SmallGapRequestUnchanged(t *testing.T) {
	// iterCap=196, maxCap=200, maxPerCheck=20: small gap. Request is still 20
	// (clamping to gap happens inside real turn_state.ExtendIterationCap, not
	// in our handler). Verifies the handler never short-circuits on gap size.
	ws := tempWorkspace(t)
	assertCheckpointExtend(t, ws, 196, 200, 20)
}

func TestGoalProgressTool_CheckpointExtend_OneSlotGap(t *testing.T) {
	// iterCap=199, maxCap=200, maxPerCheck=20: gap=1. Request is still 20.
	ws := tempWorkspace(t)
	assertCheckpointExtend(t, ws, 199, 200, 20)
}

func TestGoalProgressTool_CheckpointExtend_AtCeiling_NoExtend(t *testing.T) {
	// iterCap=200, maxCap=200: CanExtendIterationCap=false → no extend call.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-already-at-ceiling", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	ext := &fakeExtender{remaining: 0, canExtend: false, iterCap: 200, maxCap: 200, maxPerCheck: 20}
	ctx = WithIterationExtender(ctx, ext)
	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"remaining_steps": []string{"wrap up"},
		"next_action":     "wrap up",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if ext.callCount() != 0 {
		t.Errorf("expected 0 ExtendIterationCap calls when at ceiling, got %d", ext.callCount())
	}
}

func TestGoalProgressTool_NoExtend_WhenNoRemainingSteps(t *testing.T) {
	// Phase 12.20: remaining_steps is REQUIRED non-empty; Phase 12.23
	// additionally requires that empty remaining_steps does NOT trigger
	// extension even when remaining>0 + canExtend=true. Regression guard
	// after dropping the RemainingIterations() > 0 guard — the only
	// remaining gate is len(remaining) > 0.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-noremaining-extend", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 5, canExtend: true}
	ctx = WithIterationExtender(ctx, ext)

	// Note: empty remaining_steps — but Phase 12.20 returns ErrInvalidInput
	// BEFORE the extend wire fires, so the test asserts callCount==0 by
	// the time the Execute returns. We rely on the Phase 12.20 reject
	// message; we still want a positive control showing the wire is
	// gated on remaining_steps non-empty, which it is (the IsError short-
	// circuits the extension block).
	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"next_action":     "wait for user",
		// remaining_steps intentionally absent — Phase 12.20 reject.
	})
	if !res.IsError {
		t.Fatalf("expected error (Phase 12.20 empty remaining_steps), got success")
	}
	if ext.callCount() != 0 {
		t.Errorf("expected 0 ExtendIterationCap calls when remaining_steps empty, got %d", ext.callCount())
	}
}

func TestGoalProgressTool_RejectsWhenRemainingStepsEmpty(t *testing.T) {
	// Phase 12.20: remaining_steps is REQUIRED. Empty list is rejected
	// regardless of other populated fields (completed/blockers/next_action).
	// LLM must call complete_goal instead when there's no more tool work.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-noremaining", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 5, canExtend: true}
	ctx = WithIterationExtender(ctx, ext)

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code", "run tests"},
		"next_action":     "done",
		"blockers":        []string{"waiting for Maple review"},
		// no remaining_steps → must be rejected per Phase 12.20 strict rule.
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input when remaining_steps empty, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
	if ext.callCount() != 0 {
		t.Errorf("expected 0 ExtendIterationCap calls when rejected, got %d", ext.callCount())
	}
}

func TestGoalProgressTool_AcceptsRemainingSteps_WithBlockers(t *testing.T) {
	// Phase 12.20: remaining_steps non-empty is accepted even when blockers
	// are also populated. The remaining_steps drives iteration-cap
	// extension; blockers document the constraint but do not block extend.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-blockers-remaining", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 5, canExtend: true}
	ctx = WithIterationExtender(ctx, ext)

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"drafted SOP"},
		"blockers":        []string{"need legal sign-off"},
		"remaining_steps": []string{"apply legal feedback", "ship final"},
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	// Phase 12.35: deferred extend — callCount is 0 immediately after Execute.
	if ext.callCount() != 0 {
		t.Errorf("expected 0 immediate extend calls (deferred), got %d", ext.callCount())
	}
	applied, _, _ := ext.FlushPendingExtend()
	if !applied {
		t.Errorf("expected flush to apply, got applied=false")
	}
	if ext.callCount() != 1 {
		t.Errorf("expected 1 ExtendIterationCap call after flush when remaining_steps non-empty, got %d", ext.callCount())
	}
}

func TestGoalProgressTool_NoExtend_WhenExtenderAbsent(t *testing.T) {
	// Tools invoked outside normal pipeline (e.g. CLI direct) must not panic
	// when no extender is on ctx. Verify graceful no-op.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-noextender", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	// ctx has no extender attached

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"remaining_steps": []string{"run tests"},
		"next_action":     "run tests",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
}

// ---------------------------------------------------------------------------
// Phase 12.28.3: Fix A — UTF-8 rune counting on complete_goal summary
// ---------------------------------------------------------------------------

// vnRunes builds a string of n Vietnamese runes (multi-byte UTF-8 chars).
// "Phòng" is 5 runes but 7 bytes (Ph=2, ò=2, ng=2, g=1) — i.e. byte count > rune count.
func vnRunes(n int) string {
	const vn = "Phòng "
	var b strings.Builder
	for utf8.RuneCountInString(b.String()) < n {
		b.WriteString(vn)
	}
	s := b.String()
	// Trim if we overshot
	for utf8.RuneCountInString(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// T-A1: 500-rune VN summary (700+ bytes) — must SUCCEED.
func TestCompleteGoalTool_AcceptsVN_SummaryAtRuneLimit(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	summary := vnRunes(500)
	if utf8.RuneCountInString(summary) != 500 {
		t.Fatalf("test setup error: vnRunes(500) produced %d runes", utf8.RuneCountInString(summary))
	}
	if len(summary) <= 500 {
		t.Fatalf("test setup error: VN runes should be multi-byte; got %d bytes for 500 runes", len(summary))
	}

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": summary,
	})
	if res.IsError {
		t.Fatalf("500-rune VN summary should succeed (len=%d bytes, runes=%d); got isErr=true err_kind=%q msg=%q",
			len(summary), utf8.RuneCountInString(summary), res.ErrKind, res.ForLLM)
	}
}

// T-A2: 501-rune VN summary — must REJECT with clear error.
func TestCompleteGoalTool_RejectsVN_SummaryOverRuneLimit(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-B", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	summary := vnRunes(501)

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": summary,
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("501-rune VN summary should reject; got isErr=%v kind=%q msg=%q",
			res.IsError, res.ErrKind, res.ForLLM)
	}
}

// T-A3: 500-byte ASCII summary — must SUCCEED (regression: byte count was correct here).
func TestCompleteGoalTool_AcceptsASCII_SummaryAtByteLimit(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-C", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	summary := strings.Repeat("a", 500)

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": summary,
	})
	if res.IsError {
		t.Fatalf("500-char ASCII summary should succeed; got isErr=true err_kind=%q msg=%q", res.ErrKind, res.ForLLM)
	}
}

// T-A4: 500-rune emoji summary (multi-byte, non-VN) — must SUCCEED.
func TestCompleteGoalTool_AcceptsEmoji_SummaryAtRuneLimit(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-D", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	summary := strings.Repeat("🦞", 500)

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": summary,
	})
	if res.IsError {
		t.Fatalf("500-rune emoji summary should succeed; got isErr=true err_kind=%q msg=%q", res.ErrKind, res.ForLLM)
	}
}

// T-A5: 500-rune mixed VN/EN/emoji, boundary check.
func TestCompleteGoalTool_AcceptsMixed_500RunesNoRejection(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-E", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	var b strings.Builder
	for utf8.RuneCountInString(b.String()) < 500 {
		switch b.Len() % 3 {
		case 0:
			b.WriteString("a")
		case 1:
			b.WriteString("Phòng ")
		case 2:
			b.WriteString("🦞")
		}
	}
	// Trim if we overshot
	s := b.String()
	for utf8.RuneCountInString(s) > 500 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	summary := s
	if utf8.RuneCountInString(summary) != 500 {
		t.Fatalf("test setup error: expected 500 runes, got %d", utf8.RuneCountInString(summary))
	}

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": summary,
	})
	if res.IsError {
		t.Fatalf("mixed 500-rune summary should succeed; got isErr=true err_kind=%q msg=%q", res.ErrKind, res.ForLLM)
	}
}

// T-A6: 501-byte ASCII summary — must REJECT (rune count must mean BOTH byte and rune cap).
func TestCompleteGoalTool_RejectsASCII_SummaryAt501Bytes(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-F", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	summary := strings.Repeat("a", 501)

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": summary,
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("501-byte ASCII summary should reject; got isErr=%v kind=%q msg=%q",
			res.IsError, res.ErrKind, res.ForLLM)
	}
}

// T-A7: empty summary — separate code path (validation runs before rune count check).
func TestCompleteGoalTool_EmptySummary_StillAccepted(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-G", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{
		"summary": "",
	})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("empty summary should hit required-field check (ErrInvalidInput), got isErr=%v kind=%q",
			res.IsError, res.ErrKind)
	}
}

// TestGoalProgressResultStatesCapExtension (Phase 12.38 §5 F52): the
// goal_progress tool result must tell the LLM what happened to the
// iteration cap (extended from X to Y). The previous "Logged progress
// entry #N for session S." text contained NO cap info, so the LLM could
// not tell whether its extend request was honored — leading to re-calls
// at OPEN and the checkpoint whiplash pattern from main-turn-3.
func TestGoalProgressResultStatesCapExtension(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("PICOCLAW_MEDIA_DIR", tmpDir)
	sessionKey := "sk_v1_cap_ext_test"

	// Seed goal via set_goal
	NewSetGoalTool(tmpDir).Execute(ctxWithSession(sessionKey, "agent-main"), map[string]any{
		"name":             "goal_test_cap",
		"objective":        "Test cap extension visibility",
		"success_criteria": []string{"LLM sees the cap extension line"},
		"in_scope":         []string{"extend cap", "log progress"},
	})

	ext := &mockExtenderForTest{currentCap: 5, maxCap: 200, maxPerCheckpoint: 10, canExtend: true}
	ctx := WithIterationExtender(ctxWithSession(sessionKey, "agent-main"), ext)

	tool := NewGoalProgressTool(tmpDir)
	res := tool.Execute(ctx, map[string]any{
		"name":            "goal_test_cap",
		"remaining_steps": []string{"finish the work"},
	})
	if res.IsError {
		t.Fatalf("goal_progress returned error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Iteration cap was extended from 5 to 15") {
		t.Errorf("result must state cap extension line (Phase 12.38 §5), got: %s", res.ForLLM)
	}
}

func TestGoalProgressResultStatesCapAtCeiling(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("PICOCLAW_MEDIA_DIR", tmpDir)
	sessionKey := "sk_v1_cap_ceiling_test"

	NewSetGoalTool(tmpDir).Execute(ctxWithSession(sessionKey, "agent-main"), map[string]any{
		"name":             "goal_test_ceiling",
		"objective":        "Test ceiling state in result",
		"success_criteria": []string{"LLM sees ceiling warning"},
		"in_scope":         []string{"log at ceiling"},
	})

	ext := &mockExtenderForTest{currentCap: 200, maxCap: 200, maxPerCheckpoint: 10, canExtend: false}
	ctx := WithIterationExtender(ctxWithSession(sessionKey, "agent-main"), ext)

	tool := NewGoalProgressTool(tmpDir)
	res := tool.Execute(ctx, map[string]any{
		"name":            "goal_test_ceiling",
		"remaining_steps": []string{"finish the work"},
	})
	if res.IsError {
		t.Fatalf("goal_progress returned error: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "Iteration cap was extended") {
		t.Errorf("result must NOT claim extension when at ceiling, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "at the absolute ceiling") {
		t.Errorf("result must mention ceiling state, got: %s", res.ForLLM)
	}
}

type mockExtenderForTest struct {
	currentCap       int
	maxCap           int
	maxPerCheckpoint int
	canExtend        bool
}

func (m *mockExtenderForTest) RemainingIterations() int         { return 0 }
func (m *mockExtenderForTest) CanExtendIterationCap() bool       { return m.canExtend }
func (m *mockExtenderForTest) ExtendIterationCap(n int, reason string) (int, int) {
	m.currentCap += n
	if m.currentCap > m.maxCap {
		m.currentCap = m.maxCap
	}
	return m.currentCap, n
}
func (m *mockExtenderForTest) IterationCap() int              { return m.currentCap }
func (m *mockExtenderForTest) MaxIterationsPerCheckpoint() int { return m.maxPerCheckpoint }
func (m *mockExtenderForTest) MaxIterationsCap() int           { return m.maxCap }
func (m *mockExtenderForTest) RequestExtendIterationCap(n int, reason string) bool {
	if m.currentCap >= m.maxCap {
		return false
	}
	m.currentCap += n
	if m.currentCap > m.maxCap {
		m.currentCap = m.maxCap
	}
	return true
}
func (m *mockExtenderForTest) FlushPendingExtend() (applied bool, newCap int, delta int) {
	return false, m.currentCap, 0
}

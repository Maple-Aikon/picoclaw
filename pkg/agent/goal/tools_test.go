// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

func TestCompleteGoalTool_IdempotentGuard(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-A", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})
	NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "first"})

	res := NewCompleteGoalTool(ws).Execute(ctx, map[string]any{"summary": "second"})
	if !res.IsError || res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("expected invalid_input on second call, got isErr=%v kind=%q", res.IsError, res.ErrKind)
	}
	if !strings.Contains(res.ForLLM, "already completed") {
		t.Errorf("expected 'already completed' message, got: %s", res.ForLLM)
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
	extendCalls    int
	lastReason     string
	lastN          int
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
func (f *fakeExtender) ExtendIterationCap(n int, reason string) (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extendCalls++
	f.lastReason = reason
	f.lastN = n
	return f.IterationCap() + n, n
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
	if ext.callCount() != 1 {
		t.Errorf("expected 1 ExtendIterationCap call, got %d", ext.callCount())
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
	if ext.callCount() != 0 {
		t.Errorf("expected 0 ExtendIterationCap calls when ceiling reached, got %d", ext.callCount())
	}
}

func TestGoalProgressTool_NoExtend_WhenAtCeiling(t *testing.T) {
	// Both remaining==0 (no iteration slot in hand; Phase 12.8 removed
	// Tier 3 force-wrap so goal_progress IS callable at cap, but our guard
	// still requires RemainingIterations > 0 to avoid ordering edge cases
	// with the loop-cap check) AND CanExtend==false — wire must not fire.
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-ceiling", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 0, canExtend: false} // at cap + ceiling
	ctx = WithIterationExtender(ctx, ext)

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code"},
		"remaining_steps": []string{"run tests"},
		"next_action":     "run tests",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if ext.callCount() != 0 {
		t.Errorf("expected 0 ExtendIterationCap calls when at cap+ceiling, got %d", ext.callCount())
	}
}

func TestGoalProgressTool_NoExtend_WhenNoRemainingSteps(t *testing.T) {
	ws := tempWorkspace(t)
	ctx := ctxWithSession("sess-noremaining", "agent")
	NewSetGoalTool(ws).Execute(ctx, map[string]any{
		"name":             "n",
		"objective":        "o",
		"success_criteria": []string{"c"},
	})

	ext := &fakeExtender{remaining: 0, canExtend: true}
	ctx = WithIterationExtender(ctx, ext)

	res := NewGoalProgressTool(ws).Execute(ctx, map[string]any{
		"completed_steps": []string{"write code", "run tests"},
		"next_action":     "done",
		// no remaining_steps → goal effectively complete, should NOT extend.
	})
	if res.IsError {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if ext.callCount() != 0 {
		t.Errorf("expected 0 ExtendIterationCap calls when remaining_steps empty, got %d", ext.callCount())
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

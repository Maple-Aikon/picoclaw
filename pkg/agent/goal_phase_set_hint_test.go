package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestGoalPhaseSetHint_FiresOnlyInSetPhase(t *testing.T) {
	tests := []struct {
		name      string
		goalPhase string
		want      bool
	}{
		{name: "Set phase — fires", goalPhase: string(GoalPhaseSet), want: true},
		{name: "Open phase — silent", goalPhase: string(GoalPhaseOpen), want: false},
		{name: "Checkpoint phase — silent", goalPhase: string(GoalPhaseCheckpoint), want: false},
		{name: "Final phase — silent", goalPhase: string(GoalPhaseFinal), want: false},
		{name: "Empty phase — silent", goalPhase: "", want: false},
		{name: "Bogus phase — silent", goalPhase: "unknown", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: tt.goalPhase})
			if tt.want && got == nil {
				t.Errorf("expected hint part for phase %q, got nil", tt.goalPhase)
			}
			if !tt.want && got != nil {
				t.Errorf("expected nil hint for phase %q, got part %q", tt.goalPhase, got.ID)
			}
		})
	}
}

func TestGoalPhaseSetHint_ContentMentionsSetGoal(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	mustContain(t, part.Content, "set_goal",
		"hint must reference set_goal so LLM knows which tool is allowed")
}

func TestGoalPhaseSetHint_ContentMentionsLockedTools(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	mustContain(t, part.Content, "locked",
		"hint must state that other tools are temporarily locked")
}

func TestGoalPhaseSetHint_ContentMentionsTwoForwardPaths(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	// Both paths must be reachable in the text so the LLM picks based on
	// turn context, not defaults to one path only.
	mustContain(t, part.Content, "set_goal",
		"hint path 1: call set_goal first")
	mustContain(t, part.Content, "respond directly",
		"hint path 2: no-tool reply is allowed")
}

// TestGoalPhaseSetHint_IncludesSetGoalArgShapeGuide (Phase 12.12) — the hint
// must teach LLM the exact top-level field shape of set_goal args. Without
// this, the LLM wraps args in {"raw": "..."} or omits required fields (like
// name), causing silent validation failure and a stuck GoalPhaseSet.
// Lesson learned from goal "crg-update-latest" aborted 2026-07-25 08:10 ICT
// with abort_reason: bexhausted:goal_recovery — LLM tried set_goal once with
// {"raw": "{\"cadence\":..."} → tool rejected with "missing required property
// name" → LLM gave up and tried other tools (all blocked by Set allowlist) →
// body never advanced → archive after BoundedRetry exhausted.
func TestGoalPhaseSetHint_IncludesSetGoalArgShapeGuide(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	// The example must explicitly show top-level field names — name, objective,
	// success_criteria — so LLM knows the wrapper pattern is wrong.
	mustContain(t, part.Content, "name",
		"hint must mention 'name' as a top-level required field")
	mustContain(t, part.Content, "objective",
		"hint must mention 'objective' as a top-level required field")
	mustContain(t, part.Content, "success_criteria",
		"hint must mention 'success_criteria' as a top-level required field")
	// Anti-pattern: must warn against {"raw": ...} wrapper
	mustContain(t, part.Content, "raw",
		"hint must explicitly warn against the {\"raw\": ...} wrapper pattern")
	mustContain(t, part.Content, "top-level",
		"hint must explicitly state 'top-level' to anchor the shape expectation")
	// Regex constraint for name field — matches rego rule ^[A-Za-z0-9_-]{1,64}$
	mustContain(t, part.Content, "[A-Za-z0-9_-]",
		"hint must document the name regex constraint so LLM avoids invalid chars")
}

func TestGoalPhaseSetHint_PlacementCapabilityTooling(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	if part.Layer != PromptLayerCapability {
		t.Errorf("expected Layer=Capability, got %q", part.Layer)
	}
	if part.Slot != PromptSlotTooling {
		t.Errorf("expected Slot=Tooling, got %q", part.Slot)
	}
	if part.Source.ID != PromptSourceGoalPhaseSetHint {
		t.Errorf("expected Source.ID=%q, got %q", PromptSourceGoalPhaseSetHint, part.Source.ID)
	}
}

func mustContain(t *testing.T, haystack, needle, rationale string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected haystack to contain %q (%s), got:\n%s", needle, rationale, haystack)
	}
}

// TestGoalPhaseSetHint_Integration_BuildSystemPromptParts verifies the hint
// is injected into the actual prompt parts emitted by the context builder when
// GoalPhase=Set. Phase 12.3 wiring: promptBuildRequestForTurn populates
// req.GoalPhase from ts.currentGoalPhase(); BuildMessagesFromPrompt passes
// it through to systemPromptBuildOptions.GoalPhase; buildSystemPromptParts
// fires goalPhaseSetHintContributor when GoalPhase==Set.
func TestGoalPhaseSetHint_Integration_BuildSystemPromptParts(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
		IncludeToolUseRule: true,
		GoalPhase:           string(GoalPhaseSet),
		// Phase 12.16.1: iter must be populated for the hint header to
		// render "(iter N)" correctly. Tests typically run as iter 1.
		Iteration:           1,
	})
	// Hunt for our hint part by ID (stable identifier, no race on text edits).
	found := false
	for _, p := range parts {
		if p.Source.ID == PromptSourceGoalPhaseSetHint {
			found = true
			if !strings.Contains(p.Content, "set_goal") {
				t.Errorf("GoalPhaseSet hint part missing set_goal reference; got:\n%s", p.Content)
			}
			if !strings.Contains(p.Content, "respond directly") {
				t.Errorf("GoalPhaseSet hint part missing no-tool reply path; got:\n%s", p.Content)
			}
			break
		}
	}
	if !found {
		// Print part IDs to aid debugging if the wiring breaks.
		ids := make([]string, 0, len(parts))
		for _, p := range parts {
			ids = append(ids, string(p.Source.ID))
		}
		t.Fatalf("expected hint part %q in prompt parts; got parts: %v", PromptSourceGoalPhaseSetHint, ids)
	}
}

// TestGoalPhaseSetHint_Integration_OpenPhase_NotInjected confirms the hint is
// gated to Set phase only and does NOT bleed into Open/Checkpoint/Final turns.
func TestGoalPhaseSetHint_Integration_OpenPhase_NotInjected(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	for _, phase := range []string{
		string(GoalPhaseOpen),
		string(GoalPhaseCheckpoint),
		string(GoalPhaseFinal),
	} {
		parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
			IncludeToolUseRule: true,
			GoalPhase:           phase,
			Iteration:           0,
		})
		for _, p := range parts {
			if p.Source.ID == PromptSourceGoalPhaseSetHint {
				t.Errorf("GoalPhaseSet hint should NOT appear in phase %q; got part: %q", phase, p.ID)
			}
		}
	}
}
// TestGoalPhaseSetHint_IterationHeaderReflectsReq (Phase 12.16.1): the hint
// header must show the actual iter from the request, not a hardcoded
// "(iter 1)". Without this fix the iter 1 prompt was cached and reused at
// later iters, producing a wrong-context header that confused the LLM
// during the main-turn-4 oscillation.
//
// Verifies the iter in the header is the value supplied via
// PromptBuildRequest.Iteration, not a static fallback. The header format is
// "Goal phase: SET (iter N)." where N is the iter.
func TestGoalPhaseSetHint_IterationHeaderReflectsReq(t *testing.T) {
	cb := NewContextBuilder(t.TempDir())

	for _, tc := range []struct {
		name string
		iter int
	}{
		{"iter 1", 1},
		{"iter 5", 5},
		{"iter 17", 17},
		{"iter 0 (defaults to 1)", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
				IncludeToolUseRule: true,
				GoalPhase:           string(GoalPhaseSet),
				Iteration:           tc.iter,
			})

			var hint *PromptPart
			for i := range parts {
				if parts[i].ID == "capability.goal_phase_set_hint" {
					hint = &parts[i]
					break
				}
			}
			if hint == nil {
				t.Fatalf("GoalPhaseSet hint part not found in parts for iter=%d", tc.iter)
			}

			expectedIter := tc.iter
			if expectedIter == 0 {
				expectedIter = 1
			}
			wantHeader := fmt.Sprintf("Goal phase: SET (iter %d).", expectedIter)
			if !strings.Contains(hint.Content, wantHeader) {
				t.Errorf("hint header missing %q in iter=%d build; got:\n%s", wantHeader, tc.iter, hint.Content[:intMinHint(500, len(hint.Content))])
			}
		})
	}
}

// intMinHint helper for substring slicing in tests (avoids name collision with
// the Go 1.21+ builtin min on two ints; also avoids sharing a helper
// across packages).
func intMinHint(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestGoalPhaseSetHint_ContentMentionsConversationalPath (Phase 12.17): the
// hint must give LLM a concrete list of conversational patterns that
// should pick path 2 (no set_goal, plain text reply). Without this list
// MiniMax-M3 falls back to cycling canned "max_tool_iterations" strings
// when the user's message is a conversational turn or a single-question
// tool lookup that doesn't need multi-step work.
//
// Regression-proof for main-turn-2 (2026-07-26 12:40 ICT) where user
// asked "Em check ai_suite.hcl xem pmc đang chạy LiteLLM bằng binary
// nào vậy" — LLM should have answered with path 2 (plain text reply
// explaining it can't access that file in GoalPhaseSet) but instead
// emitted canned error string and ended turn.
func TestGoalPhaseSetHint_ContentMentionsConversationalPath(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	// Path 2 must say "do NOT call set_goal" explicitly so LLM treats
	// single-reply turns as valid path 2, not as goal-required.
	mustContain(t, part.Content, "do NOT call set_goal",
		"hint path 2 must explicitly say do NOT call set_goal")
	// Concrete examples of conversational patterns so LLM recognizes them
	mustContain(t, part.Content, "got it",
		"hint must mention concrete conversational examples like 'got it'")
	mustContain(t, part.Content, "training data",
		"hint must mention training data / own knowledge as path 2")
	// Path 1 examples must also be present so LLM knows when to call set_goal
	mustContain(t, part.Content, "multi-step",
		"hint must mention multi-step as path 1 trigger")
}

// TestGoalPhaseSetHint_ContentWarnsAgainstCannedErrors (Phase 12.17):
// MiniMax-M3 has a known tendency to echo canned "max_tool_iterations"
// strings from its training data when history is polluted. The hint
// must explicitly tell LLM not to emit such canned strings, treating
// them as stale artifacts.
//
// Regression-proof for main-turn-2 (2026-07-26 12:40 ICT) where LLM
// emitted exactly this canned string as the final reply.
func TestGoalPhaseSetHint_ContentWarnsAgainstCannedErrors(t *testing.T) {
	part := goalPhaseSetHintContributor(PromptBuildRequest{GoalPhase: string(GoalPhaseSet)})
	if part == nil {
		t.Fatal("expected hint part for Set phase")
	}
	// Must reference the specific canned string so LLM recognizes it
	mustContain(t, part.Content, "max_tool_iterations",
		"hint must explicitly mention the canned error string LLM is prone to echo")
	// Must tell LLM to treat it as stale artifact, not reply
	mustContain(t, part.Content, "stale",
		"hint must tell LLM to treat canned strings as stale artifacts")
	// Must tell LLM not to repeat it
	mustContain(t, part.Content, "do NOT repeat",
		"hint must explicitly forbid repeating canned error strings")
}

package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// Phase 12.80 (Goi A / P2) — the goal-phase banner must reflect the CURRENT
// runtime phase, not the phase of the turn's first iteration.
//
// Regression test for the stale-banner bug: the banner is rendered into the
// USER message tail, but the phase-change rebuild hook used to replace only
// messages[0] (the system prompt) and keep messages[1:] verbatim. The LLM
// therefore read phase="SET" at CHECKPOINT — contradicting the runtime
// allowlist and driving the 52.9% gate-rejected misfire rate.

// nl is a newline. Built at runtime so this file contains no escape
// sequences inside string literals.
const nl = string(rune(10))

// bannerPhase extracts the phase attribute from a <goal_phase ...> block.
func bannerPhase(content string) string {
	const marker = `<goal_phase phase="`
	start := strings.Index(content, marker)
	if start < 0 {
		return ""
	}
	rest := content[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// bannerIter extracts the phase_start_iter attribute from a
// <goal_phase ...> block.
//
// H2 (2026-11-09): the banner no longer carries the LIVE iteration —
// that value changed on every iteration and pinned the provider prefix
// cache. It carries phase_start_iter instead, so the banner is
// byte-stable for the whole phase.
//
// NOTE the needle: `phase_start_iter="` does NOT contain ` iter="`
// (the preceding character is `_`, not a space), so the old marker
// silently returned an empty string after H2.
func bannerIter(content string) string {
	const marker = ` phase_start_iter="`
	start := strings.Index(content, marker)
	if start < 0 {
		return ""
	}
	rest := content[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// TestRefreshGoalPhaseBanner_ReplacesStalePhaseInUserMessage pins the core
// contract: given a user message carrying a SET-phase banner, refreshing it
// with a CHECKPOINT banner must yield a CHECKPOINT banner in the SAME
// message, and the user's original text must survive.
func TestRefreshGoalPhaseBanner_ReplacesStalePhaseInUserMessage(t *testing.T) {
	setBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseSet),
		Iteration:        1,
		MaxIterationsCap: 250,
	})
	if setBanner == "" {
		t.Fatal("expected a SET banner to be rendered")
	}

	checkpointBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseCheckpoint),
		Iteration:        25,
		MaxIterationsCap: 250,
	})
	if checkpointBanner == "" {
		t.Fatal("expected a CHECKPOINT banner to be rendered")
	}

	messages := []providers.Message{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: "hello there" + bannerSeparator + setBanner},
	}

	out := refreshGoalPhaseBannerInMessages(messages, 1, checkpointBanner)

	if got := bannerPhase(out[1].Content); got != "CHECKPOINT" {
		t.Errorf("expected banner phase = CHECKPOINT, got %q"+"%s%s", got, nl, out[1].Content)
	}
	if got := bannerIter(out[1].Content); got != "25" {
		t.Errorf("expected banner iter = 25, got %q", got)
	}
	if !strings.Contains(out[1].Content, "hello there") {
		t.Errorf("original user text was lost: %s%s", nl, out[1].Content)
	}
	// The SET banner must be gone — not duplicated.
	if strings.Contains(out[1].Content, `phase="SET"`) {
		t.Errorf("stale SET banner still present: %s%s", nl, out[1].Content)
	}
	if strings.Count(out[1].Content, "<goal_phase") != 1 {
		t.Errorf("expected exactly one banner block, got %d: %s%s",
			strings.Count(out[1].Content, "<goal_phase"), nl, out[1].Content)
	}
}

// TestRefreshGoalPhaseBanner_DoesNotMutateInput pins the copy-on-write
// contract: exec.messages may be shared with other holders, so the input
// slice must not be mutated in place.
func TestRefreshGoalPhaseBanner_DoesNotMutateInput(t *testing.T) {
	setBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseSet),
		Iteration:        1,
		MaxIterationsCap: 250,
	})
	checkpointBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseCheckpoint),
		Iteration:        25,
		MaxIterationsCap: 250,
	})

	original := "hello" + bannerSeparator + setBanner
	messages := []providers.Message{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: original},
	}

	out := refreshGoalPhaseBannerInMessages(messages, 1, checkpointBanner)

	if messages[1].Content != original {
		t.Errorf("input slice was mutated in place: got %q want %q", messages[1].Content, original)
	}
	if out[1].Content == original {
		t.Error("expected output to differ from input")
	}
}

// TestRefreshGoalPhaseBanner_AppendsWhenMissing covers the case where the
// current turn's user message has no banner yet (e.g. the banner was
// stripped or the turn started from a tool-result-only shape).
func TestRefreshGoalPhaseBanner_AppendsWhenMissing(t *testing.T) {
	checkpointBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseCheckpoint),
		Iteration:        25,
		MaxIterationsCap: 250,
	})

	messages := []providers.Message{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: "just the user text"},
	}

	out := refreshGoalPhaseBannerInMessages(messages, 1, checkpointBanner)

	if got := bannerPhase(out[1].Content); got != "CHECKPOINT" {
		t.Errorf("expected appended banner phase = CHECKPOINT, got %q", got)
	}
	if !strings.Contains(out[1].Content, "just the user text") {
		t.Errorf("original user text was lost: %s%s", nl, out[1].Content)
	}
}

// TestRefreshGoalPhaseBanner_LeavesHistoryAlone pins that messages BEFORE
// currentTurnStart (history) are never modified.
func TestRefreshGoalPhaseBanner_LeavesHistoryAlone(t *testing.T) {
	setBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseSet),
		Iteration:        1,
		MaxIterationsCap: 250,
	})
	checkpointBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseCheckpoint),
		Iteration:        25,
		MaxIterationsCap: 250,
	})

	historyMsg := "old turn" + bannerSeparator + setBanner
	messages := []providers.Message{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: historyMsg},
		{Role: "user", Content: "current turn" + bannerSeparator + setBanner},
	}

	out := refreshGoalPhaseBannerInMessages(messages, 2, checkpointBanner)

	if out[1].Content != historyMsg {
		t.Errorf("history message was modified: got %q want %q", out[1].Content, historyMsg)
	}
	if got := bannerPhase(out[2].Content); got != "CHECKPOINT" {
		t.Errorf("expected current-turn banner phase = CHECKPOINT, got %q", got)
	}
}

// TestRefreshGoalPhaseBanner_StripsWhenEmpty covers the "no banner wanted"
// branch: the block must be removed and the surrounding text kept.
func TestRefreshGoalPhaseBanner_StripsWhenEmpty(t *testing.T) {
	setBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseSet),
		Iteration:        1,
		MaxIterationsCap: 250,
	})

	messages := []providers.Message{
		{Role: "system", Content: "SYSTEM"},
		{Role: "user", Content: "hello" + bannerSeparator + setBanner},
	}

	out := refreshGoalPhaseBannerInMessages(messages, 1, "")

	if strings.Contains(out[1].Content, "<goal_phase") {
		t.Errorf("banner was not stripped: %s%s", nl, out[1].Content)
	}
	if !strings.Contains(out[1].Content, "hello") {
		t.Errorf("original user text was lost: %s%s", nl, out[1].Content)
	}
}

// TestPhaseRebuild_RefreshesBannerOnPhaseChange is the WIRE-level test: it
// drives the real rebuild hook (the production path that runs when the
// phase changes mid-turn) and asserts the user message's banner reports the
// NEW phase, not the stale first-iteration phase.
//
// This is the test that fails on the pre-fix code.
func TestPhaseRebuild_RefreshesBannerOnPhaseChange(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, "", 0, GoalPhaseSet)

	setBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseSet),
		Iteration:        1,
		MaxIterationsCap: 250,
	})

	// Simulate the turn's user message carrying the SET-phase banner that
	// was rendered at iteration 1.
	messages = append(messages, providers.Message{
		Role:    "user",
		Content: "user request" + bannerSeparator + setBanner,
	})
	exec.currentTurnStart = 1

	// Now the phase flips to CHECKPOINT (cap reached) and the rebuild hook runs.
	ts.agent.PhaseOverrideForTest = string(GoalPhaseCheckpoint)

	result := ts.maybeRebuildPromptForStateChange(messages, exec, nil, 25)

	// A1 (2026-11-17): the banner is NOT refreshed in place inside the
	// turn-root user message any more — that mid-array rewrite is
	// what killed the provider prefix cache for the whole current turn
	// (measured: uncached / this-turn-traffic ~= 1.00). The banner now
	// lives in a dedicated USER message at the TAIL of the request.
	//
	// So this test now asserts the NEW contract (contract C5: update the
	// old test to assert the NEW location, do not delete it):
	//   1. the turn-root user message keeps its original text and stays
	//      byte-constant (NO banner in it);
	//   2. the banner itself is rendered from the CURRENT phase.
	rootMsg := ""
	for _, m := range result {
		if m.Role == "user" {
			rootMsg = m.Content
			break
		}
	}
	if rootMsg == "" {
		t.Fatal("expected a user message in the rebuilt result")
	}
	// The rebuild hook must NOT mutate the turn-root user message at all —
	// it stays byte-identical to what the turn was built with. (The
	// fixture pre-seeds a SET banner to simulate the legacy shape; A1
	// guarantees the hook leaves it alone instead of rewriting it
	// in place.)
	if rootMsg != "user request"+bannerSeparator+setBanner {
		t.Errorf("the rebuild hook mutated the turn-root user message (A1): %s%s", nl, rootMsg)
	}
	if !strings.Contains(rootMsg, "user request") {
		t.Errorf("original user text was lost: %s%s", nl, rootMsg)
	}

	// The affordance rendered for the CURRENT phase must say CHECKPOINT
	// with the current phase-start iteration (25).
	ts.agent.PhaseOverrideForTest = string(GoalPhaseCheckpoint)
	affordance := phaseAffordanceText(phaseAffordanceRequest(ts.agent.ContextBuilder, ts, 25))
	got := bannerPhase(affordance)
	if got == "SET" {
		t.Errorf("banner still reports SET after a CHECKPOINT phase change (stale-banner bug): %s%s", nl, affordance)
	}
	if got != "CHECKPOINT" {
		t.Errorf("expected banner phase = CHECKPOINT, got %q%s%s", got, nl, affordance)
	}
	if gotIter := bannerIter(affordance); gotIter != "25" {
		t.Errorf("expected banner iter = 25, got %q", gotIter)
	}
}

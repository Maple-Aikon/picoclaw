package agent

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ---------------------------------------------------------------------------
// H2 — PHASE-STABLE GOAL-PHASE BANNER (2026-11-09)
// ---------------------------------------------------------------------------
//
// The turn-tail <goal_phase> banner lives in the user message that OPENS the
// turn — in the MIDDLE of the request. Before H2 it carried the LIVE
// iteration, so its bytes changed on every iteration, which pinned the
// provider prefix cache (measured: cache_read frozen at 37.632 tokens while
// uncached grew linearly; the rebuild hook fired 1312/1312 times).
//
// These tests pin the H2 contract:
//   - the banner is byte-identical for every iteration inside one phase;
//   - the banner changes exactly once per phase TRANSITION;
//   - the rebuild hook does NOT fire on an iteration change inside a
//     cacheable phase (OPEN), but still fires for non-cacheable phases
//     whose system hint text embeds the live iteration.
//
// The live iteration is NOT visible to the model: the banner carries
// phase_start_iter. (The user-facing tool-feedback line does show the live
// iteration, but that line is display-only metadata and never reaches the
// provider — see the note on PromptBuildRequest.PhaseStartIter.)

// bannerPhaseStartIter extracts the phase_start_iter attribute from a
// <goal_phase ...> block.
func bannerPhaseStartIter(content string) string {
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

// bannerHash is a stable digest of the banner block in a message body.
func bannerHash(content string) string {
	loc := goalPhaseBannerBlockRe.FindStringIndex(content)
	if loc == nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content[loc[0]:loc[1]])))
}

// TestH2_Banner_ByteStableWithinPhase is the CORE H2 contract: two
// different iterations inside the SAME phase must produce byte-identical
// banners.
//
// This is the test that fails on the pre-H2 code (the old banner carried
// `iter="N"`).
func TestH2_Banner_ByteStableWithinPhase(t *testing.T) {
	cases := []struct {
		name    string
		phase   GoalPhase
		iterA   int
		iterB   int
		maxIter int
		iterCap int
	}{
		{"OPEN", GoalPhaseOpen, 3, 24, 250, 25},
		{"SET", GoalPhaseSet, 1, 1, 250, 25},
		{"CHECKPOINT", GoalPhaseCheckpoint, 25, 26, 250, 25},
		{"FINAL", GoalPhaseFinal, 250, 251, 250, 25},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := PromptBuildRequest{
				GoalPhase:        string(tc.phase),
				MaxIterationsCap: tc.maxIter,
				IterationCap:     tc.iterCap,
			}

			// Same phase, DIFFERENT live iteration, SAME phase-start
			// iteration -> banner must be byte-identical.
			reqA := base
			reqA.Iteration = tc.iterA
			reqA.PhaseStartIter = tc.iterA

			reqB := base
			reqB.Iteration = tc.iterB
			reqB.PhaseStartIter = tc.iterA

			bannerA := formatDynamicGoalPhaseBanner(reqA)
			bannerB := formatDynamicGoalPhaseBanner(reqB)

			if bannerA == "" {
				t.Fatalf("expected a %s banner to be rendered", tc.phase)
			}
			if bannerA != bannerB {
				t.Errorf("%s banner must be byte-identical within a phase. A (iter %d): %q | B (iter %d): %q",
					tc.phase, tc.iterA, bannerA, tc.iterB, bannerB)
			}

			// The live iteration must NOT leak into the banner.
			// NOTE: the needle carries a LEADING SPACE on purpose — the
			// phase-stable banner contains `phase_start_iter="N"`, and that
			// string ends with `iter="` — a needle without the space would
			// match its own banner.
			if tc.iterA != tc.iterB {
				needle := fmt.Sprintf(` iter="%d"`, tc.iterB)
				if strings.Contains(bannerA, needle) {
					t.Errorf("live iteration leaked into the banner: %q", bannerA)
				}
			}

			// The phase-start iteration must be present and correct.
			if got := bannerPhaseStartIter(bannerA); got != fmt.Sprintf("%d", tc.iterA) {
				t.Errorf("expected phase_start_iter = %d, got %q in %q", tc.iterA, got, bannerA)
			}
		})
	}
}

// TestH2_Banner_NoLiveIterAttribute pins that the OLD dynamic attribute
// (`iter="N"`) is gone entirely — a leftover would still change bytes
// every iteration.
func TestH2_Banner_NoLiveIterAttribute(t *testing.T) {
	for _, phase := range []GoalPhase{GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal} {
		banner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
			GoalPhase:        string(phase),
			Iteration:        7,
			PhaseStartIter:   3,
			MaxIterationsCap: 250,
			IterationCap:     25,
		})
		if banner == "" {
			t.Fatalf("expected a %s banner", phase)
		}
		if strings.Contains(banner, ` iter="`) {
			t.Errorf("%s banner still carries the live `iter` attribute: %q", phase, banner)
		}
		if !strings.Contains(banner, ` phase_start_iter="3"`) {
			t.Errorf("%s banner missing phase_start_iter=3: %q", phase, banner)
		}
	}
}

// TestH2_Banner_ChangesOnPhaseTransition pins that a phase TRANSITION
// does change the banner (the truthfulness half of the contract — a stale
// phase is exactly the bug that caused the 52.9% checkpoint misfire).
func TestH2_Banner_ChangesOnPhaseTransition(t *testing.T) {
	base := PromptBuildRequest{
		MaxIterationsCap: 250,
		IterationCap:     25,
		PhaseStartIter:   3,
	}

	openReq := base
	openReq.GoalPhase = string(GoalPhaseOpen)
	openReq.Iteration = 3

	checkpointReq := base
	checkpointReq.GoalPhase = string(GoalPhaseCheckpoint)
	checkpointReq.Iteration = 25
	checkpointReq.PhaseStartIter = 25

	openBanner := formatDynamicGoalPhaseBanner(openReq)
	checkpointBanner := formatDynamicGoalPhaseBanner(checkpointReq)

	if openBanner == checkpointBanner {
		t.Errorf("banner must change on phase transition; both were %q", openBanner)
	}
	if got := bannerPhase(openBanner); got != "OPEN" {
		t.Errorf("expected OPEN banner, got %q", got)
	}
	if got := bannerPhase(checkpointBanner); got != "CHECKPOINT" {
		t.Errorf("expected CHECKPOINT banner, got %q", got)
	}
}

// TestH2_Rebuild_NoRebuildWithinCacheablePhase pins the rebuild-hook half
// of the contract: inside OPEN (a cacheable phase) an iteration change
// must NOT trigger a rebuild.
//
// This is the test that fails on the pre-H2 code (the trigger included
// `lastBuiltPromptIteration != iteration` unconditionally).
func TestH2_Rebuild_NoRebuildWithinCacheablePhase(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, string(GoalPhaseOpen), 3, GoalPhaseOpen)
	messages = append(messages, providers.Message{Role: "user", Content: "userMsg1"})

	// Iteration moved from 3 to 12 — same phase.
	result := ts.maybeRebuildPromptForStateChange(messages, exec, nil, 12)

	if result[0].Content != "ORIGINAL_SYSTEM_PROMPT" {
		t.Errorf("expected NO rebuild inside a cacheable phase, but messages[0] was rebuilt")
	}
	if len(result) != len(messages) {
		t.Errorf("expected message count unchanged (%d), got %d", len(messages), len(result))
	}
	if ts.lastBuiltPromptIteration != 3 {
		t.Errorf("expected lastBuiltPromptIteration unchanged (3), got %d", ts.lastBuiltPromptIteration)
	}
}

// TestH2_Rebuild_StillRebuildsWithinNonCacheablePhase pins that phases
// whose SYSTEM hint text embeds the live iteration (e.g. "Goal phase:
// CHECKPOINT (iter N)") still rebuild on an iteration change — otherwise
// that text would go stale, which is the bug Phase 12.16.1 fixed.
func TestH2_Rebuild_StillRebuildsWithinNonCacheablePhase(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, string(GoalPhaseCheckpoint), 25, GoalPhaseCheckpoint)
	messages = append(messages, providers.Message{Role: "user", Content: "userMsg1"})

	result := ts.maybeRebuildPromptForStateChange(messages, exec, nil, 26)

	if result[0].Content == "ORIGINAL_SYSTEM_PROMPT" {
		t.Errorf("expected a rebuild for a non-cacheable phase on iter change, but messages[0] was untouched")
	}
	if ts.lastBuiltPromptIteration != 26 {
		t.Errorf("expected lastBuiltPromptIteration = 26, got %d", ts.lastBuiltPromptIteration)
	}
}

// TestH2_Rebuild_BannerStableAcrossIterationsInPhase is the WIRE-level
// test: drive the real rebuild hook across several iterations of one
// phase and assert the banner bytes never move.
//
// Pre-H2 the banner was rewritten on every iteration, so the hash changed
// each time.
func TestH2_Rebuild_BannerStableAcrossIterationsInPhase(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, "", 0, GoalPhaseOpen)

	// Initial build at iter 3 (mimics the turn's first prompt).
	messages = append(messages, providers.Message{
		Role: "user",
		Content: "userMsg1" + bannerSeparator + formatDynamicGoalPhaseBanner(PromptBuildRequest{
			GoalPhase:        string(GoalPhaseOpen),
			Iteration:        3,
			PhaseStartIter:   3,
			MaxIterationsCap: 250,
			IterationCap:     25,
		}),
	})
	ts.lastBuiltPromptPhase = string(GoalPhaseOpen)
	ts.lastBuiltPromptIteration = 3
	ts.setPhaseStartIter(3)

	var firstHash string
	for _, iter := range []int{4, 7, 12, 24} {
		out := ts.maybeRebuildPromptForStateChange(messages, exec, nil, iter)
		messages = out

		h := bannerHash(out[1].Content)
		if h == "" {
			t.Fatalf("iter %d: banner missing from the current-turn user message", iter)
		}
		if firstHash == "" {
			firstHash = h
			continue
		}
		if h != firstHash {
			t.Errorf("iter %d: banner bytes changed inside one phase. first: %s | now: %s | content: %q",
				iter, firstHash, h, out[1].Content)
		}
	}
}

// TestH2_Rebuild_BannerRefreshesOnTransition pins that a phase
// transition refreshes the banner in the CURRENT turn's user message —
// the Phase 12.80 contract must survive H2.
func TestH2_Rebuild_BannerRefreshesOnTransition(t *testing.T) {
	ts, exec, messages := newPhaseRebuildTestFixture(t, string(GoalPhaseOpen), 3, GoalPhaseCheckpoint)

	openBanner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseOpen),
		Iteration:        3,
		PhaseStartIter:   3,
		MaxIterationsCap: 250,
		IterationCap:     25,
	})
	messages = append(messages, providers.Message{
		Role:    "user",
		Content: "userMsg1" + bannerSeparator + openBanner,
	})

	out := ts.maybeRebuildPromptForStateChange(messages, exec, nil, 25)

	if got := bannerPhase(out[1].Content); got != "CHECKPOINT" {
		t.Errorf("expected the banner to refresh to CHECKPOINT on transition, got %q in %q", got, out[1].Content)
	}
	if !strings.Contains(out[1].Content, "userMsg1") {
		t.Errorf("the user's original text was lost: %q", out[1].Content)
	}
	// The phase-start iteration must be the iteration the NEW phase began.
	if got := bannerPhaseStartIter(out[1].Content); got != "25" {
		t.Errorf("expected phase_start_iter = 25 after the transition, got %q", got)
	}
}

// TestH2_PhaseStartIter_FallbackToLiveIteration pins the legacy-caller
// fallback: a request that does not thread PhaseStartIter still renders a
// banner (using the live iteration) rather than an empty or broken one.
func TestH2_PhaseStartIter_FallbackToLiveIteration(t *testing.T) {
	banner := formatDynamicGoalPhaseBanner(PromptBuildRequest{
		GoalPhase:        string(GoalPhaseOpen),
		Iteration:        9,
		MaxIterationsCap: 250,
		IterationCap:     25,
	})
	if banner == "" {
		t.Fatal("expected a banner with the legacy fallback")
	}
	if got := bannerPhaseStartIter(banner); got != "9" {
		t.Errorf("expected phase_start_iter = 9 from the live-iteration fallback, got %q", got)
	}
}

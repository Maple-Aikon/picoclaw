package agent

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// Phase 12.80 (Goi A / P2) - refresh the per-turn GOAL_PHASE_BANNER.
//
// ---------------------------------------------------------------------------
// THE BUG (measured, 2026-11-03)
// ---------------------------------------------------------------------------
// The dynamic goal-phase banner is rendered into the USER message tail
// (context.go: BuildMessagesFromPrompt -> userContent), NOT into the system
// prompt. That was a deliberate Phase 12.72 decision: keeping dynamic
// content out of the system block preserves a byte-identical system prefix
// for provider-side prompt caching.
//
// BUT the phase-change rebuild hook (turn_state_phase_rebuild.go) only
// replaces messages[0] (the system message) and keeps messages[1:] - the
// already-built user message - verbatim:
//
//	rebuiltSystem := rebuiltAll[0]            // system: refreshed
//	newMessages = append(newMessages, rebuiltSystem)
//	newMessages = append(newMessages, messages[1:]...)   // user: STALE
//
// Consequence: the banner the LLM reads at CHECKPOINT / FINAL is the one
// rendered at the FIRST iteration of the turn (SET phase). The turn-tail
// compass therefore always said phase="SET" iter="1" even while the
// runtime allowlist had already moved to CHECKPOINT.
//
// Live evidence (session sk_v1_9238bf3573, 2026-11-03 13:04):
//
//	iter=80  phase_start   phase=checkpoint
//	iter=80  llm_call      phase=checkpoint
//	iter=80  GATE_SKIP     tool=exec     <- exec blocked in the SAME iter
//
// The stale banner told the LLM "you are at SET, call set_goal", while the
// runtime allowlist only permitted goal_progress + complete_goal. The LLM
// then picked domain tools (exec, write_file) that the gate rejected -
// the misfire pattern measured at 52.9% in the CHECKPOINT phase.
//
// ---------------------------------------------------------------------------
// THE FIX
// ---------------------------------------------------------------------------
// Refresh the banner block inside the CURRENT turn's user message whenever
// the phase or iteration changes. We do NOT rebuild the whole user message
// (that would duplicate the user's text and break the cached system
// prefix); we surgically replace the existing <goal_phase ...>...</goal_phase>
// block, or append a fresh one if the message does not carry a banner yet.
//
// The banner lives at the TAIL of the user message (appended after the
// dynamic_context wrapper + the user's text), so a suffix-anchored
// replacement is sufficient and unambiguous.
//
// ---------------------------------------------------------------------------
// WHY NOT MOVE THE BANNER BACK INTO THE SYSTEM PROMPT?
// ---------------------------------------------------------------------------
// Because that is what Phase 12.72 moved it OUT of, precisely to keep the
// system prefix cache-stable. Moving it back would trade a correctness bug
// for a ~66k uncached-token regression per CHECKPOINT call (measured: the
// CHECKPOINT system prompt rebuild is the dominant token cost - see the
// checkpoint-misfire-fix plan, P9). This fix keeps the banner in user[0]
// AND makes it truthful.

// goalPhaseBannerBlockRe matches a complete <goal_phase ...>...</goal_phase>
// block in a message body.
var goalPhaseBannerBlockRe = regexp.MustCompile(`(?s)<goal_phase[^>]*>.*?</goal_phase>`)

// bannerSeparator is the blank-line separator between the user text and
// the banner block. Built at runtime to avoid escape sequences in source.
var bannerSeparator = strings.Repeat(string(rune(10)), 2)

// stripGoalPhaseBanner removes any existing goal-phase banner block from
// content, leaving the surrounding text intact.
func stripGoalPhaseBanner(content string) string {
	trimmed := goalPhaseBannerBlockRe.ReplaceAllString(content, "")
	return strings.TrimRightFunc(trimmed, unicode.IsSpace)
}

// refreshGoalPhaseBannerInMessages returns messages with the goal-phase
// banner in the CURRENT turn's user message replaced by `banner`.
//
// Rules:
//   - banner == ""  -> strip any existing banner block (no banner wanted)
//   - a user message in this turn already carries a banner -> replace it
//     in place (keeps the banner at the tail, keeps user text intact)
//   - no banner present -> append one to the current turn's user message
//
// `currentTurnStart` is the index of the first message belonging to the
// current turn (see exec.currentTurnStart). Messages before it are
// history and are never touched.
//
// When the current turn has no user message at all (e.g. a turn that only
// appends tool results), the messages are returned unchanged - there is no
// user message to carry the banner, and inventing one would corrupt the
// conversation shape.
func refreshGoalPhaseBannerInMessages(messages []providers.Message, currentTurnStart int, banner string) []providers.Message {
	if len(messages) == 0 {
		return messages
	}
	if currentTurnStart < 0 {
		currentTurnStart = 0
	}
	if currentTurnStart > len(messages) {
		currentTurnStart = len(messages)
	}

	// Locate the user message that carries the turn's banner.
	//
	// It is NOT simply the last user message: steering messages also have
	// Role=="user" (pkg/agent/steering.go), and a steering message is
	// appended AFTER the turn's root user message. Injecting the banner
	// into a steering message would (a) put the phase compass in the
	// wrong place and (b) corrupt the steering message's content, which
	// downstream code compares verbatim (steering_test.go asserts
	// m.Content == "pre-enqueued steering").
	//
	// Selection order:
	//   1. the FIRST user message of the current turn that already
	//      carries a banner block (the turn root — this is the normal
	//      case: the banner was rendered when the turn was built);
	//   2. otherwise the FIRST user message of the current turn (the
	//      root, before any steering message was appended).
	idx := -1
	for i := currentTurnStart; i < len(messages); i++ {
		if messages[i].Role != "user" {
			continue
		}
		if goalPhaseBannerBlockRe.MatchString(messages[i].Content) {
			idx = i
			break
		}
		if idx < 0 {
			idx = i // first user message of the turn (candidate root)
		}
	}
	if idx < 0 {
		return messages
	}

	original := messages[idx].Content
	hasBanner := goalPhaseBannerBlockRe.MatchString(original)

	var updated string
	switch {
	case banner == "" && !hasBanner:
		return messages // nothing to do
	case banner == "":
		updated = stripGoalPhaseBanner(original)
	case hasBanner:
		// Replace the existing block, preserving its position.
		updated = goalPhaseBannerBlockRe.ReplaceAllString(original, banner)
	default:
		// No banner yet - append it at the tail.
		if strings.TrimSpace(original) != "" {
			updated = original + bannerSeparator + banner
		} else {
			updated = banner
		}
	}

	if updated == original {
		return messages
	}

	// Copy-on-write: never mutate the caller's slice elements in place -
	// exec.messages may be shared with other holders.
	out := make([]providers.Message, len(messages))
	copy(out, messages)
	out[idx].Content = updated
	return out
}

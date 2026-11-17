package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// A1 (2026-11-17) — shared test helpers for the turn-tail affordance.
//
// A1 moved the phase affordance (the <goal_phase> banner + the
// <goal_phase_channel>) OUT of the turn-root user message and into ONE
// dedicated USER message at the TAIL of the request.
//
// Consequence for the wire-shape tests: the LAST message is no longer
// the turn-root user message. These helpers locate the right message so
// the tests keep asserting their ORIGINAL intent (contract C5: update the
// old tests to assert the NEW location, do not delete them).

// a1IsAffordanceMessage reports whether a message carries the phase
// affordance (the banner or the phase channel).
func a1IsAffordanceMessage(m providers.Message) bool {
	if m.Role != "user" {
		return false
	}
	return strings.Contains(m.Content, phaseChannelMarker) ||
		goalPhaseBannerBlockRe.MatchString(m.Content)
}

// a1TurnRootUserMessage returns the FIRST user message of the array —
// the turn-root user message (dynamic context + user text).
//
// It is NOT the last user message: steering messages also have
// Role=="user" (pkg/agent/steering.go).
func a1TurnRootUserMessage(messages []providers.Message) (providers.Message, bool) {
	for _, m := range messages {
		if m.Role == "user" {
			return m, true
		}
	}
	return providers.Message{}, false
}

// a1LastNonAffordanceMessage returns the LAST message that does NOT
// carry the phase affordance. Used by the tests that assert on the
// tool result / user text that used to be the last message.
func a1LastNonAffordanceMessage(messages []providers.Message) (providers.Message, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if !a1IsAffordanceMessage(messages[i]) {
			return messages[i], true
		}
	}
	return providers.Message{}, false
}

// a1TailAffordanceMessage returns the tail affordance message (the
// dedicated USER message at the END of the array).
func a1TailAffordanceMessage(messages []providers.Message) (providers.Message, bool) {
	if len(messages) == 0 {
		return providers.Message{}, false
	}
	last := messages[len(messages)-1]
	if !a1IsAffordanceMessage(last) {
		return providers.Message{}, false
	}
	return last, true
}

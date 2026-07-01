package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// =============================================================================
// Phase 2 (R14.5): /extend command handler tests
// =============================================================================
//
// applyExtendCommand mirrors the /use intercept pattern. It:
//   1. Matches "/extend <message>" → strips prefix, sets opts flags, passthrough
//   2. Matches "/extend" (no args) → returns usage hint
//   3. Does NOT match non-/extend messages
//

func TestApplyExtendCommand_WithMessage(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	opts := makeTestProcessOpts("test-extend-session")
	opts.Dispatch.UserMessage = "/extend analyze the codebase"

	matched, handled, reply := al.applyExtendCommand(opts.Dispatch.UserMessage, agent, &opts)

	if !matched {
		t.Fatal("expected matched=true for /extend <message>")
	}
	if handled {
		t.Fatal("expected handled=false (passthrough to LLM) for /extend <message>")
	}
	if reply != "" {
		t.Errorf("expected empty reply (passthrough), got %q", reply)
	}
	if !opts.ExtendEnabled {
		t.Error("expected opts.ExtendEnabled=true after /extend")
	}
	if opts.StrippedUserMessage != "analyze the codebase" {
		t.Errorf("expected StrippedUserMessage=%q, got %q", "analyze the codebase", opts.StrippedUserMessage)
	}
	if opts.Dispatch.UserMessage != "analyze the codebase" {
		t.Errorf("expected Dispatch.UserMessage=%q, got %q", "analyze the codebase", opts.Dispatch.UserMessage)
	}
}

func TestApplyExtendCommand_EmptyMessage(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	opts := makeTestProcessOpts("test-extend-empty")
	opts.Dispatch.UserMessage = "/extend "

	matched, handled, reply := al.applyExtendCommand(opts.Dispatch.UserMessage, agent, &opts)

	if !matched {
		t.Fatal("expected matched=true for '/extend ' (empty args)")
	}
	if !handled {
		t.Fatal("expected handled=true (usage reply) for empty /extend")
	}
	if reply == "" {
		t.Error("expected non-empty usage reply for empty /extend")
	}
	if opts.ExtendEnabled {
		t.Error("expected opts.ExtendEnabled=false (not set for empty /extend)")
	}
}

func TestApplyExtendCommand_NoArgs(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	opts := makeTestProcessOpts("test-extend-noargs")

	matched, handled, reply := al.applyExtendCommand("/extend", agent, &opts)

	if !matched {
		t.Fatal("expected matched=true for '/extend' (no trailing space)")
	}
	if !handled {
		t.Fatal("expected handled=true (usage reply) for '/extend' with no args")
	}
	if reply == "" {
		t.Error("expected non-empty usage reply")
	}
}

func TestApplyExtendCommand_NotExtend(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	opts := makeTestProcessOpts("test-not-extend")

	matched, handled, _ := al.applyExtendCommand("hello world", agent, &opts)

	if matched {
		t.Fatal("expected matched=false for non-/extend message")
	}
	if handled {
		t.Fatal("expected handled=false for non-/extend message")
	}
}

// TestApplyExtendCommand_IntegrationWithHandleCommand verifies the full
// handleCommand path: /extend message → passthrough (handled=false) →
// returns ("", false) so the LLM processes the stripped message.
func TestApplyExtendCommand_IntegrationWithHandleCommand(t *testing.T) {
	al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
	defer cleanup()

	opts := makeTestProcessOpts("test-extend-integration")
	msg := bus.InboundMessage{
		Channel: "cli",
		ChatID:  "test-chat",
		Content: "/extend do the thing",
	}

	reply, handled := al.handleCommand(nil, msg, agent, &opts)

	if handled {
		t.Fatal("expected handled=false (passthrough to LLM) for /extend <message>")
	}
	if reply != "" {
		t.Errorf("expected empty reply (passthrough), got %q", reply)
	}
	if !opts.ExtendEnabled {
		t.Error("expected opts.ExtendEnabled=true after handleCommand with /extend")
	}
	if opts.Dispatch.UserMessage != "do the thing" {
		t.Errorf("expected Dispatch.UserMessage=%q, got %q", "do the thing", opts.Dispatch.UserMessage)
	}
}
// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Phase 12.45 — recall prompt history log + replay runtime events.
//
// RecallLLM calls bypass the JS hooks (before_after_llm_memory.js) by
// design (WF1: signet double-inject, conversation_history duplicate,
// summary rotation). Before this phase the recall path was therefore
// fully invisible in both gateway.log (no runtime events) and
// prompt_history.log (no hook = no entry). This file adds:
//
//  1. Runtime events (agent.llm.request/response/retry) with the
//     dedicated tracePath replayTracePath, emitted from inside RecallLLM
//     — the single choke point covering both callers (handleGoalRecovery
//     + retryExecuteToolChain).
//  2. A Go-side [RECALL START]/[RECALL END] block written to the same
//     prompt_history.log the JS hook uses, so recall prompts/responses
//     are grep-able in the same file.
//
// By design (Q5A/Q5B, owner 2026-08-03): hooks are NOT run for recall
// calls; recall content is NOT redacted (perms 0640 + opt-out env
// PICOCLAW_REPLAY_PROMPT_LOG are the mitigations).

// replayTracePath is the correlation TraceID for all recall-path events.
// Deliberately NOT "turn.llm.replay" (that is a prefix of the existing
// "turn.llm.replay.attempt"/"turn.llm.replay.exhausted" hook-replay
// tracePaths — grep would be noisy; B-F06).
const replayTracePath = "turn.llm.replay.recall"

const (
	// MaxReplayMessageBytes caps each rendered message/content line.
	MaxReplayMessageBytes = 8 * 1024
	// MaxReplayBlockBytes caps the total rendered block
	// (JSON header + lines + footer). A-F02/B-F02.
	MaxReplayBlockBytes = 64 * 1024
)

// replayDropWarnTotal counts rate-limited drop warnings (test-visible).
var replayDropWarnTotal atomic.Int64

// lastReplayDropWarnAt is the unix second of the last drop warning.
var lastReplayDropWarnAt atomic.Int64

// emitReplayRuntimeEvent emits one recall-path runtime event and warns
// (rate-limited, once per 10s) when a subscriber dropped it (A-F05/B-F05).
func (p *Pipeline) emitReplayRuntimeEvent(kind runtimeevents.Kind, meta HookMeta, payload any) {
	if p == nil || p.al == nil {
		return
	}
	evt := p.al.buildRuntimeEvent(kind, meta, payload)
	if result := p.al.publishRuntimeEventResult(evt); result.Dropped > 0 {
		warnReplayDrop(kind)
	}
}

// warnReplayDrop logs a rate-limited warning when recall events are
// dropped by the runtime bus (best-effort observability — never blocks).
func warnReplayDrop(kind runtimeevents.Kind) {
	now := time.Now().Unix()
	last := lastReplayDropWarnAt.Load()
	if now-last < 10 && last != 0 {
		return
	}
	if !lastReplayDropWarnAt.CompareAndSwap(last, now) {
		return
	}
	replayDropWarnTotal.Add(1)
	logger.WarnCF("agent", "recall runtime event dropped (subscriber buffer full)", map[string]any{
		"kind": string(kind),
	})
}

// replayPromptLogPath returns the prompt history log path. Same knob as
// the JS hook: env PICOCLAW_HOOK_LOG_FILE, else $PICOCLAW_HOME/logs/
// prompt_history.log.
func replayPromptLogPath() string {
	if p := os.Getenv("PICOCLAW_HOOK_LOG_FILE"); p != "" {
		return p
	}
	return filepath.Join(config.GetHome(), "logs", "prompt_history.log")
}

// replayPromptLogEnabled implements Q5A: default ON (parity with the JS
// hook which always logs prompt content); opt-out via
// PICOCLAW_REPLAY_PROMPT_LOG=0|false.
func replayPromptLogEnabled() bool {
	v, ok := os.LookupEnv(config.EnvReplayPromptLog)
	if !ok {
		return true
	}
	on, err := strconv.ParseBool(v)
	if err != nil {
		return true // invalid value → default ON (fail-open, hook parity)
	}
	return on
}

// replayBlockInput carries everything renderReplayPromptBlock needs.
// err != nil renders a [RECALL FAILED] block (A-F14: exactly one such
// block per failed recall, with helper name + final error).
type replayBlockInput struct {
	turnID     string
	helperName string
	iteration  int
	messages   []providers.Message
	resp       *providers.LLMResponse
	err        error
}

// renderReplayPromptBlock renders one recall block. Pure — no IO.
//
// Format (F7 + B-F10 + A-F04):
//
//	{"type":"recall","turn_id":"...","helper":"...","iter":1,"ts":"RFC3339"}
//	>>> [RECALL START] ID: ... | Helper: ... | Iteration: 1 | 15:04:05
//	    [SYSTEM]: <content>          (escaped \n → ⏎; cap 8KB/message)
//	    [USER]: <content>
//	    [ASSISTANT]: <resp.Content>
//	    [TOOL_CALL]: <name>(<args>) [ID: <id>]
//	    [USAGE]: P: <pt> | C: <ct> | T: <total>
//	<<< [RECALL END]
//
// Size caps (A-F02/B-F02): 8KB per line, 64KB total block with a
// "...[truncated N bytes]" marker. Truncation happens here (pure), so
// it never slows the turn loop.
func renderReplayPromptBlock(in replayBlockInput, now time.Time) string {
	var b strings.Builder

	failed := in.err != nil
	meta := map[string]any{
		"type":    "recall",
		"turn_id": in.turnID,
		"helper":  in.helperName,
		"iter":    in.iteration,
		"ts":      now.Format(time.RFC3339),
	}
	if failed {
		meta["type"] = "recall_failed"
		meta["error"] = in.err.Error()
	}
	metaJSON, _ := json.Marshal(meta)
	b.Write(metaJSON)
	b.WriteByte('\n')

	marker := ">>> [RECALL START]"
	if failed {
		marker = ">>> [RECALL FAILED]"
	}
	fmt.Fprintf(&b, "%s ID: %s | Helper: %s | Iteration: %d | %s\n",
		marker, in.turnID, in.helperName, in.iteration, now.Format("15:04:05"))

	if failed {
		fmt.Fprintf(&b, "    [ERROR]: %s\n", escapeReplayLine(clipReplayLine(in.err.Error(), MaxReplayMessageBytes)))
	} else {
		for _, m := range in.messages {
			content := escapeReplayLine(clipReplayLine(m.Content, MaxReplayMessageBytes))
			fmt.Fprintf(&b, "    [%s]: %s\n", strings.ToUpper(m.Role), content)
		}
		if in.resp != nil {
			if in.resp.Content != "" {
				content := escapeReplayLine(clipReplayLine(in.resp.Content, MaxReplayMessageBytes))
				fmt.Fprintf(&b, "    [ASSISTANT]: %s\n", content)
			}
			for _, tc := range in.resp.ToolCalls {
				args, _ := json.Marshal(tc.Arguments)
				argsLine := escapeReplayLine(clipReplayLine(string(args), MaxReplayMessageBytes))
				fmt.Fprintf(&b, "    [TOOL_CALL]: %s(%s) [ID: %s]\n", tc.Name, argsLine, tc.ID)
			}
			if in.resp.Usage != nil {
				fmt.Fprintf(&b, "    [USAGE]: P: %d | C: %d | T: %d\n",
					in.resp.Usage.PromptTokens, in.resp.Usage.CompletionTokens, in.resp.Usage.TotalTokens)
			}
		}
	}
	b.WriteString("<<< [RECALL END]\n")

	// Total block cap (A-F02): trim the whole block if still oversized.
	if b.Len() > MaxReplayBlockBytes {
		s := b.String()
		over := len(s) - MaxReplayBlockBytes
		s = truncateReplayRunes(s, MaxReplayBlockBytes)
		return s + fmt.Sprintf("...[truncated %d bytes]\n<<< [RECALL END]\n", over)
	}
	return b.String()
}

// escapeReplayLine keeps each message on one line (A-F04): \n and \r are
// rendered as the literal U+23CE (⏎) so block boundaries stay greppable.
func escapeReplayLine(s string) string {
	return strings.NewReplacer("\r\n", "⏎", "\n", "⏎", "\r", "⏎").Replace(s)
}

// clipReplayLine truncates s to at most max runes and appends a visible
// "...[truncated N bytes]" marker when content was cut (A-F02/B-F02).
func clipReplayLine(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	trunc := truncateReplayRunes(s, max)
	return trunc + fmt.Sprintf("...[truncated %d bytes]", len(s)-len(trunc))
}

// truncateReplayRunes truncates s to at most max runes, stripping any
// trailing Unicode combining marks so the output never ends in a dangling
// diacritic. Rune-safe (Phase 12.28.3: byte length != rune count).
// Pattern copied from pkg/agent/goal/text.go (Phase 12.44) — kept local
// to avoid a public API change in the goal package for one log helper.
func truncateReplayRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	out := []rune(s)[:max]
	for len(out) > 0 && unicode.Is(unicode.Mn, out[len(out)-1]) {
		out = out[:len(out)-1]
	}
	return string(out)
}

// writeReplayPromptLog appends one block atomically: a single Write with
// O_APPEND (Linux serializes appends via the inode lock — never split the
// write; A-F01/B-F01). File perms 0640 (A-F11) — Chmod runs every write
// so pre-existing 0644 files from the JS hook get tightened too.
func writeReplayPromptLog(path string, block string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o640); err != nil {
		return err
	}
	_, err = f.WriteString(block)
	return err
}

// logReplayPromptBlock is the top-level entry: env gate → render → write.
// Fail-safe (R2): any write error only produces one WarnCF line — the
// turn must never fail because observability failed.
func logReplayPromptBlock(in replayBlockInput) {
	if !replayPromptLogEnabled() {
		return
	}
	block := renderReplayPromptBlock(in, time.Now())
	if err := writeReplayPromptLog(replayPromptLogPath(), block); err != nil {
		logger.WarnCF("agent", "recall prompt log write failed", map[string]any{
			"helper": in.helperName,
			"error":  err.Error(),
		})
	}
}

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Phase affordance capture — 2026-11-18.
//
// ---------------------------------------------------------------------------
// WHY
// ---------------------------------------------------------------------------
// Metric E (stale channel = 0) can only be closed by MEASURING the bytes the
// provider actually sees. Before this file the affordance was invisible in
// EVERY log:
//
//   - prompt_history.log: written by the JS hook. The hook receives
//     exec.callMessages BEFORE the tail is appended
//     (pkg/agent/pipeline_llm.go:137 vs the append inside callLLMCore) and
//     the append is copy-on-write, so exec.callMessages never changes => the
//     hook can never see the banner/channel.
//   - gateway.log: "Full LLM request" logs exec.callMessages (pre-tail) AND
//     truncates content to 200 bytes (utils.Truncate) => never contains the
//     channel (it sits after the banner).
//   - session jsonl: the tail is not persisted into the turn history.
//
// MEASURED (2026-11-18): prompt_history.log had 0 records containing the
// affordance (USER 428 / ASSISTANT 25559 / TOOL 39519 records, 0 hits).
//
// ---------------------------------------------------------------------------
// WHERE
// ---------------------------------------------------------------------------
// This capture happens at the ONE place where the exact wire bytes are
// known: the append site inside callLLMCore. The captured string is the SAME
// variable that is appended to the request, so it is byte-identical to the
// wire BY CONSTRUCTION — it is not a re-render, and it cannot drift from
// what the provider sees.
//
// ---------------------------------------------------------------------------
// CONTRACT
// ---------------------------------------------------------------------------
//   1. ADDITIVE: the wire payload is untouched. This file only writes a
//      separate log line. The A1 contracts (C6/C7/C8) still hold.
//   2. FAITHFUL: captured affordance == appended message content, byte for
//      byte (asserted by TestPhaseAffordanceCapture_WireBytesIdentical).
//   3. FAIL-SAFE: a write error only produces one WarnCF line; the turn
//      must never fail because observability failed.
//   4. ONE LINE PER REQUEST: JSONL, so a parser can read it line by line.
//   5. OPT-OUT: PICOCLAW_PHASE_AFFORDANCE_LOG=0|false disables it
//      (default ON, parity with the JS hook which always logs prompts).

// phaseAffordanceCaptureRecord is one JSONL line in phase_affordance.log.
type phaseAffordanceCaptureRecord struct {
	TS             string `json:"ts"`
	Type           string `json:"type"`
	TurnID         string `json:"turn_id"`
	SessionKey     string `json:"session_key"`
	Iter           int    `json:"iter"`
	Phase          string `json:"phase"`
	PhaseStartIter int    `json:"phase_start_iter"`
	Affordance     string `json:"affordance"`
}

// phaseAffordanceCaptureInput carries everything the capture needs. It is a
// plain struct (not a *turnState) so the capture is testable without a
// full turn.
type phaseAffordanceCaptureInput struct {
	turnID         string
	sessionKey     string
	iteration      int
	phase          string
	phaseStartIter int
	affordance     string
}

// phaseAffordanceCaptureEnabled implements the opt-out. Default ON.
//
// TEST GUARD (2026-11-18): a Go TEST binary exercises callLLMCore, so
// without a guard the unit tests wrote production-shaped records into the
// REAL capture file and poisoned the metric (measured: 533 records, every
// one from a test fixture such as "turn-retry-chain-test" / "turn-recall-test",
// written during `go test ./pkg/agent/`). A capture polluted by test
// fixtures is worse than no capture: it produces a confident 0% that is
// about nothing.
//
// In a test binary the capture is OFF unless the destination file is
// EXPLICITLY overridden. Tests that assert on the capture point it at a
// temp dir, so they still work AND can never touch the production file.
func phaseAffordanceCaptureEnabled() bool {
	if strings.HasSuffix(os.Args[0], ".test") {
		if os.Getenv("PICOCLAW_PHASE_AFFORDANCE_LOG_FILE") == "" {
			return false
		}
	}
	v, ok := os.LookupEnv(config.EnvPhaseAffordanceLog)
	if !ok {
		return true
	}
	on, err := strconv.ParseBool(v)
	if err != nil {
		return true // invalid value -> default ON (fail-open, hook parity)
	}
	return on
}

// phaseAffordanceCapturePath returns the capture file path.
// PICOCLAW_PHASE_AFFORDANCE_LOG_FILE overrides the default location.
func phaseAffordanceCapturePath() string {
	if p := os.Getenv("PICOCLAW_PHASE_AFFORDANCE_LOG_FILE"); p != "" {
		return p
	}
	return filepath.Join(config.GetHome(), "logs", "phase_affordance.log")
}

// appendPhaseAffordanceCapture appends one JSON line atomically: a single
// Write with O_APPEND (Linux serializes appends via the inode lock).
// File perms 0640, same as the Go-side recall writer.
func appendPhaseAffordanceCapture(path string, rec phaseAffordanceCaptureRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return mkErr
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if chmodErr := f.Chmod(0o640); chmodErr != nil {
		return chmodErr
	}
	_, err = f.Write(append(line, byte(10)))
	return err
}

// appendPhaseAffordanceWithCapture appends the affordance to the tail of
// the request AND captures it, using the SAME string for both.
//
// This is the only place that knows both the wire bytes and the capture
// bytes, so byte-identity between "what the provider sees" and "what the
// metric is computed on" is guaranteed BY CONSTRUCTION — there is no second
// render path that could drift.
//
// Returns the messages slice to use for the call.
func appendPhaseAffordanceWithCapture(
	messages []providers.Message,
	affordance string,
	in phaseAffordanceCaptureInput,
) []providers.Message {
	// The captured affordance IS the appended affordance. Assigned here, not
	// by the caller, so a future caller cannot pass a different string.
	in.affordance = affordance
	out := appendPhaseAffordanceAtTail(messages, affordance)
	capturePhaseAffordance(in)
	return out
}

// capturePhaseAffordance writes one capture line for a request that carried
// a phase affordance.
//
// Empty affordance => nothing was rendered (e.g. the system prompt is
// suppressed for that turn profile) => there is no banner/channel pair to
// compare, so no sample is recorded.
func capturePhaseAffordance(in phaseAffordanceCaptureInput) {
	if in.affordance == "" || !phaseAffordanceCaptureEnabled() {
		return
	}
	rec := phaseAffordanceCaptureRecord{
		TS:             time.Now().Format(time.RFC3339),
		Type:           "phase_affordance",
		TurnID:         in.turnID,
		SessionKey:     in.sessionKey,
		Iter:           in.iteration,
		Phase:          in.phase,
		PhaseStartIter: in.phaseStartIter,
		Affordance:     in.affordance,
	}
	if err := appendPhaseAffordanceCapture(phaseAffordanceCapturePath(), rec); err != nil {
		logger.WarnCF("agent", "phase affordance capture write failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// phaseAffordanceBannerPhase extracts the phase name from the
// <goal_phase phase="..."> banner. Returns "" when there is no banner.
func phaseAffordanceBannerPhase(affordance string) string {
	const marker = "<goal_phase phase=\""
	i := strings.Index(affordance, marker)
	if i < 0 {
		return ""
	}
	rest := affordance[i+len(marker):]
	j := strings.Index(rest, "\"")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// phaseAffordanceChannelPhase extracts the phase name from the
// <goal_phase_channel> block. Returns "" when there is no channel.
func phaseAffordanceChannelPhase(affordance string) string {
	start := strings.Index(affordance, "<goal_phase_channel>")
	if start < 0 {
		return ""
	}
	block := affordance[start:]
	end := strings.Index(block, "</goal_phase_channel>")
	if end >= 0 {
		block = block[:end]
	}
	if i := strings.Index(block, "Goal lifecycle tools at "); i >= 0 {
		rest := block[i+len("Goal lifecycle tools at "):]
		if j := strings.Index(rest, " phase"); j >= 0 {
			return strings.TrimSpace(rest[:j])
		}
	}
	if i := strings.Index(block, "Goal phase: "); i >= 0 {
		rest := block[i+len("Goal phase: "):]
		if j := strings.Index(rest, " "); j >= 0 {
			return strings.TrimSpace(rest[:j])
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

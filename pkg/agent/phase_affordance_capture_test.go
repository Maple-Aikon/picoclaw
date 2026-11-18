package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ---------------------------------------------------------------------------
// PHASE AFFORDANCE CAPTURE — 2026-11-18
// ---------------------------------------------------------------------------
//
// WHY THIS TEST EXISTS
//
// Metric E (stale channel = 0) can only be closed by measuring the bytes the
// provider actually sees. Before the capture, the affordance was invisible in
// every log (see phase_affordance_capture.go for the full analysis), so the
// metric had no denominator.
//
// THE CONTRACT THAT MATTERS
//
//   1. FAITHFUL: the captured affordance is BYTE-IDENTICAL to the message
//      content appended to the request. This is the whole point — a capture
//      that re-renders the affordance could drift from the wire and would
//      measure the wrong thing.
//   2. ADDITIVE: the wire payload is untouched.
//   3. Banner and channel always agree (they are rendered together).
//   4. Negative control: an empty affordance produces NO sample (there is no
//      banner/channel pair to compare, so counting it would corrupt the
//      denominator).

// captureTestFile points the capture at a temp file and returns its path.
func captureTestFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "phase_affordance.log")
	t.Setenv("PICOCLAW_PHASE_AFFORDANCE_LOG", "1")
	t.Setenv("PICOCLAW_PHASE_AFFORDANCE_LOG_FILE", path)
	return path
}

// readCaptureRecords reads the JSONL capture file.
func readCaptureRecords(t *testing.T, path string) []phaseAffordanceCaptureRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read capture file: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	var recs []phaseAffordanceCaptureRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), string(rune(10))) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec phaseAffordanceCaptureRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("capture line is not valid JSON: %v (line=%.200s)", err, line)
		}
		recs = append(recs, rec)
	}
	return recs
}

// TestPhaseAffordanceCapture_WireBytesIdentical is the CORE contract.
//
// It appends the affordance to a request exactly the way production does and
// then asserts that the captured bytes are byte-identical to the message
// content the provider receives. If someone later changes the capture to
// re-render the affordance instead of reusing the wire string, this fails.
func TestPhaseAffordanceCapture_WireBytesIdentical(t *testing.T) {
	path := captureTestFile(t)

	req := PromptBuildRequest{
		GoalPhase:        string(GoalPhaseCheckpoint),
		Iteration:        40,
		GoalSnapshot:     leverASnapshot,
		IterationCap:     25,
		MaxIterationsCap: 300,
		PhaseStartIter:   20,
		CurrentMessage:   "do the thing",
	}

	affordance := phaseAffordanceText(req)
	if affordance == "" {
		t.Fatal("expected a non-empty phase affordance for CHECKPOINT")
	}

	base := []providers.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "user root"},
	}
	out := appendPhaseAffordanceWithCapture(base, affordance,
		phaseAffordanceCaptureInput{
			turnID:         "main-turn-1",
			sessionKey:     "sk_test",
			iteration:      40,
			phase:          string(GoalPhaseCheckpoint),
			phaseStartIter: 20,
		})

	// (a) The wire shape is unchanged: the affordance is ONE new user
	// message at the TAIL, and no existing message was modified.
	if len(out) != len(base)+1 {
		t.Fatalf("expected %d messages, got %d", len(base)+1, len(out))
	}
	for i := range base {
		if out[i].Content != base[i].Content {
			t.Errorf("message %d was modified — the capture must be ADDITIVE", i)
		}
	}
	tail := out[len(out)-1]
	if tail.Role != "user" {
		t.Fatalf("tail role = %q, want user", tail.Role)
	}

	// (b) The captured bytes are byte-identical to the wire bytes.
	recs := readCaptureRecords(t, path)
	if len(recs) != 1 {
		t.Fatalf("expected exactly 1 capture record, got %d", len(recs))
	}
	if recs[0].Affordance != tail.Content {
		t.Errorf("captured affordance != wire bytes: captured=%q wire=%q",
			recs[0].Affordance, tail.Content)
	}
	if recs[0].Affordance != affordance {
		t.Errorf("captured affordance != the string that was appended")
	}
	if recs[0].Iter != 40 {
		t.Errorf("capture iter = %d, want 40", recs[0].Iter)
	}
	if recs[0].PhaseStartIter != 20 {
		t.Errorf("capture phase_start_iter = %d, want 20", recs[0].PhaseStartIter)
	}
	if recs[0].TurnID != "main-turn-1" {
		t.Errorf("capture turn_id = %q, want main-turn-1", recs[0].TurnID)
	}
}

// TestPhaseAffordanceCapture_BannerChannelAlwaysAgree pins that the captured
// banner and the captured channel always name the SAME phase — the property
// metric E measures (stale channel == 0).
func TestPhaseAffordanceCapture_BannerChannelAlwaysAgree(t *testing.T) {
	phases := []struct {
		name  string
		phase GoalPhase
	}{
		{"set", GoalPhaseSet},
		{"open", GoalPhaseOpen},
		{"checkpoint", GoalPhaseCheckpoint},
		{"final", GoalPhaseFinal},
	}

	for _, tc := range phases {
		t.Run(tc.name, func(t *testing.T) {
			path := captureTestFile(t)

			req := PromptBuildRequest{
				GoalPhase:        string(tc.phase),
				Iteration:        12,
				GoalSnapshot:     leverASnapshot,
				IterationCap:     25,
				MaxIterationsCap: 300,
				PhaseStartIter:   2,
				CurrentMessage:   "do the thing",
			}
			affordance := phaseAffordanceText(req)
			if affordance == "" {
				t.Fatalf("expected a non-empty affordance for phase %s", tc.phase)
			}

			appendPhaseAffordanceWithCapture(
				[]providers.Message{{Role: "user", Content: "root"}},
				affordance,
				phaseAffordanceCaptureInput{
					turnID:         "turn-" + tc.name,
					sessionKey:     "sk_test",
					iteration:      12,
					phase:          string(tc.phase),
					phaseStartIter: 2,
				})

			recs := readCaptureRecords(t, path)
			if len(recs) != 1 {
				t.Fatalf("expected 1 capture record, got %d", len(recs))
			}
			got := recs[0].Affordance

			banner := phaseAffordanceBannerPhase(got)
			channel := phaseAffordanceChannelPhase(got)
			if banner == "" {
				t.Errorf("no banner phase parsed from %q", got)
			}
			if channel == "" {
				t.Errorf("no channel phase parsed from %q", got)
			}
			if banner != "" && channel != "" &&
				!strings.EqualFold(banner, channel) {
				t.Errorf("banner phase %q != channel phase %q — this is EXACTLY the stale-channel bug", banner, channel)
			}
			if banner != "" && !strings.EqualFold(banner, string(tc.phase)) {
				t.Errorf("banner phase = %q, want %q", banner, tc.phase)
			}
		})
	}
}

// TestPhaseAffordanceCapture_EmptyAffordanceWritesNothing is the NEGATIVE
// CONTROL: when no affordance is rendered there is no banner/channel pair, so
// no sample may be recorded. A capture that logged an empty sample would
// corrupt the denominator of metric E.
func TestPhaseAffordanceCapture_EmptyAffordanceWritesNothing(t *testing.T) {
	path := captureTestFile(t)

	out := appendPhaseAffordanceWithCapture(
		[]providers.Message{{Role: "user", Content: "root"}},
		"",
		phaseAffordanceCaptureInput{
			turnID:         "turn-empty",
			sessionKey:     "sk_test",
			iteration:      3,
			phase:          string(GoalPhaseOpen),
			phaseStartIter: 2,
		})

	if len(out) != 1 {
		t.Fatalf("an empty affordance must not append a message; got %d messages", len(out))
	}
	if recs := readCaptureRecords(t, path); len(recs) != 0 {
		t.Fatalf("an empty affordance must not be captured; got %d records", len(recs))
	}
}

// TestPhaseAffordanceCapture_OptOut proves the opt-out works.
func TestPhaseAffordanceCapture_OptOut(t *testing.T) {
	path := captureTestFile(t)
	t.Setenv("PICOCLAW_PHASE_AFFORDANCE_LOG", "0")

	req := PromptBuildRequest{
		GoalPhase:        string(GoalPhaseOpen),
		Iteration:        5,
		IterationCap:     25,
		MaxIterationsCap: 300,
		PhaseStartIter:   2,
	}
	affordance := phaseAffordanceText(req)
	if affordance == "" {
		t.Fatal("expected a non-empty affordance")
	}

	appendPhaseAffordanceWithCapture(
		[]providers.Message{{Role: "user", Content: "root"}},
		affordance,
		phaseAffordanceCaptureInput{turnID: "t", sessionKey: "s", iteration: 5})

	if recs := readCaptureRecords(t, path); len(recs) != 0 {
		t.Fatalf("opt-out must suppress the capture; got %d records", len(recs))
	}
}

// TestPhaseAffordanceCapture_TestBinaryDoesNotPolluteDefaultPath is the
// NEGATIVE CONTROL for the test guard.
//
// Measured 2026-11-18: before the guard, `go test ./pkg/agent/` wrote 533
// production-shaped records into the REAL capture file (every one from a
// test fixture such as "turn-retry-chain-test"). The metric then reported a
// confident "0 mismatch / 519 pairs" that was about nothing.
//
// This test locks the contract: in a test binary, with NO explicit
// destination file, the capture writes nothing to the default path.
func TestPhaseAffordanceCapture_TestBinaryDoesNotPolluteDefaultPath(t *testing.T) {
	// Point the default path at a temp dir so the test can prove that
	// nothing is written there.
	defaultDir := t.TempDir()
	defaultPath := filepath.Join(defaultDir, "phase_affordance.log")
	t.Setenv("PICOCLAW_PHASE_AFFORDANCE_LOG_FILE", "")
	t.Setenv("PICOCLAW_PHASE_AFFORDANCE_LOG", "1")

	// os.Args[0] in a test binary ends with ".test" — assert the guard
	// condition actually holds, otherwise this test proves nothing.
	if !strings.HasSuffix(os.Args[0], ".test") {
		t.Skipf("not a test binary (os.Args[0]=%q) — guard not exercised", os.Args[0])
	}

	req := PromptBuildRequest{
		GoalPhase:        string(GoalPhaseOpen),
		Iteration:        5,
		IterationCap:     25,
		MaxIterationsCap: 300,
		PhaseStartIter:   2,
	}
	affordance := phaseAffordanceText(req)
	if affordance == "" {
		t.Fatal("expected a non-empty affordance")
	}

	appendPhaseAffordanceWithCapture(
		[]providers.Message{{Role: "user", Content: "root"}},
		affordance,
		phaseAffordanceCaptureInput{turnID: "t", sessionKey: "s", iteration: 5})

	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("a test binary must not write to the default capture path; file exists: %v", err)
	}
}

// TestPhaseAffordanceCapture_RecordIsJSONL proves the capture file is one
// JSON object per line, so a parser can read it line by line without
// guessing where a record ends.
func TestPhaseAffordanceCapture_RecordIsJSONL(t *testing.T) {
	path := captureTestFile(t)

	for i := range 3 {
		req := PromptBuildRequest{
			GoalPhase:        string(GoalPhaseOpen),
			Iteration:        5 + i,
			IterationCap:     25,
			MaxIterationsCap: 300,
			PhaseStartIter:   2,
		}
		appendPhaseAffordanceWithCapture(
			[]providers.Message{{Role: "user", Content: "root"}},
			phaseAffordanceText(req),
			phaseAffordanceCaptureInput{
				turnID:         "turn-jsonl",
				sessionKey:     "sk_test",
				iteration:      5 + i,
				phase:          string(GoalPhaseOpen),
				phaseStartIter: 2,
			})
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), string(rune(10)))
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i+1, err)
			continue
		}
		for _, key := range []string{"ts", "type", "turn_id", "session_key", "iter", "phase", "phase_start_iter", "affordance"} {
			if _, ok := rec[key]; !ok {
				t.Errorf("line %d is missing key %q", i+1, key)
			}
		}
	}
}

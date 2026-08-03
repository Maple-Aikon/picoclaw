// Package agent — Phase 12.45 prompt-history log tests for the recall path.
//
// renderReplayPromptBlock + writeReplayPromptLog + logReplayPromptBlock are
// the Go-side [RECALL] writers (the JS hook never runs for RecallLLM calls).
// Coverage: render format (JSON header, markers, escaping, caps), atomic
// append under concurrency, file perms, env opt-out (Q5A), and the
// [RECALL FAILED] single-block contract (A-F14).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func testReplayTime() time.Time {
	return time.Date(2026, 8, 3, 22, 0, 0, 0, time.FixedZone("ICT", 7*3600))
}

// =============================================================================
// T6 — render block: JSON header + markers + content + escaping
func TestReplayPromptLog_RendersBlock(t *testing.T) {
	in := replayBlockInput{
		turnID:     "turn-recall-test",
		helperName: "handleGoalRecovery",
		iteration:  5,
		messages: []providers.Message{
			{Role: "system", Content: "Goal phase: open (iter 5/15)\nCall complete_goal when done."},
			{Role: "user", Content: "Run horus protocol"},
		},
		resp: &providers.LLMResponse{
			Content:    "Recovered.",
			FinishReason: "stop",
			ToolCalls: []providers.ToolCall{
				{ID: "call-1", Name: "complete_goal", Arguments: map[string]any{"summary": "done"}},
			},
			Usage: &providers.UsageInfo{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		},
	}

	block := renderReplayPromptBlock(in, testReplayTime())

	// 1-line JSON meta header (B-F10), parseable.
	lines := strings.Split(block, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}
	meta := parseReplayMetaHeader(t, lines[0])
	if meta["type"] != "recall" {
		t.Errorf("expected type=recall, got %v", meta["type"])
	}
	if meta["turn_id"] != "turn-recall-test" || meta["helper"] != "handleGoalRecovery" {
		t.Errorf("unexpected meta: %v", meta)
	}
	if meta["iter"] != float64(5) {
		t.Errorf("expected iter=5, got %v", meta["iter"])
	}
	if meta["ts"] != "2026-08-03T22:00:00+07:00" {
		t.Errorf("unexpected ts: %v", meta["ts"])
	}

	// Markers + content.
	for _, want := range []string{
		">>> [RECALL START] ID: turn-recall-test | Helper: handleGoalRecovery | Iteration: 5 | 22:00:00",
		"[SYSTEM]: Goal phase: open (iter 5/15)⏎Call complete_goal when done.",
		"[USER]: Run horus protocol",
		"[ASSISTANT]: Recovered.",
		"[TOOL_CALL]: complete_goal({\"summary\":\"done\"}) [ID: call-1]",
		"[USAGE]: P: 1 | C: 2 | T: 3",
		"<<< [RECALL END]",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q\n--- block ---\n%s", want, block)
		}
	}

	// Escaping (A-F04): raw \n must not appear inside a content line.
	for _, line := range lines {
		if strings.Contains(line, "[SYSTEM]:") && strings.Contains(line, "\n") {
			t.Errorf("system line contains raw newline: %q", line)
		}
	}
}

// =============================================================================
// T6b — failed block: single [RECALL FAILED] with helper + final error
func TestReplayPromptLog_RendersFailedBlock(t *testing.T) {
	in := replayBlockInput{
		turnID:     "turn-recall-test",
		helperName: "retryExecuteToolChain",
		iteration:  7,
		err:        errors.New("provider 503: upstream down"),
	}
	block := renderReplayPromptBlock(in, testReplayTime())

	lines := strings.Split(block, "\n")
	meta := parseReplayMetaHeader(t, lines[0])
	if meta["type"] != "recall_failed" {
		t.Errorf("expected type=recall_failed, got %v", meta["type"])
	}
	if meta["error"] != "provider 503: upstream down" {
		t.Errorf("expected error in meta, got %v", meta["error"])
	}
	for _, want := range []string{
		">>> [RECALL FAILED] ID: turn-recall-test | Helper: retryExecuteToolChain | Iteration: 7",
		"[ERROR]: provider 503: upstream down",
		"<<< [RECALL END]",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("failed block missing %q\n--- block ---\n%s", want, block)
		}
	}
	if strings.Contains(block, "[RECALL START]") {
		t.Error("failed block must not contain [RECALL START]")
	}
}

func parseReplayMetaHeader(t *testing.T, line string) map[string]any {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal([]byte(line), &meta); err != nil {
		t.Fatalf("meta header not parseable JSON: %v\nline: %q", err, line)
	}
	return meta
}

// =============================================================================
// T7 — RecallLLM success writes block; all-attempts-fail writes exactly 1
// [RECALL FAILED] block
func TestReplayPromptLog_WritesBlock(t *testing.T) {
	provider := &recallTestProvider{
		responses: []*providers.LLMResponse{{Content: "recovered", FinishReason: "stop"}},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	if _, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "handleGoalRecovery", nil); err != nil {
		t.Fatalf("RecallLLM returned error: %v", err)
	}

	data, err := os.ReadFile(replayPromptLogPath())
	if err != nil {
		t.Fatalf("read prompt log: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, ">>> [RECALL START]") {
		t.Fatalf("expected recall block in log, got:\n%s", content)
	}
	if !strings.Contains(content, "Helper: handleGoalRecovery") {
		t.Fatalf("expected helper name in block, got:\n%s", content)
	}
	if strings.Contains(content, "[RECALL FAILED]") {
		t.Fatalf("unexpected failed block on success:\n%s", content)
	}
}

func TestReplayPromptLog_WritesSingleFailedBlock(t *testing.T) {
	provider := &recallTestProvider{
		// All 3 attempts transient-fail → exhausted path (A-F14: exactly 1 block).
		errors: []error{
			fmt.Errorf("connection refused: attempt 0"),
			fmt.Errorf("connection refused: attempt 1"),
			fmt.Errorf("connection refused: attempt 2"),
		},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	_, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "retryExecuteToolChain", nil)
	if err == nil {
		t.Fatal("expected error after all transient attempts fail")
	}

	data, readErr := os.ReadFile(replayPromptLogPath())
	if readErr != nil {
		t.Fatalf("read prompt log: %v", readErr)
	}
	content := string(data)
	if got := strings.Count(content, ">>> [RECALL FAILED]"); got != 1 {
		t.Fatalf("expected exactly 1 [RECALL FAILED] block, got %d:\n%s", got, content)
	}
	if !strings.Contains(content, "Helper: retryExecuteToolChain") {
		t.Fatalf("expected helper name in failed block:\n%s", content)
	}
	if !strings.Contains(content, "connection refused: attempt 2") {
		t.Fatalf("expected final error in failed block:\n%s", content)
	}
	if strings.Count(content, ">>> [RECALL START]") != 0 {
		t.Fatalf("failed recall must not write success blocks:\n%s", content)
	}
}

// =============================================================================
// T7b — write error does not fail the turn (R2 fail-safe)
func TestReplayPromptLog_WriteErrorDoesNotFailTurn(t *testing.T) {
	provider := &recallTestProvider{
		responses: []*providers.LLMResponse{{Content: "recovered", FinishReason: "stop"}},
	}
	al, _, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	pipeline := NewPipeline(al)
	ts, exec := setupRecallTestTurnState(t, al, pipeline)

	lockedDir := t.TempDir()
	t.Setenv("PICOCLAW_HOOK_LOG_FILE", filepath.Join(lockedDir, "prompt.log"))
	if err := os.Chmod(lockedDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(lockedDir, 0o755) // allow TempDir cleanup

	resp, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "handleGoalRecovery", nil)
	if err != nil {
		t.Fatalf("RecallLLM must not fail when prompt log is unwritable: %v", err)
	}
	if resp == nil || resp.Content != "recovered" {
		t.Fatalf("expected recovered response, got %+v", resp)
	}
}

// =============================================================================
// T9 — concurrent appenders: no interleaving (A-F01/B-F01/B-F08)
func TestReplayPromptLog_ConcurrentAppenders_NoInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.log")
	const writers = 4
	const blocksPerWriter = 25

	blocks := make([][]string, writers)
	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < blocksPerWriter; i++ {
				// Writer 3 mimics the JS hook format (no RECALL markers).
				marker := ">>> [RECALL START]"
				footer := "<<< [RECALL END]"
				if g == 3 {
					marker = "[JS-HOOK]"
					footer = "[JS-END]"
				}
				block := fmt.Sprintf("%s ID: turn-%d-%d | Helper: concurrent | Iteration: %d\n    [SYSTEM]: content-%d-%d\n%s\n",
					marker, g, i, i, g, i, footer)
				blocks[g] = append(blocks[g], block)
				if err := writeReplayPromptLog(path, block); err != nil {
					t.Errorf("write %d-%d: %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fileContent := string(data)

	// Every block appears byte-for-byte intact exactly once — any
	// interleaving would break a block's contiguous occurrence.
	total := 0
	for g := 0; g < writers; g++ {
		for _, block := range blocks[g] {
			if got := strings.Count(fileContent, block); got != 1 {
				t.Fatalf("block %q appeared %d times (interleaved?)", block, got)
			}
			total++
		}
	}
	if total != writers*blocksPerWriter {
		t.Fatalf("expected %d blocks, got %d", writers*blocksPerWriter, total)
	}

	// START/END pairing: every START must be followed by its END before
	// the next START (no nested/interleaved blocks from Go writers).
	starts := strings.Count(fileContent, ">>> [RECALL START]")
	ends := strings.Count(fileContent, "<<< [RECALL END]")
	if starts != blocksPerWriter*3 || ends != blocksPerWriter*3 {
		t.Fatalf("expected %d START and %d END markers, got %d/%d",
			blocksPerWriter*3, blocksPerWriter*3, starts, ends)
	}
}

// =============================================================================
// T11 — file mode 0640 (A-F11/B-F04)
func TestReplayPromptLog_FileMode_0640(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.log")
	if err := writeReplayPromptLog(path, ">>> [RECALL START] x\n<<< [RECALL END]\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("expected file mode 0640, got %o", got)
	}

	// Pre-existing file with looser perms gets tightened by Chmod.
	loosePath := filepath.Join(t.TempDir(), "prompt.log")
	if err := os.WriteFile(loosePath, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed loose file: %v", err)
	}
	if err := writeReplayPromptLog(loosePath, ">>> [RECALL START] y\n<<< [RECALL END]\n"); err != nil {
		t.Fatalf("append to loose file: %v", err)
	}
	info, err = os.Stat(loosePath)
	if err != nil {
		t.Fatalf("stat loose: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("expected tightened mode 0640, got %o", got)
	}
}

// =============================================================================
// T12 — huge messages truncated, block bounded (A-F02/B-F02)
func TestReplayPromptLog_TruncatesHugeMessages(t *testing.T) {
	huge := strings.Repeat("x", 5*1024*1024) // 5MB message
	in := replayBlockInput{
		turnID:     "turn-recall-test",
		helperName: "handleGoalRecovery",
		iteration:  1,
		messages: []providers.Message{
			{Role: "system", Content: huge},
			{Role: "user", Content: huge},
		},
		resp: &providers.LLMResponse{
			Content: strings.Repeat("y", 2*1024*1024), // 2MB response
			FinishReason: "stop",
		},
	}

	start := time.Now()
	block := renderReplayPromptBlock(in, testReplayTime())
	elapsed := time.Since(start)

	if !strings.Contains(block, "...[truncated") {
		t.Fatal("expected truncation marker in block")
	}
	if len(block) > MaxReplayBlockBytes+128 {
		t.Fatalf("block too large: %d bytes (cap %d)", len(block), MaxReplayBlockBytes)
	}
	if strings.Count(block, "<<< [RECALL END]") != 1 {
		t.Fatal("block footer must survive truncation")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("render too slow on huge input: %v", elapsed)
	}
}

// =============================================================================
// T13 — env opt-out (B-F09 + Q5A): PICOCLAW_REPLAY_PROMPT_LOG=0|false off,
// unset on
func TestReplayPromptLog_EnvOptOut(t *testing.T) {
	newProvider := func() *recallTestProvider {
		return &recallTestProvider{
			responses: []*providers.LLMResponse{{Content: "recovered", FinishReason: "stop"}},
		}
	}
	runRecall := func(t *testing.T) {
		t.Helper()
		provider := newProvider()
		al, _, cleanup := newTurnCoordTestLoop(t, provider)
		defer cleanup()
		pipeline := NewPipeline(al)
		ts, exec := setupRecallTestTurnState(t, al, pipeline)
		if _, err := pipeline.RecallLLM(context.Background(), context.Background(), ts, exec, 1, "handleGoalRecovery", nil); err != nil {
			t.Fatalf("RecallLLM returned error: %v", err)
		}
	}

	// (a) "0" → no block written
	t.Setenv("PICOCLAW_REPLAY_PROMPT_LOG", "0")
	runRecall(t)
	if data, err := os.ReadFile(replayPromptLogPath()); err == nil && strings.Contains(string(data), "[RECALL") {
		t.Fatalf("expected no recall block with PICOCLAW_REPLAY_PROMPT_LOG=0, got:\n%s", data)
	}

	// (c) "false" → also off
	t.Setenv("PICOCLAW_REPLAY_PROMPT_LOG", "false")
	runRecall(t)
	if data, err := os.ReadFile(replayPromptLogPath()); err == nil && strings.Contains(string(data), "[RECALL") {
		t.Fatalf("expected no recall block with PICOCLAW_REPLAY_PROMPT_LOG=false, got:\n%s", data)
	}

	// (b) unset → block written (default ON)
	oldVal, hadVal := os.LookupEnv("PICOCLAW_REPLAY_PROMPT_LOG")
	os.Unsetenv("PICOCLAW_REPLAY_PROMPT_LOG")
	t.Cleanup(func() {
		if hadVal {
			os.Setenv("PICOCLAW_REPLAY_PROMPT_LOG", oldVal)
		} else {
			os.Unsetenv("PICOCLAW_REPLAY_PROMPT_LOG")
		}
	})
	runRecall(t)
	data, err := os.ReadFile(replayPromptLogPath())
	if err != nil {
		t.Fatalf("expected recall block with env unset: %v", err)
	}
	if !strings.Contains(string(data), ">>> [RECALL START]") {
		t.Fatalf("expected recall block with env unset, got:\n%s", data)
	}
}

package agent

// QUALITY BAR CONTRACT — lever (a), 2026-11-10 (anh Maple duyệt 2026-11-10)
// ---------------------------------------------------------------------------
// Mục tiêu: giữ `system` prompt BYTE-CONSTANT qua mọi goal-phase transition
// trong 1 turn, để provider prefix cache không bị mất ở điểm gãy.
//
// ĐO ĐƯỢC (probe24/27 + production gateway.log 2026-11-10):
//   - transition OPEN→CHECKPOINT hiện tại: cache_read 1.536, input 80.808
//     ⇒ tax ~81K token cho 1 lần gọi.
//   - probe24 B-A-ONLY (system const): 92.654 → 83.949 = −9,4%.
//   - probe24 D-TRUE-CTRL (system const + tools const): 2.760 ⇒ tax gần 0.
//   ⇒ (a) là điều kiện CẦN, nhưng CHƯA ĐỦ: tools churn 76→2 là nguyên nhân lớn.
//   ⇒ (b) sẽ bàn riêng sau (anh nói rõ).
//
// RÀNG BUỘC CHẤT LƯỢNG (contract):
//   C1. system prompt phải BYTE-IDENTICAL qua SET→OPEN→CHECKPOINT→FINAL.
//   C2. Nội dung phase-specific (hint + goal snapshot + compass) PHẢI vẫn tới
//       được LLM — qua turn-tail channel, KHÔNG được bỏ (misfire 52,9% lịch
//       sử là hậu quả khi LLM không thấy affordance).
//   C3. Turn-tail channel PHẢI ở ĐUÔI (append), không rewrite tại chỗ —
//       rewrite tại chỗ = giữa chuỗi = mất cache phía sau (probe27:
//       K-NEW-INPLACE 94.347 vs H-NEW-TAIL 84.059).
//   C4. Không được đổi wire shape của provider (1 system message, user[0]
//       = dynamic context + user text).
//   C5. Test cũ KHÔNG được xoá; phải cập nhật để assert vị trí MỚI.
//
// LƯU Ý VỀ CHECKPOINT: ở CHECKPOINT chỉ 2 tool được phép (goal_progress,
// complete_goal) — LLM không thể gọi tool nào khác. Vì vậy turn-tail channel
// ở CHECKPOINT phải là một USER message (tool result không hợp lệ vì không
// có tool_use id để trả lời).

import (
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// removeAll — small helper so each test cleans its temp workspace.
func removeAll(dir string) { _ = os.RemoveAll(dir) }

// joinAllContents concatenates every message body so a test can assert on
// what the LLM actually sees (system + user + assistant), not just the
// system block.
func joinAllContents(msgs []providers.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString(string(rune(10)))
	}
	return b.String()
}

const leverASnapshot = `## Goal: demo

**Objective:** obj

**Success criteria:**
- a
`

// C1 — system prompt byte-constant across phases.
func TestLeverA_SystemPromptByteConstantAcrossPhases(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	phases := []string{
		string(GoalPhaseSet),
		string(GoalPhaseOpen),
		string(GoalPhaseCheckpoint),
		string(GoalPhaseFinal),
	}
	var first string
	var firstPhase string
	for _, ph := range phases {
		cb := NewContextBuilder(tmpDir)
		req := PromptBuildRequest{
			GoalPhase:        ph,
			Iteration:        21,
			GoalSnapshot:     leverASnapshot,
			IterationCap:     25,
			MaxIterationsCap: 300,
			PhaseStartIter:   2,
			CurrentMessage:   "do the thing",
		}
		msgs := cb.BuildMessagesFromPrompt(req)
		if len(msgs) == 0 {
			t.Fatalf("phase %s: no messages", ph)
		}
		got := msgs[0].Content
		if first == "" {
			first, firstPhase = got, ph
			continue
		}
		if got != first {
			t.Errorf("system prompt differs between phase %s and %s (lever (a) broken)", firstPhase, ph)
		}
	}
}

// C2 — phase-specific content must still reach the LLM via the turn-tail
// channel (never silently dropped).
func TestLeverA_PhaseHintsStillReachLLM(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	cases := []struct {
		phase   string
		postCR  bool
		wantSub string
	}{
		{string(GoalPhaseSet), false, "set_goal"},
		{string(GoalPhaseOpen), false, "goal_progress"},
		{string(GoalPhaseCheckpoint), false, "goal_progress"},
		{string(GoalPhaseFinal), false, "complete_goal"},
		{string(GoalPhaseFinal), true, "final report"},
	}
	for _, tc := range cases {
		cb := NewContextBuilder(tmpDir)
		req := PromptBuildRequest{
			GoalPhase:              tc.phase,
			PostCompleteGoalReport: tc.postCR,
			Iteration:              21,
			GoalSnapshot:           leverASnapshot,
			IterationCap:           25,
			MaxIterationsCap:       300,
			PhaseStartIter:         2,
			CurrentMessage:         "do the thing",
		}
		msgs := cb.BuildMessagesFromPrompt(req)
		joined := joinAllContents(msgs)
		if !strings.Contains(joined, tc.wantSub) {
			t.Errorf("phase %s (postCR=%v): LLM-visible content missing %q — phase affordance was dropped",
				tc.phase, tc.postCR, tc.wantSub)
		}
	}
}

// C2b — the goal snapshot (objective + success criteria) must reach the LLM
// at CHECKPOINT: the LLM decides goal_progress vs complete_goal from it.
func TestLeverA_GoalSnapshotStillReachesLLMAtCheckpoint(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	snapshot := `## Goal: demo

**Objective:** unique-objective-marker-42

**Success criteria:**
- unique-criterion-marker-77
`
	cb := NewContextBuilder(tmpDir)
	req := PromptBuildRequest{
		GoalPhase:        string(GoalPhaseCheckpoint),
		Iteration:        21,
		GoalSnapshot:     snapshot,
		IterationCap:     25,
		MaxIterationsCap: 300,
		PhaseStartIter:   2,
		CurrentMessage:   "do the thing",
	}
	msgs := cb.BuildMessagesFromPrompt(req)
	joined := joinAllContents(msgs)
	if !strings.Contains(joined, "unique-objective-marker-42") {
		t.Errorf("goal objective missing from LLM-visible content at CHECKPOINT")
	}
	if !strings.Contains(joined, "unique-criterion-marker-77") {
		t.Errorf("success criterion missing from LLM-visible content at CHECKPOINT")
	}
}

// C3 — the phase channel must be APPENDED at the tail, never rewritten in
// place. A mid-array rewrite is what pins the cache (probe27: 94.347 vs
// 84.059 uncached).
func TestLeverA_PhaseChannelAppendedNotRewrittenInPlace(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	base := PromptBuildRequest{
		GoalPhase:        string(GoalPhaseOpen),
		Iteration:        21,
		IterationCap:     25,
		MaxIterationsCap: 300,
		PhaseStartIter:   2,
		CurrentMessage:   "do the thing",
	}
	msgsOpen := cb.BuildMessagesFromPrompt(base)

	ckReq := base
	ckReq.GoalPhase = string(GoalPhaseCheckpoint)
	ckReq.GoalSnapshot = leverASnapshot
	cb2 := NewContextBuilder(tmpDir)
	msgsCk := cb2.BuildMessagesFromPrompt(ckReq)

	if len(msgsOpen) < 2 || len(msgsCk) < 2 {
		t.Fatalf("expected >=2 messages, got open=%d ckpt=%d", len(msgsOpen), len(msgsCk))
	}
	if msgsOpen[0].Content != msgsCk[0].Content {
		t.Errorf("system message changed between OPEN and CHECKPOINT — lever (a) broken")
	}
	if len(msgsCk) < len(msgsOpen) {
		t.Errorf("CHECKPOINT message count %d < OPEN %d — phase channel was spliced out", len(msgsCk), len(msgsOpen))
	}
}

// C4 — wire shape: exactly one system message; the FIRST user message of the
// turn still carries the dynamic-context wrapper + the user text.
func TestLeverA_WireShapeUnchanged(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	req := PromptBuildRequest{
		GoalPhase:        string(GoalPhaseCheckpoint),
		Iteration:        21,
		IterationCap:     25,
		MaxIterationsCap: 300,
		PhaseStartIter:   2,
		CurrentMessage:   "do the thing",
	}
	msgs := cb.BuildMessagesFromPrompt(req)

	sysCount := 0
	for _, m := range msgs {
		if m.Role == "system" {
			sysCount++
		}
	}
	if sysCount != 1 {
		t.Errorf("expected exactly 1 system message, got %d", sysCount)
	}
	found := false
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, "do the thing") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("turn user message lost its content")
	}
}

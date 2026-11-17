package agent

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ---------------------------------------------------------------------------
// A1 — TURN-TAIL PHASE AFFORDANCE (2026-11-17, anh Maple duyệt A1)
// ---------------------------------------------------------------------------
//
// ROOT CAUSE (đo được, không phải giả thuyết):
//
//	MiniMax cache theo PREFIX. Bất kỳ thay đổi byte ở GIỮA chuỗi làm mất
//	cache của TOÀN BỘ phần phía sau.
//
//	turn-root user message nằm ở wire INDEX 2 ([0] system, [1] assistant,
//	[2] user) với ~138 message phía sau nó.
//
//	refreshGoalPhaseBannerInMessages() (Phase 12.80, 11-03) VIẾT LẠI TẠI
//	CHỖ block <goal_phase> trong message đó ⇒ đổi byte giữa chuỗi ⇒ mất
//	cache cả lượt hiện tại (đo: uncached ÷ traffic lượt hiện tại ≈ 1,00;
//	80,6 % uncached nằm ở phase-transition).
//
// WIRE SHAPE MỤC TIÊU (đo ở TẦNG API, payload thật + key thật):
//
//	V-INPLACE (rewrite tại chỗ = code cũ)  uncached 18.848
//	V-TAIL    (1 USER message riêng ở đuôi) uncached  3.293   ⇒ 5,7× tốt hơn
//	V-NONE    (control)                    uncached  3.191
//
// HỢP ĐỒNG CHẤT LƯỢNG (contract) — bổ sung C6..C8 cho lever (a):
//
//	C6. user_root (message user đầu tiên của turn) PHẢI byte-constant
//	    suốt turn — qua mọi phase transition.
//	C7. Affordance (banner + channel) PHẢI là MỘT USER message RIÊNG ở
//	    ĐUÔI mảng wire — KHÔNG gộp vào message cuối, KHÔNG sửa message
//	    nào đã nằm trong mảng.
//	C8. Banner và channel PHẢI luôn cùng phase (không stale) — vì chúng
//	    được render CÙNG LẦN.
//
// 🪤 BẪY ĐO (rất đắt — đừng lặp lại):
//
//	1. "Base HEAD ≠ code đang chạy" — luôn tạo `git worktree` baseline
//	   trước khi gắn nhãn "pre-existing".
//	2. Affordance ở ĐUÔI là USER message RIÊNG. Nếu gộp vào message cuối
//	   thì wire shape đổi ⇒ vi phạm C4 của lever (a) (user[0] phải là
//	   dynamic context + user text).
//	3. KHÔNG được strip/mutate byte của message nào đã nằm trong mảng —
//	   đó chính là A1 (rewrite tại chỗ) mà ta đang sửa.
//
// 🪤 BẪY ĐO #2 (11-17, trong lúc viết test này):
//
//	`write_file` với payload chứa \n sẽ ghi NEWLINE THẬT vào file Go
//	⇒ "string literal not terminated". Dùng script Python riêng để sinh file
//	(test/impl) khi cần escape.

// a1UserRootContent returns the content of the FIRST user message of the
// turn (the "turn-root user message"), or "" when there is none.
//
// The turn-root user message is NOT simply the last user message:
// steering messages also have Role=="user" (pkg/agent/steering.go).
func a1UserRootContent(messages []providers.Message) string {
	for _, m := range messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// a1Digest is a stable digest of a message body (for byte-constancy asserts).
func a1Digest(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// ---------------------------------------------------------------------------
// C6 — user_root byte-constant across a phase transition.
// ---------------------------------------------------------------------------

// TestA1_C6_UserRootByteConstantAcrossPhaseTransition is the CORE A1
// contract. It fails on the pre-A1 code, where the phase-change rebuild
// rewrote the <goal_phase> banner INSIDE the turn-root user message —
// changing bytes in the MIDDLE of the request and killing the prefix cache
// for the whole turn.
func TestA1_C6_UserRootByteConstantAcrossPhaseTransition(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	openReq := PromptBuildRequest{
		GoalPhase:               string(GoalPhaseOpen),
		Iteration:               21,
		IterationCap:            25,
		MaxIterationsCap:        300,
		PhaseStartIter:          2,
		CurrentMessage:          "do the thing",
		SuppressPhaseAffordance: true,
	}
	ckReq := openReq
	ckReq.GoalPhase = string(GoalPhaseCheckpoint)
	ckReq.GoalSnapshot = leverASnapshot

	openMsgs := cb.BuildMessagesFromPrompt(openReq)
	ckMsgs := cb.BuildMessagesFromPrompt(ckReq)

	openRoot := a1UserRootContent(openMsgs)
	ckRoot := a1UserRootContent(ckMsgs)
	if openRoot == "" || ckRoot == "" {
		t.Fatalf("expected a turn-root user message in both builds (open=%q ckpt=%q)", openRoot, ckRoot)
	}

	if a1Digest(openRoot) != a1Digest(ckRoot) {
		t.Errorf("turn-root user message changed across the OPEN->CHECKPOINT transition — "+
			"A1 violated (this rewrite happens in the MIDDLE of the request and kills the prefix cache). "+
			"OPEN digest=%s CHECKPT digest=%s", a1Digest(openRoot), a1Digest(ckRoot))
	}
}

// TestA1_C6b_NoBannerInUserRoot pins that the turn-root user message does
// NOT carry the phase banner at all. The banner's home is the TAIL
// (contract C7) — if it is also in user_root, the bytes change again at
// the next transition.
func TestA1_C6b_NoBannerInUserRoot(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	req := PromptBuildRequest{
		GoalPhase:               string(GoalPhaseCheckpoint),
		Iteration:               21,
		IterationCap:            25,
		MaxIterationsCap:        300,
		PhaseStartIter:          2,
		CurrentMessage:          "do the thing",
		SuppressPhaseAffordance: true,
	}
	msgs := cb.BuildMessagesFromPrompt(req)
	root := a1UserRootContent(msgs)
	if root == "" {
		t.Fatal("expected a turn-root user message")
	}
	if goalPhaseBannerBlockRe.MatchString(root) {
		t.Errorf("turn-root user message carries a <goal_phase> banner — the banner must live at the TAIL only")
	}
}

// ---------------------------------------------------------------------------
// C7 — the affordance is ONE dedicated USER message at the TAIL.
// ---------------------------------------------------------------------------

// TestA1_C7_AffordanceIsOneTailUserMessage pins the wire shape validated by
// the API-level probe: the affordance is a SINGLE user message at the very
// END of the array — not merged into the last message, and no existing
// message is modified.
func TestA1_C7_AffordanceIsOneTailUserMessage(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	req := PromptBuildRequest{
		GoalPhase:               string(GoalPhaseCheckpoint),
		Iteration:               21,
		GoalSnapshot:            leverASnapshot,
		IterationCap:            25,
		MaxIterationsCap:        300,
		PhaseStartIter:          2,
		CurrentMessage:          "do the thing",
		SuppressPhaseAffordance: true,
	}

	// Build the request the way production does, then append the tail
	// affordance the way callLLMCore does.
	msgs := cb.BuildMessagesFromPrompt(req)
	msgs = appendPhaseAffordanceAtTail(msgs, phaseAffordanceText(req))

	if len(msgs) < 2 {
		t.Fatalf("expected >=2 messages, got %d", len(msgs))
	}

	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("tail message role = %q, want user", last.Role)
	}
	if !strings.Contains(last.Content, phaseChannelMarker) {
		t.Errorf("tail message does not carry the phase channel")
	}
	if !goalPhaseBannerBlockRe.MatchString(last.Content) {
		t.Errorf("tail message does not carry the <goal_phase> banner")
	}

	// The user text must NOT be duplicated into the tail message — it stays
	// in user_root (contract C4 of lever (a)).
	if strings.Contains(last.Content, "do the thing") {
		t.Errorf("tail affordance message must not duplicate the user text (user text belongs to user_root)")
	}

	// Exactly ONE affordance message in the whole array.
	affordanceCount := 0
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, phaseChannelMarker) {
			affordanceCount++
		}
	}
	if affordanceCount != 1 {
		t.Errorf("expected exactly 1 phase-channel message, got %d", affordanceCount)
	}
}

// ---------------------------------------------------------------------------
// C8 — banner and channel always agree on the phase (no stale channel).
// ---------------------------------------------------------------------------

// TestA1_C8_BannerAndChannelAgree is the stale-channel regression test.
//
// Measured on prompt_history.log (the only non-truncating source) BEFORE
// the fix: 35/35 prompts that carried a banner had a channel saying a
// DIFFERENT phase ("SET (iter 1)") — because context.go appended the
// channel ONCE at turn build and nobody refreshed it. The measured
// consequence was a CHECKPOINT misfire rate of 54,42 %.
//
// Mechanism of the fix: banner and channel are rendered TOGETHER, so they
// cannot disagree.
func TestA1_C8_BannerAndChannelAgree(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)

	_ = NewContextBuilder(tmpDir)

	for _, phase := range []GoalPhase{GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint, GoalPhaseFinal} {
		t.Run(string(phase), func(t *testing.T) {
			req := PromptBuildRequest{
				GoalPhase:               string(phase),
				Iteration:               21,
				GoalSnapshot:            leverASnapshot,
				IterationCap:            25,
				MaxIterationsCap:        300,
				PhaseStartIter:          2,
				CurrentMessage:          "do the thing",
				SuppressPhaseAffordance: true,
			}
			tail := phaseAffordanceText(req)
			if tail == "" {
				t.Fatalf("no affordance for phase %s", phase)
			}

			bannerPhase := a1BannerPhase(tail)
			if bannerPhase == "" {
				t.Fatalf("no <goal_phase> banner in the affordance for phase %s", phase)
			}
			if !strings.EqualFold(bannerPhase, string(phase)) {
				t.Errorf("banner phase = %q, want %q", bannerPhase, phase)
			}

			channelPhase := a1ChannelPhase(tail)
			if channelPhase != "" && !strings.EqualFold(channelPhase, string(phase)) {
				t.Errorf("STALE CHANNEL: channel says %q but the banner says %q (phase %s)",
					channelPhase, bannerPhase, phase)
			}
		})
	}
}

// a1BannerPhase extracts the phase attribute from a <goal_phase ...> block.
func a1BannerPhase(content string) string {
	loc := goalPhaseBannerBlockRe.FindStringIndex(content)
	if loc == nil {
		return ""
	}
	block := content[loc[0]:loc[1]]
	const marker = `phase="`
	start := strings.Index(block, marker)
	if start < 0 {
		return ""
	}
	rest := block[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// a1ChannelPhase extracts the phase the <goal_phase_channel> block claims.
//
// The channel text embeds a phase header like "Goal phase: CHECKPOINT" or
// "Goal phase: SET" — scan for the first such header.
func a1ChannelPhase(content string) string {
	start := strings.Index(content, phaseChannelMarker)
	if start < 0 {
		return ""
	}
	channel := content[start:]
	for _, phase := range []GoalPhase{
		GoalPhaseSet, GoalPhaseOpen, GoalPhaseCheckpoint,
		GoalPhaseFinal, GoalPhasePostFinal,
	} {
		needle := "Goal phase: " + string(phase)
		if strings.Contains(channel, needle) {
			return string(phase)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// C7b — appending the affordance never mutates the caller's messages.
// ---------------------------------------------------------------------------

// TestA1_C7b_AppendIsCopyOnWrite pins that appending the tail affordance
// does NOT modify any existing message in place. Mutating in place is
// exactly the A1 bug: it changes bytes in the MIDDLE of the request.
func TestA1_C7b_AppendIsCopyOnWrite(t *testing.T) {
	base := []providers.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello there"},
		{Role: "assistant", Content: "ok"},
	}
	before := make([]string, len(base))
	for i, m := range base {
		before[i] = m.Content
	}

	out := appendPhaseAffordanceAtTail(base, "goal_phase affordance")

	for i := range base {
		if base[i].Content != before[i] {
			t.Errorf("message[%d] was mutated in place: %q -> %q", i, before[i], base[i].Content)
		}
	}
	if len(out) != len(base)+1 {
		t.Fatalf("expected %d messages after append, got %d", len(base)+1, len(out))
	}
	if out[len(out)-1].Role != "user" {
		t.Errorf("appended message role = %q, want user", out[len(out)-1].Role)
	}
}

// TestA1_C7c_AppendEmptyIsNoOp pins that an empty affordance changes
// nothing (no empty user message appended).
func TestA1_C7c_AppendEmptyIsNoOp(t *testing.T) {
	base := []providers.Message{{Role: "system", Content: "sys"}}
	out := appendPhaseAffordanceAtTail(base, "")
	if len(out) != len(base) {
		t.Errorf("expected %d messages (no-op), got %d", len(base), len(out))
	}
}

// TestA1_C7d_TailAffordanceIsTheLastProviderMessage pins the wire shape at
// the level the provider actually sees: the affordance IS the last message.
func TestA1_C7d_TailAffordanceIsTheLastProviderMessage(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENTS.md":        "# Shared",
		"memory/MEMORY.md": "# Memory",
	})
	defer removeAll(tmpDir)
	cb := NewContextBuilder(tmpDir)
	req := PromptBuildRequest{
		GoalPhase:               string(GoalPhaseOpen),
		Iteration:               5,
		IterationCap:            25,
		MaxIterationsCap:        300,
		PhaseStartIter:          2,
		CurrentMessage:          "do the thing",
		SuppressPhaseAffordance: true,
	}

	msgs := cb.BuildMessagesFromPrompt(req)
	msgs = appendPhaseAffordanceAtTail(msgs, phaseAffordanceText(req))

	tail, ok := a1TailAffordanceMessage(msgs)
	if !ok {
		t.Fatalf("the tail message is not the phase affordance")
	}
	if tail.Role != "user" {
		t.Errorf("tail affordance role = %q, want user", tail.Role)
	}
	if !strings.Contains(tail.Content, phaseChannelMarker) {
		t.Errorf("tail affordance does not carry the phase channel")
	}
}

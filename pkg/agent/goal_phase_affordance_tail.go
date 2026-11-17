package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// A1 FIX — 2026-11-17 (anh Maple duyệt A1).
//
// ---------------------------------------------------------------------------
// VÌ SAO
// ---------------------------------------------------------------------------
// MiniMax cache theo PREFIX. Bất kỳ thay đổi byte ở GIỮA chuỗi làm mất cache
// của TOÀN BỘ phần phía sau.
//
// ĐO ĐƯỢC (A1, 2026-11-16 — production 11-12..16 + controlled probe):
//
//   production (265 transition open<->checkpoint):
//     E1 tools UNCHANGED 265/265            -> không phải tools churn
//     E2 system_prompt_len UNCHANGED 258/265 -> không phải system rebuild
//     E3 gap tới lượt trước median 10s        -> KHÔNG phải TTL
//     E4 offset điểm gãy CỐ ĐỊNH trong 1 turn
//        (spread 1.728 tok, payload 106k -> 166k)
//     E5 uncached / traffic lượt hiện tại ~= 1.00
//        -> MẤT CACHE CẢ LƯỢT HIỆN TẠI
//     E6 wire shape: [0] system, [1] assistant, [2] user
//        -> turn-root user message ở INDEX 2 (rất sớm), ~138 message sau nó
//
//   controlled probe (payload thật, key thật, transition ở i6):
//     V-INPLACE (banner viết lại tại chỗ = CODE CŨ)  uncached 18.848
//     V-TAIL    (banner append ở đuôi array)        uncached  3.293
//     V-NONE    (control, không banner)              uncached  3.191
//     => V-TAIL tốt hơn V-INPLACE 5,7x
//
// ROOT CAUSE: refreshGoalPhaseBannerInMessages() (Phase 12.80, 11-03) viết
// lại TẠI CHỖ block <goal_phase> trong turn-root user message (index 2)
// => đổi byte giữa chuỗi => mất cache mọi message sau đó.
//
// 🚨 ĐÂY LÀ VI PHẠM CONTRACT C3 CỦA LEVER (a)
// (pkg/agent/lever_a_system_constant_test.go):
//   "C3. Turn-tail channel PHẢI ở ĐUÔI (append), không rewrite tại chỗ."
// Probe27 (11-10) đã cảnh báo trước bằng số 94.347 vs 84.059.
//
// ---------------------------------------------------------------------------
// CÁCH LÀM (A1 v3 — wire shape đã validate ở TẦNG API)
// ---------------------------------------------------------------------------
// Wire shape mà probe V-TAIL đã VALIDATE là: MỘT MESSAGE USER RIÊNG Ở ĐUÔI
// mảng wire — KHÔNG gộp vào message cuối, KHÔNG sửa message nào đã nằm
// trong mảng.
//
// Vì vậy bản sửa này:
//
//  1. KHÔNG đụng vào bất kỳ message nào đã có trong mảng (user_root giữ
//     byte-constant suốt turn => prefix cache sống).
//  2. Affordance được APPEND vào CUỐI mảng request — tức là chỉ tồn
//     tại trong request đi ra, KHÔNG nằm trong `exec.messages` =>
//     KHÔNG tích luỹ theo số lần chuyển phase.
//  3. Bản affordance cũ ở đuôi (nếu có) bị THAY bằng bản mới, không phải
//     cộng dồn.
//
// 🐞 BUG CORRECTNESS ĐI KÈM ĐÃ ĐƯỢC SỬA LUÔN:
//   <goal_phase_channel> trước đây STALE 100% (đo 35/35 trên
//   prompt_history.log: banner nói CHECKPOINT, channel nói "SET (iter 1)")
//   vì context.go append channel 1 lần lúc build turn và không ai refresh
//   nó. Hậu quả đo được: misfire ở CHECKPOINT = 54,42% (metric gate-rejection)
//   / 50,1% (metric llm_asked) — cùng baseline 52,9% của 11-03.
//   Với bản sửa này, channel được render LẠI ở mỗi phase transition và
//   append ở đuôi => channel LUÔN khớp với phase thật.

// phaseAffordanceText renders the phase affordance (the <goal_phase> banner
// + the <goal_phase_channel>) as ONE text block.
//
// The banner and the channel are rendered TOGETHER so they can never
// disagree again (the stale-channel bug).
//
// Returns "" when there is nothing phase-specific to say.
func phaseAffordanceText(req PromptBuildRequest) string {
	var parts []string
	if banner := formatDynamicGoalPhaseBanner(req); banner != "" {
		parts = append(parts, banner)
	}
	if channel := buildPhaseChannel(req); channel != "" {
		parts = append(parts, channel)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, bannerSeparator)
}

// phaseAffordanceRequest builds the request used to render the phase
// affordance for the CURRENT turn state.
//
// It mirrors promptBuildRequestForTurn for the fields the affordance
// depends on (phase, iteration, caps, goal snapshot, discovery/MCP note)
// WITHOUT rebuilding the whole prompt: the affordance must not depend on
// history, summary, skills or overlays, because the affordance is
// rendered on EVERY iteration of the turn.
//
// The discovery / MCP state lives on the builder (it is registered once
// per agent), so it is seeded here the same way BuildMessagesFromPrompt
// does it.
func phaseAffordanceRequest(cb *ContextBuilder, ts *turnState, iteration int) PromptBuildRequest {
	req := PromptBuildRequest{
		GoalPhase:              string(ts.currentGoalPhase()),
		PostCompleteGoalReport: ts.shouldEmitPostCompleteGoalReport(),
		Iteration:              iteration,
		PhaseStartIter:         ts.currentPhaseStartIter(),
		IterationCap:           ts.iterationCap,
		MaxIterationsCap:       ts.maxIterationsCap,
	}
	// A turn profile with the system prompt turned off must produce a BLANK
	// user message (see TestTurnProfile_SystemPromptOffBlankTurnStillSendsMessage).
	// The phase channel is part of the prompt payload, so it must be
	// suppressed together with the default system prompt.
	if turnProfileSystemPromptOff(ts.profile) {
		req.SuppressDefaultSystemPrompt = true
		return req
	}
	if string(ts.currentGoalPhase()) == string(GoalPhaseCheckpoint) {
		req.GoalSnapshot = loadGoalSnapshotForHint(ts.workspace, ts.sessionKey)
	}
	if cb != nil {
		if !req.ToolDiscoveryBM25 && !req.ToolDiscoveryRegex {
			req.ToolDiscoveryBM25 = cb.toolDiscoveryBM25
			req.ToolDiscoveryRegex = cb.toolDiscoveryRegex
		}
		if len(req.DeferredMCPServers) == 0 {
			req.DeferredMCPServers = cb.deferredMCPServers
		}
	}
	return req
}

// appendPhaseAffordanceAtTail returns messages with the phase affordance
// appended as a fresh USER message at the very END of the array.
//
// Why a USER message and not a tool result: at CHECKPOINT only
// goal_progress / complete_goal are permitted, and a tool_result message
// without a matching tool_use id is invalid under the Anthropic schema.
// A user message is the only valid shape there (same reasoning as the
// lever (a) phase channel).
//
// Copy-on-write: never mutate the caller's slice elements in place.
func appendPhaseAffordanceAtTail(
	messages []providers.Message,
	affordance string,
) []providers.Message {
	if affordance == "" || len(messages) == 0 {
		return messages
	}
	out := make([]providers.Message, len(messages), len(messages)+1)
	copy(out, messages)
	out = append(out, providers.Message{Role: "user", Content: affordance})
	return out
}

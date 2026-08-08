package agent

import (
	"fmt"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// Phase 12.61 — Tool call ID uniqueness (root cause: MiniMax sinh id `fix_N` /
// `fix_fb_N` reset counter MỖI REQUEST → nhiều cặp [assistant tool_call + tool
// result] CÙNG id trong 1 payload → provider 400 2013).
//
// Tầng 1 (file này): rewrite id tại choke point nhận response (3 sites populate
// exec.normalizedToolCalls), CHỈ KHI CẦN — id trùng với id đã tồn tại trong
// exec.messages, trùng trong batch hiện tại, hoặc id rỗng. Id unique giữ
// NGUYÊN format provider (blast radius tối thiểu).
//
// Tầng 2 (context.go sanitize pass 3): rewrite duplicate/empty ids trong
// history CŨ tại tầng build payload — xem sanitizeToolCallIDUniqueness.

// allocUniqueToolCallID — shared allocator (layer 1 + sanitize pass 3).
// prefix là namespace riêng per tầng; seq monotonic tăng là nguồn uniqueness
// THẬT (mỗi vòng sinh candidate mới chưa thử → tối đa |occupied| vòng); occupied
// update ngay sau mỗi allocation → 2 id sinh cùng batch không bao giờ trùng.
// seq < 0 = int64 wrap — provably unreachable (292 năm @ 1e9 alloc/s), chỉ log.
func allocUniqueToolCallID(prefix string, seq *int64, occupied map[string]struct{}) string {
	for {
		*seq++
		if *seq < 0 {
			logger.WarnCF("agent", "tool-call id seq wrapped (int64 overflow — unreachable in practice)", map[string]any{})
		}
		id := fmt.Sprintf("%s%d", prefix, *seq)
		if _, taken := occupied[id]; !taken {
			occupied[id] = struct{}{}
			return id
		}
	}
}

// collectToolCallIDs — union mọi tool-call id (assistant calls + tool results)
// trong messages — seed cho rewriteToolCallIDs (existingIDs từ exec.messages).
func collectToolCallIDs(messages []providers.Message) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				ids[tc.ID] = struct{}{}
			}
		}
		if m.ToolCallID != "" {
			ids[m.ToolCallID] = struct{}{}
		}
	}
	return ids
}

// rewriteToolCallIDs (layer 1) — CONDITIONAL rewrite:
//   - id rỗng → allocate fresh
//   - id đã tồn tại trong existingIDs (exec.messages seeds) HOẶC trùng trong
//     batch hiện tại (existingIDs update ngay sau mỗi allocation) → allocate mới
//   - id unique → giữ nguyên + seed vào existingIDs (không tái sử dụng sau này)
//
// id mới = call_<turnID>_<unixNano>_<seq>: unixNano là namespace cross-restart
// (turnID reset sau restart — per-process counter), seq (ts.toolCallSeq,
// monotonic per turn) là nguồn uniqueness thật. now injectable để test
// deterministic (CQ-v10-F4). Idempotent: response tái xử lý lần 2 → ids lần 1
// nằm trong existingIDs → collision → rewrite LẠI nhưng collision-free;
// messages đã append không bị retroactive mutate (struct value copy).
func (ts *turnState) rewriteToolCallIDs(calls []providers.ToolCall, existingIDs map[string]struct{}, now func() time.Time) []providers.ToolCall {
	if len(calls) == 0 {
		return calls
	}
	if existingIDs == nil {
		existingIDs = make(map[string]struct{})
	}
	prefix := fmt.Sprintf("call_%s_%d_", ts.turnID, now().UnixNano())
	out := make([]providers.ToolCall, 0, len(calls))
	for _, tc := range calls {
		id := tc.ID
		if id == "" {
			id = allocUniqueToolCallID(prefix, &ts.toolCallSeq, existingIDs)
		} else if _, taken := existingIDs[id]; taken {
			id = allocUniqueToolCallID(prefix, &ts.toolCallSeq, existingIDs)
		} else {
			existingIDs[id] = struct{}{}
		}
		tc.ID = id
		out = append(out, tc)
	}
	return out
}

// sanitizeToolCallIDUniqueness (pass 3, Phase 12.61) — rewrite duplicate/empty
// tool-call ids trong history CŨ (persist trước deploy) tại tầng build payload.
// Chạy SAU pass 1 (orphan drop), TRƯỚC pass 2 (block pairing) — pass 2 phụ thuộc
// ids unique trong từng block để không drop nhầm result hợp lệ.
//
// Contract:
//   - Deterministic + pure: KHÔNG dùng ts/time/global state; localSeq reset 0
//     mỗi call → cùng history = cùng payload; multi-session concurrent turns
//     race-free.
//   - Idempotent: chỉ rewrite duplicate/empty occurrence — id unique (kể cả
//     call_san_* từ lần chạy trước, id layer-1 `call_<turnID>_...`) không bị đụng.
//   - Block-scoped FIFO per oldID: assistant calls push newID theo occurrence,
//     tool results pop theo occurrence — pairing đúng cho 1 call/block (MiniMax
//     reset counter PER REQUEST; same-block dup CHƯA TỪNG xảy ra).
//   - Same-block duplicate NON-EMPTY → DROP cả block (pairing không xác định,
//     fail-safe); empty/blank ids LUÔN qua FIFO theo occurrence (Kimi R9-F1).
//   - Id mới = call_san_<localSeq> — namespace RIÊNG (layer-1 ids bắt đầu bằng
//     `call_<turnID>_` với turnID thực; id layer-1 với turnID "san_" bắt đầu
//     `call_san_` nhưng có `_<unixNano>_` ngay sau — KHÔNG match ^call_san_[0-9]+$,
//     occupied-union mới là cơ chế chống trùng thật).
func sanitizeToolCallIDUniqueness(sanitized []providers.Message) []providers.Message {
	if len(sanitized) == 0 {
		return sanitized
	}
	occupied := make(map[string]struct{})
	counts := make(map[string]int)
	for _, m := range sanitized {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				occupied[tc.ID] = struct{}{}
			}
		}
		// counts = số lần id xuất hiện TRONG ASSISTANT CALLS (mỗi id hợp lệ
		// xuất hiện đúng 2 lần: 1 call + 1 result; duplicate THẬT = >1 call).
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				counts[tc.ID]++
			}
		}
		if m.ToolCallID != "" {
			occupied[m.ToolCallID] = struct{}{}
		}
	}
	var seq int64
	rewriteCount := 0
	out := make([]providers.Message, 0, len(sanitized))
	for i := 0; i < len(sanitized); i++ {
		msg := sanitized[i]
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			out = append(out, msg)
			continue
		}
		// Same-block duplicate non-empty → pairing không xác định → DROP cả
		// block (assistant + các tool results liền sau) fail-safe.
		seen := make(map[string]bool, len(msg.ToolCalls))
		sameBlockDup := false
		for _, tc := range msg.ToolCalls {
			if tc.ID == "" {
				continue
			}
			if seen[tc.ID] {
				sameBlockDup = true
				break
			}
			seen[tc.ID] = true
		}
		if sameBlockDup {
			logger.DebugCF("agent", "Dropping assistant message with duplicate non-empty tool_call_id (fail-safe, pairing ambiguous)", map[string]any{})
			j := i + 1
			for ; j < len(sanitized) && sanitized[j].Role == "tool"; j++ {
			}
			i = j - 1
			continue
		}
		queue := make(map[string][]string)
		newCalls := make([]providers.ToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			id := tc.ID
			if id == "" || counts[id] > 1 {
				newID := allocUniqueToolCallID("call_san_", &seq, occupied)
				queue[id] = append(queue[id], newID)
				id = newID
				rewriteCount++
			}
			tc.ID = id
			newCalls = append(newCalls, tc)
		}
		msg.ToolCalls = newCalls
		out = append(out, msg)
		j := i + 1
		for ; j < len(sanitized); j++ {
			next := sanitized[j]
			if next.Role != "tool" {
				break
			}
			nid := next.ToolCallID
			if nid == "" || counts[nid] > 1 {
				if q := queue[nid]; len(q) > 0 {
					next.ToolCallID = q[0]
					queue[nid] = q[1:]
					rewriteCount++
				}
				// queue rỗng = result dư → giữ nguyên; pass 2 drop (unexpected)
			}
			out = append(out, next)
		}
		i = j - 1
	}
	if rewriteCount > 0 {
		// ungated metric: absence = 0 rewrites (sunset criterion đọc counter này)
		logger.InfoCF("agent", "sanitize_pass3_rewrites", map[string]any{"count": rewriteCount})
	}
	return out
}

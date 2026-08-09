package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/anthropic_messages"
)

// ---- T4: sanitize pass 3 — block-scoped FIFO + deterministic + idempotent ----

func toolCallMsg(id string) providers.Message {
	return providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{makeToolCall(id)}}
}

func toolResultMsg(id string) providers.Message {
	return providers.Message{Role: "tool", ToolCallID: id}
}

// callsAndResults builds [assistant(id...), tool(id)...] as one contiguous block.
func callsAndResults(ids ...string) []providers.Message {
	msg := providers.Message{Role: "assistant"}
	for _, id := range ids {
		msg.ToolCalls = append(msg.ToolCalls, makeToolCall(id))
	}
	out := []providers.Message{msg}
	for _, id := range ids {
		out = append(out, toolResultMsg(id))
	}
	return out
}

func idsOf(msgs []providers.Message) []string {
	var ids []string
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			ids = append(ids, tc.ID)
		}
		if m.ToolCallID != "" {
			ids = append(ids, m.ToolCallID)
		}
	}
	return ids
}

// callIDsOf — chỉ ids của assistant calls (mỗi id hợp lệ xuất hiện 2 lần:
// 1 call + 1 result — dup check PHẢI áp trên calls only).
func callIDsOf(msgs []providers.Message) []string {
	var ids []string
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			ids = append(ids, tc.ID)
		}
	}
	return ids
}

func assertNoDupCallIDs(t *testing.T, msgs []providers.Message) {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range callIDsOf(msgs) {
		if id == "" {
			t.Fatalf("empty call id must never survive")
		}
		if seen[id] {
			t.Fatalf("duplicate call id %s", id)
		}
		seen[id] = true
	}
}

func TestSanitizeToolCallIDUniqueness(t *testing.T) {
	t.Run("a_cross_block_dup_paired", func(t *testing.T) {
		h := append(callsAndResults("fix_0"), callsAndResults("fix_0")...)
		out := sanitizeToolCallIDUniqueness(h)
		assertNoDupCallIDs(t, out)
		// pairing: assistant[i].ToolCalls[0].ID == tool[i].ToolCallID
		if out[0].ToolCalls[0].ID != out[1].ToolCallID {
			t.Fatalf("pair 1 broken: %s != %s", out[0].ToolCalls[0].ID, out[1].ToolCallID)
		}
		if out[2].ToolCalls[0].ID != out[3].ToolCallID {
			t.Fatalf("pair 2 broken: %s != %s", out[2].ToolCalls[0].ID, out[3].ToolCallID)
		}
	})

	t.Run("b_same_block_dup_nonempty_drop_block", func(t *testing.T) {
		h := []providers.Message{
			{Role: "assistant", ToolCalls: []providers.ToolCall{makeToolCall("fix_0"), makeToolCall("fix_0")}},
			toolResultMsg("fix_0"),
			toolResultMsg("fix_0"),
		}
		out := sanitizeToolCallIDUniqueness(h)
		if len(out) != 0 {
			t.Fatalf("same-block duplicate non-empty must DROP whole block, got %d messages (%v)", len(out), idsOf(out))
		}
	})

	t.Run("c_empty_ids_kept_with_fresh_ids", func(t *testing.T) {
		h := []providers.Message{
			{Role: "assistant", ToolCalls: []providers.ToolCall{makeToolCall(""), makeToolCall("")}},
			toolResultMsg(""),
			toolResultMsg(""),
		}
		out := sanitizeToolCallIDUniqueness(h)
		if len(out) != 3 {
			t.Fatalf("empty pairs must be KEPT with fresh ids, got %d messages", len(out))
		}
		assertNoDupCallIDs(t, out)
		if out[0].ToolCalls[0].ID != out[1].ToolCallID || out[0].ToolCalls[1].ID != out[2].ToolCallID {
			t.Fatalf("empty pairing must follow occurrence: %v", idsOf(out))
		}
	})

	t.Run("d_unique_ids_untouched", func(t *testing.T) {
		h := callsAndResults("call_019f_abc", "call_ba2e_xyz")
		out := sanitizeToolCallIDUniqueness(h)
		got := idsOf(out)
		want := []string{"call_019f_abc", "call_ba2e_xyz", "call_019f_abc", "call_ba2e_xyz"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unique ids must be untouched, got %v want %v", got, want)
		}
	})

	t.Run("e_three_occurrences_cross_block", func(t *testing.T) {
		h := append(append(callsAndResults("fix_0"), callsAndResults("fix_0")...), callsAndResults("fix_0")...)
		out := sanitizeToolCallIDUniqueness(h)
		assertNoDupCallIDs(t, out)
		// pairing intact cho cả 3 blocks
		for i := 0; i+1 < len(out); i += 2 {
			if out[i].ToolCalls[0].ID != out[i+1].ToolCallID {
				t.Fatalf("pair %d broken", i/2)
			}
		}
	})

	t.Run("f_mixed_unique_duplicate", func(t *testing.T) {
		h := append(callsAndResults("call_019f", "fix_0"), callsAndResults("fix_0")...)
		out := sanitizeToolCallIDUniqueness(h)
		ids := idsOf(out)
		if ids[0] != "call_019f" || ids[2] != "call_019f" {
			t.Fatalf("unique layer-1 id must stay (call + result), got %v", ids)
		}
		assertNoDupCallIDs(t, out)
	})

	t.Run("g_deterministic", func(t *testing.T) {
		h := append(callsAndResults("fix_0"), callsAndResults("fix_0")...)
		o1 := sanitizeToolCallIDUniqueness(h)
		o2 := sanitizeToolCallIDUniqueness(h)
		if !reflect.DeepEqual(o1, o2) {
			t.Fatalf("same history must produce same ids (deterministic), got %v vs %v", idsOf(o1), idsOf(o2))
		}
	})

	t.Run("h_leading_orphan_result_not_rewritten", func(t *testing.T) {
		h := []providers.Message{toolResultMsg("fix_0"), toolCallMsg("fix_0"), toolResultMsg("fix_0")}
		out := sanitizeToolCallIDUniqueness(h)
		if len(out) != 3 {
			t.Fatalf("orphan leading result passes through (pass 1 handles it later), got %d", len(out))
		}
		if out[0].ToolCallID != "fix_0" {
			t.Fatalf("orphan must NOT be rewritten, got %s", out[0].ToolCallID)
		}
	})

	t.Run("i_interleaved_invalid_no_mispair", func(t *testing.T) {
		h := []providers.Message{
			toolCallMsg("fix_0"),
			toolCallMsg("fix_0"),
			toolResultMsg("fix_0"),
			toolResultMsg("fix_0"),
		}
		out := sanitizeHistoryForProvider(h)
		// pass 3 không mispair; pass 2 drop block 1 (incomplete) + result thừa
		assistantCalls := map[string]bool{}
		for _, m := range out {
			for _, tc := range m.ToolCalls {
				assistantCalls[tc.ID] = true
			}
		}
		for _, m := range out {
			if m.Role == "tool" && !assistantCalls[m.ToolCallID] {
				t.Fatalf("mispair: tool result %s has no matching assistant call", m.ToolCallID)
			}
		}
	})

	t.Run("j_idempotent", func(t *testing.T) {
		h := append(callsAndResults("fix_0"), callsAndResults("fix_0")...)
		once := sanitizeToolCallIDUniqueness(h)
		twice := sanitizeToolCallIDUniqueness(once)
		if !reflect.DeepEqual(once, twice) {
			t.Fatalf("sanitize(sanitize(h)) must equal sanitize(h), got %v vs %v", idsOf(once), idsOf(twice))
		}
	})

	t.Run("k_collision_with_provider_call_san", func(t *testing.T) {
		h := []providers.Message{
			{Role: "assistant", ToolCalls: []providers.ToolCall{makeToolCall("call_san_1"), makeToolCall("fix_0")}},
			toolResultMsg("call_san_1"),
			toolResultMsg("fix_0"),
			toolCallMsg("fix_0"),
			toolResultMsg("fix_0"),
		}
		out := sanitizeToolCallIDUniqueness(h)
		assertNoDupCallIDs(t, out)
		// call_san_1 unique → giữ; fix_0 (count 3) → rewrite sang call_san_2+ (skip call_san_1)
		if out[0].ToolCalls[0].ID != "call_san_1" {
			t.Fatalf("unique provider call_san_1 must stay, got %s", out[0].ToolCalls[0].ID)
		}
	})

	t.Run("l_mixed_layer1_and_dup", func(t *testing.T) {
		h := []providers.Message{
			{Role: "assistant", ToolCalls: []providers.ToolCall{makeToolCall("call_main-turn-1_1000_1"), makeToolCall("fix_0")}},
			toolResultMsg("call_main-turn-1_1000_1"),
			toolResultMsg("fix_0"),
			toolCallMsg("fix_0"),
			toolResultMsg("fix_0"),
		}
		out := sanitizeToolCallIDUniqueness(h)
		ids := idsOf(out)
		if ids[0] != "call_main-turn-1_1000_1" || ids[2] != "call_main-turn-1_1000_1" {
			t.Fatalf("layer-1 unique ids must stay untouched (call + result), got %v", ids)
		}
		assertNoDupCallIDs(t, out)
	})

	t.Run("m_concurrency_deterministic", func(t *testing.T) {
		h := append(append(callsAndResults("fix_0"), callsAndResults("fix_0")...), callsAndResults("fix_0")...)
		want := sanitizeToolCallIDUniqueness(h)
		var wg sync.WaitGroup
		errs := make(chan string, 40)
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					got := sanitizeToolCallIDUniqueness(h)
					if !reflect.DeepEqual(got, want) {
						errs <- "concurrent sanitize diverged"
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
	})

	t.Run("n_full_sanitize_pipeline_unique", func(t *testing.T) {
		// T5: wire pass 3 vào sanitizeHistoryForProvider (sau pass 1, trước pass 2) —
		// history cũ (persist trước deploy) với dup cross-block → payload unique
		// + pairing intact. (Pass 1 drop assistant-tool-call ở history start,
		// nên history phải mở đầu bằng user message như production.)
		h := []providers.Message{{Role: "user", Content: "hello"}}
		h = append(h, callsAndResults("fix_0")...)
		h = append(h, callsAndResults("fix_0")...)
		out := sanitizeHistoryForProvider(h)
		assertNoDupCallIDs(t, out)
		assistantCalls := map[string]bool{}
		for _, m := range out {
			for _, tc := range m.ToolCalls {
				assistantCalls[tc.ID] = true
			}
		}
		for _, m := range out {
			if m.Role == "tool" && !assistantCalls[m.ToolCallID] {
				t.Fatalf("orphaned tool result %s after full sanitize", m.ToolCallID)
			}
		}
		// pairing theo thứ tự: call 1 = result 1, call 2 = result 2
		if out[1].ToolCalls[0].ID != out[2].ToolCallID || out[3].ToolCalls[0].ID != out[4].ToolCallID {
			t.Fatalf("pairing broken after full sanitize: %v", idsOf(out))
		}
	})
}

// TestSanitizeThenAnthropicWire_SeahorseReplayShape — Phase 12.62 regression.
// main-turn-2 (2026-08-09): seahorse replay reconstructs ToolCalls with
// Function.Name only (tc.Name empty) → anthropic_messages buildRequestBody
// dropped every tool_use block → orphaned tool_results → MiniMax 400 2013
// "tool result's tool id(call_san_1) not found".
// This test runs the FULL chain on the real shapes: seahorse replay →
// sanitizeHistoryForProvider (pass 3 id rewrite) → anthropic wire conversion.
func TestSanitizeThenAnthropicWire_SeahorseReplayShape(t *testing.T) {
	// seahorse replay shape: tc.Name empty, name lives in Function.Name only.
	funcOnly := func(id, name, args string) providers.ToolCall {
		return providers.ToolCall{
			ID:   id,
			Type: "function",
			Function: &providers.FunctionCall{
				Name:      name,
				Arguments: args,
			},
		}
	}
	// Duplicate ids exactly like the MiniMax payload (fix_0 ×2, fix_fb_0 ×2).
	msgs := []providers.Message{
		{Role: "user", Content: "Run the task"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{funcOnly("fix_0", "set_goal", `{"objective":"x"}`)}},
		{Role: "tool", ToolCallID: "fix_0", Content: "goal created"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{funcOnly("fix_0", "read_file", `{"path":"/tmp"}`)}},
		{Role: "tool", ToolCallID: "fix_0", Content: "file content"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{funcOnly("fix_fb_0", "exec", `{"cmd":"ls"}`)}},
		{Role: "tool", ToolCallID: "fix_fb_0", Content: "ok"},
		{Role: "user", Content: "continue"},
	}

	// Stage 1: sanitize (pass 1 + pass 3 rewrite + pass 2 pairing).
	sanitized := sanitizeHistoryForProvider(msgs)
	// Pairing survives with rewritten unique ids.
	calls := map[string]string{} // id -> name
	for _, m := range sanitized {
		for _, tc := range m.ToolCalls {
			if tc.Name == "" && tc.Function != nil {
				calls[tc.ID] = tc.Function.Name
			} else {
				calls[tc.ID] = tc.Name
			}
		}
	}
	for _, m := range sanitized {
		if m.Role == "tool" {
			if _, ok := calls[m.ToolCallID]; !ok {
				t.Fatalf("sanitize left orphaned tool result %s", m.ToolCallID)
			}
		}
	}

	// Stage 2: anthropic wire conversion (the exact provider MiniMax uses).
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","model":"MiniMax-M3","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	p := anthropicmessages.NewProviderWithTimeout("test-key", srv.URL, "test", 10)
	if _, err := p.Chat(context.Background(), sanitized, nil, "MiniMax-M3", map[string]any{"max_tokens": 1024}); err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("no request body captured")
	}

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	// Every tool_use block must be present with id + name + input.
	wireToolUse := map[string]map[string]any{} // id -> block
	for _, msg := range body.Messages {
		blocks, _ := msg.Content.([]any) // plain text messages have string content — skip
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block == nil || block["type"] != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			if id == "" {
				t.Fatalf("tool_use block without id: %+v", block)
			}
			if name, _ := block["name"].(string); name == "" {
				t.Fatalf("tool_use block %s without name: %+v", id, block)
			}
			wireToolUse[id] = block
		}
	}
	if len(wireToolUse) != 3 {
		t.Fatalf("expected 3 tool_use blocks on the wire, got %d (%+v)", len(wireToolUse), wireToolUse)
	}

	// Every tool_result must reference a present tool_use id.
	resultCount := 0
	for _, msg := range body.Messages {
		blocks, _ := msg.Content.([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block == nil || block["type"] != "tool_result" {
				continue
			}
			resultCount++
			id, _ := block["tool_use_id"].(string)
			if _, ok := wireToolUse[id]; !ok {
				t.Fatalf("orphaned tool_result on the wire: tool_use_id=%q not found among tool_use blocks %v", id, keysOf(wireToolUse))
			}
		}
	}
	if resultCount != 3 {
		t.Fatalf("expected 3 tool_result blocks on the wire, got %d", resultCount)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

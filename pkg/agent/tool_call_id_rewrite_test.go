package agent

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func makeToolCall(id string) providers.ToolCall {
	return providers.ToolCall{ID: id, Name: "tool_x", Type: "function"}
}

// ---- T1: allocator chung (SIMP-F2) ----

func TestAllocUniqueToolCallID(t *testing.T) {
	occupied := map[string]struct{}{"call_san_1": {}}
	var seq int64
	id1 := allocUniqueToolCallID("call_san_", &seq, occupied)
	if id1 != "call_san_2" {
		t.Fatalf("expected call_san_2 (skip occupied 1), got %s", id1)
	}
	if _, ok := occupied["call_san_2"]; !ok {
		t.Fatalf("allocator must seed occupied after allocation")
	}
	id2 := allocUniqueToolCallID("call_san_", &seq, occupied)
	if id2 != "call_san_3" {
		t.Fatalf("expected call_san_3 (monotonic), got %s", id2)
	}

	// layer-1 style prefix: monotonic per seq pointer
	var seq2 int64
	a := allocUniqueToolCallID("call_turn_123_", &seq2, map[string]struct{}{})
	b := allocUniqueToolCallID("call_turn_123_", &seq2, map[string]struct{}{})
	if a == b || !strings.HasPrefix(a, "call_turn_123_") || !strings.HasPrefix(b, "call_turn_123_") {
		t.Fatalf("monotonic prefix ids expected, got %s %s", a, b)
	}
}

// ---- T1: rewriteToolCallIDs conditional (a)-(g) ----

func TestRewriteToolCallIDs(t *testing.T) {
	now := func() time.Time { return time.Unix(0, 1000) }

	t.Run("a_two_dup_ids_rewritten_unique", func(t *testing.T) {
		ts := &turnState{turnID: "main-turn-1"}
		// existingIDs seeds = exec.messages (chứa fix_0 từ iteration trước)
		existing := map[string]struct{}{"fix_0": {}}
		out := ts.rewriteToolCallIDs(
			[]providers.ToolCall{makeToolCall("fix_0"), makeToolCall("fix_0")},
			existing, now)
		if out[0].ID == out[1].ID {
			t.Fatalf("ids must differ after rewrite, got %s == %s", out[0].ID, out[1].ID)
		}
		for _, tc := range out {
			if !strings.HasPrefix(tc.ID, "call_main-turn-1_1000_") {
				t.Fatalf("prefix call_<turnID>_<unixNano>_ expected, got %s", tc.ID)
			}
		}
		if ts.toolCallSeq != 2 {
			t.Fatalf("toolCallSeq must advance per allocation, got %d", ts.toolCallSeq)
		}
	})

	t.Run("b_empty_id_gets_fresh_id", func(t *testing.T) {
		ts := &turnState{turnID: "main-turn-1"}
		out := ts.rewriteToolCallIDs(
			[]providers.ToolCall{makeToolCall("")},
			map[string]struct{}{}, now)
		if out[0].ID == "" || !strings.HasPrefix(out[0].ID, "call_main-turn-1_1000_") {
			t.Fatalf("empty id must be allocated fresh, got %q", out[0].ID)
		}
	})

	t.Run("c_same_turnid_diff_clock_diff_ids", func(t *testing.T) {
		tsA := &turnState{turnID: "main-turn-1"}
		tsB := &turnState{turnID: "main-turn-1"}
		nowA := func() time.Time { return time.Unix(0, 1000) }
		nowB := func() time.Time { return time.Unix(0, 2000) }
		// restart-sim: turn mới, history cũ vẫn chứa fix_0 (seed existingIDs)
		existingA := map[string]struct{}{"fix_0": {}}
		existingB := map[string]struct{}{"fix_0": {}}
		outA := tsA.rewriteToolCallIDs([]providers.ToolCall{makeToolCall("fix_0")}, existingA, nowA)
		outB := tsB.rewriteToolCallIDs([]providers.ToolCall{makeToolCall("fix_0")}, existingB, nowB)
		if outA[0].ID == outB[0].ID {
			t.Fatalf("unixNano namespace must differ across restart-sim, got %s", outA[0].ID)
		}
	})

	t.Run("d_unique_id_kept_when_not_occupied", func(t *testing.T) {
		ts := &turnState{turnID: "main-turn-1"}
		in := providers.ToolCall{ID: "call_019f_abc", Name: "x", Type: "function"}
		out := ts.rewriteToolCallIDs([]providers.ToolCall{in}, map[string]struct{}{}, now)
		if out[0].ID != "call_019f_abc" {
			t.Fatalf("unique provider id must be kept untouched (conditional), got %s", out[0].ID)
		}
		if ts.toolCallSeq != 0 {
			t.Fatalf("no allocation must not advance seq, got %d", ts.toolCallSeq)
		}
	})

	t.Run("e_collision_skips_occupied", func(t *testing.T) {
		ts := &turnState{turnID: "main-turn-1"}
		// fix_0 occupied (history cũ) + candidate call_main-turn-1_1000_1 cũng occupied
		existing := map[string]struct{}{"fix_0": {}, "call_main-turn-1_1000_1": {}}
		out := ts.rewriteToolCallIDs(
			[]providers.ToolCall{makeToolCall("fix_0")},
			existing, now)
		if out[0].ID != "call_main-turn-1_1000_2" {
			t.Fatalf("collision must advance seq until free, got %s", out[0].ID)
		}
	})

	t.Run("f_empty_existing_ok", func(t *testing.T) {
		ts := &turnState{turnID: "main-turn-1"}
		out := ts.rewriteToolCallIDs(
			[]providers.ToolCall{makeToolCall("fix_0"), makeToolCall("call_ba2e")},
			map[string]struct{}{}, now)
		if out[0].ID == out[1].ID {
			t.Fatalf("ids must differ, got %s", out[0].ID)
		}
	})

	t.Run("g_san_turnid_never_matches_san_digit", func(t *testing.T) {
		ts := &turnState{turnID: "san_"}
		out := ts.rewriteToolCallIDs(
			[]providers.ToolCall{makeToolCall("fix_0")},
			map[string]struct{}{}, now)
		// layer-1 id = "call_san__1000_1" — must NOT match ^call_san_[0-9]+$ (pass-3 namespace)
		if matched, _ := regexp.MatchString(`^call_san_[0-9]+$`, out[0].ID); matched {
			t.Fatalf("layer-1 id %q must not collide with pass-3 namespace", out[0].ID)
		}
	})

	t.Run("h_occupied_unique_incoming_rewritten", func(t *testing.T) {
		ts := &turnState{turnID: "main-turn-1"}
		existing := map[string]struct{}{"call_019f_abc": {}}
		out := ts.rewriteToolCallIDs(
			[]providers.ToolCall{makeToolCall("call_019f_abc")},
			existing, now)
		if out[0].ID == "call_019f_abc" {
			t.Fatalf("id already in exec.messages must be rewritten, got %s", out[0].ID)
		}
	})
}

func TestCollectToolCallIDs(t *testing.T) {
	messages := []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{makeToolCall("fix_0")}},
		{Role: "tool", ToolCallID: "fix_0"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{makeToolCall("call_ba2e")}},
	}
	ids := collectToolCallIDs(messages)
	if _, ok := ids["fix_0"]; !ok {
		t.Fatalf("assistant call id must be collected")
	}
	if _, ok := ids["call_ba2e"]; !ok {
		t.Fatalf("second assistant call id must be collected")
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 unique ids, got %d (%v)", len(ids), ids)
	}
}

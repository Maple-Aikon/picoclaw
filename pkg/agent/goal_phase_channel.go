package agent

// Lever (a) - 2026-11-10 (anh Maple duyệt).
//
// ---------------------------------------------------------------------------
// VÌ SAO
// ---------------------------------------------------------------------------
// MiniMax cache theo PREFIX, theo ranh giới message (đo thật: plan 2.2).
// Mọi byte đổi ở GIỮA chuỗi làm mất cache của toàn bộ phần phía sau.
//
// System prompt hiện tại CHỨA nội dung phụ thuộc phase:
//   1. goalPhaseSetHintContributor        (SET)
//   2. goalPhaseOpenHintContributor       (OPEN)
//   3. goalPhaseCheckpointHintContributor (CHECKPOINT + GoalSnapshot)
//   4. goalPhaseFinalHintContributor      (FINAL)
//   5. goalCompleteReportHintContributor  (POST-FINAL)
//   6. toolDiscoveryPromptContributor    (DiscoveryRuleText theo phase)
//   7. mcpServerPromptContributor        (MCPAvailText theo phase)
//
// => transition OPEN->CHECKPOINT đổi system => mất TOÀN BỘ prefix cache.
//
// ĐO ĐƯỢC (production gateway.log, 2026-11-10):
//   OPEN steady        : cache_read 104.192
//   OPEN->CHECKPOINT   : cache_read  1.536 + input 80.808  => tax ~81K token
//   CHECKPOINT->OPEN   : cache_read 40.448 + input 46.458
//
// ---------------------------------------------------------------------------
// CÁCH LÀM
// ---------------------------------------------------------------------------
// Tách identity (constant) khỏi lifecycle (phase-dependent):
//
//   system message : identity + skills + memory + Goal Lifecycle Protocol
//                   (byte-constant qua mọi phase trong 1 turn)
//   phase channel : 1 USER message ở ĐUÔI messages[], chứa toàn bộ
//                   phase affordance (hint + goal snapshot + compass +
//                   tool-discovery/MCP note)
//
// VÌ SAO Ở ĐUÔI (không rewrite tại chỗ):
//   - rewrite tại chỗ = đổi byte ở GIỮA chuỗi => mất cache phía sau.
//     Đo: probe27 K-NEW-INPLACE 94.347 vs H-NEW-TAIL 84.059 uncached.
//   - append ở đuôi = mở rộng prefix => cache phía trước GIỮ NGUYÊN.
//
// VÌ SAO là USER message (không phải tool result):
//   Ở CHECKPOINT chỉ 2 tool được phép (goal_progress, complete_goal).
//   Một tool_result message không có tool_use id để trả lời => invalid
//   theo Anthropic schema. User message là dạng hợp lệ duy nhất.
//
// ---------------------------------------------------------------------------
// HỢP ĐỒNG CHẤT LƯỢNG
// ---------------------------------------------------------------------------
// Xem file lever_a_system_constant_test.go (C1..C5).

import (
	"fmt"
	"strings"
)

// phaseHintContributors lists the phase-specific hint contributors. This
// is the single place that knows which hints exist.
//
// LEVER (a) - 2026-11-10: these used to be added to the SYSTEM prompt in
// buildSystemPromptParts. They now feed the turn-tail phase channel
// instead, so the system prefix stays byte-constant across transitions.
var phaseHintContributors = []func(PromptBuildRequest) *PromptPart{
	goalPhaseSetHintContributor,
	goalPhaseOpenHintContributor,
	goalPhaseCheckpointHintContributor,
	goalPhaseFinalHintContributor,
}

// phaseChannelMarker is the opening tag of the turn-tail phase channel.
// The LLM sees it as an explicit, current-state block (the same role the
// goal_phase banner played) but it now lives OUT of the system prefix.
const phaseChannelMarker = "<goal_phase_channel>"

// buildPhaseChannel renders the phase-specific LLM-facing content for the
// current phase. Returns an empty string when there is nothing
// phase-specific to say.
//
// The content is intentionally the SAME TEXT the system prompt used to
// carry (phase hints + goal snapshot + compass), so the LLM's affordance
// knowledge is unchanged - only its POSITION moves.
func buildPhaseChannel(req PromptBuildRequest) string {
	// A profile with the system prompt turned off must produce a BLANK
	// user message (see TestTurnProfile_SystemPromptOffBlankTurnStillSendsMessage).
	// The phase channel is part of the prompt payload, so it must be
	// suppressed together with the default system prompt.
	if req.SuppressDefaultSystemPrompt {
		return ""
	}
	var sections []string

	// 1. The phase hint itself (SET / OPEN / CHECKPOINT / FINAL).
	for _, buildHint := range phaseHintContributors {
		if part := buildHint(req); part != nil {
			sections = append(sections, strings.TrimRight(part.Content, string(rune(10))))
		}
	}
	if part := goalCompleteReportHintContributor(req); part != nil {
		sections = append(sections, strings.TrimRight(part.Content, string(rune(10))))
	}

	// 2. The tool-discovery rule - phase-dependent (locked at SET /
	//    CHECKPOINT / FINAL / POST-FINAL, generic at OPEN). It moved here
	//    with the rest of the phase affordance so the system prefix stays
	//    byte-constant.
	if rule := phaseToolDiscoveryNote(req); rule != "" {
		sections = append(sections, rule)
	}

	// 3. The MCP availability note - phase-dependent for DEFERRED servers
	//    (available at OPEN, locked elsewhere).
	if note := phaseMCPNote(req); note != "" {
		sections = append(sections, note)
	}

	if len(sections) == 0 {
		return ""
	}
	return phaseChannelMarker + string(rune(10)) + strings.Join(sections, string(rune(10))+string(rune(10))) + string(rune(10)) + "</goal_phase_channel>"
}

// phaseToolDiscoveryNote renders the phase-scoped tool-discovery rule.
func phaseToolDiscoveryNote(req PromptBuildRequest) string {
	if !req.ToolDiscoveryBM25 && !req.ToolDiscoveryRegex {
		return ""
	}
	return formatToolDiscoveryRule(req.ToolDiscoveryBM25, req.ToolDiscoveryRegex, GoalPhase(req.GoalPhase))
}

// phaseMCPNote renders the phase-scoped MCP availability note for deferred
// servers. Non-deferred servers are always natively available, so they
// contribute nothing phase-specific.
func phaseMCPNote(req PromptBuildRequest) string {
	if len(req.DeferredMCPServers) == 0 {
		return ""
	}
	policy := PhasePolicyFor(GoalPhase(req.GoalPhase))
	availability := "hidden behind tool discovery until unlocked"
	if policy != nil && policy.MCPAvailText != "" {
		availability = policy.MCPAvailText
	}
	var b strings.Builder
	for _, srv := range req.DeferredMCPServers {
		fmt.Fprintf(&b, "MCP server `%s` is connected and contributes tools, currently %s.", srv, availability)
		b.WriteString(string(rune(10)))
	}
	return strings.TrimRight(b.String(), string(rune(10)))
}

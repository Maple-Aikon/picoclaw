package routing

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ── ExtractFeatures ──────────────────────────────────────────────────────────

func TestExtractFeatures_EmptyMessage(t *testing.T) {
	f := ExtractFeatures("", nil)
	if f.TokenEstimate != 0 {
		t.Errorf("TokenEstimate: got %d, want 0", f.TokenEstimate)
	}
	if f.CodeBlockCount != 0 {
		t.Errorf("CodeBlockCount: got %d, want 0", f.CodeBlockCount)
	}
	if f.RecentToolCalls != 0 {
		t.Errorf("RecentToolCalls: got %d, want 0", f.RecentToolCalls)
	}
	if f.ConversationDepth != 0 {
		t.Errorf("ConversationDepth: got %d, want 0", f.ConversationDepth)
	}
	if f.HasAttachments {
		t.Error("HasAttachments: got true, want false")
	}
}

func TestExtractFeatures_TokenEstimate(t *testing.T) {
	// 30 ASCII runes: 0 CJK + 30/4 = 7 tokens
	msg := strings.Repeat("a", 30)
	f := ExtractFeatures(msg, nil)
	if f.TokenEstimate != 7 {
		t.Errorf("TokenEstimate: got %d, want 7", f.TokenEstimate)
	}
}

func TestExtractFeatures_TokenEstimate_CJK(t *testing.T) {
	// 9 CJK runes → 9 tokens (each CJK rune ≈ 1 token).
	// Using a rune slice literal avoids CJK string literals in source.
	msg := string([]rune{
		0x4F60, 0x597D, 0x4E16, 0x754C,
		0x4F60, 0x597D, 0x4E16, 0x754C,
		0x4F60,
	})
	f := ExtractFeatures(msg, nil)
	if f.TokenEstimate != 9 {
		t.Errorf("CJK TokenEstimate: got %d, want 9", f.TokenEstimate)
	}
}

func TestExtractFeatures_TokenEstimate_Mixed(t *testing.T) {
	// Mixed: 4 CJK runes + 8 ASCII runes → 4 + 8/4 = 6 tokens.
	msg := string([]rune{0x4F60, 0x597D, 0x4E16, 0x754C}) + "hello ok"
	f := ExtractFeatures(msg, nil)
	if f.TokenEstimate != 6 {
		t.Errorf("Mixed TokenEstimate: got %d, want 6", f.TokenEstimate)
	}
}

func TestExtractFeatures_CodeBlocks(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{"no code here", 0},
		{"```go\nfmt.Println()\n```", 1},
		{"```python\npass\n```\n```js\nconsole.log()\n```", 2},
		{"```unclosed", 0}, // odd number of fences = 0 complete blocks
	}
	for _, tc := range cases {
		f := ExtractFeatures(tc.msg, nil)
		if f.CodeBlockCount != tc.want {
			t.Errorf("msg=%q: CodeBlockCount got %d, want %d", tc.msg, f.CodeBlockCount, tc.want)
		}
	}
}

func TestExtractFeatures_RecentToolCalls(t *testing.T) {
	// History longer than lookbackWindow — only last lookbackWindow entries count.
	history := make([]providers.Message, 10)
	// Put 2 tool calls at positions 8 and 9 (within the last 6)
	history[8] = providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{Name: "exec"}}}
	history[9] = providers.Message{
		Role:      "assistant",
		ToolCalls: []providers.ToolCall{{Name: "read_file"}, {Name: "write_file"}},
	}
	// Position 3 is outside the lookback window and must NOT be counted
	history[3] = providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{Name: "old_tool"}}}

	f := ExtractFeatures("test", history)
	// 1 (position 8) + 2 (position 9) = 3
	if f.RecentToolCalls != 3 {
		t.Errorf("RecentToolCalls: got %d, want 3", f.RecentToolCalls)
	}
}

func TestExtractFeatures_ConversationDepth(t *testing.T) {
	history := make([]providers.Message, 7)
	f := ExtractFeatures("msg", history)
	if f.ConversationDepth != 7 {
		t.Errorf("ConversationDepth: got %d, want 7", f.ConversationDepth)
	}
}

func TestExtractFeatures_HasAttachments_DataURI(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"plain text", false},
		{"here is an image: data:image/png;base64,abc123", true},
		{"audio: data:audio/mp3;base64,xyz", true},
		{"video: data:video/mp4;base64,xyz", true},
	}
	for _, tc := range cases {
		f := ExtractFeatures(tc.msg, nil)
		if f.HasAttachments != tc.want {
			t.Errorf("msg=%q: HasAttachments got %v, want %v", tc.msg, f.HasAttachments, tc.want)
		}
	}
}

func TestExtractFeatures_HasAttachments_Extension(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"check out photo.jpg", true},
		{"see screenshot.png", true},
		{"listen to audio.mp3", true},
		{"watch clip.mp4", true},
		{"just a .go file", false},
		{"document.pdf", false}, // pdf is not in the media list
	}
	for _, tc := range cases {
		f := ExtractFeatures(tc.msg, nil)
		if f.HasAttachments != tc.want {
			t.Errorf("msg=%q: HasAttachments got %v, want %v", tc.msg, f.HasAttachments, tc.want)
		}
	}
}

// ── RuleClassifier ───────────────────────────────────────────────────────────

func TestRuleClassifier_ZeroFeatures(t *testing.T) {
	c := &RuleClassifier{}
	score := c.Score(Features{})
	if score != 0.0 {
		t.Errorf("zero features: got %f, want 0.0", score)
	}
}

func TestRuleClassifier_AttachmentsHardGate(t *testing.T) {
	c := &RuleClassifier{}
	score := c.Score(Features{HasAttachments: true})
	if score != 1.0 {
		t.Errorf("attachments: got %f, want 1.0", score)
	}
}

func TestRuleClassifier_CodeBlockAlone(t *testing.T) {
	c := &RuleClassifier{}
	// Code block alone = 0.40, above default threshold 0.35
	score := c.Score(Features{CodeBlockCount: 1})
	if score < 0.35 {
		t.Errorf("code block: score %f is below default threshold 0.35", score)
	}
}

func TestRuleClassifier_LongMessage(t *testing.T) {
	c := &RuleClassifier{}
	// >200 tokens = 0.35, exactly at default threshold → heavy
	score := c.Score(Features{TokenEstimate: 250})
	if score < 0.35 {
		t.Errorf("long message: score %f is below default threshold 0.35", score)
	}
}

func TestRuleClassifier_MediumMessage(t *testing.T) {
	c := &RuleClassifier{}
	// 50-200 tokens = 0.15, below threshold → light
	score := c.Score(Features{TokenEstimate: 100})
	if score >= 0.35 {
		t.Errorf("medium message: score %f should be below default threshold 0.35", score)
	}
}

func TestRuleClassifier_ShortMessage(t *testing.T) {
	c := &RuleClassifier{}
	// <50 tokens, no other signals = 0.0 → light
	score := c.Score(Features{TokenEstimate: 10})
	if score != 0.0 {
		t.Errorf("short message: got %f, want 0.0", score)
	}
}

func TestRuleClassifier_ToolCallDensity(t *testing.T) {
	c := &RuleClassifier{}

	scoreNone := c.Score(Features{RecentToolCalls: 0})
	scoreLow := c.Score(Features{RecentToolCalls: 2})
	scoreHigh := c.Score(Features{RecentToolCalls: 5})

	if scoreNone != 0.0 {
		t.Errorf("no tools: got %f, want 0.0", scoreNone)
	}
	if scoreLow <= scoreNone {
		t.Errorf("low tools should score higher than none: %f vs %f", scoreLow, scoreNone)
	}
	if scoreHigh <= scoreLow {
		t.Errorf("high tools should score higher than low: %f vs %f", scoreHigh, scoreLow)
	}
}

func TestRuleClassifier_DeepConversation(t *testing.T) {
	c := &RuleClassifier{}
	shallow := c.Score(Features{ConversationDepth: 5})
	deep := c.Score(Features{ConversationDepth: 15})
	if deep <= shallow {
		t.Errorf("deep conversation should score higher: %f vs %f", deep, shallow)
	}
}

func TestRuleClassifier_ScoreDoesNotExceedOne(t *testing.T) {
	c := &RuleClassifier{}
	// Max all signals simultaneously
	f := Features{
		TokenEstimate:     500,
		CodeBlockCount:    3,
		RecentToolCalls:   10,
		ConversationDepth: 20,
	}
	score := c.Score(f)
	if score > 1.0 {
		t.Errorf("score %f exceeds 1.0", score)
	}
}

// ── Router ───────────────────────────────────────────────────────────────────

func TestRouter_DefaultThreshold(t *testing.T) {
	r := New(RouterConfig{LightModel: "gemini-flash"})
	if r.cfg.Threshold != defaultThreshold {
		t.Errorf("default light threshold: got %f, want %f", r.cfg.Threshold, defaultThreshold)
	}
	if r.cfg.MediumThreshold != defaultMediumThreshold {
		t.Errorf("default medium threshold: got %f, want %f", r.cfg.MediumThreshold, defaultMediumThreshold)
	}
}

func TestRouter_SelectModel_SimpleMessageUsesLight(t *testing.T) {
	r := New(RouterConfig{LightModel: "gemini-flash"})
	msg := "hi"
	model, tier, _ := r.SelectModel(msg, nil, "claude-sonnet-4-6")
	if tier != TierLight {
		t.Errorf("simple message: expected TierLight, got %v", tier)
	}
	if model != "gemini-flash" {
		t.Errorf("simple message: model got %q, want %q", model, "gemini-flash")
	}
}

func TestRouter_SelectModel_MediumMessageUsesMedium(t *testing.T) {
	// RuleClassifier: CodeBlockCount 1 = 0.40. 
	// Default thresholds: Light 0.25, Medium 0.55.
	// 0.25 <= 0.40 < 0.55 -> TierMedium
	r := New(RouterConfig{LightModel: "light", MediumModel: "medium"})
	msg := "```go\nfmt.Println(\"hello\")\n```"
	model, tier, _ := r.SelectModel(msg, nil, "heavy")
	if tier != TierMedium {
		t.Errorf("code block: expected TierMedium, got %v", tier)
	}
	if model != "medium" {
		t.Errorf("code block: model got %q, want %q", model, "medium")
	}
}

func TestRouter_SelectModel_AttachmentUsesHeavy(t *testing.T) {
	r := New(RouterConfig{LightModel: "light", MediumModel: "medium"})
	msg := "can you analyze this? screenshot.png"
	model, tier, _ := r.SelectModel(msg, nil, "heavy")
	if tier != TierHeavy {
		t.Errorf("attachment: expected TierHeavy, got %v", tier)
	}
	if model != "heavy" {
		t.Errorf("attachment: model got %q, want %q", model, "heavy")
	}
}

func TestRouter_SelectModel_LongMessageUsesMedium(t *testing.T) {
	r := New(RouterConfig{LightModel: "light", MediumModel: "medium"})
	// >200 token estimate: 210 * 3 = 630 chars -> score 0.35
	// 0.25 <= 0.35 < 0.55 -> TierMedium
	msg := strings.Repeat("word ", 210)
	model, tier, _ := r.SelectModel(msg, nil, "heavy")
	if tier != TierMedium {
		t.Errorf("long message: expected TierMedium, got %v", tier)
	}
	if model != "medium" {
		t.Errorf("long message: model got %q, want %q", model, "medium")
	}
}

func TestRouter_SelectModel_DeepToolChainUsesLight(t *testing.T) {
	// Tool calls (3) -> score 0.10 < 0.25 (Threshold) -> TierLight
	r := New(RouterConfig{LightModel: "light", MediumModel: "medium"})
	history := []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{{Name: "read_file"}, {Name: "write_file"}}},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{Name: "exec"}}},
	}
	msg := "ok"
	_, tier, _ := r.SelectModel(msg, history, "heavy")
	if tier != TierLight {
		t.Errorf("short message + moderate tool calls: expected TierLight, got %v", tier)
	}
}

func TestRouter_LightModel(t *testing.T) {
	r := New(RouterConfig{LightModel: "my-fast-model"})
	if r.LightModel() != "my-fast-model" {
		t.Errorf("LightModel: got %q, want %q", r.LightModel(), "my-fast-model")
	}
}

func TestRouter_MediumModel(t *testing.T) {
	r := New(RouterConfig{MediumModel: "my-medium-model"})
	if r.MediumModel() != "my-medium-model" {
		t.Errorf("MediumModel: got %q, want %q", r.MediumModel(), "my-medium-model")
	}
}

func TestRouter_CustomClassifier_LowScore_SelectsLight(t *testing.T) {
	r := newWithClassifier(
		RouterConfig{LightModel: "light", Threshold: 0.5},
		&fixedScoreClassifier{score: 0.2},
	)
	_, tier, _ := r.SelectModel("anything", nil, "heavy")
	if tier != TierLight {
		t.Errorf("low score: expected TierLight, got %v", tier)
	}
}

func TestRouter_CustomClassifier_MediumScore_SelectsMedium(t *testing.T) {
	r := newWithClassifier(
		RouterConfig{LightModel: "light", MediumModel: "medium", Threshold: 0.2, MediumThreshold: 0.6},
		&fixedScoreClassifier{score: 0.4},
	)
	_, tier, _ := r.SelectModel("anything", nil, "heavy")
	if tier != TierMedium {
		t.Errorf("medium score: expected TierMedium, got %v", tier)
	}
}

func TestRouter_CustomClassifier_HighScore_SelectsHeavy(t *testing.T) {
	r := newWithClassifier(
		RouterConfig{LightModel: "light", MediumModel: "medium", MediumThreshold: 0.6},
		&fixedScoreClassifier{score: 0.8},
	)
	_, tier, _ := r.SelectModel("anything", nil, "heavy")
	if tier != TierHeavy {
		t.Errorf("high score: expected TierHeavy, got %v", tier)
	}
}

func TestRouter_SelectModel_ReturnsScore(t *testing.T) {
	r := newWithClassifier(
		RouterConfig{LightModel: "light", Threshold: 0.5},
		&fixedScoreClassifier{score: 0.42},
	)
	_, _, score := r.SelectModel("anything", nil, "heavy")
	if score != 0.42 {
		t.Errorf("score: got %f, want 0.42", score)
	}
}

type fixedScoreClassifier struct {
	score float64
}

func (c *fixedScoreClassifier) Score(f Features) float64 {
	return c.score
}

func TestRouter_Normalization(t *testing.T) {
	cases := []struct {
		name       string
		in         RouterConfig
		wantT      float64
		wantMT     float64
	}{
		{
			"Negative values use defaults",
			RouterConfig{Threshold: -1, MediumThreshold: -0.5},
			defaultThreshold,
			defaultMediumThreshold,
		},
		{
			"Above 1.0 clamped to 1.0",
			RouterConfig{Threshold: 1.5, MediumThreshold: 2.0},
			1.0,
			1.0,
		},
		{
			"Inverted thresholds are swapped",
			RouterConfig{Threshold: 0.8, MediumThreshold: 0.4},
			0.4,
			0.8,
		},
		{
			"Zero values use defaults",
			RouterConfig{Threshold: 0, MediumThreshold: 0},
			defaultThreshold,
			defaultMediumThreshold,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(tc.in)
			if r.cfg.Threshold != tc.wantT {
				t.Errorf("Threshold: got %v, want %v", r.cfg.Threshold, tc.wantT)
			}
			if r.cfg.MediumThreshold != tc.wantMT {
				t.Errorf("MediumThreshold: got %v, want %v", r.cfg.MediumThreshold, tc.wantMT)
			}
		})
	}
}

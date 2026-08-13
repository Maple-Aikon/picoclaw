package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestInferMediaType(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		contentType string
		want        string
	}{
		{
			name:        "png content type",
			filename:    "diagram",
			contentType: "image/png",
			want:        "image",
		},
		{
			name:        "jpeg extension fallback",
			filename:    "photo.JPG",
			contentType: "",
			want:        "image",
		},
		{
			name:        "svg content type is file",
			filename:    "diagram",
			contentType: "image/svg+xml",
			want:        "file",
		},
		{
			name:        "svg content type with parameters is file",
			filename:    "diagram",
			contentType: "image/svg+xml; charset=utf-8",
			want:        "file",
		},
		{
			name:        "svg extension fallback is file",
			filename:    "diagram.SVG",
			contentType: "",
			want:        "file",
		},
		{
			name:        "audio content type",
			filename:    "voice",
			contentType: "audio/ogg",
			want:        "audio",
		},
		{
			name:        "ogg application content type",
			filename:    "voice.ogg",
			contentType: "application/ogg",
			want:        "audio",
		},
		{
			name:        "video extension fallback",
			filename:    "clip.MP4",
			contentType: "",
			want:        "video",
		},
		{
			name:        "unknown type",
			filename:    "archive.bin",
			contentType: "application/octet-stream",
			want:        "file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferMediaType(tt.filename, tt.contentType)
			if got != tt.want {
				t.Fatalf("inferMediaType(%q, %q) = %q, want %q", tt.filename, tt.contentType, got, tt.want)
			}
		})
	}
}

func TestToolFeedbackIterContext(t *testing.T) {
	tests := []struct {
		name string
		ts   *turnState
		want string
	}{
		{name: "nil ts", ts: nil, want: ""},
		{
			name: "with turn id",
			ts: &turnState{
				turnID:           "main-turn-20",
				iteration:        2,
				maxIterationsCap: 250,
				agent:            &AgentInstance{PhaseOverrideForTest: string(GoalPhaseOpen)},
			},
			want: "📊 main-turn-20 (#2/250) Goal-Open",
		},
		{
			name: "without turn id",
			ts: &turnState{
				iteration:        3,
				maxIterationsCap: 250,
				agent:            &AgentInstance{PhaseOverrideForTest: string(GoalPhaseCheckpoint)},
			},
			want: "📊 (#3/250) Goal-Checkpoint",
		},
		{
			name: "post final display name",
			ts: &turnState{
				turnID:           "main-turn-1",
				iteration:        5,
				maxIterationsCap: 250,
				agent:            &AgentInstance{PhaseOverrideForTest: string(GoalPhasePostFinal)},
			},
			want: "📊 main-turn-1 (#5/250) Goal-Post-Final",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolFeedbackIterContext(tt.ts)
			if got != tt.want {
				t.Fatalf("toolFeedbackIterContext() = %q, want %q", got, tt.want)
			}
		})
	}
}
func TestToolFeedbackCardExplanationForRender(t *testing.T) {
	// Build a ToolCall whose ExtraContent.ToolFeedbackExplanation is set,
	// so when the helper does NOT suppress, it should return that text.
	tc := providers.ToolCall{
		ExtraContent: &providers.ExtraContent{
			ToolFeedbackExplanation: "Reading the project layout first.",
		},
	}
	messages := []providers.Message{}

	mkCfg := func(enabled, explanationMessages, separateMessages bool) *config.Config {
		return &config.Config{
			Agents: config.AgentsConfig{
				Defaults: config.AgentDefaults{
					ToolFeedback: config.ToolFeedbackConfig{
						Enabled:             enabled,
						ExplanationMessages: explanationMessages,
						SeparateMessages:    separateMessages,
					},
				},
			},
		}
	}

	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "tool_feedback disabled -> explanation embedded",
			cfg:  mkCfg(false, true, false),
			want: "Reading the project layout first.",
		},
		{
			name: "explanation_messages=false, separate_messages=false -> embedded",
			cfg:  mkCfg(true, false, false),
			want: "Reading the project layout first.",
		},
		{
			name: "explanation_messages=true, separate_messages=true -> embedded (each message carries its own)",
			cfg:  mkCfg(true, true, true),
			want: "Reading the project layout first.",
		},
		{
			name: "explanation_messages=true, separate_messages=false -> suppressed (Phase 12.70)",
			cfg:  mkCfg(true, true, false),
			want: "",
		},
		{
			name: "nil config -> embedded (no suppression when we cannot decide)",
			cfg:  nil,
			want: "Reading the project layout first.",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := toolFeedbackCardExplanationForRender(tt.cfg, nil, tc, messages)
			if got != tt.want {
				t.Fatalf("toolFeedbackCardExplanationForRender() = %q, want %q", got, tt.want)
			}
		})
	}
}

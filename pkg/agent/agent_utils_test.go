package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
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
			got := toolFeedbackCardExplanationForRender(tt.cfg, nil, nil, tc, messages)
			if got != tt.want {
				t.Fatalf("toolFeedbackCardExplanationForRender() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestToolFeedbackCardExplanationForRender_GoalObjectiveFallback verifies that
// when the LLM emits a tool call without explanation (no ExtraContent, empty
// response.Content, no useful user message), the fallback text comes from the
// active goal's objective instead of an empty card. This keeps the user
// informed of what task the agent is working on for every tool call.
//
// Phase 12.71 (proposed).
func TestToolFeedbackCardExplanationForRender_GoalObjectiveFallback(t *testing.T) {
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				ToolFeedback: config.ToolFeedbackConfig{
					Enabled:             true,
					ExplanationMessages: false,
					SeparateMessages:    false,
				},
			},
		},
	}

	t.Run("active goal + empty everything -> goal objective", func(t *testing.T) {
		ws := t.TempDir()
		sessionKey := "sk_v1_fallback_goal"
		seedActiveGoalForFeedback(t, ws, sessionKey, "Deploy v0.5 to production")
		ts := &turnState{
			workspace:   ws,
			sessionKey:  sessionKey,
			agent:       &AgentInstance{Workspace: ws},
		}
		// tc has no ExtraContent, response is nil, messages is empty.
		tc := providers.ToolCall{Name: "exec", Arguments: map[string]any{"cmd": "ls"}}
		got := toolFeedbackCardExplanationForRender(cfg, ts, nil, tc, nil)
		want := "Working on: Deploy v0.5 to production"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("active goal + useful user message -> user message wins (no goal fallback)", func(t *testing.T) {
		ws := t.TempDir()
		sessionKey := "sk_v1_fallback_user_wins"
		seedActiveGoalForFeedback(t, ws, sessionKey, "Upgrade runtime")
		ts := &turnState{
			workspace:   ws,
			sessionKey:  sessionKey,
			agent:       &AgentInstance{Workspace: ws},
		}
		tc := providers.ToolCall{Name: "exec", Arguments: map[string]any{}}
		messages := []providers.Message{
			{Role: "user", Content: "check disk space first"},
		}
		got := toolFeedbackCardExplanationForRender(cfg, ts, nil, tc, messages)
		// existing fallback "Continuing the current task.: <user content>" should
		// win — only when both are empty do we fall back to the goal.
		want := "Continuing the current task.: check disk space first"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("active goal + tc ExtraContent -> tc wins (no goal fallback)", func(t *testing.T) {
		ws := t.TempDir()
		sessionKey := "sk_v1_fallback_tc_wins"
		seedActiveGoalForFeedback(t, ws, sessionKey, "Refactor the registry")
		ts := &turnState{
			workspace:   ws,
			sessionKey:  sessionKey,
			agent:       &AgentInstance{Workspace: ws},
		}
		tc := providers.ToolCall{
			ExtraContent: &providers.ExtraContent{
				ToolFeedbackExplanation: "explaining here",
			},
		}
		got := toolFeedbackCardExplanationForRender(cfg, ts, nil, tc, nil)
		if got != "explaining here" {
			t.Fatalf("got %q, want %q", got, "explaining here")
		}
	})

	t.Run("active goal + long objective -> truncated to 200 runes + ...", func(t *testing.T) {
		ws := t.TempDir()
		sessionKey := "sk_v1_fallback_long"
		longObjective := strings.Repeat("a", 250)
		seedActiveGoalForFeedback(t, ws, sessionKey, longObjective)
		ts := &turnState{
			workspace:   ws,
			sessionKey:  sessionKey,
			agent:       &AgentInstance{Workspace: ws},
		}
		tc := providers.ToolCall{Name: "exec", Arguments: map[string]any{}}
		got := toolFeedbackCardExplanationForRender(cfg, ts, nil, tc, nil)
		// "Working on: " (12 runes) + 197 'a's + "..." = 200 runes after "Working on: "
		// Total length = 12 + 200 = 212 runes
		if utf8.RuneCountInString(got) != 212 {
			t.Fatalf("rune count = %d, want 212", utf8.RuneCountInString(got))
		}
		if !strings.HasSuffix(got, "...") {
			t.Fatalf("got %q, want trailing ...", got)
		}
		if !strings.HasPrefix(got, "Working on: ") {
			t.Fatalf("got %q, want Working on: prefix", got)
		}
	})

	t.Run("completed goal -> empty (terminal, no work pending)", func(t *testing.T) {
		ws := t.TempDir()
		sessionKey := "sk_v1_fallback_completed"
		seedGoalForFeedback(t, ws, sessionKey, "Done task", goal.StatusCompleted)
		ts := &turnState{
			workspace:   ws,
			sessionKey:  sessionKey,
			agent:       &AgentInstance{Workspace: ws},
		}
		tc := providers.ToolCall{Name: "exec", Arguments: map[string]any{}}
		got := toolFeedbackCardExplanationForRender(cfg, ts, nil, tc, nil)
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("no goal file -> empty", func(t *testing.T) {
		ws := t.TempDir()
		ts := &turnState{
			workspace:   ws,
			sessionKey:  "sk_v1_no_goal",
			agent:       &AgentInstance{Workspace: ws},
		}
		tc := providers.ToolCall{Name: "exec", Arguments: map[string]any{}}
		got := toolFeedbackCardExplanationForRender(cfg, ts, nil, tc, nil)
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("ts nil -> empty (CLI mode without turn state)", func(t *testing.T) {
		tc := providers.ToolCall{Name: "exec", Arguments: map[string]any{}}
		got := toolFeedbackCardExplanationForRender(cfg, nil, nil, tc, nil)
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("ts.workspace empty -> empty", func(t *testing.T) {
		ts := &turnState{
			sessionKey: "sk_v1_some",
		}
		tc := providers.ToolCall{Name: "exec", Arguments: map[string]any{}}
		got := toolFeedbackCardExplanationForRender(cfg, ts, nil, tc, nil)
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func seedActiveGoalForFeedback(t *testing.T, workspace, sessionKey, objective string) {
	t.Helper()
	store := goal.NewStore(workspace)
	g := &goal.Goal{
		Name: "test-goal",
		Description: goal.Description{
			Objective:       objective,
			SuccessCriteria: []string{"done"},
		},
		Status: goal.StatusActive,
	}
	if err := store.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}
}

func seedGoalForFeedback(t *testing.T, workspace, sessionKey, objective string, status goal.Status) {
	t.Helper()
	store := goal.NewStore(workspace)
	g := &goal.Goal{
		Name: "test-goal",
		Description: goal.Description{
			Objective:       objective,
			SuccessCriteria: []string{"done"},
		},
		Status: status,
	}
	if err := store.Write(sessionKey, g); err != nil {
		t.Fatalf("seed goal: %v", err)
	}
}

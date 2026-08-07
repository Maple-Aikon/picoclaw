package agent

import "testing"

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

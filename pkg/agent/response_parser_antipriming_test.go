package agent

import (
	"testing"
)

func TestResponseParser_AntiprimingRegexVariants(t *testing.T) {
	testCases := []struct {
		name         string
		content      string
		expectedName string
		expectedArg  string
	}{
		{
			name:         "Standard format",
			content:      `I will check files [tool_use: read_file, args: {"path":"test.txt"}] now.`,
			expectedName: "read_file",
			expectedArg:  "test.txt",
		},
		{
			name:         "Missing comma format",
			content:      `Let me run [tool_use: exec args: {"command":"ls -la"}] please.`,
			expectedName: "exec",
			expectedArg:  "ls -la",
		},
		{
			name: "Markdown codeblock wrapper",
			content: "I will call complete_goal\n```json\n[tool_use: complete_goal, args: {\"summary\":\"all done\"}]\n```",
			expectedName: "complete_goal",
			expectedArg:  "all done",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			extracted := ExtractPseudoToolCalls(tc.content)
			if len(extracted) != 1 {
				t.Fatalf("expected 1 extracted tool call, got %d", len(extracted))
			}
			if extracted[0].Name != tc.expectedName {
				t.Errorf("expected tool name %s, got %s", tc.expectedName, extracted[0].Name)
			}
		})
	}
}

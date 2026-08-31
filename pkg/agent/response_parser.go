package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

var (
	// regexStandard matches standard format: [tool_use: name, args: {...}]
	regexStandard = regexp.MustCompile(`(?s)\[tool_use:\s*([a-zA-Z0-9_.-]+)\s*,\s*args:\s*(\{.*?\})\]`)
	// regexNoComma matches missing comma variant: [tool_use: name args: {...}]
	regexNoComma = regexp.MustCompile(`(?s)\[tool_use:\s*([a-zA-Z0-9_.-]+)\s*args:\s*(\{.*?\})\]`)
	// regexMarkdown matches codeblock-wrapped variant: ```json [tool_use: name, args: {...}] ```
	regexMarkdown = regexp.MustCompile("(?s)```(?:json|text)?\\s*\\[tool_use:\\s*([a-zA-Z0-9_.-]+)\\s*,?\\s*args:\\s*(\\{.*?\\})\\]\\s*```")
)

// ExtractedPseudoToolCall represents a tool call parsed from raw assistant text
type ExtractedPseudoToolCall struct {
	Name      string
	Arguments map[string]any
	RawMatch  string
}

// ExtractPseudoToolCalls finds any tool calls embedded in assistant text responses
// using resilient multi-pattern matching.
func ExtractPseudoToolCalls(content string) []ExtractedPseudoToolCall {
	var calls []ExtractedPseudoToolCall
	if strings.TrimSpace(content) == "" {
		return calls
	}

	patterns := []*regexp.Regexp{regexMarkdown, regexStandard, regexNoComma}
	currentText := content

	for _, pat := range patterns {
		for {
			loc := pat.FindStringSubmatchIndex(currentText)
			if loc == nil || len(loc) < 6 {
				break
			}
			raw := currentText[loc[0]:loc[1]]
			name := strings.TrimSpace(currentText[loc[2]:loc[3]])
			argsJSON := strings.TrimSpace(currentText[loc[4]:loc[5]])

			var args map[string]any
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				args = map[string]any{"raw": argsJSON}
			}

			calls = append(calls, ExtractedPseudoToolCall{
				Name:      name,
				Arguments: args,
				RawMatch:  raw,
			})

			padding := strings.Repeat(" ", loc[1]-loc[0])
			currentText = currentText[:loc[0]] + padding + currentText[loc[1]:]
		}
	}
	return calls
}

// ConvertExtractedToToolCalls turns extracted pseudo-calls into providers.ToolCall structs
func ConvertExtractedToToolCalls(extracted []ExtractedPseudoToolCall) []providers.ToolCall {
	var toolCalls []providers.ToolCall
	for i, ec := range extracted {
		argsBytes, _ := json.Marshal(ec.Arguments)
		toolCalls = append(toolCalls, providers.ToolCall{
			ID:   fmt.Sprintf("pseudo_call_%d", i+1),
			Type: "function",
			Name: ec.Name,
			Function: &providers.FunctionCall{
				Name:      ec.Name,
				Arguments: string(argsBytes),
			},
			Arguments: ec.Arguments,
		})
	}
	return toolCalls
}

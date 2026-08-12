package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const ToolFeedbackContinuationHint = "Continuing the current task."

func FormatArgsJSON(args map[string]any, prettyPrint, disableEscapeHTML bool) string {
	// Normalize nil to empty map for consistent output
	if args == nil {
		args = map[string]any{}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if prettyPrint {
		enc.SetIndent("", "  ")
	}
	if disableEscapeHTML {
		enc.SetEscapeHTML(false)
	}
	if err := enc.Encode(args); err != nil {
		// Fallback to fmt.Sprintf to preserve visibility of problematic args
		return fmt.Sprintf("%v", args)
	}
	return strings.TrimSpace(buf.String())
}

// FormatToolFeedbackMessage renders a tool feedback message for chat channels.
// It keeps the tool name on the first line for animation and can include both
// a human explanation and the serialized tool arguments in the body.
//
// iterContext is an optional one-line progress context (e.g. "📊 #3/250 ·
// Goal-Checkpoint") rendered right after the tool header, before the
// explanation body. An empty iterContext omits the line entirely.
//
// pendingExplanation is an optional LLM-authored continuation explanation
// rendered below the args preview with a divider (10 box-drawing chars)
// and a speech-balloon prefix. It carries over from the previous tool-call
// iteration so the tracked Telegram feedback card stays the bottom-of-
// timeline message instead of a separate durable explanation message
// pushing the card up (Phase 12.58.3 — folded into tracked card; complete_goal
// path removed in 12.58.3). An empty pendingExplanation omits the divider
// and the speech-balloon line entirely.
//
// Header lines are separated by "\n\n" (blank line), NOT a single "\n":
// Telegram Rich Messages collapse a single newline into inline whitespace
// inside a paragraph block (verified 2026-08-09), so a single "\n" would
// render the tool name and the 📊 context on the same line. "\n\n" forces a
// paragraph break in rich mode and renders as a blank line in HTML mode.
func FormatToolFeedbackMessage(toolName, explanation, argsPreview, iterContext, pendingExplanation string) string {
	toolName = strings.TrimSpace(toolName)
	explanation = strings.TrimSpace(explanation)
	argsPreview = strings.TrimSpace(argsPreview)
	iterContext = strings.TrimSpace(iterContext)
	pendingExplanation = strings.TrimSpace(pendingExplanation)

	bodyLines := make([]string, 0, 3)
	if explanation != "" {
		bodyLines = append(bodyLines, explanation)
	}
	if argsPreview != "" {
		bodyLines = append(bodyLines, "```json\n"+argsPreview+"\n```")
	}
	if pendingExplanation != "" {
		bodyLines = append(bodyLines, "──────────", "💬 "+pendingExplanation)
	}
	body := strings.Join(bodyLines, "\n")

	if toolName == "" {
		if iterContext == "" {
			return body
		}
		if body == "" {
			return iterContext
		}
		return iterContext + "\n\n" + body
	}
	if body == "" {
		if iterContext == "" {
			return fmt.Sprintf("\U0001f527 `%s`", toolName)
		}
		return fmt.Sprintf("\U0001f527 `%s`\n\n%s", toolName, iterContext)
	}
	if iterContext == "" {
		return fmt.Sprintf("\U0001f527 `%s`\n\n%s", toolName, body)
	}

	return fmt.Sprintf("\U0001f527 `%s`\n\n%s\n\n%s", toolName, iterContext, body)
}

// FitToolFeedbackMessage keeps tool feedback within a single outbound message.
// It preserves the first line when possible and truncates the explanation body
// instead of letting the message be split into multiple chunks.
func FitToolFeedbackMessage(content string, maxLen int) string {
	content = strings.TrimSpace(content)
	if content == "" || maxLen <= 0 {
		return ""
	}
	if len([]rune(content)) <= maxLen {
		return content
	}

	firstLine, rest, hasRest := strings.Cut(content, "\n")
	firstLine = strings.TrimSpace(firstLine)
	rest = strings.TrimSpace(rest)

	if !hasRest || rest == "" {
		return Truncate(firstLine, maxLen)
	}

	if len([]rune(firstLine)) >= maxLen {
		return Truncate(firstLine, maxLen)
	}

	remaining := maxLen - len([]rune(firstLine)) - 1
	if remaining <= 0 {
		return Truncate(firstLine, maxLen)
	}

	return firstLine + "\n" + Truncate(rest, remaining)
}

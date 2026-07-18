package tools

import (
	"context"
	"fmt"
	"strings"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// ToolKnowledgeTool exposes ToolKnowledgeStore to the LLM as a structured
// tool call. The LLM can save/recall "lessons learned" per tool — facts
// that survive across turns and across the signature tracker reset.
//
// Wired by default in NewToolRegistry below; visibility is hidden (only
// reachable via TTL lookup) so it does not pollute every prompt.
//
// Action argument (required): one of "save" | "read" | "list" | "delete"
// Tool argument (required for save/read/delete): the canonical tool name
//   (e.g. "fs.read", "web.fetch"). Must match sanitizeToolName regex.
// Body argument (required for save): the knowledge body in markdown.
//   Plain text or markdown is fine; YAML frontmatter is auto-stamped.
type ToolKnowledgeTool struct {
	store *ToolKnowledgeStore
}

// NewToolKnowledgeTool wraps an existing store. Pass the workspace dir
// via ToolKnowledgeTool via the registry's SetToolKnowledgeStore helper
// (see registry.go) — the tool itself does not own its storage root.
func NewToolKnowledgeTool(store *ToolKnowledgeStore) *ToolKnowledgeTool {
	return &ToolKnowledgeTool{store: store}
}

// Name is the canonical tool name; MUST be stable across versions so
// historical knowledge entries remain addressable.
func (t *ToolKnowledgeTool) Name() string {
	return "tool_knowledge"
}

// Description is shown to the LLM. Kept terse to preserve prompt budget.
func (t *ToolKnowledgeTool) Description() string {
	return "Save or recall persistent 'lessons learned' notes per tool name. " +
		"Use `action=save` after a tool fails twice with the same approach to record " +
		"what went wrong and what works instead. Use `action=read <tool>` before retrying " +
		"a tool that you (or a prior turn) previously saved knowledge for. " +
		"Use `action=list` to enumerate saved tools. Use `action=delete <tool>` to forget. " +
		"Knowledge is per-workspace, persisted across sessions, and overrides transient tool hints."
}

// Parameters exposes the action-based schema.
func (t *ToolKnowledgeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"save", "read", "list", "delete"},
				"description": "REQUIRED. Which operation to perform: save | read | list | delete.",
			},
			"tool": map[string]any{
				"type":        "string",
				"description": "REQUIRED for save/read/delete. The canonical tool name (e.g. 'fs.read', 'web.fetch'). Names are normalized to lower-case; only [a-z0-9._-] allowed; no slashes, no '..'.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "REQUIRED for save. The knowledge body in markdown. Plain text fine. Recommended sections: '## What goes wrong' and '## What works instead'. Aim for 50+ chars; bodies shorter than that get a 'consider expanding' hint.",
			},
		},
		"required": []string{"action"},
	}
}

// PromptMetadata marks this as a hidden-tooling slot so it does not
// pollute the always-visible prompt; the LLM reaches it via TTL lookup.
func (t *ToolKnowledgeTool) PromptMetadata() toolshared.PromptMetadata {
	return toolshared.PromptMetadata{
		Layer:  toolshared.ToolPromptLayerCapability,
		Slot:   toolshared.ToolPromptSlotTooling,
		Source: toolshared.ToolPromptSourceRegistry,
	}
}

// Execute dispatches based on the action argument.
func (t *ToolKnowledgeTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	if t.store == nil {
		return toolshared.ErrorResult(
			"tool_knowledge is not configured: no ToolKnowledgeStore attached to the registry. " +
				"This is a deployment bug — please report to the operator.",
		).WithErrorKind(toolshared.ErrDependencyDown)
	}

	action, _ := args["action"].(string)
	action = strings.TrimSpace(strings.ToLower(action))
	if action == "" {
		return toolshared.ErrorResult(
			"tool_knowledge requires an `action` argument: save | read | list | delete.",
		).WithErrorKind(toolshared.ErrInvalidInput)
	}

	switch action {
	case "save":
		return t.doSave(args)
	case "read":
		return t.doRead(args)
	case "list":
		return t.doList()
	case "delete":
		return t.doDelete(args)
	default:
		return toolshared.ErrorResult(
			fmt.Sprintf("unknown action %q: must be save | read | list | delete", action),
		).WithErrorKind(toolshared.ErrInvalidInput)
	}
}

func (t *ToolKnowledgeTool) doSave(args map[string]any) *toolshared.ToolResult {
	tool, _ := args["tool"].(string)
	body, _ := args["body"].(string)

	if strings.TrimSpace(tool) == "" {
		return toolshared.ErrorResult(
			"tool_knowledge action=save requires a `tool` argument " +
				"(e.g. tool='fs.read').",
		).WithErrorKind(toolshared.ErrInvalidInput)
	}

	path, size, err := t.store.Save(tool, body)
	if err != nil {
		// Distinguish user errors (retryable advice) vs internal (donut retry).
		switch err {
		case ErrKnowledgeEmpty:
			return toolshared.ErrorResult(
				"tool_knowledge action=save requires a non-empty `body` argument. " +
					"Plain text or markdown; aim for 50+ chars across " +
					"'## What goes wrong' / '## What works instead' sections.",
			).WithErrorKind(toolshared.ErrInvalidInput)
		case ErrKnowledgeToolNameInvalid:
			return toolshared.ErrorResult(
				"tool_knowledge action=save received an invalid `tool` name. " +
					"Only [a-z0-9._-] characters allowed; no slashes, no '..'. " +
					"Use the canonical tool name (e.g. 'fs.read').",
			).WithErrorKind(toolshared.ErrInvalidInput)
		default:
			return toolshared.ErrorResult(
				fmt.Sprintf("tool_knowledge save failed: %v", err),
			).WithErrorKind(toolshared.ErrDependencyDown)
		}
	}

	// Warn on short bodies — soft nudge, not an error.
	hint := ""
	if size < MinBodyChars+150 { // frontmatter overhead means body itself can be slightly < cap
		hint = "\n\nNote: body is short. Consider expanding under " +
			"'## What goes wrong' and '## What works instead' headings " +
			"so future turns benefit from the lesson."
	}

	return toolshared.SilentResult(fmt.Sprintf(
		"Saved knowledge for tool %q (%d bytes) at %s.%s",
		strings.ToLower(strings.TrimSpace(tool)),
		size,
		path,
		hint,
	))
}

func (t *ToolKnowledgeTool) doRead(args map[string]any) *toolshared.ToolResult {
	tool, _ := args["tool"].(string)
	if strings.TrimSpace(tool) == "" {
		return toolshared.ErrorResult(
			"tool_knowledge action=read requires a `tool` argument " +
				"(e.g. tool='fs.read').",
		).WithErrorKind(toolshared.ErrInvalidInput)
	}

	body, err := t.store.Read(tool)
	if err != nil {
		if err == ErrKnowledgeNotFound {
			return toolshared.SilentResult(fmt.Sprintf(
				"No saved knowledge for tool %q yet. "+
					"If you just hit a non-trivial failure on this tool, consider calling "+
					"`tool_knowledge action=save tool=%q body='<lesson>'` so future turns benefit.",
				strings.ToLower(strings.TrimSpace(tool)),
				strings.ToLower(strings.TrimSpace(tool)),
			))
		}
		if err == ErrKnowledgeToolNameInvalid {
			return toolshared.ErrorResult(
				"tool_knowledge action=read received an invalid `tool` name. " +
					"Only [a-z0-9._-] characters allowed; no slashes, no '..'.",
			).WithErrorKind(toolshared.ErrInvalidInput)
		}
		return toolshared.ErrorResult(
			fmt.Sprintf("tool_knowledge read failed: %v", err),
		).WithErrorKind(toolshared.ErrDependencyDown)
	}
	return toolshared.SilentResult(body)
}

func (t *ToolKnowledgeTool) doList() *toolshared.ToolResult {
	tools, err := t.store.List()
	if err != nil {
		return toolshared.ErrorResult(
			fmt.Sprintf("tool_knowledge list failed: %v", err),
		).WithErrorKind(toolshared.ErrDependencyDown)
	}
	if len(tools) == 0 {
		return toolshared.SilentResult(
			"No tool knowledge saved yet. Call `tool_knowledge action=save tool=<name> body=<lesson>` " +
				"after a non-trivial tool failure to start building the knowledge base.",
		)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Saved knowledge for %d tool(s):\n", len(tools)))
	for _, n := range tools {
		sb.WriteString("  - ")
		sb.WriteString(n)
		sb.WriteByte('\n')
	}
	return toolshared.SilentResult(sb.String())
}

func (t *ToolKnowledgeTool) doDelete(args map[string]any) *toolshared.ToolResult {
	tool, _ := args["tool"].(string)
	if strings.TrimSpace(tool) == "" {
		return toolshared.ErrorResult(
			"tool_knowledge action=delete requires a `tool` argument.",
		).WithErrorKind(toolshared.ErrInvalidInput)
	}
	if err := t.store.Delete(tool); err != nil {
		if err == ErrKnowledgeToolNameInvalid {
			return toolshared.ErrorResult(
				"tool_knowledge action=delete received an invalid `tool` name.",
			).WithErrorKind(toolshared.ErrInvalidInput)
		}
		return toolshared.ErrorResult(
			fmt.Sprintf("tool_knowledge delete failed: %v", err),
		).WithErrorKind(toolshared.ErrDependencyDown)
	}
	return toolshared.SilentResult(fmt.Sprintf(
		"Forgot knowledge for tool %q (no-op if none was saved).",
		strings.ToLower(strings.TrimSpace(tool)),
	))
}

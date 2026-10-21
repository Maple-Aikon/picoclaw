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
// Action argument (required): one of "replace" | "append" | "read" | "list" | "delete"
// Tool argument (required for replace/append/read/delete): the RUNTIME tool name,
// stored verbatim as <tool>.md and loaded only on a byte-identical name.
//
//	Native tools are snake_case ('exec'); MCP tools are 'mcp_<server>_<tool>'
//	('mcp_janus-graph_search_memory'). See Parameters() for the 5 naming rules.
//
// Body argument (required for replace/append): the knowledge body in markdown.
//
//	Plain text or markdown is fine; YAML frontmatter is auto-stamped.
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

// Description is shown to the LLM. Kept terse to preserve prompt budget —
// the naming rules live on the `tool` parameter, which is where the decision
// actually gets made.
func (t *ToolKnowledgeTool) Description() string {
	return "Store or recall persistent 'lessons learned' notes per tool name. " +
		"Use `action=replace` to OVERWRITE an entry after a tool fails twice with the same approach. " +
		"Use `action=append` to ADD to an existing entry without overwriting it. " +
		"Use `action=read <tool>` before retrying a tool that you (or a prior turn) previously stored knowledge for. " +
		"Use `action=list` to enumerate stored tools. Use `action=delete <tool>` to forget. " +
		"Keys match the RUNTIME tool name EXACTLY — an entry under a near-miss name never loads. " +
		"Knowledge is per-workspace, persisted across sessions, and overrides transient tool hints."
}

// Parameters exposes the action-based schema.
func (t *ToolKnowledgeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []string{"replace", "append", "read", "list", "delete"},
				"description": "REQUIRED. Which operation to perform: replace | append | read | list | delete. " +
					"'replace' overwrites the entry, 'append' adds to it.",
			},
			"tool": map[string]any{
				"type": "string",
				"description": "REQUIRED for replace/append/read/delete. " +
					"KEY MATCHING IS EXACT: the entry is stored at <tool>.md and loaded only when a tool with " +
					"byte-identical runtime name is about to fail. A near-miss ('ctx_execute_run' vs " +
					"'ctx-execute-run') is a different entry that never loads, so there is no separator to " +
					"'normalize' -- pick the name the runtime actually reports. " +
					"Allowed characters: [a-z0-9._-]; lower-cased automatically; no slashes, no '..'. " +
					"RULE 1 (strongest, use whenever the tool exists): use the RUNTIME name verbatim -- native " +
					"tools are snake_case ('exec', 'read_file', 'write_file', 'edit_file', 'list_dir', " +
					"'tool_knowledge'); MCP tools are 'mcp_<server>_<tool>' ('mcp_janus-graph_search_memory', " +
					"'mcp_context-mode_ctx_execute', 'mcp_planning-mcp_plan_get'). " +
					"RULE 2 (a native tool's recurring topic, when there is no such tool): '<tool>.<aspect>' " +
					"('exec.background'). " +
					"RULE 3 (an MCP server's topic, not one of its tools): '<server>.<tool>' without the mcp_ " +
					"prefix, keeping the server's hyphens ('janus-graph.search-config'). " +
					"RULE 4 (a system domain, not a tool): '<domain>.<aspect>' ('janus.dlq-replay'). " +
					"RULE 5: pick ONE separator for the aspect part and stay with it ('-._' mix is the thing " +
					"that creates look-alike keys). Rules 2-4 are retrieval-only labels -- they are found by " +
					"searching, not auto-loaded; only Rule 1 fires on failure.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "REQUIRED for replace/append. The knowledge body in markdown. Plain text fine. Recommended sections: '## What goes wrong' and '## What works instead'. Aim for 50+ chars; bodies shorter than that get a 'consider expanding' hint.",
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
			"tool_knowledge requires an `action` argument: replace | append | read | list | delete.",
		).WithErrorKind(toolshared.ErrInvalidInput)
	}

	switch action {
	case "replace":
		return t.doReplace(args)
	case "append":
		return t.doAppend(args)
	case "read":
		return t.doRead(args)
	case "list":
		return t.doList()
	case "delete":
		return t.doDelete(args)
	case "save":
		return toolshared.ErrorResult(
			"the action `save` was removed on 2026-10-20 because it silently OVERWROTE the whole entry, destroying anything already stored. Use `replace` to overwrite deliberately, or `append` to add to the entry while keeping every byte already there.",
		).WithErrorKind(toolshared.ErrInvalidInput)
	default:
		return toolshared.ErrorResult(
			fmt.Sprintf("unknown action %q: must be replace | append | read | list | delete", action),
		).WithErrorKind(toolshared.ErrInvalidInput)
	}
}

// writeBody dispatches a write action at the store level and renders the
// shared validation messages. write is one of "replace" or "append".
func (t *ToolKnowledgeTool) writeBody(write string, args map[string]any) *toolshared.ToolResult {
	tool, _ := args["tool"].(string)
	body, _ := args["body"].(string)

	if strings.TrimSpace(tool) == "" {
		return toolshared.ErrorResult(
			fmt.Sprintf("tool_knowledge action=%s requires a %s argument "+
				"(e.g. tool='exec' for a native tool, or "+
				"'mcp_janus-graph_search_memory' for an MCP tool).", write, "`tool`"),
		).WithErrorKind(toolshared.ErrInvalidInput)
	}

	var path string
	var size int
	var err error
	switch write {
	case "replace":
		path, size, err = t.store.Replace(tool, body)
	case "append":
		path, size, err = t.store.Append(tool, body)
	default:
		// Unreachable: Execute validated the action before dispatching here.
		return toolshared.ErrorResult(
			fmt.Sprintf("internal: unhandled knowledge write action %q", write),
		).WithErrorKind(toolshared.ErrInvalidInput)
	}
	if err != nil {
		// Distinguish user errors (retryable advice) vs internal (donut retry).
		switch err {
		case ErrKnowledgeEmpty:
			return toolshared.ErrorResult(
				fmt.Sprintf("tool_knowledge action=%s requires a non-empty `body` argument. "+
					"Plain text or markdown; aim for 50+ chars across "+
					"'## What goes wrong' / '## What works instead' sections.", write),
			).WithErrorKind(toolshared.ErrInvalidInput)
		case ErrKnowledgeToolNameInvalid:
			return toolshared.ErrorResult(
				fmt.Sprintf("tool_knowledge action=%s received an invalid `tool` name. "+
					"Only [a-z0-9._-] characters allowed; no slashes, no '..'. "+
					"Use the runtime tool name verbatim ('exec', 'write_file', or "+
					"'mcp_janus-graph_search_memory' for MCP tools) — an entry under a "+
					"near-miss name never loads, so there is no separator to normalize.", write),
			).WithErrorKind(toolshared.ErrInvalidInput)
		default:
			return toolshared.ErrorResult(
				fmt.Sprintf("tool_knowledge %s failed: %v", write, err),
			).WithErrorKind(toolshared.ErrDependencyDown)
		}
	}

	// Warn on short bodies — soft nudge, not an error.
	hint := ""
	if size < MinBodyChars+150 {
		hint = "\n\nNote: body is short. Consider expanding under " +
			"'## What goes wrong' and '## What works instead' headings " +
			"so future turns benefit from the lesson."
	}

	return toolshared.SilentResult(fmt.Sprintf(
		"%s knowledge for tool %q (%d bytes) at %s.%s",
		strings.ToUpper(write[:1])+write[1:],
		strings.ToLower(strings.TrimSpace(tool)),
		size,
		path,
		hint,
	))
}

// doReplace overwrites the entry. This is the explicit-destruction path —
// prefer doAppend when adding to a lesson.
func (t *ToolKnowledgeTool) doReplace(args map[string]any) *toolshared.ToolResult {
	return t.writeBody("replace", args)
}

// doAppend adds to the entry, leaving every byte already stored in place.
func (t *ToolKnowledgeTool) doAppend(args map[string]any) *toolshared.ToolResult {
	return t.writeBody("append", args)
}

func (t *ToolKnowledgeTool) doRead(args map[string]any) *toolshared.ToolResult {
	tool, _ := args["tool"].(string)
	if strings.TrimSpace(tool) == "" {
		return toolshared.ErrorResult(
			"tool_knowledge action=read requires a `tool` argument " +
				"(e.g. tool='exec' for a native tool, or " +
				"'mcp_janus-graph_search_memory' for an MCP tool).",
		).WithErrorKind(toolshared.ErrInvalidInput)
	}

	body, err := t.store.Read(tool)
	if err != nil {
		if err == ErrKnowledgeNotFound {
			return toolshared.SilentResult(fmt.Sprintf(
				"No saved knowledge for tool %q yet. "+
					"If you just hit a non-trivial failure on this tool, consider calling "+
					"`tool_knowledge action=replace tool=%q body='<lesson>'` so future turns benefit.",
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
			"No tool knowledge saved yet. Call `tool_knowledge action=replace tool=<name> body=<lesson>` " +
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

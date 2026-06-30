package tools

import (
	"context"
	"fmt"
	"strings"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// IterationExtender is implemented by a wrapper around the agent's turnState.
// It allows extend_turn_iteration to bump the iteration cap without importing
// pkg/agent (avoiding a circular dependency). The wrapper is injected into the
// tool context by the pipeline; see WithIterationExtender.
type IterationExtender interface {
	ExtendIterationCap(requested int, reason string) (int, error)
	RemainingIterations() int
	CurrentIteration() int
	IterationCap() int
	MaxIterationsCap() int
}

// --- Context injection ---

type iterationExtenderKeyType struct{}

var iterationExtenderKey = iterationExtenderKeyType{}

// WithIterationExtender injects an IterationExtender into the context so that
// ExtendTurnIterationTool.Execute can access the current turn's iteration state.
// Called by the pipeline when building the tool-execution context.
func WithIterationExtender(ctx context.Context, extender IterationExtender) context.Context {
	return context.WithValue(ctx, iterationExtenderKey, extender)
}

func iterationExtenderFromContext(ctx context.Context) IterationExtender {
	ext, _ := ctx.Value(iterationExtenderKey).(IterationExtender)
	return ext
}

// --- Tool ---

// ExtendTurnIterationTool grants the current turn additional tool iterations
// when approaching the iteration cap. The tool always extends by the agent's
// MaxIterations (the default budget), clamped to MaxIterationsCap — a partial
// extension is applied when only the residual budget is available.
//
// The tool requires an `intent` argument: a forward-looking plan describing
// what the LLM intends to do with the additional iterations. This is not a
// justification — it is used to surface goal drift during the extension segment.
type ExtendTurnIterationTool struct{}

// NewExtendTurnIterationTool returns a new instance.
func NewExtendTurnIterationTool() *ExtendTurnIterationTool {
	return &ExtendTurnIterationTool{}
}

func (t *ExtendTurnIterationTool) Name() string {
	return "extend_turn_iteration"
}

func (t *ExtendTurnIterationTool) Description() string {
	return "Extend the current turn's tool iteration limit. Use this when approaching the iteration cap (you'll receive a soft reminder when 2 or fewer iterations remain). You MUST provide an `intent` argument describing what you plan to do with the additional iterations — this is a forward-looking plan, not a justification. The tool cannot extend beyond the agent's absolute maximum. After calling, the turn continues normally with the new cap in effect."
}

func (t *ExtendTurnIterationTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"intent": map[string]any{
				"type":        "string",
				"description": "REQUIRED. Describe what you intend to do with the additional iterations. This is not a justification — it is a forward-looking plan: which tool calls you will make, what you expect to learn or change, and why the task is not yet complete. Used to surface goal drift during the extension segment.",
			},
		},
		"required": []string{"intent"},
	}
}

func (t *ExtendTurnIterationTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	ext := iterationExtenderFromContext(ctx)
	if ext == nil {
		return &toolshared.ToolResult{
			ForLLM:  "extend_turn_iteration called outside of an active turn context.",
			IsError: true,
			ErrKind: toolshared.ErrInvalidInput,
		}
	}

	intent, _ := args["intent"].(string)
	intent = strings.TrimSpace(intent)
	if intent == "" {
		return &toolshared.ToolResult{
			ForLLM:  "extend_turn_iteration requires an `intent` argument describing what you plan to do with the additional iterations. The intent is a forward-looking plan (which tools you will call, what you expect to learn/change, why the task is not yet complete), not a justification. Please retry with an explicit intent.",
			IsError: true,
			ErrKind: toolshared.ErrInvalidInput,
		}
	}

	// Tool takes no iteration-count argument: extend by MaxIterations (the
	// default budget), clamped so the new cap never exceeds MaxIterationsCap.
	// A partial extension may be applied when only the residual budget is
	// available (e.g. iterationCap=18, MaxIterationsCap=25 → extends by 7
	// to land at 25). If no room remains, ExtendIterationCap returns an error.
	newCap, err := ext.ExtendIterationCap(0, intent)
	if err != nil {
		return &toolshared.ToolResult{
			ForLLM: fmt.Sprintf(
				"Could not extend iteration cap: %v. Current cap: %d, current iteration: %d, absolute ceiling: %d.",
				err, ext.IterationCap(), ext.CurrentIteration(), ext.MaxIterationsCap(),
			),
			IsError: true,
			ErrKind: toolshared.ErrInvalidInput,
		}
	}

	return toolshared.SilentResult(fmt.Sprintf(
		"Turn iteration cap extended. New cap: %d. Remaining: %d.",
		newCap, ext.RemainingIterations(),
	))
}
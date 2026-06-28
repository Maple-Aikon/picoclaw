package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/tools"
)

// PromptSourceToolHealth is the source ID for the transient tool-availability
// steering directive emitted by ToolHealthContributor. Lives in the Turn layer
// because tool health is per-conversation-state, not a stable capability.
const PromptSourceToolHealth PromptSourceID = "turn:tool_health"

// ToolHealthContributor emits a transient Steering directive listing tools
// whose circuit breaker is currently open. The directive tells the LLM to
// avoid those tools on the next call and prefer alternatives or direct
// answers. Zero cost when no tools are unhealthy (returns nil parts).
//
// Wired into the ContextBuilder at agent init time:
//
//	cb.RegisterPromptContributor(&ToolHealthContributor{
//	    listOpen: toolRegistry.OpenTools,
//	})
type ToolHealthContributor struct {
	// listOpen returns the current open-tool snapshot. Re-evaluated on each
	// prompt build so transient circuit state is always reflected.
	listOpen func() []tools.OpenToolInfo
}

// PromptSource declares the placement contract: only the Turn-layer Steering
// slot, marked unstable (cache-bypassing) because breaker state changes minute
// to minute.
func (c *ToolHealthContributor) PromptSource() PromptSourceDescriptor {
	return PromptSourceDescriptor{
		ID:              PromptSourceToolHealth,
		Owner:           "agent",
		Description:     "Tools whose circuit breaker is currently open (transient steering)",
		Allowed:         []PromptPlacement{{Layer: PromptLayerTurn, Slot: PromptSlotSteering}},
		StableByDefault: false,
	}
}

// ContributePrompt produces the Steering part (or nil when nothing is broken).
// The format is intentionally human-ish so the LLM parses intent at a glance:
// "X is unavailable. Prefer Y or answer directly." plus a per-tool table of
// name / age / failure count so the model can decide whether to wait or work
// around the outage.
func (c *ToolHealthContributor) ContributePrompt(
	_ context.Context,
	_ PromptBuildRequest,
) ([]PromptPart, error) {
	if c.listOpen == nil {
		return nil, nil
	}
	open := c.listOpen()
	if len(open) == 0 {
		return nil, nil // ← zero-cost when all tools are healthy
	}

	var b strings.Builder
	b.WriteString("# Tool Availability\n\n")
	b.WriteString("The following tools are currently unavailable. Do not call them. ")
	b.WriteString("Prefer an alternative tool that solves the same problem, or answer directly if you have enough context.\n\n")

	now := time.Now()
	for _, info := range open {
		age := now.Sub(info.OpenedAt).Truncate(time.Second)
		fmt.Fprintf(&b, "- `%s` — open for %s (%d consecutive failures)\n",
			info.Name, formatAge(age), info.Failures)
	}

	return []PromptPart{{
		ID:      "turn.tool_health",
		Layer:   PromptLayerTurn,
		Slot:    PromptSlotSteering,
		Source:  PromptSource{ID: PromptSourceToolHealth, Name: "tool_health"},
		Title:   "tool availability",
		Content: b.String(),
		Stable:  false,
		Cache:   PromptCacheNone,
	}}, nil
}

// formatAge renders a Duration as a compact human label: "30s", "2m15s",
// "1h10m". Anything beyond hours stays readable because tool outages don't
// usually last that long, and a day-long outage is its own kind of incident.
func formatAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
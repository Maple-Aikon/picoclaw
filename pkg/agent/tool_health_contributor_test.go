package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/tools"
)

// Tests for ToolHealthContributor — plan:
// exception-handling-recovery-pattern-gap-closure-20260628 (Task B1).
//
// The contributor is a Steering-layer prompt source that emits a transient
// "tool unavailable" directive whenever at least one circuit breaker is open.
// Zero-cost when all tools are healthy (returns nil parts).

func TestToolHealthContributor_NoOpenTools_ReturnsNil(t *testing.T) {
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo { return nil },
	}
	parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parts != nil {
		t.Fatalf("parts = %+v, want nil (zero-cost path when no tools are open)", parts)
	}
}

func TestToolHealthContributor_OneOpenTool_EmitsDirective(t *testing.T) {
	// Use 125s ago so the formatter renders "2m5s" — robust against
	// millisecond-level test jitter (a 1s delay still shows as "2m5s" or
	// "2m6s", both of which contain "2m").
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo {
			return []tools.OpenToolInfo{
				{
					Name:     "web_search",
					OpenedAt: time.Now().Add(-125 * time.Second),
					Failures: 3,
				},
			}
		},
	}
	parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts count = %d, want 1", len(parts))
	}
	p := parts[0]
	if p.Layer != PromptLayerTurn {
		t.Errorf("Layer = %q, want %q", p.Layer, PromptLayerTurn)
	}
	if p.Slot != PromptSlotSteering {
		t.Errorf("Slot = %q, want %q", p.Slot, PromptSlotSteering)
	}
	if p.Cache != PromptCacheNone {
		t.Errorf("Cache = %q, want %q (transient steering must not cache)",
			p.Cache, PromptCacheNone)
	}
	if p.Stable {
		t.Errorf("Stable = true, want false (transient by nature)")
	}
	if !strings.Contains(p.Content, "web_search") {
		t.Errorf("Content missing tool name: %q", p.Content)
	}
	if !strings.Contains(p.Content, "2m") {
		t.Errorf("Content missing age value (expected '2m...' for 125s): %q", p.Content)
	}
	if !strings.Contains(p.Content, "3") {
		t.Errorf("Content missing failure count: %q", p.Content)
	}
	lower := strings.ToLower(p.Content)
	if !strings.Contains(lower, "unavailable") &&
		!strings.Contains(lower, "do not call") {
		t.Errorf("Content missing directive language: %q", p.Content)
	}
}

func TestToolHealthContributor_MultipleOpenTools_AllListed(t *testing.T) {
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo {
			return []tools.OpenToolInfo{
				{
					Name:     "tavily_search",
					OpenedAt: time.Now().Add(-125 * time.Second),
					Failures: 5,
				},
				{
					Name:     "web_search",
					OpenedAt: time.Now().Add(-65 * time.Second),
					Failures: 3,
				},
			}
		},
	}
	parts, err := c.ContributePrompt(context.Background(), PromptBuildRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("parts count = %d, want 1 (single Steering part listing all)", len(parts))
	}
	if !strings.Contains(parts[0].Content, "tavily_search") {
		t.Errorf("Content missing tavily_search: %q", parts[0].Content)
	}
	if !strings.Contains(parts[0].Content, "web_search") {
		t.Errorf("Content missing web_search: %q", parts[0].Content)
	}
}

func TestToolHealthContributor_PromptSource_DeclaresSteeringSlot(t *testing.T) {
	c := &ToolHealthContributor{
		listOpen: func() []tools.OpenToolInfo { return nil },
	}
	desc := c.PromptSource()
	if desc.ID != PromptSourceToolHealth {
		t.Errorf("ID = %q, want %q", desc.ID, PromptSourceToolHealth)
	}
	if len(desc.Allowed) != 1 {
		t.Fatalf("Allowed count = %d, want 1", len(desc.Allowed))
	}
	p := desc.Allowed[0]
	if p.Layer != PromptLayerTurn {
		t.Errorf("Placement.Layer = %q, want %q", p.Layer, PromptLayerTurn)
	}
	if p.Slot != PromptSlotSteering {
		t.Errorf("Placement.Slot = %q, want %q", p.Slot, PromptSlotSteering)
	}
	if desc.StableByDefault {
		t.Error("StableByDefault = true, want false (transient steering directive)")
	}
}
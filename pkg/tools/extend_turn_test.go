package tools

import (
	"context"
	"strings"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// mockIterationExtender implements IterationExtender for testing.
type mockIterationExtender struct {
	cap              int
	absCap           int
	current          int
	extendErr        error
	extendResult     int
	lastRequested    int
	lastReason       string
}

func (m *mockIterationExtender) ExtendIterationCap(requested int, reason string) (int, error) {
	m.lastRequested = requested
	m.lastReason = reason
	if m.extendErr != nil {
		return m.cap, m.extendErr
	}
	m.cap = m.extendResult
	return m.extendResult, nil
}

func (m *mockIterationExtender) RemainingIterations() int {
	return m.cap - m.current
}

func (m *mockIterationExtender) CurrentIteration() int { return m.current }

func (m *mockIterationExtender) IterationCap() int { return m.cap }

func (m *mockIterationExtender) MaxIterationsCap() int { return m.absCap }

// --- Tests ---

func TestExtendTurnIterationTool_Name(t *testing.T) {
	tool := NewExtendTurnIterationTool()
	if got := tool.Name(); got != "extend_turn_iteration" {
		t.Errorf("Name() = %q, want %q", got, "extend_turn_iteration")
	}
}

func TestExtendTurnIterationTool_Description_ContainsIntent(t *testing.T) {
	tool := NewExtendTurnIterationTool()
	desc := tool.Description()
	if !contains(desc, "intent") {
		t.Error("Description() should mention `intent` argument")
	}
}

func TestExtendTurnIterationTool_Parameters_SchemaShape(t *testing.T) {
	tool := NewExtendTurnIterationTool()
	params := tool.Parameters()

	if params["type"] != "object" {
		t.Errorf("type = %v, want \"object\"", params["type"])
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties is not a map")
	}

	intentProp, ok := props["intent"].(map[string]any)
	if !ok {
		t.Fatal("missing `intent` property in schema")
	}
	if intentProp["type"] != "string" {
		t.Errorf("intent.type = %v, want \"string\"", intentProp["type"])
	}

	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("required is not a []string")
	}
	if len(required) != 1 || required[0] != "intent" {
		t.Errorf("required = %v, want [\"intent\"]", required)
	}
}

func TestExtendTurnIterationTool_Execute_NoExtenderInContext(t *testing.T) {
	tool := NewExtendTurnIterationTool()
	res := tool.Execute(context.Background(), map[string]any{
		"intent": "continue working",
	})

	if res == nil {
		t.Fatal("Execute returned nil")
	}
	if !res.IsError {
		t.Error("expected IsError=true when no extender in context")
	}
	if res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("ErrKind = %v, want ErrInvalidInput", res.ErrKind)
	}
}

func TestExtendTurnIterationTool_Execute_MissingIntent(t *testing.T) {
	tool := NewExtendTurnIterationTool()
	ctx := WithIterationExtender(context.Background(), &mockIterationExtender{
		cap:      20,
		absCap:   100,
		current:  18,
	})
	res := tool.Execute(ctx, map[string]any{})

	if res == nil {
		t.Fatal("Execute returned nil")
	}
	if !res.IsError {
		t.Error("expected IsError=true when intent is missing")
	}
	if res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("ErrKind = %v, want ErrInvalidInput", res.ErrKind)
	}
}

func TestExtendTurnIterationTool_Execute_EmptyIntent(t *testing.T) {
	tool := NewExtendTurnIterationTool()
	ctx := WithIterationExtender(context.Background(), &mockIterationExtender{
		cap:      20,
		absCap:   100,
		current:  18,
	})
	res := tool.Execute(ctx, map[string]any{
		"intent": "   ",
	})

	if res == nil {
		t.Fatal("Execute returned nil")
	}
	if !res.IsError {
		t.Error("expected IsError=true when intent is whitespace-only")
	}
}

func TestExtendTurnIterationTool_Execute_Success(t *testing.T) {
	tool := NewExtendTurnIterationTool()
	mock := &mockIterationExtender{
		cap:          20,
		absCap:       100,
		current:      18,
		extendResult: 40,
	}
	ctx := WithIterationExtender(context.Background(), mock)
	res := tool.Execute(ctx, map[string]any{
		"intent": "I need to read 3 more files and compare their contents",
	})

	if res == nil {
		t.Fatal("Execute returned nil")
	}
	if res.IsError {
		t.Errorf("expected success, got error: %s", res.ForLLM)
	}
	if !res.Silent {
		t.Error("expected Silent=true for successful extension")
	}
	if mock.lastRequested != 0 {
		t.Errorf("ExtendIterationCap called with requested=%d, want 0 (default budget)", mock.lastRequested)
	}
	if mock.lastReason != "I need to read 3 more files and compare their contents" {
		t.Errorf("ExtendIterationCap called with reason=%q, want the intent string", mock.lastReason)
	}
}

func TestExtendTurnIterationTool_Execute_ExtendError(t *testing.T) {
	tool := NewExtendTurnIterationTool()
	mock := &mockIterationExtender{
		cap:       100,
		absCap:    100,
		current:   100,
		extendErr: errAlreadyAtCeiling,
	}
	ctx := WithIterationExtender(context.Background(), mock)
	res := tool.Execute(ctx, map[string]any{
		"intent": "continue",
	})

	if res == nil {
		t.Fatal("Execute returned nil")
	}
	if !res.IsError {
		t.Error("expected IsError=true when ExtendIterationCap fails")
	}
	if res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("ErrKind = %v, want ErrInvalidInput", res.ErrKind)
	}
	// Error message should include context values
	if !contains(res.ForLLM, "Current cap") {
		t.Error("error message should include current cap info")
	}
}

// --- helpers ---

func contains(s, substr string) bool {
	return len(s) >= len(substr) && strings.Contains(s, substr)
}

// sentinel error for mock
var errAlreadyAtCeiling = &simpleErr{"iteration cap already at absolute ceiling (100)"}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }
package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// --- mock types ---

type mockRegistryTool struct {
	name   string
	desc   string
	params map[string]any
	result *ToolResult
}

func (m *mockRegistryTool) Name() string               { return m.name }
func (m *mockRegistryTool) Description() string        { return m.desc }
func (m *mockRegistryTool) Parameters() map[string]any { return m.params }
func (m *mockRegistryTool) Execute(_ context.Context, _ map[string]any) *ToolResult {
	return m.result
}

type mockContextAwareTool struct {
	mockRegistryTool
	lastCtx context.Context
}

func (m *mockContextAwareTool) Execute(ctx context.Context, _ map[string]any) *ToolResult {
	m.lastCtx = ctx
	return m.result
}

type mockPromptMetadataTool struct {
	mockRegistryTool
	metadata PromptMetadata
}

func (m *mockPromptMetadataTool) PromptMetadata() PromptMetadata {
	return m.metadata
}

type mockAsyncRegistryTool struct {
	mockRegistryTool
	lastCB AsyncCallback
}

func (m *mockAsyncRegistryTool) ExecuteAsync(
	_ context.Context,
	args map[string]any,
	cb AsyncCallback,
) *ToolResult {
	m.lastCB = cb
	return m.result
}

type mockMediaStoreAwareTool struct {
	mockRegistryTool
	store media.MediaStore
}

func (m *mockMediaStoreAwareTool) SetMediaStore(store media.MediaStore) {
	m.store = store
}

// --- helpers ---

func newMockTool(name, desc string) *mockRegistryTool {
	return &mockRegistryTool{
		name:   name,
		desc:   desc,
		params: map[string]any{"type": "object"},
		result: SilentResult("ok"),
	}
}

// --- tests ---

func TestNewToolRegistry(t *testing.T) {
	r := NewToolRegistry()
	if r.Count() != 0 {
		t.Errorf("expected empty registry, got count %d", r.Count())
	}
	if len(r.List()) != 0 {
		t.Errorf("expected empty list, got %v", r.List())
	}
}

func TestToolRegistry_RegisterAndGet(t *testing.T) {
	r := NewToolRegistry()
	tool := newMockTool("echo", "echoes input")
	r.Register(tool)

	got, ok := r.Get("echo")
	if !ok {
		t.Fatal("expected to find registered tool")
	}
	if got.Name() != "echo" {
		t.Errorf("expected name 'echo', got %q", got.Name())
	}
}

func TestToolRegistry_AllowlistFiltersRegistrations(t *testing.T) {
	r := NewToolRegistry()
	r.SetAllowlist([]string{"Allowed_Tool"})

	r.Register(newMockTool("allowed_tool", "allowed"))
	r.Register(newMockTool("blocked_tool", "blocked"))
	r.RegisterHidden(newMockTool("hidden_blocked", "hidden blocked"))

	if _, ok := r.Get("allowed_tool"); !ok {
		t.Fatal("expected allowed_tool to be registered")
	}
	if _, ok := r.Get("blocked_tool"); ok {
		t.Fatal("blocked_tool should not be registered")
	}
	if _, ok := r.Get("hidden_blocked"); ok {
		t.Fatal("hidden_blocked should not be registered")
	}
	if got := r.List(); len(got) != 1 || got[0] != "allowed_tool" {
		t.Fatalf("registry list = %v, want [allowed_tool]", got)
	}
}

func TestToolRegistry_AllowlistStillAllowsDiscoveryTools(t *testing.T) {
	r := NewToolRegistry()
	r.SetAllowlist([]string{"mcp_github_search"})

	r.Register(newMockTool(BM25SearchToolName, "discover hidden tools"))
	r.Register(newMockTool(RegexSearchToolName, "discover hidden tools via regex"))
	r.Register(newMockTool("blocked_tool", "blocked"))

	if _, ok := r.Get(BM25SearchToolName); !ok {
		t.Fatal("expected BM25 discovery tool to bypass allowlist filtering")
	}
	if _, ok := r.Get(RegexSearchToolName); !ok {
		t.Fatal("expected regex discovery tool to bypass allowlist filtering")
	}
	if _, ok := r.Get("blocked_tool"); ok {
		t.Fatal("blocked_tool should not be registered")
	}
}

func TestToolRegistry_HasRegisteredIncludesHiddenTools(t *testing.T) {
	r := NewToolRegistry()
	r.SetAllowlist([]string{"visible", "hidden"})

	r.Register(newMockTool("visible", "visible"))
	r.RegisterHidden(newMockTool("hidden", "hidden"))
	r.RegisterHidden(newMockTool("blocked", "blocked"))

	if !r.HasRegistered("visible") {
		t.Fatal("expected visible tool to be registered")
	}
	if !r.HasRegistered("hidden") {
		t.Fatal("expected hidden tool to be reported as registered")
	}
	if r.HasRegistered("blocked") {
		t.Fatal("blocked tool should not be registered")
	}
	if _, ok := r.Get("hidden"); ok {
		t.Fatal("hidden tool with zero TTL should not be callable through Get")
	}
}

func TestToolRegistry_Get_NotFound(t *testing.T) {
	r := NewToolRegistry()
	_, ok := r.Get("nonexistent")
	if ok {
		t.Error("expected ok=false for unregistered tool")
	}
}

func TestToolRegistry_RegisterOverwrite(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("dup", "first"))
	r.Register(newMockTool("dup", "second"))

	if r.Count() != 1 {
		t.Errorf("expected count 1 after overwrite, got %d", r.Count())
	}
	tool, _ := r.Get("dup")
	if tool.Description() != "second" {
		t.Errorf("expected overwritten description 'second', got %q", tool.Description())
	}
}

func TestToolRegistry_Execute_Success(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockRegistryTool{
		name:   "greet",
		desc:   "says hello",
		params: map[string]any{},
		result: SilentResult("hello"),
	})

	result := r.Execute(context.Background(), "greet", nil)
	if result.IsError {
		t.Errorf("expected success, got error: %s", result.ForLLM)
	}
	// Phase 3 (tool-knowledge-...-20260718): first success in an anon
	// session now appends SoftPromptFirstSuccess. Tests assert the body
	// plus the suffix; the body itself is preserved verbatim.
	want := "hello" + SoftPromptFirstSuccess
	if result.ForLLM != want {
		t.Errorf("expected ForLLM %q, got %q", want, result.ForLLM)
	}
}

func TestToolRegistry_Execute_NotFound(t *testing.T) {
	r := NewToolRegistry()
	result := r.Execute(context.Background(), "missing", nil)
	if !result.IsError {
		t.Error("expected error for missing tool")
	}
	if !strings.Contains(result.ForLLM, "not found") {
		t.Errorf("expected 'not found' in error, got %q", result.ForLLM)
	}
	if result.Err == nil {
		t.Error("expected Err to be set via WithError")
	}
}

func TestToolRegistry_ExecuteWithContext_InjectsToolContext(t *testing.T) {
	r := NewToolRegistry()
	ct := &mockContextAwareTool{
		mockRegistryTool: *newMockTool("ctx_tool", "needs context"),
	}
	r.Register(ct)

	r.ExecuteWithContext(context.Background(), "ctx_tool", nil, "telegram", "chat-42", nil)

	if ct.lastCtx == nil {
		t.Fatal("expected Execute to be called")
	}
	if got := ToolChannel(ct.lastCtx); got != "telegram" {
		t.Errorf("expected channel 'telegram', got %q", got)
	}
	if got := ToolChatID(ct.lastCtx); got != "chat-42" {
		t.Errorf("expected chatID 'chat-42', got %q", got)
	}
}

func TestToolRegistry_ExecuteWithContext_EmptyContext(t *testing.T) {
	r := NewToolRegistry()
	ct := &mockContextAwareTool{
		mockRegistryTool: *newMockTool("ctx_tool", "needs context"),
	}
	r.Register(ct)

	r.ExecuteWithContext(context.Background(), "ctx_tool", nil, "", "", nil)

	if ct.lastCtx == nil {
		t.Fatal("expected Execute to be called")
	}
	// Empty values are still injected; tools decide what to do with them.
	if got := ToolChannel(ct.lastCtx); got != "" {
		t.Errorf("expected empty channel, got %q", got)
	}
	if got := ToolChatID(ct.lastCtx); got != "" {
		t.Errorf("expected empty chatID, got %q", got)
	}
}

func TestToolRegistry_ExecuteWithContext_PreservesMessageContext(t *testing.T) {
	r := NewToolRegistry()
	ct := &mockContextAwareTool{
		mockRegistryTool: *newMockTool("ctx_tool", "needs context"),
	}
	r.Register(ct)

	baseCtx := WithToolMessageContext(context.Background(), "msg-123", "msg-100")
	r.ExecuteWithContext(baseCtx, "ctx_tool", nil, "telegram", "chat-42", nil)

	if ct.lastCtx == nil {
		t.Fatal("expected Execute to be called")
	}
	if got := ToolChannel(ct.lastCtx); got != "telegram" {
		t.Errorf("expected channel 'telegram', got %q", got)
	}
	if got := ToolChatID(ct.lastCtx); got != "chat-42" {
		t.Errorf("expected chatID 'chat-42', got %q", got)
	}
	if got := ToolMessageID(ct.lastCtx); got != "msg-123" {
		t.Errorf("expected messageID 'msg-123', got %q", got)
	}
	if got := ToolReplyToMessageID(ct.lastCtx); got != "msg-100" {
		t.Errorf("expected replyToMessageID 'msg-100', got %q", got)
	}
}

func TestToolRegistry_ExecuteWithContext_AsyncCallback(t *testing.T) {
	r := NewToolRegistry()
	at := &mockAsyncRegistryTool{
		mockRegistryTool: *newMockTool("async_tool", "async work"),
	}
	at.result = AsyncResult("started")
	r.Register(at)

	called := false
	cb := func(_ context.Context, _ *ToolResult) { called = true }

	result := r.ExecuteWithContext(context.Background(), "async_tool", nil, "", "", cb)
	if at.lastCB == nil {
		t.Error("expected ExecuteAsync to have received a callback")
	}
	if !result.Async {
		t.Error("expected async result")
	}

	at.lastCB(context.Background(), SilentResult("done"))
	if !called {
		t.Error("expected callback to be invoked")
	}
}

func TestToolRegistry_GetDefinitions(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("alpha", "tool A"))

	defs := r.GetDefinitions()
	if len(defs) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(defs))
	}
	if defs[0]["type"] != "function" {
		t.Errorf("expected type 'function', got %v", defs[0]["type"])
	}
	fn, ok := defs[0]["function"].(map[string]any)
	if !ok {
		t.Fatal("expected 'function' key to be a map")
	}
	if fn["name"] != "alpha" {
		t.Errorf("expected name 'alpha', got %v", fn["name"])
	}
	if fn["description"] != "tool A" {
		t.Errorf("expected description 'tool A', got %v", fn["description"])
	}
}

func TestToolRegistry_ToProviderDefs(t *testing.T) {
	r := NewToolRegistry()
	params := map[string]any{"type": "object", "properties": map[string]any{}}
	r.Register(&mockRegistryTool{
		name:   "beta",
		desc:   "tool B",
		params: params,
		result: SilentResult("ok"),
	})

	defs := r.ToProviderDefs()
	if len(defs) != 1 {
		t.Fatalf("expected 1 provider def, got %d", len(defs))
	}

	want := providers.ToolDefinition{
		Type: "function",
		Function: providers.ToolFunctionDefinition{
			Name:        "beta",
			Description: "tool B",
			Parameters:  params,
		},
	}
	got := defs[0]
	if got.Type != want.Type {
		t.Errorf("Type: want %q, got %q", want.Type, got.Type)
	}
	if got.Function.Name != want.Function.Name {
		t.Errorf("Name: want %q, got %q", want.Function.Name, got.Function.Name)
	}
	if got.Function.Description != want.Function.Description {
		t.Errorf(
			"Description: want %q, got %q",
			want.Function.Description,
			got.Function.Description,
		)
	}
}

func TestToolRegistry_List(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("x", ""))
	r.Register(newMockTool("y", ""))

	names := r.List()
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}

	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["x"] || !nameSet["y"] {
		t.Errorf("expected names {x, y}, got %v", names)
	}
}

func TestToolRegistry_Count(t *testing.T) {
	r := NewToolRegistry()
	if r.Count() != 0 {
		t.Errorf("expected 0, got %d", r.Count())
	}

	r.Register(newMockTool("a", ""))
	r.Register(newMockTool("b", ""))
	if r.Count() != 2 {
		t.Errorf("expected 2, got %d", r.Count())
	}

	r.Register(newMockTool("a", "replaced"))
	if r.Count() != 2 {
		t.Errorf("expected 2 after overwrite, got %d", r.Count())
	}
}

func TestToolRegistry_GetSummaries(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("read_file", "Reads a file"))

	summaries := r.GetSummaries()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if !strings.Contains(summaries[0], "`read_file`") {
		t.Errorf("expected backtick-quoted name in summary, got %q", summaries[0])
	}
	if !strings.Contains(summaries[0], "Reads a file") {
		t.Errorf("expected description in summary, got %q", summaries[0])
	}
}

func TestToolToSchema(t *testing.T) {
	tool := newMockTool("demo", "demo tool")
	schema := ToolToSchema(tool)

	if schema["type"] != "function" {
		t.Errorf("expected type 'function', got %v", schema["type"])
	}
	fn, ok := schema["function"].(map[string]any)
	if !ok {
		t.Fatal("expected 'function' to be a map")
	}
	if fn["name"] != "demo" {
		t.Errorf("expected name 'demo', got %v", fn["name"])
	}
	if fn["description"] != "demo tool" {
		t.Errorf("expected description 'demo tool', got %v", fn["description"])
	}
	if fn["parameters"] == nil {
		t.Error("expected parameters to be set")
	}
}

func TestToolRegistry_ToProviderDefsAttachesPromptMetadata(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("native", "native tool"))
	r.Register(&mockPromptMetadataTool{
		mockRegistryTool: mockRegistryTool{
			name:   "mcp_demo",
			desc:   "mcp tool",
			params: map[string]any{"type": "object"},
		},
		metadata: PromptMetadata{
			Layer:  ToolPromptLayerCapability,
			Slot:   ToolPromptSlotMCP,
			Source: "mcp:demo",
		},
	})

	defs := r.ToProviderDefs()
	if len(defs) != 2 {
		t.Fatalf("ToProviderDefs() len = %d, want 2", len(defs))
	}

	byName := make(map[string]providers.ToolDefinition, len(defs))
	for _, def := range defs {
		byName[def.Function.Name] = def
	}

	native := byName["native"]
	if native.PromptLayer != ToolPromptLayerCapability ||
		native.PromptSlot != ToolPromptSlotTooling ||
		native.PromptSource != ToolPromptSourceRegistry {
		t.Fatalf("native prompt metadata = %#v, want default tooling source", native)
	}

	mcp := byName["mcp_demo"]
	if mcp.PromptLayer != ToolPromptLayerCapability ||
		mcp.PromptSlot != ToolPromptSlotMCP ||
		mcp.PromptSource != "mcp:demo" {
		t.Fatalf("mcp prompt metadata = %#v, want mcp source", mcp)
	}
}

func TestToolRegistry_Clone(t *testing.T) {
	r := NewToolRegistry()
	r.Register(newMockTool("read_file", "reads files"))
	r.Register(newMockTool("exec", "runs commands"))
	r.Register(newMockTool("web_search", "searches the web"))

	clone := r.Clone()

	// Clone should have the same tools
	if clone.Count() != 3 {
		t.Errorf("expected clone to have 3 tools, got %d", clone.Count())
	}
	for _, name := range []string{"read_file", "exec", "web_search"} {
		if _, ok := clone.Get(name); !ok {
			t.Errorf("expected clone to have tool %q", name)
		}
	}

	// Registering on parent should NOT affect clone
	r.Register(newMockTool("spawn", "spawns subagent"))
	if r.Count() != 4 {
		t.Errorf("expected parent to have 4 tools, got %d", r.Count())
	}
	if clone.Count() != 3 {
		t.Errorf(
			"expected clone to still have 3 tools after parent mutation, got %d",
			clone.Count(),
		)
	}
	if _, ok := clone.Get("spawn"); ok {
		t.Error("expected clone NOT to have 'spawn' tool registered on parent after cloning")
	}

	// Registering on clone should NOT affect parent
	clone.Register(newMockTool("custom", "custom tool"))
	if clone.Count() != 4 {
		t.Errorf("expected clone to have 4 tools, got %d", clone.Count())
	}
	if _, ok := r.Get("custom"); ok {
		t.Error("expected parent NOT to have 'custom' tool registered on clone")
	}
}

func TestToolRegistry_Clone_Empty(t *testing.T) {
	r := NewToolRegistry()
	clone := r.Clone()
	if clone.Count() != 0 {
		t.Errorf("expected empty clone, got count %d", clone.Count())
	}
}

func TestToolRegistry_Clone_PreservesHiddenToolState(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterHidden(newMockTool("mcp_tool", "dynamic MCP tool"))

	clone := r.Clone()

	// Hidden tools with TTL=0 should not be gettable (same behavior as parent)
	if _, ok := clone.Get("mcp_tool"); ok {
		t.Error("expected hidden tool with TTL=0 to be invisible in clone")
	}

	// But the entry should exist (count includes hidden tools)
	if clone.Count() != 1 {
		t.Errorf("expected clone count 1 (hidden entry exists), got %d", clone.Count())
	}
}

func TestToolRegistry_Clone_PreservesTTLValue(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterHidden(newMockTool("ttl_tool", "tool with TTL"))

	// Manually set a non-zero TTL on the entry
	r.mu.RLock()
	if entry, ok := r.tools["ttl_tool"]; ok {
		entry.TTL = 5
	}
	r.mu.RUnlock()

	clone := r.Clone()

	// Verify TTL value is preserved in the clone
	clone.mu.RLock()
	defer clone.mu.RUnlock()
	entry, ok := clone.tools["ttl_tool"]
	if !ok {
		t.Fatal("expected ttl_tool to exist in clone")
	}
	if entry.TTL != 5 {
		t.Errorf("expected TTL=5 in clone, got %d", entry.TTL)
	}
}

func TestToolRegistry_ConcurrentAccess(t *testing.T) {
	r := NewToolRegistry()
	var wg sync.WaitGroup

	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			name := string(rune('A' + n%26))
			r.Register(newMockTool(name, "concurrent"))
			r.Get(name)
			r.Count()
			r.List()
			r.GetDefinitions()
		}(i)
	}

	wg.Wait()

	if r.Count() == 0 {
		t.Error("expected tools to be registered after concurrent access")
	}
}

// --- Panic and abnormal exit tests ---

// mockPanicTool is a tool that panics during execution
type mockPanicTool struct {
	name       string
	panicValue any
}

func (m *mockPanicTool) Name() string               { return m.name }
func (m *mockPanicTool) Description() string        { return "a tool that panics" }
func (m *mockPanicTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (m *mockPanicTool) Execute(_ context.Context, _ map[string]any) *ToolResult {
	panic(m.panicValue)
}

// mockNilResultTool is a tool that returns nil
type mockNilResultTool struct {
	name string
}

func (m *mockNilResultTool) Name() string               { return m.name }
func (m *mockNilResultTool) Description() string        { return "a tool that returns nil" }
func (m *mockNilResultTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (m *mockNilResultTool) Execute(_ context.Context, _ map[string]any) *ToolResult {
	return nil
}

func TestToolRegistry_Execute_PanicRecovery(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockPanicTool{
		name:       "panic_tool",
		panicValue: "something went terribly wrong",
	})

	// Should not panic, should return error result
	result := r.Execute(context.Background(), "panic_tool", nil)

	if result == nil {
		t.Fatal("expected non-nil result after panic recovery")
	}
	if !result.IsError {
		t.Error("expected IsError=true after panic")
	}
	if !strings.Contains(result.ForLLM, "panic") {
		t.Errorf("expected 'panic' in error message, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "panic_tool") {
		t.Errorf("expected tool name in error message, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "something went terribly wrong") {
		t.Errorf("expected panic value in error message, got %q", result.ForLLM)
	}
	if result.Err == nil {
		t.Error("expected Err to be set")
	}
}

func TestToolRegistry_Execute_PanicRecovery_ErrorType(t *testing.T) {
	r := NewToolRegistry()

	// Test with error type panic
	r.Register(&mockPanicTool{
		name:       "error_panic_tool",
		panicValue: errors.New("custom error panic"),
	})

	result := r.Execute(context.Background(), "error_panic_tool", nil)

	if !result.IsError {
		t.Error("expected IsError=true")
	}
	if !strings.Contains(result.ForLLM, "custom error panic") {
		t.Errorf("expected error message in ForLLM, got %q", result.ForLLM)
	}
}

func TestToolRegistry_Execute_PanicRecovery_IntType(t *testing.T) {
	r := NewToolRegistry()

	// Test with int type panic
	r.Register(&mockPanicTool{
		name:       "int_panic_tool",
		panicValue: 42,
	})

	result := r.Execute(context.Background(), "int_panic_tool", nil)

	if !result.IsError {
		t.Error("expected IsError=true")
	}
	if !strings.Contains(result.ForLLM, "42") {
		t.Errorf("expected panic value '42' in ForLLM, got %q", result.ForLLM)
	}
}

func TestToolRegistry_Execute_NilResultHandling(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockNilResultTool{name: "nil_tool"})

	result := r.Execute(context.Background(), "nil_tool", nil)

	if result == nil {
		t.Fatal("expected non-nil result when tool returns nil")
	}
	if !result.IsError {
		t.Error("expected IsError=true for nil result")
	}
	if !strings.Contains(result.ForLLM, "nil_tool") {
		t.Errorf("expected tool name in error message, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "nil result") {
		t.Errorf("expected 'nil result' in error message, got %q", result.ForLLM)
	}
	if result.Err == nil {
		t.Error("expected Err to be set")
	}
}

func TestToolRegistry_ExecuteWithContext_PanicRecovery(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockPanicTool{
		name:       "ctx_panic_tool",
		panicValue: "context panic test",
	})

	// Should not panic even with context
	result := r.ExecuteWithContext(
		context.Background(),
		"ctx_panic_tool",
		map[string]any{"key": "value"},
		"telegram",
		"chat-123",
		nil,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.IsError {
		t.Error("expected IsError=true")
	}
	if !strings.Contains(result.ForLLM, "context panic test") {
		t.Errorf("expected panic message, got %q", result.ForLLM)
	}
}

func TestToolRegistry_Execute_PanicDoesNotAffectOtherTools(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockPanicTool{name: "bad_tool", panicValue: "boom"})
	r.Register(&mockRegistryTool{
		name:   "good_tool",
		desc:   "works fine",
		params: map[string]any{},
		result: SilentResult("success"),
	})

	// First, trigger the panic
	result1 := r.Execute(context.Background(), "bad_tool", nil)
	if !result1.IsError {
		t.Error("expected error from panic tool")
	}

	// Then, verify the good tool still works
	result2 := r.Execute(context.Background(), "good_tool", nil)
	if result2.IsError {
		t.Errorf("expected success from good tool, got error: %s", result2.ForLLM)
	}
	// Phase 3 (tool-knowledge-...-20260718): first success in anon session
	// now appends SoftPromptFirstSuccess. The body itself is preserved.
	want := "success" + SoftPromptFirstSuccess
	if result2.ForLLM != want {
		t.Errorf("expected %q, got %q", want, result2.ForLLM)
	}
}

func TestToolRegistry_SetMediaStore_PropagatesToExistingAndNewTools(t *testing.T) {
	r := NewToolRegistry()
	store := media.NewFileMediaStore()

	existing := &mockMediaStoreAwareTool{
		mockRegistryTool: *newMockTool("existing", "existing tool"),
	}
	r.Register(existing)

	r.SetMediaStore(store)
	if existing.store != store {
		t.Fatal("expected existing tool to receive media store")
	}

	later := &mockMediaStoreAwareTool{
		mockRegistryTool: *newMockTool("later", "later tool"),
	}
	r.Register(later)

	if later.store != store {
		t.Fatal("expected newly registered tool to inherit media store")
	}
}

func TestToolRegistry_ExecuteWithContext_SanitizesLargeBase64Payload(t *testing.T) {
	r := NewToolRegistry()
	payload := strings.Repeat("QUJD", 400)
	r.Register(&mockRegistryTool{
		name:   "base64_tool",
		desc:   "returns huge base64",
		params: map[string]any{},
		result: SilentResult(payload),
	})

	result := r.ExecuteWithContext(
		context.Background(),
		"base64_tool",
		nil,
		"telegram",
		"chat-1",
		nil,
	)

	// Phase 3 (tool-knowledge-...-20260718): successful execution in a
	// named session appends SoftPromptFirstSuccess once per turn. The
	// sanitization still strips the payload — only the suffix is added.
	want := largeBase64OmittedMessage + SoftPromptFirstSuccess
	if result.ForLLM != want {
		t.Fatalf("expected %q, got %q", want, result.ForLLM)
	}
}

func TestToolRegistry_ExecuteWithContext_ExtractsInlineMediaDataURL(t *testing.T) {
	r := NewToolRegistry()
	store := media.NewFileMediaStore()
	r.SetMediaStore(store)

	payload := "![screenshot](data:image/png;base64,aGVsbG8=)"
	r.Register(&mockRegistryTool{
		name:   "inline_media_tool",
		desc:   "returns inline data url",
		params: map[string]any{},
		result: SilentResult(payload),
	})

	result := r.ExecuteWithContext(
		context.Background(),
		"inline_media_tool",
		nil,
		"telegram",
		"chat-42",
		nil,
	)

	if len(result.Media) != 1 {
		t.Fatalf("expected 1 media ref, got %d", len(result.Media))
	}
	if strings.Contains(result.ForLLM, "data:image/png;base64") {
		t.Fatalf("expected inline data URL to be stripped from ForLLM, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "registered as a media attachment") {
		t.Fatalf("expected delivery note in ForLLM, got %q", result.ForLLM)
	}

	path, err := store.Resolve(result.Media[0])
	if err != nil {
		t.Fatalf("expected stored media ref to resolve: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected stored media file to exist: %v", err)
	}
	if filepath.Ext(path) != ".png" {
		t.Fatalf("expected stored inline media to use png extension, got %q", path)
	}
}

func TestToolRegistry_ExecuteWithContext_SanitizesInlineMediaWithoutStore(t *testing.T) {
	r := NewToolRegistry()

	payload := "before ![img](data:image/png;base64,aGVsbG8=) after"
	r.Register(&mockRegistryTool{
		name:   "inline_media_no_store",
		desc:   "returns inline data url without store",
		params: map[string]any{},
		result: SilentResult(payload),
	})

	result := r.ExecuteWithContext(
		context.Background(),
		"inline_media_no_store",
		nil,
		"telegram",
		"chat-42",
		nil,
	)

	if strings.Contains(result.ForLLM, "data:image/png;base64") {
		t.Fatalf("expected inline data URL to be removed from ForLLM, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, inlineMediaOmittedMessage) {
		t.Fatalf("expected inline media omission note, got %q", result.ForLLM)
	}
}

// Tests for ToolRegistry.OpenTools() added in plan
// exception-handling-recovery-pattern-gap-closure-20260628 (Task B0).
//
// OpenTools() is the read-side aggregation that surfaces "which tools
// have an open circuit breaker right now?" to the ToolHealthContributor.
// It deduplicates across (channel, chatID) sessions and sorts oldest-
// opened first so the LLM sees the longest outage at the top.

// --- 1. Empty / healthy paths ---

func TestToolRegistry_OpenTools_EmptyRegistryReturnsEmptySlice(t *testing.T) {
	r := NewToolRegistry()
	got := r.OpenTools()
	if len(got) != 0 {
		t.Fatalf("OpenTools() on empty registry = %v, want empty slice", got)
	}
}

func TestToolRegistry_OpenTools_NoBreakersAfterSuccessfulExecution(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockRegistryTool{
		name:   "ok_tool",
		desc:   "always succeeds",
		params: map[string]any{},
		result: okResult(),
	})

	// Execute successfully — this allocates a breaker but leaves it Closed.
	res := r.ExecuteWithContext(context.Background(), "ok_tool", nil, "telegram", "111", nil)
	if res == nil || res.IsError {
		t.Fatalf("setup: ok_tool should succeed, got %+v", res)
	}

	got := r.OpenTools()
	if len(got) != 0 {
		t.Fatalf("OpenTools() after success = %v, want empty (breaker stays Closed)", got)
	}
}

func TestToolRegistry_OpenTools_OnlyReturnsOpenOnes(t *testing.T) {
	r := NewToolRegistry()
	// One tool that always fails (will trip its breaker), one that always succeeds.
	r.Register(&mockRegistryTool{
		name:   "flaky",
		desc:   "always fails",
		params: map[string]any{},
		result: failResult("boom"),
	})
	r.Register(&mockRegistryTool{
		name:   "ok_tool",
		desc:   "always succeeds",
		params: map[string]any{},
		result: okResult(),
	})

	// Trip the "flaky" breaker past threshold via the public execute path.
	for i := 0; i < 3; i++ {
		_ = r.ExecuteWithContext(context.Background(), "flaky", nil, "telegram", "111", nil)
	}
	// Exercise the healthy tool so its breaker exists but stays Closed.
	_ = r.ExecuteWithContext(context.Background(), "ok_tool", nil, "telegram", "111", nil)

	got := r.OpenTools()
	if len(got) != 1 {
		t.Fatalf("OpenTools() = %d entries, want 1 (only flaky should appear)", len(got))
	}
	if got[0].Name != "flaky" {
		t.Fatalf("OpenTools()[0].Name = %q, want %q", got[0].Name, "flaky")
	}
	if got[0].Failures < 3 {
		t.Fatalf("OpenTools()[0].Failures = %d, want >= 3", got[0].Failures)
	}
	if got[0].OpenedAt.IsZero() {
		t.Fatal("OpenTools()[0].OpenedAt must be set")
	}
}

// --- 2. Cross-session deduplication ---

func TestToolRegistry_OpenTools_DeduplicatesAcrossSessions(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockRegistryTool{
		name:   "flaky",
		desc:   "always fails",
		params: map[string]any{},
		result: failResult("boom"),
	})

	// Trip the breaker in session A.
	for i := 0; i < 3; i++ {
		_ = r.ExecuteWithContext(context.Background(), "flaky", nil, "telegram", "111", nil)
	}
	// Trip the breaker in session B (independent breaker, same tool name).
	for i := 0; i < 3; i++ {
		_ = r.ExecuteWithContext(context.Background(), "flaky", nil, "telegram", "222", nil)
	}

	got := r.OpenTools()
	if len(got) != 1 {
		t.Fatalf("OpenTools() = %d entries, want 1 (same tool deduped across sessions)", len(got))
	}
	if got[0].Name != "flaky" {
		t.Fatalf("OpenTools()[0].Name = %q, want %q", got[0].Name, "flaky")
	}
}

// --- 3. Sort order ---

func TestToolRegistry_OpenTools_SortsByOpenedAtOldestFirst(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockRegistryTool{
		name:   "first",
		desc:   "always fails",
		params: map[string]any{},
		result: failResult("boom"),
	})
	r.Register(&mockRegistryTool{
		name:   "second",
		desc:   "always fails",
		params: map[string]any{},
		result: failResult("boom"),
	})

	// Trip "first" first, then sleep enough that openedAt timestamps differ
	// at sub-millisecond resolution (Linux time.Now() is typically monotonic).
	for i := 0; i < 3; i++ {
		_ = r.ExecuteWithContext(context.Background(), "first", nil, "telegram", "111", nil)
	}
	time.Sleep(2 * time.Millisecond)
	for i := 0; i < 3; i++ {
		_ = r.ExecuteWithContext(context.Background(), "second", nil, "telegram", "111", nil)
	}

	got := r.OpenTools()
	if len(got) != 2 {
		t.Fatalf("OpenTools() = %d entries, want 2", len(got))
	}
	if got[0].Name != "first" || got[1].Name != "second" {
		t.Fatalf("OpenTools() sort order = [%q, %q], want [first, second]",
			got[0].Name, got[1].Name)
	}
	// Also verify the underlying slice is actually sorted (catches regressions
	// where someone removes the sort.Slice call).
	if !sort.SliceIsSorted(got, func(i, j int) bool {
		return got[i].OpenedAt.Before(got[j].OpenedAt)
	}) {
		t.Fatalf("OpenTools() result not sorted by OpenedAt: %+v", got)
	}
}

// --- 4. HalfOpen breakers are excluded ---

func TestToolRegistry_OpenTools_ExcludesHalfOpenBreakers(t *testing.T) {
	r := NewToolRegistry()
	cb := r.getCircuitBreaker("telegram", "111", "web_search")
	tripBreaker(t, cb, "web_search")

	// Force the breaker into HalfOpen state by making openedAt ancient.
	cb.mu.Lock()
	cb.openedAt = time.Now().Add(-2 * cb.recoveryTimeout)
	cb.mu.Unlock()
	if !cb.Allow() {
		t.Fatal("setup: breaker should transition to HalfOpen after timeout")
	}

	got := r.OpenTools()
	if len(got) != 0 {
		t.Fatalf("OpenTools() with HalfOpen breaker = %+v, want empty (only Open counts)", got)
	}
}

// --- 5. Unnamed breakers are ignored ---

func TestToolRegistry_OpenTools_IgnoresUnnamedBreakers(t *testing.T) {
	r := NewToolRegistry()

	// Hand-craft a breaker that has no name (legacy shape), open it.
	anon := NewCircuitBreaker()
	for i := 0; i < 3; i++ {
		anon.RecordResult("legacy", true, ErrTransient)
	}
	if anon.Name() != "" {
		t.Fatal("setup: anon breaker should have empty name")
	}
	if anon.Allow() {
		t.Fatal("setup: anon breaker should be Open after 3 failures")
	}
	r.cbMu.Lock()
	r.breakers["legacy"] = anon
	r.cbMu.Unlock()

	// Also add a named, Open breaker — must still appear.
	named := r.getCircuitBreaker("telegram", "111", "web_search")
	tripBreaker(t, named, "web_search")

	got := r.OpenTools()
	if len(got) != 1 {
		t.Fatalf("OpenTools() = %d entries, want 1 (anon should be filtered out)", len(got))
	}
	if got[0].Name != "web_search" {
		t.Fatalf("OpenTools()[0].Name = %q, want %q", got[0].Name, "web_search")
	}
}

// TestToolRegistry_SetAllowlistFiltersToProviderDefs covers the Phase 11 writer-
// without-reader bug fix: ToProviderDefs must honour the runtime allowlist the
// same way Register-time filtering does. Previously the allowlist was stored
// (SetAllowlist) but ignored at projection time, so iter-1 forced-funnel
// ([set_goal] only) leaked the full tool list to the LLM. Regression-guard for
// the 2026-07-23 incident where a Telegram turn with no active goal saw 85
// tools at iter 1 instead of 1.
func TestToolRegistry_SetAllowlistFiltersToProviderDefs(t *testing.T) {
	r := NewToolRegistry()
	r.SetAllowlist([]string{"set_goal"})

	r.Register(newMockTool("set_goal", "set the active goal"))
	r.Register(newMockTool("read_file", "read a file"))
	r.Register(newMockTool("mcp_signet_memory_search", "memory search"))

	defs := r.ToProviderDefs()
	got := make([]string, 0, len(defs))
	for _, d := range defs {
		got = append(got, d.Function.Name)
	}

	if len(got) != 1 {
		t.Fatalf("ToProviderDefs() returned %d tool(s), want 1; got=%v", len(got), got)
	}
	if got[0] != "set_goal" {
		t.Fatalf("ToProviderDefs()[0].Function.Name = %q, want %q", got[0], "set_goal")
	}

	// Empty allowlist = no filter (Phase 11 fail-open semantics).
	r.SetAllowlist(nil)
	r2 := NewToolRegistry()
	r2.Register(newMockTool("set_goal", "set the active goal"))
	r2.Register(newMockTool("read_file", "read a file"))

	if got := r2.ToProviderDefs(); len(got) != 2 {
		t.Fatalf("ToProviderDefs() with nil allowlist returned %d, want 2", len(got))
	}
}

// TestToolRegistry_SetAllowlistFiltersToProviderDefs_DiscoveryBypass verifies
// that the discovery tools (BM25SearchToolName, RegexSearchToolName) still
// bypass the allowlist even after the fix, matching the Register-time
// semantics in TestToolRegistry_AllowlistStillAllowsDiscoveryTools.
func TestToolRegistry_SetAllowlistFiltersToProviderDefs_DiscoveryBypass(t *testing.T) {
	r := NewToolRegistry()
	r.SetAllowlist([]string{"some_real_tool"})

	r.Register(newMockTool(BM25SearchToolName, "discover hidden tools"))
	r.Register(newMockTool(RegexSearchToolName, "discover hidden tools via regex"))
	r.Register(newMockTool("some_real_tool", "real"))

	defs := r.ToProviderDefs()
	got := make([]string, 0, len(defs))
	for _, d := range defs {
		got = append(got, d.Function.Name)
	}

	// Expect 3: 2 discovery tools (bypass) + 1 allowed real tool.
	want := map[string]bool{BM25SearchToolName: true, RegexSearchToolName: true, "some_real_tool": true}
	if len(got) != len(want) {
		t.Fatalf("ToProviderDefs() = %v (len %d), want exactly %v entries", got, len(got), want)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("ToProviderDefs() leaked %q; want only the 3 allowlisted/discovery tools", n)
		}
	}
}

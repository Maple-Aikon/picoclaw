package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// Contract tests for the action rename of 2026-10-20.
//
// Before: tool_knowledge had action="save", which overwrote the whole entry.
// Measured damage: an entry that had just been recovered was destroyed by a
// routine save-shaped write. The destructive action is now split:
//
//	replace - overwrites the entry (explicit destruction, opt-in)
//	append  - adds to the entry, preserving every byte already there
//
// action="save" must now be REJECTED with a message naming both new actions,
// so a stale caller gets a migration hint instead of silently overwriting.

func knowledgeStoreForTest(t *testing.T) *ToolKnowledgeStore {
	t.Helper()
	s, err := NewToolKnowledgeStore(tempWorkspace(t), "")
	if err != nil {
		t.Fatalf("NewToolKnowledgeStore: %v", err)
	}
	return s
}

func knowledgePathForTest(t *testing.T, s *ToolKnowledgeStore, tool string) string {
	t.Helper()
	p, err := s.PathFor(tool)
	if err != nil {
		t.Fatalf("PathFor(%q): %v", tool, err)
	}
	return p
}

// TestToolKnowledge_SaveActionIsRejected is the migration guard: the old
// destructive action must not silently do anything at all.
func TestToolKnowledge_SaveActionIsRejected(t *testing.T) {
	s := knowledgeStoreForTest(t)
	tool := NewToolKnowledgeTool(s)

	res := tool.Execute(nil, map[string]any{
		"action": "save",
		"tool":   "fs.read",
		"body":   "a lesson that must never land",
	})
	if res == nil {
		t.Fatal("Execute returned nil for action=save")
	}
	if !res.IsError {
		t.Fatal("action=save was accepted; it must be rejected")
	}
	if res.ErrKind != toolshared.ErrInvalidInput {
		t.Errorf("rejection ErrKind = %v, want ErrInvalidInput", res.ErrKind)
	}
	if !strings.Contains(res.ForLLM, "replace") || !strings.Contains(res.ForLLM, "append") {
		t.Errorf("rejection message must name both replace and append; got: %q", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "unknown action") {
		t.Errorf("rejection must be an explicit migration hint, not a generic unknown-action error; got: %q", res.ForLLM)
	}

	// Nothing may have been written.
	if _, err := s.Read("fs.read"); err == nil {
		t.Error("action=save created a file; a rejected action must be a no-op")
	}
}

// TestToolKnowledge_ActionEnumNoLongerAdvertisesSave pins the schema: the enum
// is what the LLM sees, so a stale "save" there is what causes the destructive
// call in the first place.
func TestToolKnowledge_ActionEnumNoLongerAdvertisesSave(t *testing.T) {
	tool := NewToolKnowledgeTool(knowledgeStoreForTest(t))

	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters() has no properties map: %T", params["properties"])
	}
	actionProp, ok := props["action"].(map[string]any)
	if !ok {
		t.Fatalf("no action property: %T", props["action"])
	}
	enum, ok := actionProp["enum"].([]string)
	if !ok {
		t.Fatalf("action has no enum: %T", actionProp["enum"])
	}
	for _, e := range enum {
		if e == "save" {
			t.Errorf("action enum still advertises save: %v", enum)
		}
	}
	for _, want := range []string{"replace", "append", "read", "list", "delete"} {
		found := false
		for _, e := range enum {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("action enum missing %q: %v", want, enum)
		}
	}

	desc := tool.Description()
	if strings.Contains(desc, "action=save") {
		t.Errorf("Description() still advertises action=save: %q", desc)
	}
	if !strings.Contains(desc, "action=replace") || !strings.Contains(desc, "action=append") {
		t.Errorf("Description() must advertise both action=replace and action=append: %q", desc)
	}
}

func TestToolKnowledge_SchemaTeachesRuntimeNames(t *testing.T) {
	// The `tool` parameter description is the ONLY place the LLM learns which
	// name to key on. Before 2026-10-21 it taught 'fs.read' -- a dotted name
	// matching no runtime tool -- and never mentioned the MCP namespace, so
	// entries were filed under keys escalation can never load.
	tool := NewToolKnowledgeTool(knowledgeStoreForTest(t))

	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters() has no properties map: %T", params["properties"])
	}
	toolProp, ok := props["tool"].(map[string]any)
	if !ok {
		t.Fatalf("no tool property: %T", props["tool"])
	}
	desc, ok := toolProp["description"].(string)
	if !ok {
		t.Fatalf("tool description is %T, want string", toolProp["description"])
	}

	// Stale examples must be gone.
	if strings.Contains(desc, "fs.read") || strings.Contains(desc, "web.fetch") {
		t.Errorf("tool description still teaches the unmatchable 'fs.read'/'web.fetch' "+
			"examples: %q", desc)
	}
	// The real namespace shape must be present.
	for _, want := range []string{
		"mcp_<server>_<tool>",
		"mcp_janus-graph_search_memory",
		"mcp_context-mode_ctx_execute",
		"exec",
		"RULE 1",
		"RULE 5",
		"never loads",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("tool description missing %q", want)
		}
	}

	// Description() must warn that matching is exact. Assert on the words the
	// description actually carries -- an earlier version of this test needle'd
	// "byte-identical", which lives in the parameter description, not here.
	if d := tool.Description(); !strings.Contains(d, "EXACTLY") || !strings.Contains(d, "never loads") {
		t.Errorf("Description() must warn that key matching is exact: %q", d)
	}
}

// The LLM also learns names from RUNTIME error messages -- a missing-`tool`
// error is exactly where it picks the name to retry with. On 2026-10-21 the
// parameter description was fixed but action=read's error message still said
// "tool='fs.read'", so the stale name kept shipping to the model on every
// malformed call. This test walks every runtime path that can echo a tool
// name and forbids the unmatchable examples in ALL of them.
func TestToolKnowledge_NoRuntimePathTeachesStaleNames(t *testing.T) {
	stale := []string{"fs.read", "web.fetch", "shell.run"}
	tool := NewToolKnowledgeTool(knowledgeStoreForTest(t))

	messages := map[string]string{
		"Description()": tool.Description(),
	}

	// Every action that can be called with a missing/invalid `tool`.
	for _, action := range []string{"replace", "append", "read"} {
		res := tool.Execute(context.Background(), map[string]any{"action": action})
		if res == nil {
			t.Fatalf("action=%s returned nil result", action)
		}
		messages["action="+action+"(no tool)"] = res.ForLLM
	}

	for label, msg := range messages {
		for _, bad := range stale {
			if strings.Contains(msg, bad) {
				t.Errorf("%s teaches the unmatchable name %q: %q", label, bad, msg)
			}
		}
	}
}

// Near-miss names are DISTINCT entries, not variants of one. This is the
// property that makes separator-guessing fail, so it is pinned here: if a
// future change makes sanitizeToolName collapse separators, this test fails
// and the naming spec has to be rewritten rather than silently contradicted.
func TestToolKnowledge_NearMissNamesStayDistinct(t *testing.T) {
	s := knowledgeStoreForTest(t)
	const a, b = "ctx_execute_run", "ctx-execute-run"

	if _, _, err := s.Replace(a, "BODY-UNDERSCORE"); err != nil {
		t.Fatalf("Replace(%q): %v", a, err)
	}
	if _, _, err := s.Replace(b, "BODY-HYPHEN"); err != nil {
		t.Fatalf("Replace(%q): %v", b, err)
	}

	ga, err := s.Read(a)
	if err != nil {
		t.Fatalf("Read(%q): %v", a, err)
	}
	gb, err := s.Read(b)
	if err != nil {
		t.Fatalf("Read(%q): %v", b, err)
	}
	if !strings.Contains(ga, "BODY-UNDERSCORE") {
		t.Errorf("read(%q) = %q, want BODY-UNDERSCORE", a, ga)
	}
	if !strings.Contains(gb, "BODY-HYPHEN") {
		t.Errorf("read(%q) = %q, want BODY-HYPHEN", b, gb)
	}
	if pa, pb := knowledgePathForTest(t, s, a), knowledgePathForTest(t, s, b); pa == pb {
		t.Errorf("near-miss names share one path %q", pa)
	}
}

// TestToolKnowledge_AppendPreservesExistingBytes is the core safety property:
// append must not touch a single byte that was already stored.
func TestToolKnowledge_AppendPreservesExistingBytes(t *testing.T) {
	s := knowledgeStoreForTest(t)
	path := knowledgePathForTest(t, s, "fs.read")

	const first = `## What goes wrong
FIRST-BODY-MARKER line one`
	const second = `## What works instead
SECOND-BODY-MARKER line two`

	if _, _, err := s.Replace("fs.read", first); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before append: %v", err)
	}

	if _, _, err := s.Append("fs.read", second); err != nil {
		t.Fatalf("Append: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after append: %v", err)
	}

	if !strings.HasPrefix(string(after), string(before)) {
		t.Errorf("append changed the bytes already on disk\nbefore: %q\nafter:  %q", before, after)
	}
	if !strings.Contains(string(after), "FIRST-BODY-MARKER") {
		t.Error("append dropped the original body")
	}
	if !strings.Contains(string(after), "SECOND-BODY-MARKER") {
		t.Error("append did not add the new body")
	}
}

// TestToolKnowledge_AppendAboveCapKeepsBothHalves proves append is not
// truncated by the 2 KB prompt cap - that cap is a read-path concern only.
func TestToolKnowledge_AppendAboveCapKeepsBothHalves(t *testing.T) {
	s := knowledgeStoreForTest(t)

	head := "HEAD-MARKER " + strings.Repeat("filler line to exceed the cap. ", 120)
	tail := "TAIL-MARKER " + strings.Repeat("more filler after the cap. ", 120)

	if _, size, err := s.Replace("fs.read", head); err != nil {
		t.Fatalf("Replace: %v", err)
	} else if size <= KnowledgeSizeCap {
		t.Fatalf("head is not above the cap: size=%d cap=%d", size, KnowledgeSizeCap)
	}
	if _, size, err := s.Append("fs.read", tail); err != nil {
		t.Fatalf("Append: %v", err)
	} else if size <= KnowledgeSizeCap {
		t.Fatalf("appended file is not above the cap: size=%d cap=%d", size, KnowledgeSizeCap)
	}

	raw, err := os.ReadFile(knowledgePathForTest(t, s, "fs.read"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "HEAD-MARKER") {
		t.Error("append lost the head half")
	}
	if !strings.Contains(string(raw), "TAIL-MARKER") {
		t.Error("append lost the tail half")
	}
	if strings.Contains(string(raw), "... [truncated]") {
		t.Error("append wrote a truncation marker to disk")
	}
}

// TestToolKnowledge_AppendCreatesMissingEntry covers the bootstrap case: an
// append onto a non-existent entry creates it, because there is nothing to
// destroy.
func TestToolKnowledge_AppendCreatesMissingEntry(t *testing.T) {
	s := knowledgeStoreForTest(t)

	if _, _, err := s.Append("new.tool", "first ever body"); err != nil {
		t.Fatalf("Append on missing entry: %v", err)
	}
	got, err := s.Read("new.tool")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(got, "first ever body") {
		t.Errorf("Append did not create the entry; got %q", got)
	}
}

// TestToolKnowledge_ReplaceOverwritesFully pins the destructive path: replace
// must actually replace, not merge.
func TestToolKnowledge_ReplaceOverwritesFully(t *testing.T) {
	s := knowledgeStoreForTest(t)

	if _, _, err := s.Replace("fs.read", "OLD-BODY that must disappear"); err != nil {
		t.Fatalf("Replace 1: %v", err)
	}
	if _, _, err := s.Replace("fs.read", "NEW-BODY only"); err != nil {
		t.Fatalf("Replace 2: %v", err)
	}
	got, err := s.Read("fs.read")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if strings.Contains(got, "OLD-BODY") {
		t.Errorf("replace kept the old body: %q", got)
	}
	if !strings.Contains(got, "NEW-BODY") {
		t.Errorf("replace lost the new body: %q", got)
	}
}

// TestToolKnowledge_ReplaceAboveCapIsNotTruncated is the regression guard for
// the 2026-10-20 truncation bug, now under the new action name.
func TestToolKnowledge_ReplaceAboveCapIsNotTruncated(t *testing.T) {
	s := knowledgeStoreForTest(t)

	body := "## What goes wrong\n" + strings.Repeat("SENTINEL-RULE payload text. ", 120)
	wantSent := strings.Count(body, "SENTINEL-RULE")

	path, size, err := s.Replace("fs.read", body)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if size <= KnowledgeSizeCap {
		t.Fatalf("body is not above the cap: size=%d cap=%d", size, KnowledgeSizeCap)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.Count(string(raw), "SENTINEL-RULE"); got != wantSent {
		t.Errorf("replace lost body: sentinels in=%d out=%d (lost %d)", wantSent, got, wantSent-got)
	}
	if strings.Contains(string(raw), "... [truncated]") {
		t.Error("replace wrote a truncation marker to disk")
	}
}

// TestToolKnowledge_ReadReturnsFullEntry proves the read path is not serving
// a 2 KB-truncated view to the caller.
func TestToolKnowledge_ReadReturnsFullEntry(t *testing.T) {
	s := knowledgeStoreForTest(t)

	body := "HEAD " + strings.Repeat("payload to push past the cap. ", 120) + " TAIL"
	if _, _, err := s.Replace("fs.read", body); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := s.Read("fs.read")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(got, "TAIL") {
		t.Error("Read returned a truncated view; the tail is missing")
	}
	if len(got) <= KnowledgeSizeCap {
		t.Errorf("Read returned %d bytes, expected the full body above the %d cap", len(got), KnowledgeSizeCap)
	}
}

// TestToolKnowledge_DeleteStillRemoves keeps the delete action working after
// the rename.
func TestToolKnowledge_DeleteStillRemoves(t *testing.T) {
	s := knowledgeStoreForTest(t)

	if _, _, err := s.Replace("fs.read", "lesson"); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if err := s.Delete("fs.read"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Read("fs.read"); err == nil {
		t.Error("delete did not remove the entry")
	}
	// Idempotent.
	if err := s.Delete("fs.read"); err != nil {
		t.Errorf("second delete should be a no-op, got %v", err)
	}
}

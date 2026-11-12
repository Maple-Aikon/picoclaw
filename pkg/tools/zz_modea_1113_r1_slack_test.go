package tools

// zz_modea_1113_r1_slack_test.go — Mode A 2026-11-13: per-R1 slack.
//
// WHY A NEW FILE
//
// zz_modea_1112_slack_test.go reports the tightest entries of the WHOLE
// store. On 2026-11-13 that ranking was topped by TOPIC entries
// (memory-optimization.mode-a slack=68). A TOPIC entry is SEARCH-ONLY:
// LoadForEscalation is keyed by the runtime tool name, so a topic
// entry never passes through digestSections at runtime and its slack
// cannot starve anything.
//
// So the tightest TOPIC entry is a MISLEADING headline. The entries
// whose slack actually decides whether the model sees a lesson are the
// R1 ones (stem == a runtime tool name). This file ranks THOSE.
//
// MEASURED 2026-11-13 (live store): the tightest R1 entry is `exec`
// (slack=188). Everything else has room to spare.
//
// ESCAPE DISCIPLINE: no escape sequences inside string or rune
// literals - a literal backslash-n is decoded by the write tool into a
// REAL newline and breaks the source (measured 2026-11-13).

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// zzModea1113R1Names is the AUTHORITATIVE R1 set for the live store.
//
// WHY NOT "no dot in the stem"
// The dot rule is a HEURISTIC. Measured 2026-11-13: it over-counts by
// 3 on the live store:
//
//	go-dead-code-proof                (TOPIC - no such runtime tool)
//	mcp_context-mode_ctx_execute      (TOPIC - the context-mode MCP
//	                                   server is not connected here)
//	mcp_context-mode_ctx_execute_file (TOPIC - same)
//
// MEMORY.md already warned: "khong co dot != R1".
//
// THE ACTUAL RULE (production):
//
//	pkg/agent/recovery_goal.go:562 toolNameFromMessages resolves the
//	runtime tool name from the assistant message's ToolCalls[].Name;
//	pkg/tools/tool_knowledge.go:368 LoadForEscalation(toolName) then
//	reads <stem>.md. => an entry auto-loads iff its stem EQUALS a
//	runtime tool name.
//
// The list below is the native tool Name() methods (read from the
// source on 2026-11-13) plus the MCP names in the
// mcp_<sanitized server>_<sanitized tool> form produced by
// pkg/tools/integration/mcp_tool.go:147.
//
// ONLY the entries with an on-disk file matter here; the rest are
// runtime names that simply have no entry yet.
var zzModea1113R1Names = map[string]bool{
	// native
	"append_file": true, "cron": true, "edit_file": true, "exec": true,
	"read_file": true, "set_goal": true, "spawn": true, "subagent": true,
	"tool_knowledge": true, "tool_search_tool_bm25": true, "write_file": true,
	// mcp
	"mcp_code-review-graph_get_knowledge_gaps_tool": true,
	"mcp_codebase-memory_delete_project":            true,
	"mcp_codebase-memory_query_graph":               true,
	"mcp_codebase-memory_search_code":               true,
	"mcp_janus-graph_add_episode":                   true,
	"mcp_janus-graph_cypher_query":                  true,
	"mcp_janus-graph_graphiti_health":               true,
	"mcp_janus-graph_retry_dlq_episode":             true,
	"mcp_janus-graph_search_memory":                 true,
	"mcp_planning-mcp_plan_archive_toggle":          true,
	"mcp_planning-mcp_plan_get":                     true,
	"mcp_planning-mcp_plan_link":                    true,
	"mcp_planning-mcp_plan_list":                    true,
	"mcp_planning-mcp_plan_update":                  true,
}

// TestZZModea1113R1Slack ranks the R1 entries by slack.
//
// Only an R1 entry can starve anything at runtime: LoadForEscalation
// is keyed by the runtime tool name, so a TOPIC entry never passes
// through digestSections and its slack is irrelevant to the model.
func TestZZModea1113R1Slack(t *testing.T) {
	ws := liveStoreWorkspace(t)
	store, err := NewToolKnowledgeStore(ws, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	names, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	nl := string(rune(digestNL))
	type row struct {
		name    string
		fileB   int
		h2      int
		room    int
		fixed   int
		sumMin  int
		slack   int
		starved int
		fb      bool
		r1      bool
	}
	var rows []row
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(store.Root(), name+".md"))
		if err != nil {
			continue
		}
		body := extractBody(string(raw))
		rendered := renderKnowledgeFile(name, body, time.Now().UTC())
		if len(rendered) <= KnowledgeSizeCap {
			continue
		}
		head, bodyOnly := splitFrontmatter(rendered)
		sections := splitH2Sections(bodyOnly)
		room := KnowledgeSizeCap - len(head) - len(truncationMarker)
		fixed := 0
		sumMin := 0
		for _, s := range sections {
			if s.heading != "" {
				fixed += len(s.heading) + 1
			}
			bt := strings.TrimSpace(s.body)
			if bt != "" {
				fixed++
				firstLine := strings.Split(bt, nl)[0]
				sumMin += len(digestMinWordSafeExcerpt(firstLine))
			}
		}
		plan := allocateDigest(sections, room)
		starved := 0
		for i, p := range plan {
			if strings.TrimSpace(sections[i].body) == "" {
				continue
			}
			if !p.hasBody {
				starved++
			}
		}
		_, fits := renderDigestAlloc(head, plan, KnowledgeSizeCap)
		rows = append(rows, row{
			name:    name,
			fileB:   len(raw),
			h2:      len(sections),
			room:    room,
			fixed:   fixed,
			sumMin:  sumMin,
			slack:   room - fixed - sumMin,
			starved: starved,
			fb:      !fits,
			r1:      zzModea1113R1Names[name],
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].slack < rows[j].slack })
	r1n := 0
	topicn := 0
	for _, r := range rows {
		if r.r1 {
			r1n++
		} else {
			topicn++
		}
	}
	t.Logf("OVER_CAP=%d  R1=%d  TOPIC=%d", len(rows), r1n, topicn)
	t.Logf("--- R1 entries only, tightest first ---")
	shown := 0
	for _, r := range rows {
		if !r.r1 {
			continue
		}
		shown++
		t.Logf("slack=%-6d file=%-7d h2=%-3d room=%-5d fixed=%-6d sumMin=%-4d starved=%d fallback=%-5v %s",
			r.slack, r.fileB, r.h2, r.room, r.fixed, r.sumMin, r.starved, r.fb, r.name)
	}
	t.Logf("R1 over-cap entries shown: %d", shown)
}

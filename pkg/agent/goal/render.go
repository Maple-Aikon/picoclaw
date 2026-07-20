// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"bytes"
	"fmt"
	"strings"
)

// RenderHeader returns a compact text rendering of the immutable Description
// block (the part of the goal that should always be in context). Pagination is
// not applied — this section is bounded (~10–15 lines) and stable, so the
// LLM benefits from seeing it in full on every tool call.
//
// Layout:
//   ## Goal: <name>
//   **Status:** <status>
//   **Objective:** <objective>
//   **Success criteria:**
//   - <criterion>
//   ...
//   **Scope (in):** <list>     (only if set)
//   **Scope (out):** <list>    (only if set)
//   **Cadence:** <text>         (only if set)
func (g *Goal) RenderHeader() string {
	if g == nil {
		return ""
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "## Goal: %s\n\n", g.Name)
	fmt.Fprintf(&buf, "**Status:** %s  \n", g.Status)
	fmt.Fprintf(&buf, "**Created:** %s  \n", g.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(&buf, "**Updated:** %s\n\n", g.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(&buf, "**Objective:** %s\n\n", g.Description.Objective)
	if n := len(g.Description.SuccessCriteria); n > 0 {
		buf.WriteString("**Success criteria:**\n")
		for _, c := range g.Description.SuccessCriteria {
			fmt.Fprintf(&buf, "- %s\n", c)
		}
		buf.WriteString("\n")
	}
	if in := g.Description.InScope; len(in) > 0 {
		fmt.Fprintf(&buf, "**Scope (in):** %s\n\n", strings.Join(in, ", "))
	}
	if out := g.Description.OutOfScope; len(out) > 0 {
		fmt.Fprintf(&buf, "**Scope (out):** %s\n\n", strings.Join(out, ", "))
	}
	if c := strings.TrimSpace(g.Description.Cadence); c != "" {
		fmt.Fprintf(&buf, "**Cadence:** %s\n\n", c)
	}
	// Strip the trailing extra newline.
	return strings.TrimRight(buf.String(), "\n")
}

// RenderProgress renders the progress log section as plain text. Each line is
// numbered (1-indexed) so the LLM can refer to specific lines via
// view_goal(start_line=N) on a follow-up call. The function is designed for
// pagination with the same semantics as read_file:
//
//   startLine is 0-indexed (startLine=0 → first line).
//   maxLines <= 0 means "no limit" (returns the whole body).
//   hasMore is true when more lines exist beyond the returned window.
//
// Returns (body, totalLines, hasMore, error).
//
// body looks like:
//
//   ## Progress log (lines <from>-<to> of <total>)
//
//   N: ### Progress I — <timestamp>
//   N+1: Completed: <steps>
//   N+2: Blockers: <blockers>     (only if any)
//   N+3: Remaining: <steps>
//   N+4: Drift: <bool>
//   N+5: Next action: <text>
//   ...
func (g *Goal) RenderProgress(startLine, maxLines int) (string, int, bool) {
	if g == nil {
		return "<no goal loaded>", 1, false
	}

	// Build the full body first so totalLines is exact.
	var all bytes.Buffer
	if n := len(g.Progress); n == 0 {
		fmt.Fprintf(&all, "<no progress entries yet>")
	} else {
		for i, p := range g.Progress {
			fmt.Fprintf(&all, "### Progress %d \u2014 %s\n", i+1, p.Timestamp.UTC().Format("2006-01-02T15:04:05Z"))
			if len(p.CompletedSteps) > 0 {
				fmt.Fprintf(&all, "Completed: %s\n", joinList(p.CompletedSteps))
			}
			if len(p.Blockers) > 0 {
				fmt.Fprintf(&all, "Blockers: %s\n", joinList(p.Blockers))
			}
			if len(p.RemainingSteps) > 0 {
				fmt.Fprintf(&all, "Remaining: %s\n", joinList(p.RemainingSteps))
			}
			if p.DriftDetected {
				all.WriteString("Drift: true\n")
			}
			if p.NextAction != "" {
				fmt.Fprintf(&all, "Next action: %s\n", p.NextAction)
			}
			all.WriteString("\n")
		}
		// drop the final blank so totalLines reflects actual lines.
		allStr := strings.TrimRight(all.String(), "\n")
		all.Reset()
		all.WriteString(allStr)
	}

	lines := strings.Split(all.String(), "\n")
	totalLines := len(lines)

	// Normalize bounds.
	if startLine < 0 {
		startLine = 0
	}
	if startLine >= totalLines {
		// Caller asked for a window past EOF. Return an empty body with
		// hasMore=false and totalLines accurate. The tool layer is expected
		// to translate this into "end of file" in the user-facing message.
		return "", totalLines, false
	}

	end := totalLines
	if maxLines > 0 {
		end = startLine + maxLines
		if end > totalLines {
			end = totalLines
		}
	}

	hasMore := end < totalLines

	// Number each line (1-indexed display) for easy LLM reference.
	var buf bytes.Buffer
	for i := startLine; i < end; i++ {
		fmt.Fprintf(&buf, "%d: %s\n", i+1, lines[i])
	}

	// Trim trailing single newline so callers can compose safely.
	out := strings.TrimRight(buf.String(), "\n")
	return out, totalLines, hasMore
}

func joinList(items []string) string {
	return strings.Join(items, "; ")
}

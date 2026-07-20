// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MarshalYAML returns the YAML encoding of Goal. Validate must pass first.
func MarshalYAML(g *Goal) ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(g)
}

// ErrEmpty is returned by Parse when the file is whitespace-only.
var ErrEmpty = errors.New("goal file is empty")

// ErrMissingFrontmatter is returned when the file lacks a leading "---\n".
var ErrMissingFrontmatter = errors.New("goal file missing YAML frontmatter")

// ErrUnterminatedFrontmatter is returned when the closing "---" is missing.
var ErrUnterminatedFrontmatter = errors.New("goal file has unterminated YAML frontmatter")

// Parse extracts the YAML frontmatter from a Markdown file and decodes it.
// The Markdown body is intentionally ignored — structure lives in YAML so
// that view_goal (Phase 2) can reconstruct the body deterministically.
func Parse(path string, data []byte) (*Goal, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrEmpty, path)
	}
	if !bytes.HasPrefix(trimmed, []byte("---\n")) {
		return nil, fmt.Errorf("%w: %s", ErrMissingFrontmatter, path)
	}
	rest := trimmed[4:]
	const sep = "\n---"
	endIdx := bytes.Index(rest, []byte(sep))
	if endIdx < 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnterminatedFrontmatter, path)
	}
	frontBytes := rest[:endIdx]

	var g Goal
	if err := yaml.Unmarshal(frontBytes, &g); err != nil {
		return nil, fmt.Errorf("goal file %s: invalid YAML: %w", path, err)
	}
	return &g, nil
}

// Serialize renders a Goal as Markdown with YAML frontmatter. The body is a
// minimal human-readable recap that view_goal (Phase 2) can return directly.
// A full pretty-rendering pass can be added later; round-trip fidelity is
// what matters in Phase 1.
func Serialize(g *Goal) ([]byte, error) {
	frontBytes, err := MarshalYAML(g)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.Write(frontBytes)
	buf.WriteString("---\n\n")
	fmt.Fprintf(&buf, "# Goal: %s\n\n", g.Name)
	fmt.Fprintf(&buf, "**Status**: %s  \n", g.Status)
	fmt.Fprintf(&buf, "**Created**: %s  \n", g.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(&buf, "**Updated**: %s\n\n", g.UpdatedAt.Format(time.RFC3339))
	if g.Status == StatusAborted {
		if g.AbortedAt != nil {
			fmt.Fprintf(&buf, "**Aborted at**: %s  \n", g.AbortedAt.Format(time.RFC3339))
		}
		if g.AbortReason != "" {
			fmt.Fprintf(&buf, "**Abort reason**: %s  \n\n", g.AbortReason)
		}
	}
	fmt.Fprintf(&buf, "## Objective\n\n%s\n\n", g.Description.Objective)
	if len(g.Progress) > 0 {
		buf.WriteString("## Progress log\n\n")
		for i, p := range g.Progress {
			fmt.Fprintf(&buf, "### Progress %d — %s\n\n", i+1, p.Timestamp.Format(time.RFC3339))
			if len(p.CompletedSteps) > 0 {
				fmt.Fprintf(&buf, "**Completed**: %s\n\n", joinClauses(p.CompletedSteps))
			}
			if len(p.Blockers) > 0 {
				fmt.Fprintf(&buf, "**Blockers**: %s\n\n", joinClauses(p.Blockers))
			}
			if len(p.RemainingSteps) > 0 {
				fmt.Fprintf(&buf, "**Remaining**: %s\n\n", joinClauses(p.RemainingSteps))
			}
			if p.NextAction != "" {
				fmt.Fprintf(&buf, "**Next action**: %s\n\n", p.NextAction)
			}
		}
	}
	return buf.Bytes(), nil
}

func joinClauses(items []string) string {
	return strings.Join(items, "; ")
}

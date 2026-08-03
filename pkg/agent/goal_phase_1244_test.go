// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors
//
// Phase 12.44 T17 (F20 + F22B): stale "500" summary-limit claims must not
// survive anywhere in the pkg/ tree (hints, descriptions, schema caps).
// Mirrors the Phase 12.43 assertNoPhaseClaim grep-test pattern.

package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoStale500SummaryHint — the complete_goal summary limit was raised
// 500 → 1000 runes (owner decision 2026-08-03). Any surviving "1-500" /
// "maxLength: 500" claim in non-test source is a stale hint that would
// mislead the LLM (F20). Scoped to the whole pkg/ tree (pkg/agent +
// pkg/agent/goal + pkg/tools — F22B), excluding test files (historical
// comments) and unrelated packages (providers/cli, tools/subagent use
// "500" for unrelated output truncation).
func TestNoStale500SummaryHint(t *testing.T) {
	dirs := []string{".", "./goal", "../tools"} // pkg/agent, pkg/agent/goal, pkg/tools
	var stale []string
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(data)
			if strings.Contains(text, "1-500") ||
				strings.Contains(text, "maxLength: 500") ||
				strings.Contains(text, `"maxLength": 500`) ||
				strings.Contains(text, "500 characters") {
				stale = append(stale, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(stale) > 0 {
		t.Errorf("stale 500-char summary claims still present in %d file(s): %v", len(stale), stale)
	}
}

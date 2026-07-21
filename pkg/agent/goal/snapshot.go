// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package goal

import (
	"fmt"
	"strings"
	"unicode"
)

// snapshotMaxLen is the hard cap on the rendered StatusSnapshot text. The text
// is injected into every LLM prompt as "[Task context reminder] <text>" so it
// must stay short. 180 chars matches the planning budget agreed in Phase 8 §6
// (Q3 default). RenderGoalSnapshot appends "…" if the rendered text exceeds
// the cap so callers always receive a deterministic-length string.
const snapshotMaxLen = 180

// snapshotLangVNDiacritics is the set of code points that indicate Vietnamese
// diacritics. We use this as a lightweight language heuristic for snapshot
// formatting — Vietnamese text retains its accented chars while English text
// has none. Goal.Objective is the canonical source for the active language.
var snapshotLangVNDiacritics = map[rune]bool{
	// a-variants
	224: true, 225: true, 259: true, 7855: true, 7857: true, 7859: true, 7861: true, 7863: true,
	// e-variants
	232: true, 233: true, 7867: true, 7869: true, 7871: true, 7873: true, 7875: true, 7877: true, 7879: true,
	// i-variants
	236: true, 237: true, 7881: true, 7883: true,
	// o-variants
	242: true, 243: true, 245: true, 7887: true, 7889: true, 7891: true, 7893: true, 7895: true, 7897: true, 7899: true, 7901: true, 7903: true, 7905: true, 7907: true,
	// u-variants
	249: true, 250: true, 361: true, 431: true, 7909: true, 7911: true, 7913: true, 7915: true, 7917: true, 7919: true, 7921: true,
	// y-variants
	253: true, 7923: true, 7925: true, 7927: true, 7929: true,
	// d
	273: true,
}

// isVietnameseText returns true when the input contains at least one
// Vietnamese diacritic. The heuristic intentionally tolerates mixed-language
// input (some goals are written half-VN half-EN) — we only need a hint, not a
// classification.
func isVietnameseText(s string) bool {
	for _, r := range s {
		if snapshotLangVNDiacritics[r] {
			return true
		}
	}
	return false
}

// RenderGoalSnapshot produces the 1-2 sentence StatusSnapshot text for the
// given goal. Pass lastEntry=nil to render the *initial* snapshot after a
// fresh set_goal (Objective + first in_scope item). Pass lastEntry!=nil to
// render the *post-progress* snapshot (last entry's next_action + first
// remaining_step, with optional drift prefix).
//
// The output is bounded by snapshotMaxLen (180 chars). If the natural render
// would exceed the cap, the function trims at the nearest rune boundary and
// appends "…" so the LLM can see it was clipped. Callers receive the trimmed
// string verbatim — there is no error path because every valid Goal/Entry
// combo can produce some non-empty string.
//
// Phase 8.1 — replaces the LLM-driven extractTaskWithFallback path that ran
// once per turn (and per error recovery, and per user steering). Now the
// snapshot text is computed deterministically from the goal's own state, so
// the LLM gets the same reminder it would have built for itself.
func RenderGoalSnapshot(g *Goal, lastEntry *ProgressEntry) string {
	if g == nil {
		return ""
	}
	var s string
	if lastEntry == nil {
		s = renderInitialSnapshot(g)
	} else {
		s = renderProgressSnapshot(g, lastEntry)
	}
	s = strings.TrimSpace(s)
	if len(s) > snapshotMaxLen {
		// Trim at rune boundary, drop any partial trailing rune, append "…".
		runes := []rune(s)
		// Find the largest prefix whose byte length <= snapshotMaxLen-3 (for "…").
		// Walk runes from the start accumulating byte count.
		byteLen := 0
		cutAt := 0
		for i, r := range runes {
			rb := len(string(r))
			if byteLen+rb > snapshotMaxLen-3 {
				cutAt = i
				break
			}
			byteLen += rb
			cutAt = i + 1
		}
		if cutAt <= 0 {
			cutAt = 1
		}
		s = string(runes[:cutAt]) + "…"
	}
	return s
}

// renderInitialSnapshot produces the snapshot text after a fresh set_goal.
//
// Format: "Goal: <objective>. Next: <first in_scope item>." (or "Next: <first
// success criterion>." if in_scope is empty).
//
// The Objective and Next-step text are concatenated verbatim from the goal
// struct — no LLM rewriting. We deliberately preserve the language the goal
// was authored in (Vietnamese vs English) because the LLM prompt context
// mirrors the user's language. Drift is always false on initial snapshot
// because there is no progress yet.
func renderInitialSnapshot(g *Goal) string {
	objective := strings.TrimSpace(g.Description.Objective)
	if objective == "" {
		return ""
	}
	next := firstNonEmpty(
		firstOfSlice(g.Description.InScope),
		firstOfSlice(g.Description.SuccessCriteria),
	)
	if next == "" {
		return fmt.Sprintf("Goal: %s.", truncateForDot(objective))
	}
	return fmt.Sprintf("Goal: %s. Next: %s.", truncateForDot(objective), truncateForDot(next))
}

// renderProgressSnapshot produces the snapshot text after a goal_progress
// entry. If the entry has a NextAction, that becomes the "Next:" slot; if
// not, the first remaining_step fills in. Completed steps are not echoed
// (they live in the persistent progress log, not in the snapshot).
//
// Drift prefix: when entry.DriftDetected is true, prepend "⚠ DRIFT: " so the
// LLM immediately knows the work is off-track. This is the only case where
// the snapshot is allowed to grow beyond the 1-line norm — drift requires
// attention and the LLM benefits from seeing it inline.
func renderProgressSnapshot(g *Goal, entry *ProgressEntry) string {
	next := strings.TrimSpace(entry.NextAction)
	if next == "" {
		next = firstOfSlice(entry.RemainingSteps)
	}
	if next == "" {
		// No forward signal at all — try to recover from the goal itself.
		next = firstNonEmpty(
			firstOfSlice(g.Description.InScope),
			firstOfSlice(g.Description.SuccessCriteria),
		)
	}
	if next == "" {
		return ""
	}
	objective := strings.TrimSpace(g.Description.Objective)
	core := fmt.Sprintf("Goal: %s. Next: %s.", truncateForDot(objective), truncateForDot(next))
	if entry.DriftDetected {
		return "⚠ DRIFT: " + core
	}
	return core
}

// firstOfSlice returns the first non-empty trimmed element of s, or "" if
// s is empty / all-whitespace.
func firstOfSlice(s []string) string {
	for _, x := range s {
		if t := strings.TrimSpace(x); t != "" {
			return t
		}
	}
	return ""
}

// firstNonEmpty returns the first non-empty string in the variadic list.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// truncateForDot strips trailing punctuation (., ; :) and trailing whitespace
// from s so the snapshot template can safely append its own period. We don't
// cap length here — that's RenderGoalSnapshot's job after composition.
func truncateForDot(s string) string {
	s = strings.TrimSpace(s)
	for {
		if len(s) == 0 {
			return ""
		}
		last := s[len(s)-1]
		if last == '.' || last == ';' || last == ':' || unicode.IsSpace(rune(last)) {
			s = strings.TrimRight(s, string(last))
			continue
		}
		return s
	}
}

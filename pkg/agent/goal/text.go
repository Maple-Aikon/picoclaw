// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors
//
// Phase 12.44: rune-safe text primitives for goal tools.
//
// MaxGoalSummaryRunes caps the ARCHIVED copy of complete_goal's summary.
// The user-facing publish is NOT capped here (PublishToUser passes the full
// summary verbatim — splitting > maxLen is the outbound layer's
// responsibility, Phase 12.44 F22A). Owner decision (anh Maple 2026-08-03):
// summaries longer than the cap are TRUNCATED for the archive (never
// rejected), and the FULL summary still reaches the user.

package goal

import (
	"unicode"
	"unicode/utf8"
)

const MaxGoalSummaryRunes = 1000

// truncateRunes truncates s to at most max runes, then strips any trailing
// Unicode combining marks (Mn category — e.g. NFD Vietnamese diacritics)
// so the output never ends in a dangling accent (F17).
//
// Rune-safe: never splits a multi-byte UTF-8 sequence (uses rune slicing,
// Phase 12.28.3 lesson — byte length != rune count for VN/CJK).
//
// Known limitation (F24A, accepted cosmetic): grapheme-perfect truncation
// (ZWJ emoji sequences, variation selectors U+FE0F) is intentionally NOT
// handled — a ZWJ sequence may split at an internal boundary, producing
// valid UTF-8 that renders as separate emoji. Vietnamese text (the primary
// concern) is unaffected. Grapheme-perfect handling would require the
// golang.org/x/text/unicode/norm dependency — deferred (§7.1).
func truncateRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	out := []rune(s)[:max]
	// F17: strip trailing combining marks (Mn) so truncation never leaves a
	// dangling diacritic (e.g. "Phò" + U+0301 → "Phò").
	for len(out) > 0 && unicode.Is(unicode.Mn, out[len(out)-1]) {
		out = out[:len(out)-1]
	}
	return string(out)
}

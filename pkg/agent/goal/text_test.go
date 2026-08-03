// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors
//
// Phase 12.44: truncateRunes primitive tests (T13). Multi-byte safe, NFD
// combining-mark stripping, ZWJ/variation-selector limitation documented.

package goal

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// TestTruncateRunes_ASCII — plain ASCII truncation at exact boundary.
func TestTruncateRunes_ASCII(t *testing.T) {
	s := strings.Repeat("a", 1200)
	got := truncateRunes(s, 1000)
	if utf8.RuneCountInString(got) != 1000 {
		t.Fatalf("expected 1000 runes, got %d", utf8.RuneCountInString(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("output must be valid UTF-8")
	}
	if got != s[:1000] {
		t.Error("ASCII truncation must be byte-simple prefix")
	}
}

// TestTruncateRunes_MultiByteSafe — VN diacritics: rune-safe, no broken UTF-8.
func TestTruncateRunes_MultiByteSafe(t *testing.T) {
	s := vnRunes(1200)
	if utf8.RuneCountInString(s) != 1200 {
		t.Fatalf("test setup error: vnRunes(1200) produced %d runes", utf8.RuneCountInString(s))
	}
	got := truncateRunes(s, 1000)
	if utf8.RuneCountInString(got) != 1000 {
		t.Fatalf("expected 1000 runes, got %d", utf8.RuneCountInString(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("output must be valid UTF-8 (no broken multi-byte sequence)")
	}
	if len(got) >= len(s) {
		t.Fatal("truncated output must be shorter than input")
	}
}

// TestTruncateRunes_Emoji — emoji (4-byte runes) don't break.
func TestTruncateRunes_Emoji(t *testing.T) {
	s := strings.Repeat("🦞", 600) // 2400 bytes, 600 runes — under 1000 runes
	got := truncateRunes(s, 1000)
	if got != s {
		t.Error("600-runne input under the 1000 cap must be returned unchanged")
	}
	if !utf8.ValidString(got) {
		t.Fatal("output must be valid UTF-8")
	}
	s2 := strings.Repeat("🦞", 1200)
	got2 := truncateRunes(s2, 1000)
	if utf8.RuneCountInString(got2) != 1000 {
		t.Fatalf("expected 1000 runes, got %d", utf8.RuneCountInString(got2))
	}
	if !utf8.ValidString(got2) {
		t.Fatal("emoji truncation must not split a 4-byte rune")
	}
}

// TestTruncateRunes_NFDStripsTrailingCombiningMarks (F17) — NFD Vietnamese
// (base char + U+0301 combining acute) truncated at a combining mark must
// strip the dangling mark, not leave it orphaned.
func TestTruncateRunes_NFDStripsTrailingCombiningMarks(t *testing.T) {
	// NFD: 'e' + combining acute accent (2 runes per logical char).
	const combiningAcute = "\u0301"
	// Build 1100 NFD chars = 2200 runes.
	var b strings.Builder
	for i := 0; i < 1100; i++ {
		b.WriteString("e")
		b.WriteString(combiningAcute)
	}
	s := b.String()
	if utf8.RuneCountInString(s) != 2200 {
		t.Fatalf("test setup error: expected 2200 runes, got %d", utf8.RuneCountInString(s))
	}
	got := truncateRunes(s, 1000)
	if utf8.RuneCountInString(got) != 1000 && utf8.RuneCountInString(got) != 999 {
		t.Fatalf("expected 1000 runes (999 if the cut lands on a combining mark), got %d", utf8.RuneCountInString(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("output must be valid UTF-8")
	}
	// Last rune must NOT be a combining mark (Mn).
	last, _ := utf8.DecodeLastRuneInString(got)
	if unicode.Is(unicode.Mn, last) {
		t.Fatalf("trailing rune %q (U+%04X) is a dangling combining mark — must be stripped", last, last)
	}
}

// TestTruncateRunes_ExactBoundary — exactly 1000 runes → unchanged.
func TestTruncateRunes_ExactBoundary(t *testing.T) {
	s := vnRunes(1000)
	got := truncateRunes(s, 1000)
	if got != s {
		t.Error("input at exactly the cap must be returned unchanged")
	}
}

// TestTruncateRunes_ZeroAndOverflow — n=0 → empty; n > len → unchanged.
func TestTruncateRunes_ZeroAndOverflow(t *testing.T) {
	if got := truncateRunes("abc", 0); got != "" {
		t.Errorf("truncateRunes(abc, 0) = %q, want empty", got)
	}
	s := vnRunes(50)
	if got := truncateRunes(s, 5000); got != s {
		t.Error("truncateRunes with cap > len must return the input unchanged")
	}
	if got := truncateRunes("", 100); got != "" {
		t.Errorf("truncateRunes(\"\", 100) = %q, want empty", got)
	}
}

// TestTruncateRunes_ZWJSequenceValid (F24A) — ZWJ emoji sequences may split
// at the ZWJ boundary (accepted cosmetic limitation); output must stay valid
// UTF-8 and never panic.
func TestTruncateRunes_ZWJSequenceValid(t *testing.T) {
	// Family emoji: 👨‍👩‍👧‍👦 — 7 runes incl. ZWJ (U+200D).
	const family = "\U0001F468\u200D\U0001F469\u200D\U0001F467\u200D\U0001F466"
	s := ""
	for utf8.RuneCountInString(s) < 1500 {
		s += family
	}
	got := truncateRunes(s, 1000)
	if !utf8.ValidString(got) {
		t.Fatal("ZWJ split must never produce invalid UTF-8")
	}
	if utf8.RuneCountInString(got) != 1000 {
		t.Fatalf("expected 1000 runes, got %d", utf8.RuneCountInString(got))
	}
}

// TestTruncateRunes_AllMnStripped — truncating inside a run of combining
// marks strips ALL trailing Mn runes, not just one.
func TestTruncateRunes_AllMnStripped(t *testing.T) {
	// 995 base chars + 5 combining marks each at the tail → 1000 runes exactly
	// at cap if we cut there; ensure marks are dropped back to a base char.
	var b strings.Builder
	for i := 0; i < 997; i++ {
		b.WriteString("x")
	}
	b.WriteString("e\u0301\u0301\u0301") // base + 3 combining marks = 4 runes
	s := b.String()                     // 1001 runes
	got := truncateRunes(s, 1000)
	// 1000 runes cut → 997 x + e + 2 marks → both marks stripped → 998 runes.
	if utf8.RuneCountInString(got) != 998 {
		t.Fatalf("expected 998 runes (997 x + base e after stripping both trailing marks), got %d", utf8.RuneCountInString(got))
	}
	last, _ := utf8.DecodeLastRuneInString(got)
	if unicode.Is(unicode.Mn, last) {
		t.Fatalf("trailing rune U+%04X is a combining mark — must be stripped", last)
	}
}

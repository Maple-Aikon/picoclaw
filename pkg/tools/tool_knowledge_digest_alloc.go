package tools

import (
	"strings"
)

// digestNL is the newline byte. This file deliberately avoids escape
// sequences inside string and rune literals so it survives JSON-decoding
// round trips intact.
const digestNL byte = 10

// digestBudgetLadder was the OLD per-section excerpt budget ladder
// [100, 60, 40, 25, 0].
//
// It was removed in Phase 12.63 (2026-10-26). The ladder handed the SAME
// byte budget to EVERY section, while the digest's fixed overhead
// (frontmatter + EVERY heading + the truncation marker) grows with the
// section count. The two facts are incompatible:
//
//	overhead(frontmatter + N headings + marker) + N * budget <= cap
//
// has no solution for a non-zero budget once N is large enough, so the
// ladder fell through to budget=0 and wiped EVERY body -- the digest
// degraded into a pure table of contents. Measured 2026-10-26 on the real
// store (48 over-cap entries): a 60-section synthetic entry lost the body
// of 58/60 sections.
//
// The replacement (see digestSections) computes the ACTUAL room left after
// the fixed overhead and water-fills it across the sections.

// digestMinWordSafeExcerpt returns the shortest prefix of s that ends on a
// word boundary and still carries at least one whole word.
//
// It is the cheapest excerpt that is never cut mid-word. A digest cut that
// lands inside a token produces text like "1. **`go version -m <bin>" --
// unreadable, and misleading (the LLM cannot tell a truncated word from a
// typo).
func digestMinWordSafeExcerpt(s string) string {
	if s == "" {
		return ""
	}
	// Find the first space; everything before it is the first whole word.
	if sp := strings.IndexByte(s, ' '); sp > 0 {
		return s[:sp]
	}
	// A single long word with no space: keep it whole (it is the cheapest
	// option available and cutting it would be mid-word anyway).
	return s
}

// trimToRuneBoundary was REMOVED here (2026-11-12).
//
// It was added in c7bdda25 next to the water-filling allocator, but no
// call site was ever wired up: the boundary handling is done by
// digestMinWordSafeExcerpt (word boundary) and extendExcerpt (line or
// word boundary), both of which are on the LIVE path.
//
// It also had a silent difference from the live helpers: it snapped to a
// RUNE boundary only, never to a line boundary. A digest cut with it
// could therefore end mid-line. The live path snaps to both.
//
// Go's linker pruned it from the binary (verified with `go tool nm`:
// linked=0), so removing it changes no runtime behavior.

// extendExcerpt grows the excerpt of a section body from `cur` to at most
// `budget` bytes, and returns the new length.
//
// The growth always stops on a line or word boundary, so the returned
// excerpt is still word-safe: the byte right after it in the original body
// is a space or a newline.
//
// It returns `cur` when it cannot grow without cutting mid-word.
func extendExcerpt(bodyText string, cur, budget int) int {
	if cur >= len(bodyText) || budget <= cur {
		return cur
	}
	limit := budget
	if limit > len(bodyText) {
		limit = len(bodyText)
	}
	tail := bodyText[cur:limit]
	// Prefer a line boundary -- it carries the most content per byte.
	if nl := strings.LastIndexByte(tail, digestNL); nl > 0 {
		return cur + nl
	}
	// Otherwise a word boundary.
	if sp := strings.LastIndexByte(tail, ' '); sp > 0 {
		return cur + sp
	}
	// No boundary inside the window: growing further would cut mid-word
	// or exceed the budget, so stop here.
	return cur
}

// nextWordEnd returns the offset just past the next complete word in
// bodyText starting at `cur`, or `cur` when there is no further word.
func nextWordEnd(bodyText string, cur int) int {
	if cur >= len(bodyText) {
		return cur
	}
	sp := strings.IndexByte(bodyText[cur:], ' ')
	if sp < 0 {
		return cur
	}
	return cur + sp
}

// digestSectionPlan is one section's share of the digest budget.
type digestSectionPlan struct {
	// heading is the H2 heading (empty for a preamble section).
	heading string
	// excerpt is the text to render for this section.
	excerpt string
	// hasBody reports whether the original section carried any body text.
	hasBody bool
}

// renderDigestAlloc renders the digest using a per-section excerpt plan.
//
// The output is guaranteed to be at most byteCap bytes INCLUDING the
// truncation marker: the caller sized the plan against the exact fixed
// overhead (frontmatter + every heading + marker + newlines), and the
// water-filling step never hands a section more bytes than the room that
// was left.
func renderDigestAlloc(head string, plan []digestSectionPlan, byteCap int) (string, bool) {
	var b strings.Builder
	b.WriteString(head)
	for _, p := range plan {
		if p.heading != "" {
			b.WriteString(p.heading)
			b.WriteString(string(rune(digestNL)))
		}
		if p.hasBody {
			b.WriteString(p.excerpt)
			b.WriteString(string(rune(digestNL)))
		}
	}
	b.WriteString(truncationMarker)
	out := b.String()
	if len(out) > byteCap {
		return "", false
	}
	return out, true
}

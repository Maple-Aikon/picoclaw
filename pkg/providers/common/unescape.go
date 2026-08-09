package common

import "strings"

// UnescapeArgStrings recursively decodes whitespace escape sequences that
// survived a single JSON unmarshal as literals (e.g. the 2-char string "\n"
// instead of a real newline). Only whitespace escapes are handled — backslash
// sequences that carry meaning ("\\", "\"") are left untouched.
//
// Shared by the provider parse paths that hit the MiniMax-M3 double-escape
// quirk: the model serializes the arguments JSON, embeds it into a text
// marker, then the text layer escapes again — so a real newline arrives as
// the literal 2-char "\n" after one unmarshal. Originally introduced for the
// OpenAI-compat path (Phase 12.55.1, commit 88ccf144); the Anthropic-compatible
// path (minimax-anthropic, Phase 12.59) had the same quirk and the same
// missing decode — user-facing strings (complete_goal summaries) rendered
// literal "\n" instead of line breaks. Correct providers are unaffected:
// their strings already contain real newlines after unmarshal.
//
// Lives in pkg/providers/common (not the providers root) because the root
// package imports openai_compat/anthropic_messages via the factory — a helper
// imported by both subpackages must live below the import boundary.
func UnescapeArgStrings(v any) any {
	switch t := v.(type) {
	case string:
		if !strings.ContainsRune(t, '\\') {
			return t
		}
		t = strings.ReplaceAll(t, `\n`, "\n")
		t = strings.ReplaceAll(t, `\t`, "\t")
		t = strings.ReplaceAll(t, `\r`, "\r")
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = UnescapeArgStrings(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = UnescapeArgStrings(val)
		}
		return t
	default:
		return v
	}
}

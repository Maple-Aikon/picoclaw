package openai_compat

import (
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/common"
)

func TestUnescapeArgStrings_LiteralBackslashNBecomesNewline(t *testing.T) {
	args := map[string]any{
		"summary": "line1\\nline2",
	}
	common.UnescapeArgStrings(args)
	if got, want := args["summary"], "line1\nline2"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestUnescapeArgStrings_MultipleEscapes(t *testing.T) {
	args := map[string]any{
		"summary": "a\\n\\n**Bold**\\n- item",
	}
	common.UnescapeArgStrings(args)
	if got, want := args["summary"], "a\n\n**Bold**\n- item"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestUnescapeArgStrings_AlreadyDecodedStringsUntouched(t *testing.T) {
	// Providers that escape correctly (DeepSeek/OpenAI-style) already have real
	// newlines after one unmarshal — decoding must be a no-op for them.
	args := map[string]any{
		"summary": "line1\nline2",
		"count":   3,
		"ok":      true,
	}
	common.UnescapeArgStrings(args)
	if got, want := args["summary"], "line1\nline2"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if got := args["count"]; got != 3 {
		t.Fatalf("count = %v, want 3", got)
	}
}

func TestUnescapeArgStrings_NestedValuesRecursed(t *testing.T) {
	args := map[string]any{
		"steps": []any{"step1\\n", "step2"},
		"meta":  map[string]any{"note": "x\\ty"},
	}
	common.UnescapeArgStrings(args)
	steps := args["steps"].([]any)
	if got, want := steps[0], "step1\n"; got != want {
		t.Fatalf("steps[0] = %q, want %q", got, want)
	}
	meta := args["meta"].(map[string]any)
	if got, want := meta["note"], "x\ty"; got != want {
		t.Fatalf("meta.note = %q, want %q", got, want)
	}
}

func TestUnescapeArgStrings_NonStringValuesPreserved(t *testing.T) {
	in := map[string]any{
		"n":   42,
		"f":   1.5,
		"b":   false,
		"nil": nil,
	}
	want := map[string]any{
		"n":   42,
		"f":   1.5,
		"b":   false,
		"nil": nil,
	}
	common.UnescapeArgStrings(in)
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("args = %#v, want %#v", in, want)
	}
}

package seahorse

import (
	"strings"
	"testing"
)

// TestStoreSerialization_NeutralNarrativeAntiPriming verifies that
// partsToReadableContent generates neutral narrative text rather than
// bracketed [tool_use: ...] syntax that primes the LLM to emit raw pseudo-syntax.
func TestStoreSerialization_NeutralNarrativeAntiPriming(t *testing.T) {
	parts := []MessagePart{
		{Type: "text", Text: "I will inspect the workspace"},
		{Type: "tool_use", Name: "read_file", Arguments: `{"path":"config.json"}`, ToolCallID: "call_abc123"},
		{Type: "tool_result", Text: `{"status":"ok"}`, ToolCallID: "call_abc123"},
		{Type: "media", MediaURI: "/tmp/photo.jpg", MimeType: "image/jpeg"},
	}

	readable := partsToReadableContent(parts)

	// Verify no priming syntax exists
	if strings.Contains(readable, "[tool_use:") {
		t.Errorf("readable content contains priming string '[tool_use:':\n%s", readable)
	}
	if strings.Contains(readable, "[tool_result for") {
		t.Errorf("readable content contains priming string '[tool_result for':\n%s", readable)
	}

	// Verify neutral narrative representations exist
	if !strings.Contains(readable, "⚙️ Action: called tool \"read_file\"") {
		t.Errorf("missing neutral action header:\n%s", readable)
	}
	if !strings.Contains(readable, "↳ Result: {\"status\":\"ok\"}") {
		t.Errorf("missing neutral result header:\n%s", readable)
	}
	if !strings.Contains(readable, "📎 Media: /tmp/photo.jpg (image/jpeg)") {
		t.Errorf("missing neutral media header:\n%s", readable)
	}
}

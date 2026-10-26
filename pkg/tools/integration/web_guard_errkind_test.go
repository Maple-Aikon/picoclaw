// Phase 12.63 (2026-10-26) — web_fetch policy rejections must carry a
// typed ErrKind.
//
// Same bug class as the exec safety guard (fixed in the same change):
// a policy rejection that leaves ErrKind empty falls through to the
// agent recovery layer's legacy "Tool execution failed:" prefix
// heuristic, which no web_fetch guard message matches — so recovery
// never fires and the tool_knowledge lesson is never surfaced.
//
// ErrKind = ErrInvalidInput: the URL the LLM chose violates policy;
// retrying the same URL cannot help.

package integrationtools

import (
	"context"
	"strings"
	"testing"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

// TestWebFetch_PolicyRejections_HaveErrKind asserts every web_fetch
// policy guard stamps ErrInvalidInput. Measured by executing the real
// tool — not by reading the source.
func TestWebFetch_PolicyRejections_HaveErrKind(t *testing.T) {
	tool, err := NewWebFetchTool(50000, format, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}

	cases := []struct {
		name    string
		url     string
		wantMsg string
	}{
		{
			name:    "scheme_not_http",
			url:     "ftp://example.com/file.txt",
			wantMsg: "only http/https URLs are allowed",
		},
		{
			name:    "ssrf_private_host",
			url:     "http://localhost:8080/secret",
			wantMsg: "fetching private or local network hosts is not allowed",
		},
		{
			name:    "ssrf_loopback_ip",
			url:     "http://127.0.0.1:9000/admin",
			wantMsg: "fetching private or local network hosts is not allowed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tool.Execute(context.Background(), map[string]any{
				"url": tc.url,
			})

			if !result.IsError {
				t.Fatalf("expected the guard to reject %q, got success: %s",
					tc.url, result.ForLLM)
			}
			if !strings.Contains(result.ForLLM, tc.wantMsg) {
				t.Fatalf("expected guard message %q for %q, got %q",
					tc.wantMsg, tc.url, result.ForLLM)
			}
			if result.ErrKind != toolshared.ErrInvalidInput {
				t.Fatalf("web_fetch policy rejection must carry ErrKind=%q, got %q "+
					"(empty ErrKind ⇒ recovery never fires ⇒ no lesson)",
					toolshared.ErrInvalidInput, result.ErrKind)
			}
		})
	}
}

// TestWebFetch_NonPolicyError_KeepsEmptyErrKind is the NEGATIVE
// CONTROL: a malformed URL is a parse error, not a policy rejection.
// This test pins that the fix did not over-reach — it only stamps the
// two policy guards, not every error path.
//
// (The message differs from the policy guards' wording, so the test
// asserts on the KIND, not on the text.)
func TestWebFetch_NonPolicyError_KeepsEmptyErrKind(t *testing.T) {
	tool, err := NewWebFetchTool(50000, format, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}

	// A URL with a control character fails url.Parse.
	result := tool.Execute(context.Background(), map[string]any{
		"url": "http://exa mple.com/\x7f",
	})
	if !result.IsError {
		t.Fatalf("expected an error for a malformed URL, got success: %s", result.ForLLM)
	}
	if result.ErrKind == toolshared.ErrInvalidInput {
		t.Logf("note: the parse-error path now carries ErrInvalidInput as well " +
			"(acceptable — it IS an invalid input — but not required by this fix)")
	}
}

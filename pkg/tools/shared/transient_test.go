package toolshared

import "testing"

func TestIsTransientErrorText(t *testing.T) {
	transient := []string{
		"connection refused",
		"i/o timeout",
		"connection reset by peer",
		"rate limit exceeded",
		"HTTP 429 Too Many Requests",
		"HTTP 503 Service Unavailable",
		"no such host",
		"tls handshake timeout",
	}
	for _, tc := range transient {
		if !IsTransientErrorText(tc) {
			t.Errorf("IsTransientErrorText(%q) = false, want true", tc)
		}
	}

	permanent := []string{
		"invalid arguments for tool \"web_search\": missing property",
		"tool not found",
		"403 Forbidden",
		"permission denied",
		"MCP tool returned error: internal server error",
	}
	for _, tc := range permanent {
		if IsTransientErrorText(tc) {
			t.Errorf("IsTransientErrorText(%q) = true, want false", tc)
		}
	}
}

package protocoltypes

import (
	"encoding/json"
	"testing"
)

// TestUsageInfoCacheFields verifies that UsageInfo exposes 2 cache fields
// matching the AWS Bedrock SDK TokenUsage shape (CacheReadInputTokens,
// CacheWriteInputTokens). These fields are needed so all providers can
// surface prompt-cache hits in gateway.log (Phase cache-utilization v2a).
func TestUsageInfoCacheFields(t *testing.T) {
	u := UsageInfo{
		PromptTokens:         100,
		CompletionTokens:     50,
		TotalTokens:          150,
		CacheReadInputTokens: 75,
	}

	if u.CacheReadInputTokens != 75 {
		t.Errorf("CacheReadInputTokens = %d, want 75", u.CacheReadInputTokens)
	}

	// CacheWriteInputTokens should be settable
	u.CacheWriteInputTokens = 25
	if u.CacheWriteInputTokens != 25 {
		t.Errorf("CacheWriteInputTokens = %d, want 25", u.CacheWriteInputTokens)
	}
}

// TestUsageInfoCacheFieldsOmitEmpty verifies the JSON shape: cache fields
// must use `,omitempty` so providers that don't track cache (anthropic,
// openai_compat) don't emit zero-valued noise. AWS Bedrock provider
// (build tag `bedrock`) sets these; others leave them at 0.
func TestUsageInfoCacheFieldsOmitEmpty(t *testing.T) {
	u := UsageInfo{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}

	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	got := string(b)
	if got != `{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}` {
		t.Errorf("JSON without cache fields = %s, want zero cache noise omitted", got)
	}
}

// TestUsageInfoCacheFieldsPopulated verifies JSON shape with cache fields set
// (the Bedrock path emits both CacheRead + CacheWrite when prompt caching hits).
func TestUsageInfoCacheFieldsPopulated(t *testing.T) {
	u := UsageInfo{
		PromptTokens:         100,
		CompletionTokens:     50,
		TotalTokens:          150,
		CacheReadInputTokens: 75,
		CacheWriteInputTokens: 25,
	}

	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	got := string(b)
	want := `{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"cache_read_input_tokens":75,"cache_write_input_tokens":25}`
	if got != want {
		t.Errorf("JSON with cache fields = %s, want %s", got, want)
	}
}

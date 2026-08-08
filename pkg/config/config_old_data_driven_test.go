package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestV0ProvidersMapToModelList_DataDriven is the characterization test for the
// data-driven table refactor of v0ProvidersMapToModelList. It freezes the
// behavior of ALL 25 provider rows: ordering, model_name, per-provider field
// sets, model override semantics and empty-entry skipping.
func TestV0ProvidersMapToModelList_DataDriven(t *testing.T) {
	// Every provider with its full field set. The 3 field-set variants are
	// covered explicitly: openai (auth_method+web_search), anthropic
	// (auth_method), github_copilot (connect_mode), antigravity (auth_method
	// only), all others use the default set.
	providers := map[string]any{
		"openai":       fullProv("sk-openai", "https://openai", "bearer", "web_search"),
		"anthropic":    fullProv("sk-anthropic", "https://anthropic", "bearer", ""),
		"litellm":      fullProv("sk-litellm", "https://litellm", "", ""),
		"openrouter":   fullProv("sk-openrouter", "https://openrouter", "", ""),
		"groq":         fullProv("sk-groq", "https://groq", "", ""),
		"zhipu":        fullProv("sk-zhipu", "https://zhipu", "", ""),
		"vllm":         fullProv("sk-vllm", "https://vllm", "", ""),
		"gemini":       fullProv("sk-gemini", "https://gemini", "", ""),
		"nvidia":       fullProv("sk-nvidia", "https://nvidia", "", ""),
		"ollama":       fullProv("sk-ollama", "https://ollama", "", ""),
		"moonshot":     fullProv("sk-moonshot", "https://moonshot", "", ""),
		"shengsuanyun": fullProv("sk-shengsuanyun", "https://shengsuanyun", "", ""),
		"deepseek":     fullProv("sk-deepseek", "https://deepseek", "", ""),
		"cerebras":     fullProv("sk-cerebras", "https://cerebras", "", ""),
		"vivgrid":      fullProv("sk-vivgrid", "https://vivgrid", "", ""),
		"volcengine":   fullProv("sk-volcengine", "https://volcengine", "", ""),
		"github_copilot": map[string]any{ // non-default field set: connect_mode replaces proxy/request_timeout
			"api_key":      "sk-github_copilot",
			"api_base":     "https://copilot",
			"connect_mode": "oauth",
		},
		"antigravity": map[string]any{ // minimal field set: api_key + auth_method only
			"api_key":     "sk-antigravity",
			"auth_method": "bearer",
		},
		"qwen":       fullProv("sk-qwen", "https://qwen", "", ""),
		"mistral":    fullProv("sk-mistral", "https://mistral", "", ""),
		"avian":      fullProv("sk-avian", "https://avian", "", ""),
		"minimax":    fullProv("sk-minimax", "https://minimax", "", ""),
		"longcat":    fullProv("sk-longcat", "https://longcat", "", ""),
		"modelscope": fullProv("sk-modelscope", "https://modelscope", "", ""),
		"novita":     fullProv("sk-novita", "https://novita", "", ""),
	}

	// Expected rows in EXACT order of the migration table (model_name = first jsonKey).
	expectedOrder := []struct {
		modelName string
		defModel  string
	}{
		{"openai", "openai/gpt-5.4"},
		{"anthropic", "anthropic/claude-sonnet-4.6"},
		{"litellm", "litellm/auto"},
		{"openrouter", "openrouter/auto"},
		{"groq", "groq/llama-3.1-70b-versatile"},
		{"zhipu", "zhipu/glm-4"},
		{"vllm", "vllm/auto"},
		{"gemini", "gemini/gemini-pro"},
		{"nvidia", "nvidia/meta/llama-3.1-8b-instruct"},
		{"ollama", "ollama/llama3"},
		{"moonshot", "moonshot/kimi"},
		{"shengsuanyun", "shengsuanyun/auto"},
		{"deepseek", "deepseek/deepseek-chat"},
		{"cerebras", "cerebras/llama-3.3-70b"},
		{"vivgrid", "vivgrid/auto"},
		{"volcengine", "volcengine/doubao-pro"},
		{"github_copilot", "github-copilot/gpt-5.4"},
		{"antigravity", "antigravity/gemini-2.0-flash"},
		{"qwen", "qwen/qwen-max"},
		{"mistral", "mistral/mistral-small-latest"},
		{"avian", "avian/deepseek/deepseek-v3.2"},
		{"minimax", "minimax/minimax"},
		{"longcat", "longcat/LongCat-Flash-Thinking"},
		{"modelscope", "modelscope/Qwen/Qwen3-235B-A22B-Instruct-2507"},
		{"novita", "novita/auto"},
	}

	t.Run("ordering_and_default_models", func(t *testing.T) {
		result := v0ProvidersMapToModelList(providers, "", "")
		require.Len(t, result, len(expectedOrder))
		for i, want := range expectedOrder {
			entry := result[i].(map[string]any)
			require.Equal(t, want.modelName, entry["model_name"], "row %d model_name", i)
			require.Equal(t, want.defModel, entry["model"], "row %d model", i)
		}
	})

	t.Run("field_sets", func(t *testing.T) {
		result := v0ProvidersMapToModelList(providers, "", "")
		byName := map[string]map[string]any{}
		for _, r := range result {
			e := r.(map[string]any)
			byName[e["model_name"].(string)] = e
		}

		// Default set for every provider.
		for _, name := range []string{"deepseek", "qwen", "minimax", "novita", "ollama"} {
			e := byName[name]
			require.Equal(t, "sk-"+name, e["api_key"], name)
			require.Equal(t, "https://"+name, e["api_base"], name)
			require.Equal(t, "proxy-"+name, e["proxy"], name)
			require.Equal(t, float64(30), e["request_timeout"], name)
			require.NotContains(t, e, "auth_method", name)
			require.NotContains(t, e, "web_search", name)
			require.NotContains(t, e, "connect_mode", name)
		}

		// openai: default + auth_method + web_search (bool true preserved).
		openai := byName["openai"]
		require.Equal(t, "bearer", openai["auth_method"])
		require.Equal(t, true, openai["web_search"])
		require.Equal(t, "proxy-openai", openai["proxy"])

		// anthropic: default + auth_method.
		anthropic := byName["anthropic"]
		require.Equal(t, "bearer", anthropic["auth_method"])
		require.NotContains(t, anthropic, "web_search")

		// github_copilot: api_key + api_base + connect_mode, NO proxy/timeout.
		copilot := byName["github_copilot"]
		require.Equal(t, "oauth", copilot["connect_mode"])
		require.NotContains(t, copilot, "proxy")
		require.NotContains(t, copilot, "request_timeout")

		// antigravity: api_key + auth_method only.
		ag := byName["antigravity"]
		require.Equal(t, "bearer", ag["auth_method"])
		require.NotContains(t, ag, "api_base")
		require.NotContains(t, ag, "proxy")
		require.NotContains(t, ag, "request_timeout")
	})

	t.Run("user_provider_model_override", func(t *testing.T) {
		// userProvider matches an alias key (kimi), not the first key (moonshot).
		result := v0ProvidersMapToModelList(providers, "kimi", "kimi-2.1")
		byName := map[string]map[string]any{}
		for _, r := range result {
			e := r.(map[string]any)
			byName[e["model_name"].(string)] = e
		}
		// No slash in userModel -> protocol prefix.
		require.Equal(t, "moonshot/kimi-2.1", byName["moonshot"]["model"])
		// Other providers keep defaults.
		require.Equal(t, "deepseek/deepseek-chat", byName["deepseek"]["model"])

		// userModel already has a slash -> used verbatim.
		result = v0ProvidersMapToModelList(providers, "openai", "custom/org-model")
		byName = map[string]map[string]any{}
		for _, r := range result {
			e := r.(map[string]any)
			byName[e["model_name"].(string)] = e
		}
		require.Equal(t, "custom/org-model", byName["openai"]["model"])

		// userProvider that matches nothing -> defaults everywhere.
		result = v0ProvidersMapToModelList(providers, "nonexistent", "some-model")
		for _, r := range result {
			e := r.(map[string]any)
			require.Contains(t, e["model"], "/", "no-match provider must keep prefixed default")
		}
	})

	t.Run("empty_and_zero_values", func(t *testing.T) {
		// litellm present but with zero values -> skipped entirely.
		// openai with empty api_key/web_search=false -> those fields dropped,
		// but api_base/proxy still copied.
		p := map[string]any{
			"litellm": map[string]any{
				"api_key": "", "api_base": "", "proxy": "",
			},
			"openai": map[string]any{
				"api_key":         "",
				"api_base":        "https://openai",
				"proxy":           "proxy-openai",
				"request_timeout": float64(10),
				"auth_method":     "",
				"web_search":      false,
			},
		}
		result := v0ProvidersMapToModelList(p, "", "")
		require.Len(t, result, 1, "litellm with only zero fields must be skipped")
		entry := result[0].(map[string]any)
		require.Equal(t, "openai", entry["model_name"])
		require.NotContains(t, entry, "api_key")
		require.Equal(t, "https://openai", entry["api_base"])
		require.Equal(t, "proxy-openai", entry["proxy"])
		require.Equal(t, float64(10), entry["request_timeout"])
		require.NotContains(t, entry, "auth_method")
		require.NotContains(t, entry, "web_search", "web_search=false must be dropped")
	})

	t.Run("alias_keys_resolve_to_same_row", func(t *testing.T) {
		// Provider present under the SECOND alias key only -> still migrated.
		p := map[string]any{
			"gpt":    fullProv("sk-gpt", "https://gpt", "bearer", "web_search"),
			"glm":    fullProv("sk-glm", "https://glm", "", ""),
			"doubao": fullProv("sk-doubao", "https://doubao", "", ""),
			"tongyi": fullProv("sk-tongyi", "https://tongyi", "", ""),
			"kimi":   fullProv("sk-kimi", "https://kimi", "", ""),
			"copilot": map[string]any{
				"api_key": "sk-copilot", "api_base": "https://copilot", "connect_mode": "oauth",
			},
		}
		result := v0ProvidersMapToModelList(p, "", "")
		byName := map[string]map[string]any{}
		for _, r := range result {
			e := r.(map[string]any)
			byName[e["model_name"].(string)] = e
		}
		// model_name is ALWAYS the first jsonKey, not the alias used.
		require.Equal(t, "openai", byName["openai"]["model_name"])
		require.Equal(t, "zhipu", byName["zhipu"]["model_name"])
		require.Equal(t, "volcengine", byName["volcengine"]["model_name"])
		require.Equal(t, "qwen", byName["qwen"]["model_name"])
		require.Equal(t, "moonshot", byName["moonshot"]["model_name"])
		require.Equal(t, "github_copilot", byName["github_copilot"]["model_name"])
		// Entry content comes from the alias-provided map.
		require.Equal(t, "sk-kimi", byName["moonshot"]["api_key"])
		require.Equal(t, "oauth", byName["github_copilot"]["connect_mode"])
	})
}

// fullProv builds a provider map with the default field set plus optional extras.
func fullProv(apiKey, apiBase, authMethod, webSearch string) map[string]any {
	m := map[string]any{
		"api_key":         apiKey,
		"api_base":        apiBase,
		"proxy":           "proxy-" + apiKey[3:], // sk-<name> -> proxy-<name>
		"request_timeout": float64(30),
	}
	if authMethod != "" {
		m["auth_method"] = authMethod
	}
	if webSearch != "" {
		m["web_search"] = true
	}
	return m
}

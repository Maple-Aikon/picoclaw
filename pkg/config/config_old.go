// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package config

import "strings"

// isProvidersMapEmpty checks if a providers map has any non-empty provider configurations.
func isProvidersMapEmpty(providers map[string]any) bool {
	for _, prov := range providers {
		if provMap, ok := prov.(map[string]any); ok {
			if apiKey, ok := provMap["api_key"]; ok && apiKey != "" {
				return false
			}
			if apiBase, ok := provMap["api_base"]; ok && apiBase != "" {
				return false
			}
			if connectMode, ok := provMap["connect_mode"]; ok && connectMode != "" {
				return false
			}
			if authMethod, ok := provMap["auth_method"]; ok && authMethod != "" {
				return false
			}
		}
	}
	return true
}

// v0ProviderRule defines the migration rule for one V0 provider row.
type v0ProviderRule struct {
	keys     []string
	protocol string
	defModel string
	// fields lists which provider-map keys are copied into the model entry,
	// each copied only when non-zero ("" / false / nil are skipped).
	// nil means the default set: api_key, api_base, proxy, request_timeout.
	fields []string
}

// v0DefaultFields is the field set shared by most providers.
var v0DefaultFields = []string{"api_key", "api_base", "proxy", "request_timeout"}

// v0ProvidersMapToModelList converts a V0 providers map to a model_list slice.
// Row order is significant: entries appear in the same order as the rules.
func v0ProvidersMapToModelList(providers map[string]any, userProvider, userModel string) []any {
	rules := []v0ProviderRule{
		{keys: []string{"openai", "gpt"}, protocol: "openai", defModel: "openai/gpt-5.4",
			fields: []string{"api_key", "api_base", "proxy", "request_timeout", "auth_method", "web_search"}},
		{keys: []string{"anthropic", "claude"}, protocol: "anthropic", defModel: "anthropic/claude-sonnet-4.6",
			fields: []string{"api_key", "api_base", "proxy", "request_timeout", "auth_method"}},
		{keys: []string{"litellm"}, protocol: "litellm", defModel: "litellm/auto"},
		{keys: []string{"openrouter"}, protocol: "openrouter", defModel: "openrouter/auto"},
		{keys: []string{"groq"}, protocol: "groq", defModel: "groq/llama-3.1-70b-versatile"},
		{keys: []string{"zhipu", "glm"}, protocol: "zhipu", defModel: "zhipu/glm-4"},
		{keys: []string{"vllm"}, protocol: "vllm", defModel: "vllm/auto"},
		{keys: []string{"gemini", "google"}, protocol: "gemini", defModel: "gemini/gemini-pro"},
		{keys: []string{"nvidia"}, protocol: "nvidia", defModel: "nvidia/meta/llama-3.1-8b-instruct"},
		{keys: []string{"ollama"}, protocol: "ollama", defModel: "ollama/llama3"},
		{keys: []string{"moonshot", "kimi"}, protocol: "moonshot", defModel: "moonshot/kimi"},
		{keys: []string{"shengsuanyun"}, protocol: "shengsuanyun", defModel: "shengsuanyun/auto"},
		{keys: []string{"deepseek"}, protocol: "deepseek", defModel: "deepseek/deepseek-chat"},
		{keys: []string{"cerebras"}, protocol: "cerebras", defModel: "cerebras/llama-3.3-70b"},
		{keys: []string{"vivgrid"}, protocol: "vivgrid", defModel: "vivgrid/auto"},
		{keys: []string{"volcengine", "doubao"}, protocol: "volcengine", defModel: "volcengine/doubao-pro"},
		{keys: []string{"github_copilot", "copilot"}, protocol: "github-copilot", defModel: "github-copilot/gpt-5.4",
			fields: []string{"api_key", "api_base", "connect_mode"}},
		{keys: []string{"antigravity"}, protocol: "antigravity", defModel: "antigravity/gemini-2.0-flash",
			fields: []string{"api_key", "auth_method"}},
		{keys: []string{"qwen", "tongyi"}, protocol: "qwen", defModel: "qwen/qwen-max"},
		{keys: []string{"mistral"}, protocol: "mistral", defModel: "mistral/mistral-small-latest"},
		{keys: []string{"avian"}, protocol: "avian", defModel: "avian/deepseek/deepseek-v3.2"},
		{keys: []string{"minimax"}, protocol: "minimax", defModel: "minimax/minimax"},
		{keys: []string{"longcat"}, protocol: "longcat", defModel: "longcat/LongCat-Flash-Thinking"},
		{keys: []string{"modelscope"}, protocol: "modelscope", defModel: "modelscope/Qwen/Qwen3-235B-A22B-Instruct-2507"},
		{keys: []string{"novita"}, protocol: "novita", defModel: "novita/auto"},
	}

	var result []any

	for _, rule := range rules {
		// Find the provider in the providers map (first matching key wins).
		var provData map[string]any
		found := false
		for _, key := range rule.keys {
			if v, ok := providers[key]; ok {
				if provMap, ok := v.(map[string]any); ok {
					provData = provMap
					found = true
					break
				}
			}
		}
		if !found {
			continue
		}

		// Copy configured fields (non-zero only); empty entries are skipped.
		entry := make(map[string]any)
		for _, field := range rule.fieldsOrDefault() {
			addIfNonZero(entry, provData, field)
		}
		if len(entry) == 0 {
			continue
		}

		// model_name is always the first key; model uses the user's model when
		// their provider matches, prefixed with the protocol unless it already
		// contains a slash.
		entry["model_name"] = rule.keys[0]
		modelToUse := rule.defModel
		if userProvider != "" && userModel != "" {
			for _, key := range rule.keys {
				if userProvider == key {
					if !strings.Contains(userModel, "/") {
						modelToUse = rule.protocol + "/" + userModel
					} else {
						modelToUse = userModel
					}
					break
				}
			}
		}
		entry["model"] = modelToUse

		result = append(result, entry)
	}

	return result
}

// fieldsOrDefault returns the rule's field set, falling back to the default.
func (r v0ProviderRule) fieldsOrDefault() []string {
	if r.fields != nil {
		return r.fields
	}
	return v0DefaultFields
}

// addIfNonZero copies prov[name] into entry unless the value is zero
// (nil, empty string, or false). Numeric values (e.g. request_timeout) are
// always copied.
func addIfNonZero(entry, prov map[string]any, name string) {
	if v, ok := prov[name]; ok && !v0IsZero(v) {
		entry[name] = v
	}
}

// v0IsZero reports whether v is a zero value under V0 migration rules.
func v0IsZero(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	}
	return false
}

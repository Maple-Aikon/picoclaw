package config

import (
	"testing"
)

func TestApplyLegacySummarizeTaskModelMigration_LogsAndNoopsWhenSet(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Defaults: AgentDefaults{
				SummarizeTaskModel: "openai/gpt-4o-mini",
			},
		},
	}

	// Should not panic, should not error. Verifies the field read and
	// WarnCF call path.
	applyLegacySummarizeTaskModelMigration(cfg)
}

func TestApplyLegacySummarizeTaskModelMigration_NoopWhenEmpty(t *testing.T) {
	cfg := &Config{
		Agents: AgentsConfig{
			Defaults: AgentDefaults{
				SummarizeTaskModel: "",
			},
		},
	}

	applyLegacySummarizeTaskModelMigration(cfg)
}

func TestApplyLegacySummarizeTaskModelMigration_NilSafe(t *testing.T) {
	// nil cfg
	applyLegacySummarizeTaskModelMigration(nil)

	// zero-value Config (no Agents block)
	applyLegacySummarizeTaskModelMigration(&Config{})
}
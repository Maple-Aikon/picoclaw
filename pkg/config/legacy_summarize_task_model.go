package config

import (
	"github.com/sipeed/picoclaw/pkg/logger"
)

// applyLegacySummarizeTaskModelMigration logs a boot-time warning when
// `agents.defaults.summarize_task_model` (env: PICOCLAW_AGENTS_DEFAULTS_SUMMARIZE_TASK_MODEL)
// is set. The field is no-op since Phase 8.2 — task context now flows through
// the goal.StatusSnapshot pipeline instead of an LLM-driven extraction call.
//
// We keep the struct field for one minor to avoid breaking deployed configs
// (the JSON key + env var stay readable) but flag it via WarnCF so operators
// can clean up their config.
//
// See plan: memory/plan/picoclaw-phase8-replace-task-summary-with-goal-checkpoint-20260721.md §8.3.
func applyLegacySummarizeTaskModelMigration(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.Agents.Defaults.SummarizeTaskModel == "" {
		return
	}

	logger.WarnCF(
		"config",
		"agents.defaults.summarize_task_model is deprecated and ignored",
		map[string]any{
			"reason":  "Phase 8.2 replaced LLM-based task summary extraction with goal.StatusSnapshot",
			"action":  "remove the field from your config (and unset PICOCLAW_AGENTS_DEFAULTS_SUMMARIZE_TASK_MODEL env var, if set)",
			"setting": cfg.Agents.Defaults.SummarizeTaskModel,
		},
	)
}
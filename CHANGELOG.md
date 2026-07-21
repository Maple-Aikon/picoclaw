# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Removed

- **Phase 10: Remove `extend_turn_iteration` tool + `/extend` command (vĩnh viễn)**
  - Removed `extend_turn_iteration` core tool. The iteration cap is now constant per turn (equal to `agent.MaxIterations`). Users typing `/extend` are treated as unknown commands — no error message, no announcement (silent rollout).
  - Removed 3-tier windowed hint logic in `pkg/agent/pipeline_llm.go` (`iterationExtendingHintMessage`, `iterationCapReachedMessage`); only Tier 3 (tool-stripped final summary) remains.
  - Removed `MaxIterationsCap` field from `AgentInstance` and `Config`; `max_iterations_cap` is now rejected as an unknown config field (same pattern as Phase 9's `summarize_task_model`).
  - Removed `/extend` slash command from `BuiltinDefinitions` and its dispatcher in `pkg/agent/agent_command.go`.
  - Net: −1952 LOC across 21 files. Test count: `pkg/agent/` 694 PASS / 2 FAIL (pre-existing baseline accepted since Phase 4), `pkg/agent/goal/` 72 PASS / 0 FAIL.

### Added

- **Phase 12 (deferred): `/new-goal <objective>` slash command** — first-class user UX to seed a goal from the user side (replaces what `/extend` previously provided). Skeleton in plan §7.1.

### Changed

- **Phase 9: Remove `extractTaskWithFallback` / `resolveTaskModel` / `extractTaskSummary` (post-replacement cleanup)**
  - Removed 3 dead LLM-driven task-summary functions in `pkg/agent/pipeline_setup.go` (−165 LOC).
  - Removed `pkg/config/legacy_summarize_task_model.go` + its test (−73 LOC).
  - Removed `SummarizeTaskModel` field + env binding + call site in `pkg/config/config.go`. The `agents.defaults.summarize_task_model` config field is now rejected as an unknown field at boot.
  - Net: −209 LOC. Live verified: post-deploy gateway restart fires 0 WarnCF logs (Phase 8.3 had fired `setting="pico-gemma-local"` every boot).
  - Removed `legacyTaskSummary sync.Map` field from `pkg/agent/agent.go` and the `useGoalProgress` migration flag. Phase 7's graceful-degradation path is no longer reachable in production (`useGoalProgress=true` since 2026-07-21, confirmed unreachable on live binary).
  - Simplified `pkg/agent/turn_state_snapshot.go` to single-path implementation: all cross-turn context reads/writes go through `goal.Store.UpdateStatusSnapshot` / `LoadStatusSnapshot`. The two-tier flag dispatch (`!useGoalProgress` vs `useGoalProgress`) is gone.
  - `extractTaskWithFallback` and `extractTaskSummary` remain as no-op stubs for one minor (Q2 default from plan §8.3) — full removal targeted for Phase 9.
  - `agents.defaults.summarize_task_model` config field is now deprecated. Setting it triggers a boot-time `WarnCF` log (`reason="Phase 8.2 replaced LLM-based task summary extraction with goal.StatusSnapshot"`) but the field is otherwise ignored. Removal target: Phase 9.

### Tests

- `pkg/agent/turn_state_snapshot_test.go` rewritten — 15 single-path tests covering goal-store read/write/preserve contracts and raw-text fallback synthesis. Legacy-mode tests deleted.
- `pkg/config/legacy_summarize_task_model_test.go` added — 3 tests covering nil-safety, no-op when empty, and live WarnCF when set.

## [v0.3.0.1] - 2026-07-01

### Added

- **`extend_turn_iteration` tool** — LLM-controlled iteration budget for long-running turns.
  - Opt-in via new `max_iterations_cap` agent field (default `0` = extension disabled, preserving legacy behavior). When `> 0`, the `extend_turn_iteration` tool is auto-registered.
  - Three-tier windowed behavior in `CallLLM`:
    1. **Soft hint** — when 1 or 2 iterations remain, append a reminder to the LLM call describing the available tool and current budget.
    2. **Cap-reached** — when current iteration equals the cap, only the extend tool is exposed; all other tools are stripped.
    3. **Absolute ceiling** — when `iterationCap == max_iterations_cap`, all tools are stripped and the turn ends via the existing `toolLimitResponse` fallback.
  - The tool requires an `intent` argument describing the LLM's forward-looking plan (used for goal-drift detection during the extension segment).
  - Extension is by the agent's default `MaxIterations`, clamped to `MaxIterationsCap`. A partial extension is applied when only the residual budget remains.
  - Post-extension reminders: an immediate `[Task context reminder]` fires at iteration N+1 (reusing the existing task summary); a midpoint reminder fires at the midpoint of the new extension segment.
  - Circuit breaker around `extend_turn_iteration` execution: 3 consecutive failures trigger a per-session break, preventing runaway loops when the cap ceiling is reached.

### Configuration

```json
{
  "agents": {
    "defaults": {
      "max_tool_iterations": 20,
      "max_iterations_cap": 50
    }
  }
}
```

When `max_iterations_cap` is `0` (default) or omitted, the `extend_turn_iteration` tool is not registered and behavior is identical to pre-feature turns.

### Changed

- **`/extend` slash command (per-turn opt-in)** — `extend_turn_iteration` is no longer auto-enabled by `max_iterations_cap` alone.
  - The tool is always registered when `max_iterations_cap > 0`, but only callable in turns opened via `/extend <message>`.
  - Per-turn flag `ts.extendEnabled` is set by `applyExtendCommand` (intercepting handler before the command executor), strips the `/extend ` prefix, and forwards the remainder as the user message to the LLM.
  - `filterOutExtendTool()` removes `extend_turn_iteration` from the provider tool list on non-`/extend` turns. Turns without opt-in retain the legacy `toolLimitResponse` ceiling via the absolute-ceiling tier (Tier 3) regardless.
  - The 3-tier windowed-hint logic (Tier 1 soft hint, Tier 2 cap-reached) is now gated on `ts.extendEnabled`; Tier 3 always fires. This preserves the prior ceiling behavior for ordinary turns.
  - `/help` automatically includes `/extend <message>` via the `BuiltinDefinitions` registry.
  - Misconfiguration: `max_iterations_cap == 0` with the tool registered surfaces an info log at startup; the tool returns a runtime error when called.

[v0.3.0.1]: https://github.com/sipeed/picoclaw/compare/v0.3.0...v0.3.0.1
[Unreleased]: https://github.com/sipeed/picoclaw/compare/v0.3.0.1...HEAD

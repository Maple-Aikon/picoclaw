# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/sipeed/picoclaw/compare/v0.3.0...HEAD
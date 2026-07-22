// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/routing"
)

// SetupTurn extracts the one-time initialization phase, returning a
// turnExecution populated with history, messages, and candidate selection.
// It replaces lines 56-145 of the original runTurn.
func (p *Pipeline) SetupTurn(ctx context.Context, ts *turnState) (*turnExecution, error) {
	// Phase 11: stale goal recovery (Hook 1 for turn boundary). Must run
	// before any other state read on this turn so the LLM never sees a
	// goal left over from a prior turn (which would confuse the
	// per-turn scope). Idempotent — no-op if no active goal exists.
	if ts.sessionKey != "" {
		if err := archiveStaleGoalOnTurnStart(p.al, ts.sessionKey); err != nil {
			// Best-effort: log and continue. A failure here means
			// a stale file might leak to the next view_goal read;
			// recoverable on the next iteration's view_goal.
			logger.WarnCF("agent", "stale goal recovery failed", map[string]any{
				"session_key": ts.sessionKey,
				"error":       err.Error(),
			})
		}
	}

	cfg := p.Cfg
	maxMediaSize := cfg.Agents.Defaults.GetMaxMediaSize()

	var history []providers.Message
	var summary string
	if !ts.opts.NoHistory {
		if resp, err := p.ContextManager.Assemble(ctx, &AssembleRequest{
			SessionKey: ts.sessionKey,
			Budget:     ts.agent.ContextWindow,
			MaxTokens:  ts.agent.MaxTokens,
		}); err == nil && resp != nil {
			history = resp.History
			summary = resp.Summary
		}
	}
	ts.captureRestorePoint(history, summary)

	contextualSkills := ts.activeSkills
	if ts.agent.ContextBuilder != nil {
		contextualSkills = ts.agent.ContextBuilder.ResolveActiveSkillsForContext(ts.activeSkills)
	}
	ts.recordSkillContextSnapshot(skillContextTriggerInitialBuild, contextualSkills)
	initialPromptReq := promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media, cfg)
	initialPromptReq.ActiveSkills = append([]string(nil), contextualSkills...)
	messages := ts.agent.ContextBuilder.BuildMessagesFromPrompt(initialPromptReq)
	currentTurnStart := len(messages)
	if strings.TrimSpace(ts.userMessage) != "" || len(ts.media) > 0 {
		currentTurnStart = len(messages) - 1
	}

	messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize, currentTurnStart)

	if !ts.opts.NoHistory {
		toolDefs := filterToolsByTurnProfile(ts.agent.Tools.ToProviderDefs(), ts.profile)
		if isOverContextBudget(ts.agent.ContextWindow, messages, toolDefs, ts.agent.MaxTokens) {
			logger.WarnCF("agent", "Proactive compression: context budget exceeded before LLM call",
				map[string]any{"session_key": ts.sessionKey})
			compactReq := &CompactHookRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonProactive,
				Budget:     ts.agent.ContextWindow,
			}
			if p.Hooks != nil {
				compactReq, _ = p.Hooks.BeforeCompact(ctx, compactReq)
			}
			if err := p.ContextManager.Compact(ctx, &CompactRequest{
				SessionKey: compactReq.SessionKey,
				Reason:     compactReq.Reason,
				Budget:     compactReq.Budget,
			}); err != nil {
				logger.WarnCF("agent", "Proactive compact failed", map[string]any{
					"session_key": ts.sessionKey,
					"error":       err.Error(),
				})
			}
			ts.refreshRestorePointFromSession(ts.agent)
			if resp, err := p.ContextManager.Assemble(ctx, &AssembleRequest{
				SessionKey: ts.sessionKey,
				Budget:     ts.agent.ContextWindow,
				MaxTokens:  ts.agent.MaxTokens,
			}); err == nil && resp != nil {
				history = resp.History
				summary = resp.Summary
			}
			rebuildPromptReq := promptBuildRequestForTurn(
				ts,
				history,
				summary,
				ts.userMessage,
				ts.media,
				cfg,
			)
			rebuildPromptReq.ActiveSkills = append([]string(nil), contextualSkills...)
			messages = ts.agent.ContextBuilder.BuildMessagesFromPrompt(rebuildPromptReq)
			rebuiltCurrentTurnStart := len(messages)
			if strings.TrimSpace(ts.userMessage) != "" || len(ts.media) > 0 {
				rebuiltCurrentTurnStart = len(messages) - 1
			}
			messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize, rebuiltCurrentTurnStart)
		}
	}

	if !ts.opts.NoHistory && (strings.TrimSpace(ts.userMessage) != "" || len(ts.media) > 0) {
		rootMsg := userPromptMessage(ts.userMessage, ts.media)
		if len(rootMsg.Media) > 0 {
			ts.agent.Sessions.AddFullMessage(ts.sessionKey, rootMsg)
		} else {
			ts.agent.Sessions.AddMessage(ts.sessionKey, rootMsg.Role, rootMsg.Content)
		}
		ts.recordPersistedMessage(rootMsg)
		ts.ingestMessage(ctx, p.al, rootMsg)
	}

	activeCandidates, activeModel, tier := p.al.selectCandidates(ts.agent, ts.userMessage, messages)
	activeProvider := ts.agent.Provider
	switch tier {
	case routing.TierLight:
		if ts.agent.LightProvider != nil {
			activeProvider = ts.agent.LightProvider
		}
	case routing.TierMedium:
		if ts.agent.MediumProvider != nil {
			activeProvider = ts.agent.MediumProvider
		}
	}
	activeModelName := strings.TrimSpace(ts.agent.Model)
	if tier == routing.TierLight {
		activeModelName = strings.TrimSpace(sideQuestionModelName(ts.agent, routing.TierLight))
	}
	activeModelName = resolvedCandidateModelName(activeCandidates, activeModelName)

	exec := newTurnExecution(
		ts.agent,
		ts.opts,
		history,
		summary,
		messages,
	)
	exec.currentTurnStart = currentTurnStart
	exec.activeCandidates = activeCandidates
	exec.activeModel = activeModel
	exec.activeModelConfig = resolveActiveModelConfig(
		p.Cfg,
		ts.agent.Workspace,
		activeCandidates,
		activeModel,
		p.Cfg.Agents.Defaults.Provider,
	)
	exec.llmModelName = activeModelName
	exec.activeProvider = activeProvider
	exec.tier = tier
	exec.usedLight = tier == routing.TierLight

	// Phase 8.2 — task context reminder is now sourced from the active
	// goal's StatusSnapshot (written by set_goal / goal_progress, see
	// pkg/agent/goal/snapshot.go). No LLM call is made here: the snapshot
	// is the LLM's own self-evaluation from its last goal_progress entry,
	// so what we inject into the next CallLLM is exactly what the agent
	// last said it was working on.
	//
	// When no goal is set (or the snapshot is empty), we fall back to a
	// raw-text concat of the user's message + the last assistant content.
	// The reminder is then re-evaluated by CallLLM at the threshold iteration
	// and can also be refreshed by user steering in turn_coord.go.
	if strings.TrimSpace(ts.userMessage) != "" {
		// Preserve isErrorRecovery semantics: when the prior turn ended with
		// a tool result, mark the next CallLLM as recovery so it can inject
		// the snapshot at iteration 1 instead of waiting for the threshold.
		if len(history) > 0 && history[len(history)-1].Role == "tool" {
			exec.isErrorRecovery = true
			logger.InfoCF("agent", "Error recovery mode detected: previous turn ended with tool result", map[string]any{
				"session_key": ts.sessionKey,
			})
		}

		// Phase 11: per-turn goal scope. No cross-turn injectedTaskSummary.
		// exec.injectedTaskSummary is left as ""; pipeline_llm.go no
		// longer reads it.
		// (Preserve isErrorRecovery detection above for any future
		//  error-recovery path that still wants to short-circuit.)
	}

	return exec, nil
}

// lastAssistantContent returns the Content of the most recent assistant
// message in history, or "" when there is none. Extracted from the inline
// loop that used to live in SetupTurn before Phase 8.2.
func lastAssistantContent(history []providers.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" {
			return history[i].Content
		}
	}
	return ""
}

// buildRawTextReminder assembles the fallback reminder text from the user's
// message and the tail of the last assistant content. Capped at 280 chars so
// the [Task context reminder] line stays compact.
//
// When one of the two halves is empty, we emit only the non-empty half
// (no stray " | " separator) so a no-goal session whose history is also
// empty produces a clean user-message-only reminder.
func buildRawTextReminder(userMessage, lastAssistant string) string {
	const maxTail = 200
	tail := lastAssistant
	if len(tail) > maxTail {
		tail = tail[len(tail)-maxTail:]
	}
	user := strings.TrimSpace(userMessage)
	tail = strings.TrimSpace(tail)
	var out string
	switch {
	case user == "" && tail == "":
		return ""
	case user == "":
		out = tail
	case tail == "":
		out = user
	default:
		out = user + " | " + tail
	}
	if len(out) > 280 {
		out = out[len(out)-280:]
	}
	return out
}

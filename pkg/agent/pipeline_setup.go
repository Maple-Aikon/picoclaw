// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
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
	// raw-text concat of the user's message + the last assistant content,
	// same as the legacy extractTaskWithFallback tier #4 used to do. The
	// reminder is then re-evaluated by CallLLM at the threshold iteration
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

// extractTaskWithFallback is DEPRECATED as of Phase 8.2 (2026-07-21).
//
// The function previously tried a 4-tier LLM fallback chain (task_model →
// light_model → medium_model → active_model) to produce a 1-2 sentence
// task summary that was injected into the LLM as "[Task context reminder]".
// The mechanism was redundant with the goal_progress tool's self-evaluation
// (Phase 2 of the goal lifecycle), so callers now read the reminder text
// directly from the active goal's StatusSnapshot via al.loadTaskSummary.
//
// This stub is kept for one minor version (per plan §6 Q2 default) to
// preserve the function signature for any external callers; the body is a
// no-op that always returns "". Do not reintroduce the LLM call chain —
// the reminder is now generated by the goal writer (see
// pkg/agent/goal/snapshot.go::RenderGoalSnapshot). Full removal is
// scheduled for Phase 9.
//
// Removal target: Phase 9. See plan
// picoclaw-phase8-replace-task-summary-with-goal-checkpoint-20260721.md
// §6 Q2.
func extractTaskWithFallback(
	_ context.Context,
	_ *AgentLoop,
	_ *turnState,
	_ *turnExecution,
	_ string,
	_ string,
	lastAssistantMsg string,
	userContent string,
) string {
	_ = lastAssistantMsg
	_ = userContent
	// Phase 8.2: stub — see DEPRECATED comment on extractTaskWithFallback above.
	return ""
}

// resolveTaskModel resolves a task extraction model, preferring pre-resolved
// provider/candidates (from routing initialization) and falling back to
// direct config lookup when routing is disabled.
//
// DEPRECATED as of Phase 8.2 (2026-07-21): was only used by
// extractTaskWithFallback which is now a no-op stub. Kept for one minor
// version to avoid breaking external imports; full removal in Phase 9.
func resolveTaskModel(
	cfg *config.Config,
	preResolvedProvider providers.LLMProvider,
	preResolvedCandidates []providers.FallbackCandidate,
	modelName string,
) (providers.LLMProvider, string) {
	// Use pre-resolved provider if available
	if preResolvedProvider != nil && len(preResolvedCandidates) > 0 {
		return preResolvedProvider, preResolvedCandidates[0].Model
	}

	// Otherwise, resolve directly from config
	if modelName == "" {
		return nil, ""
	}

	mc := lookupModelConfigByRef(cfg, modelName)
	if mc == nil {
		mc = &config.ModelConfig{Model: ensureProtocolModel(modelName)}
	}

	lp, model, err := providers.CreateProviderFromConfig(mc)
	if err != nil {
		logger.WarnCF("agent", "Task extraction: failed to resolve model from config", map[string]any{
			"model": modelName,
			"error": err.Error(),
		})
		return nil, ""
	}
	return lp, model
}

// extractTaskSummary calls the LLM to produce a 1-2 sentence task summary.
// It is used during SetupTurn (background/blocking) and when steering
// messages arrive mid-turn. The summary is used for goal-drift prevention.
// prevTaskSummary and convSummary provide context; userContent is what to
// extract the task from. Returns "" if extraction fails for any reason.
//
// DEPRECATED as of Phase 8.2 (2026-07-21): was only used by
// extractTaskWithFallback which is now a no-op stub. Kept for one minor
// version to avoid breaking external imports; full removal in Phase 9.
// See plan picoclaw-phase8-replace-task-summary-with-goal-checkpoint-20260721.md
// §6 Q2.
func extractTaskSummary(
	ctx context.Context,
	al *AgentLoop,
	provider providers.LLMProvider,
	model string,
	prevTaskSummary string,
	convSummary string,
	lastAssistantMsg string,
	userContent string,
) string {
	logger.DebugCF("agent", "Task extraction attempt", map[string]any{
		"model": model,
	})

	// 1. Truncate aggressive for 4B models
	if lastAssistantMsg != "" && len(lastAssistantMsg) > 300 {
		lastAssistantMsg = lastAssistantMsg[:300] + "... (truncated)"
	}

	// 2. Collapse context into flat text
	priorContext := ""
	if convSummary != "" {
		priorContext += "Summary: " + convSummary + "\n"
	}
	if prevTaskSummary != "" {
		priorContext += "Previous task: " + prevTaskSummary + "\n"
	}
	if lastAssistantMsg != "" {
		priorContext += "Last assistant response: " + lastAssistantMsg + "\n"
	}

	// 3. Schema-first prompt
	prompt := "Your task: Extract the core task requested by the user in <user_message> as a single concise, action-oriented command.\n\n" +
		"Rules:\n" +
		"1. Output ONLY the active command/task. Do NOT use descriptive prefixes like 'The user wants to...', 'Yêu cầu...', or passive voice.\n" +
		"2. Keep it extremely concise (typically 3-12 words).\n" +
		"3. Match the language of the output to the language of <user_message> (Vietnamese or English).\n" +
		"4. Base your output ONLY on <user_message>. Ignore <context>.\n\n" +
		"Examples:\n" +
		"- Input: \"em kiểm tra các service đang chạy đi...\"\n" +
		"  Output: Kiểm tra các dịch vụ đang chạy.\n" +
		"- Input: \"restart lại PMC đi, nó bị treo rồi...\"\n" +
		"  Output: Khởi động lại PMC.\n" +
		"- Input: \"Run memory-optimization Mode A\"\n" +
		"  Output: Run memory-optimization Mode A.\n" +
		"- Input: \"Cho phép lập trình thay vì prompt language models, nghĩa là sao\"\n" +
		"  Output: Giải thích khái niệm lập trình thay vì prompt.\n" +
		"- Input: \"Em thêm error handling rồi test trước 1 round nhé. Ok thì chạy hết luôn\"\n" +
		"  Output: Thêm error handling và chạy thử nghiệm 1 vòng.\n\n"

	if priorContext != "" {
		prompt += "<context>\n" + priorContext + "</context>\n\n"
	}

	prompt += "<user_message>\n" + userContent + "\n</user_message>\n\n" +
		"Output:"

	resp, err := func() (*providers.LLMResponse, error) {
		// Track this background provider request as an in-flight call so
		// ReloadProviderAndConfig waits for it to complete before closing
		// the underlying provider instance. Without this guard, the reload
		// path can race ahead and close the provider mid-Chat.
		al.activeRequestsInc()
		defer al.activeRequestsDec()
		return provider.Chat(ctx,
			[]providers.Message{{Role: "user", Content: prompt}},
			nil, model, map[string]any{"max_tokens": 256, "stop": []string{"\n\n"}})
	}()
	if err != nil {
		logger.WarnCF("agent", "Task extraction failed", map[string]any{
			"model": model,
			"error": err.Error(),
		})
		return ""
	}
	if resp == nil || resp.Content == "" {
		logger.WarnCF("agent", "Task extraction returned empty response", map[string]any{
			"model": model,
		})
		return ""
	}
	logger.DebugCF("agent", "Task extraction succeeded", map[string]any{
		"model": model,
	})
	return strings.TrimSpace(resp.Content)
}

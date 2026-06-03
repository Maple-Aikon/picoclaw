// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/routing"
)

// SetupTurn extracts the one-time initialization phase, returning a
// turnExecution populated with history, messages, and candidate selection.
// It replaces lines 56-145 of the original runTurn.
func (p *Pipeline) SetupTurn(ctx context.Context, ts *turnState) (*turnExecution, error) {
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

	messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize)

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
			rebuildPromptReq := promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media, cfg)
			rebuildPromptReq.ActiveSkills = append([]string(nil), contextualSkills...)
			messages = ts.agent.ContextBuilder.BuildMessagesFromPrompt(rebuildPromptReq)
			messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize)
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
	if usedLight {
		activeModelName = strings.TrimSpace(sideQuestionModelName(ts.agent, true))
	}
	activeModelName = resolvedCandidateModelName(activeCandidates, activeModelName)

	exec := newTurnExecution(
		ts.agent,
		ts.opts,
		history,
		summary,
		messages,
	)
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

	// Background task extraction for goal-drift prevention.
	// Spawn a goroutine to extract a concise task summary from the user's initial
	// message + conversation context. The result is delivered via taskSummaryChan
	// and read by CallLLM at the threshold iteration (maxIter/2) for ephemeral
	// steering injection. Failures are non-critical — the turn proceeds normally.
	//
	// Error recovery: when the prior turn failed (last history message is a tool
	// result with no subsequent assistant response), the extraction runs
	// synchronously with the previous task summary as additional context so the
	// LLM can pick up where it left off. The new summary is stored to
	// sessionTaskSummary for cross-turn recovery.
	if strings.TrimSpace(ts.userMessage) != "" {
		isErrorRecovery := false
		if len(history) > 0 && history[len(history)-1].Role == "tool" {
			isErrorRecovery = true
			exec.isErrorRecovery = true
			logger.InfoCF("agent", "Error recovery mode detected: previous turn ended with tool result", map[string]any{
				"session_key": ts.sessionKey,
			})
		}

		var prevTaskSummary string
		if val, ok := p.al.sessionTaskSummary.Load(ts.sessionKey); ok {
			prevTaskSummary = val.(string)
		}

		var lastAssistantMsg string
		if !isErrorRecovery {
			for i := len(history) - 1; i >= 0; i-- {
				if history[i].Role == "assistant" {
					lastAssistantMsg = history[i].Content
					break
				}
			}
		}

		if isErrorRecovery {
			// Blocking extraction: the result must be available before the
			// iteration loop starts so CallLLM can inject it at iteration 1.
			extractCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			taskSummary := extractTaskWithFallback(extractCtx, p.al, ts, exec, prevTaskSummary, summary, lastAssistantMsg, ts.userMessage)
			if taskSummary != "" {
				select {
				case exec.taskSummaryChan <- taskSummary:
				default:
				}
				p.al.sessionTaskSummary.Store(ts.sessionKey, taskSummary)
			}
		} else {
			// Background extraction: context + cancel exposed via exec so steering
			// in runTurn can cancel it mid-flight and prevent stale overwrites.
			extractCtx, taskCancel := context.WithTimeout(context.Background(), 60*time.Second)
			exec.taskExtractCancel = taskCancel
			go func() {
				defer func() {
					if r := recover(); r != nil {
						logger.WarnCF("agent", "Task extraction panic",
							map[string]any{"session_key": ts.sessionKey})
					}
				}()
				defer taskCancel()

				taskSummary := extractTaskWithFallback(extractCtx, p.al, ts, exec, prevTaskSummary, summary, lastAssistantMsg, ts.userMessage)
				if extractCtx.Err() != nil {
					// Context was cancelled (steering) — discard results.
					return
				}
				if taskSummary != "" {
					select {
					case exec.taskSummaryChan <- taskSummary:
					default:
					}
					p.al.sessionTaskSummary.Store(ts.sessionKey, taskSummary)
				}
			}()
		}
	}

	return exec, nil
}

// extractTaskWithFallback attempts to produce a task summary using a fallback chain.
func extractTaskWithFallback(
	ctx context.Context,
	al *AgentLoop,
	ts *turnState,
	exec *turnExecution,
	prevTaskSummary string,
	convSummary string,
	lastAssistantMsg string,
	userContent string,
) string {
	cfg := al.cfg
	logger.DebugCF("agent", "Starting task extraction fallback chain", nil)

	// 1. Try summarize_task_model (30s timeout)
	if cfg.Agents.Defaults.SummarizeTaskModel != "" {
		logger.DebugCF("agent", "Task extraction: trying summarize_task_model", map[string]any{
			"model": cfg.Agents.Defaults.SummarizeTaskModel,
		})
		// Look up the full ModelConfig from model_list (with api_key, api_base, etc.)
		// instead of passing a bare Model{Model: "alias"} which fails provider init.
		mc := lookupModelConfigByRef(cfg, cfg.Agents.Defaults.SummarizeTaskModel)
		if mc == nil {
			// No explicit config — fall back to constructing one with ensureProtocolModel
			mc = &config.ModelConfig{Model: ensureProtocolModel(cfg.Agents.Defaults.SummarizeTaskModel)}
		}
		provider, model, err := al.providerFactory(mc)
		if err == nil {
			summarizeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			summary := extractTaskSummary(summarizeCtx, provider, model, prevTaskSummary, convSummary, lastAssistantMsg, userContent)
			cancel()
			if summary != "" {
				return summary
			}
			logger.DebugCF("agent", "Task extraction: summarize_task_model returned empty, falling back to light_model", map[string]any{
				"model": cfg.Agents.Defaults.SummarizeTaskModel,
			})
		} else {
			logger.WarnCF("agent", "Task extraction: summarize_task_model provider init failed, falling back to light_model", map[string]any{
				"model": cfg.Agents.Defaults.SummarizeTaskModel,
				"error": err.Error(),
			})
		}
	} else {
		logger.DebugCF("agent", "Task extraction: summarize_task_model not configured, skipping", nil)
	}

	// 2. Try light_model (10s timeout)
	var lightModelName string
	if cfg.Agents.Defaults.Routing != nil {
		lightModelName = cfg.Agents.Defaults.Routing.LightModel
	}
	lightProvider, lightModel := resolveTaskModel(cfg, ts.agent.LightProvider, ts.agent.LightCandidates, lightModelName)
	if lightProvider != nil && lightModel != "" {
		logger.DebugCF("agent", "Task extraction: trying light_model", map[string]any{"model": lightModel})
		lightCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		summary := extractTaskSummary(lightCtx, lightProvider, lightModel, prevTaskSummary, convSummary, lastAssistantMsg, userContent)
		cancel()
		if summary != "" {
			return summary
		}
		logger.DebugCF("agent", "Task extraction: light_model returned empty, falling back to medium_model", map[string]any{"model": lightModel})
	} else {
		logger.DebugCF("agent", "Task extraction: light_model not configured, falling back to medium_model", nil)
	}

	// 3. Try medium_model (10s timeout)
	var mediumModelName string
	if cfg.Agents.Defaults.Routing != nil {
		mediumModelName = cfg.Agents.Defaults.Routing.MediumModel
	}
	mediumProvider, mediumModel := resolveTaskModel(cfg, ts.agent.MediumProvider, ts.agent.MediumCandidates, mediumModelName)
	if mediumProvider != nil && mediumModel != "" {
		logger.DebugCF("agent", "Task extraction: trying medium_model", map[string]any{"model": mediumModel})
		mediumCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		summary := extractTaskSummary(mediumCtx, mediumProvider, mediumModel, prevTaskSummary, convSummary, lastAssistantMsg, userContent)
		cancel()
		if summary != "" {
			return summary
		}
		logger.DebugCF("agent", "Task extraction: medium_model returned empty, falling back to active_model", map[string]any{"model": mediumModel})
	} else {
		logger.DebugCF("agent", "Task extraction: medium_model not configured, falling back to active_model", nil)
	}

	// 4. Try active model (10s timeout)
	logger.DebugCF("agent", "Task extraction: trying active_model (medium_model failed)", map[string]any{"model": exec.activeModel})
	activeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	summary := extractTaskSummary(activeCtx, exec.activeProvider, exec.activeModel, prevTaskSummary, convSummary, lastAssistantMsg, userContent)
	cancel()
	if summary == "" {
		logger.WarnCF("agent", "Task extraction: all models failed, falling back to raw text concatenation", nil)
		// Fallback: combine previous summary with the latest user message
		if prevTaskSummary != "" && userContent != "" {
			return prevTaskSummary + "\n---\n" + userContent
		} else if prevTaskSummary != "" {
			return prevTaskSummary
		} else if userContent != "" {
			return userContent
		}
		return ""
	}
	return summary
}

// resolveTaskModel resolves a task extraction model, preferring pre-resolved
// provider/candidates (from routing initialization) and falling back to
// direct config lookup when routing is disabled.
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
func extractTaskSummary(
	ctx context.Context,
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

	resp, err := provider.Chat(ctx,
		[]providers.Message{{Role: "user", Content: prompt}},
		nil, model, map[string]any{"max_tokens": 256, "stop": []string{"\n\n"}})
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

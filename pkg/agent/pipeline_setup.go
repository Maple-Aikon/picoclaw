// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
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

	messages := ts.agent.ContextBuilder.BuildMessagesFromPrompt(
		promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media),
	)

	messages = resolveMediaRefs(messages, p.MediaStore, maxMediaSize)

	if !ts.opts.NoHistory {
		toolDefs := ts.agent.Tools.ToProviderDefs()
		if isOverContextBudget(ts.agent.ContextWindow, messages, toolDefs, ts.agent.MaxTokens) {
			logger.WarnCF("agent", "Proactive compression: context budget exceeded before LLM call",
				map[string]any{"session_key": ts.sessionKey})
			if err := p.ContextManager.Compact(ctx, &CompactRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonProactive,
				Budget:     ts.agent.ContextWindow,
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
			messages = ts.agent.ContextBuilder.BuildMessagesFromPrompt(
				promptBuildRequestForTurn(ts, history, summary, ts.userMessage, ts.media),
			)
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

	activeCandidates, activeModel, usedLight := p.al.selectCandidates(ts.agent, ts.userMessage, messages)
	activeProvider := ts.agent.Provider
	if usedLight && ts.agent.LightProvider != nil {
		activeProvider = ts.agent.LightProvider
	}

	exec := newTurnExecution(
		ts.agent,
		ts.opts,
		history,
		summary,
		messages,
	)
	exec.activeCandidates = activeCandidates
	exec.activeModel = activeModel
	exec.activeProvider = activeProvider
	exec.usedLight = usedLight

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
			extractCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			taskSummary := extractTaskWithFallback(extractCtx, p.al, ts, exec, prevTaskSummary, summary, lastAssistantMsg, ts.userMessage)
			cancel()
			if taskSummary != "" {
				p.al.sessionTaskSummary.Delete(ts.sessionKey)
				select {
				case exec.taskSummaryChan <- taskSummary:
				default:
				}
				p.al.sessionTaskSummary.Store(ts.sessionKey, taskSummary)
			}
		} else {
			// Background extraction: context + cancel exposed via exec so steering
			// in runTurn can cancel it mid-flight and prevent stale overwrites.
			extractCtx, taskCancel := context.WithTimeout(context.Background(), 5*time.Second)
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
					p.al.sessionTaskSummary.Delete(ts.sessionKey)
					select {
					case exec.taskSummaryChan <- taskSummary:
						p.al.sessionTaskSummary.Store(ts.sessionKey, taskSummary)
					default:
					}
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

	// 1. Try summarize_task_model
	if cfg.Agents.Defaults.SummarizeTaskModel != "" {
		provider, model, err := al.providerFactory(&config.ModelConfig{Model: cfg.Agents.Defaults.SummarizeTaskModel})
		if err == nil {
			summary := extractTaskSummary(ctx, provider, model, prevTaskSummary, convSummary, lastAssistantMsg, userContent)
			if summary != "" {
				return summary
			}
		}
	}

	// 2. Try light_model
	if ts.agent.LightProvider != nil && len(ts.agent.LightCandidates) > 0 {
		summary := extractTaskSummary(ctx, ts.agent.LightProvider, ts.agent.LightCandidates[0].Model, prevTaskSummary, convSummary, lastAssistantMsg, userContent)
		if summary != "" {
			return summary
		}
	}

	// 3. Try active model
	summary := extractTaskSummary(ctx, exec.activeProvider, exec.activeModel, prevTaskSummary, convSummary, lastAssistantMsg, userContent)
	return summary
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
	prompt := "You are a task extractor. Extract the single most important task or question the user wants accomplished from the messages below. Output ONLY 1-2 sentences describing the core task. Do not include any metadata or explanation.\n\n"
	if convSummary != "" {
		prompt += "Conversation summary:\n" + convSummary + "\n\n"
	}
	if prevTaskSummary != "" {
		prompt += "Previous task summary (the user is continuing this task):\n" + prevTaskSummary + "\n\n"
	}
	if lastAssistantMsg != "" {
		prompt += "Last status from assistant:\n" + lastAssistantMsg + "\n\n"
	}
	prompt += "Latest user message:\n" + userContent

	resp, err := provider.Chat(ctx,
		[]providers.Message{{Role: "user", Content: prompt}},
		nil, model, map[string]any{"max_tokens": 256})
	if err != nil {
		logger.DebugCF("agent", "Task extraction failed (non-critical)",
			map[string]any{"error": err.Error()})
		return ""
	}
	if resp == nil || resp.Content == "" {
		return ""
	}
	return strings.TrimSpace(resp.Content)
}

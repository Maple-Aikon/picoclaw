// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/constants"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// CallLLM performs an LLM call with fallback support, hook invocation, and retry logic.
// It handles PreLLM setup, the actual LLM invocation with retry, and AfterLLM processing.
// Returns Control indicating what the coordinator should do next.
func (p *Pipeline) CallLLM(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) (Control, error) {
	al := p.al
	maxMediaSize := p.Cfg.Agents.Defaults.GetMaxMediaSize()

	// PreLLM: resolve media refs (except on iteration 1 where user media is already resolved)
	if iteration > 1 {
		exec.messages = resolveMediaRefs(exec.messages, p.MediaStore, maxMediaSize, exec.currentTurnStart)
	}

	// PreLLM: graceful terminal handling
	exec.gracefulTerminal, _ = ts.gracefulInterruptRequested()

	// Per-iteration goal-phase allowlist (Delivery Phase 4). Recompute
	// the tool allowlist based on the session's current goal phase so
	// the LLM only sees the lifecycle tools appropriate to where it is
	// in the goal lifecycle (Lock → set_goal only; Open → base ∪
	// view_goal + complete_goal; Checkpoint → base ∪ goal_progress +
	// complete_goal). This call is cheap (one YAML-less store read) and
	// idempotent — it must run on EVERY iteration so phase transitions
	// (e.g. set_goal during a turn) are picked up on the very next
	// iteration without an explicit transition hook.
	ts.applyPhaseAllowlist(ts.currentGoalPhase())

	exec.providerToolDefs = ts.agent.Tools.ToProviderDefs()
	exec.providerToolDefs = filterToolsByTurnProfile(exec.providerToolDefs, ts.profile)

	// Native web search support
	webSearchEnabled := al.cfg.Tools.IsToolEnabled("web") && turnProfileToolAllowed(ts.profile, "web_search")
	exec.useNativeSearch = webSearchEnabled && al.cfg.Tools.Web.PreferNative &&
		func() bool {
			if ns, ok := ts.agent.Provider.(providers.NativeSearchCapable); ok {
				return ns.SupportsNativeSearch()
			}
			return false
		}()
	if exec.useNativeSearch {
		filtered := make([]providers.ToolDefinition, 0, len(exec.providerToolDefs))
		for _, td := range exec.providerToolDefs {
			if td.Function.Name != "web_search" {
				filtered = append(filtered, td)
			}
		}
		exec.providerToolDefs = filtered
	}

	exec.callMessages = exec.messages
	if exec.gracefulTerminal {
		exec.callMessages = append(append([]providers.Message(nil), exec.messages...), ts.interruptHintMessage())
		exec.providerToolDefs = nil
		ts.markGracefulTerminalUsed()
	}
	// Phase 12.8: removed the legacy Tier 3 force-wrap (toolLimitHintMessage +
	// providerToolDefs=nil) that fired on RemainingIterations() <= 0. The
	// cap-hit case is now owned by the goal-phase machinery (Phase 11):
	//   - iter == iterationCap  → GoalPhaseCheckpoint allowlist (goal_progress
	//     + complete_goal only) lets the LLM either self-extend the cap
	//     (goal_progress → ExtendIterationCap) or finalize the goal
	//     (complete_goal → Phase 12.7 final-report iter).
	//   - iter >= maxIterationsCap → GoalPhaseFinal allowlist
	//     ([complete_goal] only).
	// If the LLM is text-only at GoalPhaseCheckpoint, Phase 12 text-only
	// recovery fires (soft → hard → archive) and breaks the turn.

	// Task summary injection: REMOVED in Phase 11.
	//
	// Per-turn goal scope means there is no cross-turn task context to
	// inject. The previous mechanism (StatusSnapshot +
	// injectedTaskSummary + taskSummaryChan + 50% threshold reminder) is
	// gone: turns are independent, the LLM seeds a fresh goal at turn
	// start, and the user-facing reply is emitted via complete_goal's
	// `summary` arg (or assistantText) at the end of the turn. There is
	// no longer a [Task context reminder] slot.
	//
	// Recovery hint injection (Phase 11.1): per-iteration recovery messages
	// (empty response, text-only streak, tool exec error) are stashed in
	// ts.pendingRecoveryMessage by recovery_goal.go before ControlContinue
	// re-enters this loop. We consume them here exactly once — next
	// iteration's LLM call sees the hint, then the field clears so the
	// hint does not repeat. Without this consumer (the Phase 5 → 11 gap),
	// the message was set but never reached the LLM context.
	if recoveryMsg := ts.recoveryHintMessage(); recoveryMsg.Content != "" {
		exec.callMessages = append(append([]providers.Message(nil), exec.callMessages...), recoveryMsg)
	}
	if err := p.routeMediaTurn(ts, exec); err != nil {
		return ControlBreak, err
	}

	exec.llmOpts = map[string]any{
		"max_tokens":       ts.agent.MaxTokens,
		"temperature":      ts.agent.Temperature,
		"prompt_cache_key": ts.agent.ID,
	}
	if exec.useNativeSearch {
		exec.llmOpts["native_search"] = true
	}
	applyTurnThinkingOptions(exec, ts.agent, exec.activeProvider, true)

	exec.llmModel = exec.activeModel

	// BeforeLLM hook
	if p.Hooks != nil {
		llmReq, decision := p.Hooks.BeforeLLM(turnCtx, &LLMHookRequest{
			Meta:             ts.eventMeta("runTurn", "turn.llm.request"),
			Context:          cloneTurnContext(ts.turnCtx),
			Model:            exec.llmModel,
			Messages:         exec.callMessages,
			Tools:            exec.providerToolDefs,
			Options:          exec.llmOpts,
			GracefulTerminal: exec.gracefulTerminal,
		})
		switch decision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmReq != nil {
				prevModel := exec.llmModel
				exec.llmModel = llmReq.Model
				exec.callMessages = llmReq.Messages
				exec.providerToolDefs = filterToolsByTurnProfile(llmReq.Tools, ts.profile)
				exec.llmOpts = llmReq.Options
				nativeSearchAllowed := exec.useNativeSearch &&
					turnProfileToolAllowed(ts.profile, "web_search")
				if !nativeSearchAllowed {
					delete(exec.llmOpts, "native_search")
				}
				if strings.TrimSpace(exec.llmModel) != "" && exec.llmModel != prevModel {
					p.applyBeforeLLMModelRewrite(ts, exec)
					applyTurnThinkingOptions(exec, ts.agent, exec.activeProvider, true)
				}
			}
		case HookActionAbortTurn:
			cancelConfiguredStreamingLLM(turnCtx, exec)
			exec.abortedByHook = true
			return ControlBreak, nil
		case HookActionHardAbort:
			cancelConfiguredStreamingLLM(turnCtx, exec)
			_ = ts.requestHardAbort()
			exec.abortedByHardAbort = true
			return ControlBreak, nil
		}
	}

	al.emitEvent(
		runtimeevents.KindAgentLLMRequest,
		ts.eventMeta("runTurn", "turn.llm.request"),
		LLMRequestPayload{
			Model:         exec.llmModel,
			MessagesCount: len(exec.callMessages),
			ToolsCount:    len(exec.providerToolDefs),
			MaxTokens:     ts.agent.MaxTokens,
			Temperature:   ts.agent.Temperature,
		},
	)

	logger.DebugCF("agent", "LLM request",
		map[string]any{
			"agent_id":          ts.agent.ID,
			"iteration":         iteration,
			"model":             exec.llmModel,
			"messages_count":    len(exec.callMessages),
			"tools_count":       len(exec.providerToolDefs),
			"max_tokens":        ts.agent.MaxTokens,
			"temperature":       ts.agent.Temperature,
			"system_prompt_len": len(exec.callMessages[0].Content),
		})
	logger.DebugCF("agent", "Full LLM request",
		map[string]any{
			"iteration":     iteration,
			"messages_json": formatMessagesForLog(exec.callMessages),
			"tools_json":    formatToolsForLog(exec.providerToolDefs),
		})

	// Single LLM call per retry iteration (Phase 2 refactor: extracted to callLLMCore method
	// to enable handleHookReplay helper to perform same-iteration replay without burning the
	// main iteration budget. See plan same-iteration-replay-loop-with-reusable-boundedretry-primitive-20260717.)
	var err error
	maxRetries := p.Cfg.Agents.Defaults.MaxLLMRetries
	if maxRetries <= 0 {
		maxRetries = 2
	}
	backoffSecs := p.Cfg.Agents.Defaults.LLMRetryBackoffSecs
	if backoffSecs <= 0 {
		backoffSecs = 2
	}
	for retry := 0; retry <= maxRetries; retry++ {
		exec.response, err = p.callLLMCore(ctx, turnCtx, ts, exec, exec.callMessages, exec.providerToolDefs, iteration)
		if err == nil {
			break
		}
		if ts.hardAbortRequested() && errors.Is(err, context.Canceled) {
			_ = ts.requestHardAbort()
			exec.abortedByHardAbort = true
			return ControlBreak, nil
		}
		if isConfiguredStreamingVisibleError(err) {
			break
		}

		if hasMediaRefs(exec.callMessages) && isVisionUnsupportedError(err) {
			return ControlBreak, visionUnsupportedModelError(
				exec.llmModelName,
				len(ts.agent.ImageCandidates) > 0,
			)
		}

		errMsg := strings.ToLower(err.Error())
		retryReason, isTransientError := transientLLMRetryReason(err)
		isContextError := !isTransientError && (strings.Contains(errMsg, "context_length_exceeded") ||
			strings.Contains(errMsg, "context window") ||
			strings.Contains(errMsg, "context_window") ||
			strings.Contains(errMsg, "maximum context length") ||
			strings.Contains(errMsg, "token limit") ||
			strings.Contains(errMsg, "too many tokens") ||
			strings.Contains(errMsg, "max_tokens") ||
			strings.Contains(errMsg, "invalidparameter") ||
			strings.Contains(errMsg, "prompt is too long") ||
			strings.Contains(errMsg, "request too large"))

		if isTransientError && retry < maxRetries {
			backoff := time.Duration(retry+1) * time.Duration(backoffSecs) * time.Second
			al.emitEvent(
				runtimeevents.KindAgentLLMRetry,
				ts.eventMeta("runTurn", "turn.llm.retry"),
				LLMRetryPayload{
					Attempt:    retry + 1,
					MaxRetries: maxRetries,
					Reason:     retryReason,
					Error:      err.Error(),
					Backoff:    backoff,
				},
			)
			logger.WarnCF("agent", "Transient LLM error, retrying after backoff", map[string]any{
				"error":   err.Error(),
				"reason":  retryReason,
				"retry":   retry,
				"backoff": backoff.String(),
			})
			if sleepErr := sleepWithContext(turnCtx, backoff); sleepErr != nil {
				if ts.hardAbortRequested() {
					_ = ts.requestHardAbort()
					return ControlBreak, nil
				}
				err = sleepErr
				break
			}
			continue
		}

		if isContextError && retry < maxRetries && !ts.opts.NoHistory {
			al.emitEvent(
				runtimeevents.KindAgentLLMRetry,
				ts.eventMeta("runTurn", "turn.llm.retry"),
				LLMRetryPayload{
					Attempt:    retry + 1,
					MaxRetries: maxRetries,
					Reason:     "context_limit",
					Error:      err.Error(),
				},
			)
			logger.WarnCF(
				"agent",
				"Context window error detected, attempting compression",
				map[string]any{
					"error": err.Error(),
					"retry": retry,
				},
			)

			if retry == 0 && !constants.IsInternalChannel(ts.channel) {
				al.bus.PublishOutbound(ctx, outboundMessageForTurn(
					ts,
					"Context window exceeded. Compressing history and retrying...",
				))
			}

			compactReq := &CompactHookRequest{
				SessionKey: ts.sessionKey,
				Reason:     ContextCompressReasonRetry,
				Budget:     ts.agent.ContextWindow,
			}
			if p.Hooks != nil {
				compactReq, _ = p.Hooks.BeforeCompact(ctx, compactReq)
			}
			if compactErr := p.ContextManager.Compact(ctx, &CompactRequest{
				SessionKey: compactReq.SessionKey,
				Reason:     compactReq.Reason,
				Budget:     compactReq.Budget,
			}); compactErr != nil {
				logger.WarnCF("agent", "Context overflow compact failed", map[string]any{
					"session_key": ts.sessionKey,
					"error":       compactErr.Error(),
				})
			}
			ts.refreshRestorePointFromSession(ts.agent)
			if asmResp, asmErr := p.ContextManager.Assemble(ctx, &AssembleRequest{
				SessionKey: ts.sessionKey,
				Budget:     ts.agent.ContextWindow,
				MaxTokens:  ts.agent.MaxTokens,
			}); asmErr == nil && asmResp != nil {
				exec.history = asmResp.History
				exec.summary = asmResp.Summary
			}
			contextualSkills := ts.activeSkills
			if ts.agent.ContextBuilder != nil {
				contextualSkills = ts.agent.ContextBuilder.ResolveActiveSkillsForContext(ts.activeSkills)
			}
			ts.recordSkillContextSnapshot(skillContextTriggerContextRetryRebuild, contextualSkills)
			stableHistory, protectedTurnTail := splitHistoryForActiveTurn(
				exec.history,
				ts.persistedMessagesSnapshot(),
			)
			buildMessages := func(trimmedHistory []providers.Message) []providers.Message {
				fullHistory := append(append([]providers.Message(nil), trimmedHistory...), protectedTurnTail...)
				rebuildPromptReq := promptBuildRequestForTurn(ts, fullHistory, exec.summary, "", nil, p.Cfg)
				rebuildPromptReq.ActiveSkills = append([]string(nil), contextualSkills...)
				rebuilt := ts.agent.ContextBuilder.BuildMessagesFromPrompt(rebuildPromptReq)
				return resolveMediaRefs(
					rebuilt,
					p.MediaStore,
					maxMediaSize,
					len(rebuilt)-len(protectedTurnTail),
				)
			}
			originalHistoryCount := len(exec.history)
			var fit bool
			var trimmedStableHistory []providers.Message
			trimmedStableHistory, exec.callMessages, fit = trimHistoryToFitContextWindow(
				stableHistory,
				func(trimmedHistory []providers.Message) []providers.Message {
					rebuilt := buildMessages(trimmedHistory)
					if exec.gracefulTerminal {
						return append(append([]providers.Message(nil), rebuilt...), ts.interruptHintMessage())
					}
					return rebuilt
				},
				ts.agent.ContextWindow,
				exec.providerToolDefs,
				ts.agent.MaxTokens,
			)
			exec.history = append(trimmedStableHistory, protectedTurnTail...)
			exec.messages = buildMessages(trimmedStableHistory)
			exec.currentTurnStart = len(exec.messages) - len(protectedTurnTail)
			if exec.gracefulTerminal {
				msgs := append([]providers.Message(nil), exec.messages...)
				exec.callMessages = append(msgs, ts.interruptHintMessage())
			}
			if dropped := originalHistoryCount - len(exec.history); dropped > 0 {
				logger.WarnCF("agent", "Trimmed rebuilt history after context retry compaction", map[string]any{
					"session_key":     ts.sessionKey,
					"retry":           retry,
					"dropped_msgs":    dropped,
					"remaining_msgs":  len(exec.history),
					"context_window":  ts.agent.ContextWindow,
					"max_tokens":      ts.agent.MaxTokens,
					"still_overlimit": !fit,
				})
			} else if !fit {
				logger.WarnCF("agent", "Context still exceeds budget after retry compaction rebuild", map[string]any{
					"session_key":         ts.sessionKey,
					"retry":               retry,
					"history_msgs":        len(exec.history),
					"protected_turn_msgs": len(protectedTurnTail),
					"context_window":      ts.agent.ContextWindow,
					"max_tokens":          ts.agent.MaxTokens,
				})
			}
			if !fit {
				err = fmt.Errorf(
					"context window still exceeded after retry compaction; refusing to drop active turn messages: %w",
					err,
				)
				break
			}
			continue
		}
		break
	}

	if err != nil {
		al.emitEvent(
			runtimeevents.KindAgentError,
			ts.eventMeta("runTurn", "turn.error"),
			ErrorPayload{
				Stage:   "llm",
				Message: err.Error(),
			},
		)
		logger.ErrorCF("agent", "LLM call failed",
			map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"model":     exec.llmModel,
				"error":     err.Error(),
			})
		return ControlBreak, fmt.Errorf("LLM call failed after retries: %w", err)
	}

	// AfterLLM hook
	if p.Hooks != nil {
		originalResponseContent := exec.response.Content // Save before hook may overwrite — needed by HookActionReplay
		llmResp, decision := p.Hooks.AfterLLM(turnCtx, &LLMHookResponse{
			Meta:     ts.eventMeta("runTurn", "turn.llm.response"),
			Context:  cloneTurnContext(ts.turnCtx),
			Model:    exec.llmModel,
			Response: exec.response,
		})
		switch decision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmResp != nil && llmResp.Response != nil {
				exec.response = llmResp.Response
			}
		case HookActionReplay:
			// Diagnostic auto-recovery (Step 2): hook detected malformed tool call,
			// injects recovery message via response.Content, and requests retry.
			//
			// Phase 2 wiring: we keep the pre-Phase-1 message-mutation semantics for
			// the FIRST iteration (append original assistant + recovery to exec.messages
			// so the LLM has context), then hand off to handleHookReplay which runs a
			// BoundedRetry loop within the same iteration. Subsequent retries (if the
			// hook keeps returning Replay) append more context to exec.messages without
			// burning the main iteration budget. See plan
			// same-iteration-replay-loop-with-reusable-boundedretry-primitive-20260717.
			cancelConfiguredStreamingLLM(turnCtx, exec)
			if llmResp != nil && llmResp.Response != nil {
				exec.response = llmResp.Response
			}
			if originalResponseContent != "" {
				exec.messages = append(exec.messages, providers.Message{
					Role:    "assistant",
					Content: originalResponseContent,
				})
			}
			recoveryContent := ""
			if exec.response != nil && exec.response.Content != "" {
				recoveryContent = exec.response.Content
			}
			if recoveryContent != "" {
				exec.messages = append(exec.messages, providers.Message{
					Role:    "user",
					Content: recoveryContent,
				})
			}
			return p.handleHookReplay(ctx, turnCtx, ts, exec, iteration)
		case HookActionAbortTurn:
			cancelConfiguredStreamingLLM(turnCtx, exec)
			exec.abortedByHook = true
			return ControlBreak, nil
		case HookActionHardAbort:
			cancelConfiguredStreamingLLM(turnCtx, exec)
			_ = ts.requestHardAbort()
			exec.abortedByHardAbort = true
			return ControlBreak, nil
		}
	}

	// Post-LLM processing (save finishReason, reasoning, emit event, tool-call
	// path). Extracted to proceedPastLLM so handleHookReplay can route the same
	// processing on a recovered response without duplicating the logic at this
	// call site. See plan
	// same-iteration-replay-loop-with-reusable-boundedretry-primitive-20260717.
	return p.proceedPastLLM(ctx, turnCtx, ts, exec, iteration)
}


func (p *Pipeline) applyBeforeLLMModelRewrite(ts *turnState, exec *turnExecution) {
	if p == nil || ts == nil || ts.agent == nil || exec == nil {
		return
	}
	rawModel := strings.TrimSpace(exec.llmModel)
	if rawModel == "" {
		return
	}

	defaultProvider := "openai"
	if p.Cfg != nil {
		if provider := strings.TrimSpace(p.Cfg.Agents.Defaults.Provider); provider != "" {
			defaultProvider = provider
		}
	}
	defaultProvider = effectiveDefaultProvider(defaultProvider)
	candidates := resolveModelCandidates(p.Cfg, defaultProvider, rawModel, nil)
	exec.activeCandidates = candidates
	exec.activeModel = resolvedCandidateModel(candidates, rawModel)
	exec.llmModel = exec.activeModel
	exec.activeModelConfig = resolveActiveModelConfig(p.Cfg, ts.agent.Workspace, candidates, rawModel, defaultProvider)
}

// callLLMCore performs a single LLM call with streaming + fallback candidate
// support. It was extracted from the CallLLM method as part of Phase 2 so that
// the handleHookReplay helper can re-invoke the same call path during
// same-iteration replay without invoking the outer retry loop or bumping the
// iteration counter. See plan
// same-iteration-replay-loop-with-reusable-boundedretry-primitive-20260717.
func (p *Pipeline) callLLMCore(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	messagesForCall []providers.Message,
	toolDefsForCall []providers.ToolDefinition,
	iteration int,
) (*providers.LLMResponse, error) {
	providerCtx, providerCancel := context.WithCancel(turnCtx)
	ts.setProviderCancel(providerCancel)
	defer func() {
		providerCancel()
		ts.clearProviderCancel(providerCancel)
	}()

	p.al.activeRequestsInc()
	defer p.al.activeRequestsDec()

	if response, handled, streamErr := p.tryConfiguredStreamingLLM(
		providerCtx,
		ts,
		exec,
		messagesForCall,
		toolDefsForCall,
	); handled {
		return response, streamErr
	}

	runCandidate := func(
		ctx context.Context,
		candidate providers.FallbackCandidate,
	) (*providers.LLMResponse, error) {
		candidateProvider, err := providerForFallbackCandidate(
			ts.agent,
			exec.activeProvider,
			exec.activeCandidates,
			candidate.Provider,
			candidate.Model,
		)
		if err != nil {
			return nil, err
		}
		callOpts := shallowCloneLLMOptions(exec.llmOpts)
		delete(callOpts, "thinking_level")
		candidateCfg := resolveActiveModelConfig(
			p.Cfg,
			ts.agent.Workspace,
			[]providers.FallbackCandidate{candidate},
			candidate.Model,
			p.Cfg.Agents.Defaults.Provider,
		)
		candidateThinking := thinkingSettingsFromModelConfig(candidateCfg)
		applyThinkingOption(callOpts, candidateProvider, candidateThinking, true, ts.agent.ID)
		exec.suppressReasoning = shouldSuppressReasoningFor(candidateThinking)
		return candidateProvider.Chat(ctx, messagesForCall, toolDefsForCall, candidate.Model, callOpts)
	}

	if len(exec.activeCandidates) > 1 && p.Fallback != nil {
		var (
			fbResult *providers.FallbackResult
			fbErr    error
		)
		if hasMediaRefs(messagesForCall) {
			fbResult, fbErr = p.Fallback.ExecuteImage(
				providerCtx,
				exec.activeCandidates,
				func(ctx context.Context, provider, model string) (*providers.LLMResponse, error) {
					candidate := providers.FallbackCandidate{Provider: provider, Model: model}
					for _, configured := range exec.activeCandidates {
						if configured.Provider == provider && configured.Model == model {
							candidate = configured
							break
						}
					}
					return runCandidate(ctx, candidate)
				},
			)
		} else {
			fbResult, fbErr = p.Fallback.ExecuteCandidate(
				providerCtx,
				exec.activeCandidates,
				runCandidate,
			)
		}
		if fbErr != nil {
			return nil, fbErr
		}
		if fbResult.Provider != "" && len(fbResult.Attempts) > 0 {
			logger.InfoCF(
				"agent",
				fmt.Sprintf("Fallback: succeeded with %s/%s after %d attempts",
					fbResult.Provider, fbResult.Model, len(fbResult.Attempts)+1),
				map[string]any{"agent_id": ts.agent.ID, "iteration": iteration},
			)
		}
		for _, candidate := range exec.activeCandidates {
			if candidate.StableKey() != fbResult.IdentityKey {
				continue
			}
			exec.llmModelName = resolvedCandidateModelName(
				[]providers.FallbackCandidate{candidate},
				exec.llmModelName,
			)
			break
		}
		return fbResult.Response, nil
	}
	return exec.activeProvider.Chat(providerCtx, messagesForCall, toolDefsForCall, exec.llmModel, exec.llmOpts)
}

// proceedPastLLM handles the post-LLM-response pipeline: saving finishReason
// to turnState, suppressing reasoning, publishing pico reasoning, emitting
// the LLM response event, then dispatching to either the no-tool-call path
// (direct answer or steering) or the tool-call normalization path.
//
// Phase 2 refactor: extracted from CallLLM so that handleHookReplay can return
// ControlContinue and the caller can chain into the same processing without
// having to repeat all of this code at the call site.
func (p *Pipeline) proceedPastLLM(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) (Control, error) {
	al := p.al

	// Save finishReason to turnState for SubTurn truncation detection
	if innerTS := turnStateFromContext(ctx); innerTS != nil {
		innerTS.SetLastFinishReason(exec.response.FinishReason)
		if exec.response.Usage != nil {
			innerTS.SetLastUsage(exec.response.Usage)
		}
	}

	if exec.suppressReasoning {
		exec.response.Reasoning = ""
		exec.response.ReasoningContent = ""
		exec.response.ReasoningDetails = nil
	}
	reasoningContent := responseReasoningContent(exec.response)
	shouldPublishPicoToolCallInterim := ts.channel == "pico" && len(exec.response.ToolCalls) > 0
	if shouldPublishPicoToolCallInterim {
		// Pico tool-call turns publish their reasoning/content/tool summary as a
		// structured sequence after the tool-call payload is normalized below.
	} else if ts.channel == "pico" {
		if exec.streamingPublisher != nil && exec.streamingPublisher.ReasoningPublished() {
			if err := exec.streamingPublisher.FinalizeReasoning(turnCtx, reasoningContent); err != nil {
				logger.WarnCF("agent", "Failed to finalize streamed pico reasoning", map[string]any{
					"channel": ts.channel,
					"chat_id": ts.chatID,
					"error":   err.Error(),
				})
			}
		} else {
			// Publish pico thoughts before the turn context is canceled at return time.
			// The async variant can race with turn teardown and intermittently drop the
			// thought message in CI even though the LLM produced reasoning content.
			al.publishPicoReasoning(turnCtx, reasoningContent, ts.chatID, ts.sessionKey, exec.llmModelName)
		}
	} else {
		go al.handleReasoning(
			turnCtx,
			reasoningContent,
			ts.channel,
			al.targetReasoningChannelID(ts.channel),
		)
	}
	al.emitEvent(
		runtimeevents.KindAgentLLMResponse,
		ts.eventMeta("runTurn", "turn.llm.response"),
		LLMResponsePayload{
			ContentLen:   len(exec.response.Content),
			ToolCalls:    len(exec.response.ToolCalls),
			HasReasoning: exec.response.Reasoning != "" || exec.response.ReasoningContent != "" || len(exec.response.ReasoningDetails) > 0,
		},
	)

	llmResponseFields := map[string]any{
		"agent_id":       ts.agent.ID,
		"iteration":      iteration,
		"content_chars":  len(exec.response.Content),
		"tool_calls":     len(exec.response.ToolCalls),
		"reasoning":      exec.response.Reasoning,
		"target_channel": al.targetReasoningChannelID(ts.channel),
		"channel":        ts.channel,
	}
	if exec.response.Usage != nil {
		llmResponseFields["prompt_tokens"] = exec.response.Usage.PromptTokens
		llmResponseFields["completion_tokens"] = exec.response.Usage.CompletionTokens
		llmResponseFields["total_tokens"] = exec.response.Usage.TotalTokens
	}
	logger.DebugCF("agent", "LLM response", llmResponseFields)

	// No-tool-call path: steering check and direct response
	if len(exec.response.ToolCalls) == 0 || exec.gracefulTerminal {
		// Phase 5: evaluate goal-lifecycle recovery triggers (empty text + text-only streak).
		// Only fires when a goal is active. RecoveryAction determines whether we
		// re-invoke LLM in the same iteration, force-complete, or archive.
		if ts.hasGoal() && !exec.gracefulTerminal {
			if action, msg := evaluateRecovery(ts, RecoveryContext{
				Phase:                 string(ts.currentGoalPhase()),
				Iteration:             iteration,
				TextEmpty:             exec.response.Content == "",
				HasToolCalls:          false,
				MaxIterations:         ts.iterationCap,
				ToolKnowledgeRegistry: ts.agent.Tools,
			}); action != RecoveryNone {
				return p.handleGoalRecovery(ctx, turnCtx, ts, exec, iteration, action, msg)
			}
		}

		responseContent := exec.response.Content
		// Phase 11: capture LLM text output so complete_goal can use it as
		// the final user reply when the tool's `summary` arg is empty.
		ts.assistantText = responseContent
		if responseContent == "" && reasoningContent != "" && ts.channel != "pico" {
			// Only fall back to ReasoningContent when the channel has a
			// configured reasoning_channel_id. Without one, publishing
			// reasoning as the main response leaks the model's internal
			// thinking into the user's primary chat. The coordinator's
			// DefaultResponse fallback (turn_coord.go) handles the empty
			// case instead.
			if reasoningTargetChatID := al.targetReasoningChannelID(ts.channel); reasoningTargetChatID != "" {
				responseContent = reasoningContent
			} else {
				logger.WarnCF("agent", "Reasoning content suppressed: no reasoning_channel_id configured for channel; relying on DefaultResponse fallback",
					map[string]any{
						"channel":         ts.channel,
						"chat_id":         ts.chatID,
						"reasoning_chars": len(reasoningContent),
						"iteration":       iteration,
					})
			}
		}
		if steerMsgs := al.dequeueSteeringMessagesForScope(ts.sessionKey); len(steerMsgs) > 0 {
			cancelConfiguredStreamingLLM(turnCtx, exec)
			logger.InfoCF("agent", "Steering arrived after direct LLM response; continuing turn",
				map[string]any{
					"agent_id":       ts.agent.ID,
					"iteration":      iteration,
					"steering_count": len(steerMsgs),
				})
			exec.pendingMessages = append(exec.pendingMessages, steerMsgs...)
			return ControlContinue, nil
		}

		exec.finalContent = responseContent
		logger.InfoCF("agent", "LLM response without tool calls (direct answer)",
			map[string]any{
				"agent_id":      ts.agent.ID,
				"iteration":     iteration,
				"content_chars": len(exec.finalContent),
			})
		return ControlBreak, nil
	}
	cancelConfiguredStreamingLLM(turnCtx, exec)

	// Tool-call path: normalize and prepare for tool execution
	exec.normalizedToolCalls = make([]providers.ToolCall, 0, len(exec.response.ToolCalls))
	for _, tc := range exec.response.ToolCalls {
		exec.normalizedToolCalls = append(exec.normalizedToolCalls, providers.NormalizeToolCall(tc))
	}

	toolNames := make([]string, 0, len(exec.normalizedToolCalls))
	for _, tc := range exec.normalizedToolCalls {
		toolNames = append(toolNames, tc.Name)
	}
	logger.InfoCF("agent", "LLM requested tool calls",
		map[string]any{
			"agent_id":  ts.agent.ID,
			"tools":     toolNames,
			"count":     len(exec.normalizedToolCalls),
			"iteration": iteration,
		})

	exec.allResponsesHandled = len(exec.normalizedToolCalls) > 0
	assistantMsg := providers.Message{
		Role:             "assistant",
		Content:          exec.response.Content,
		ModelName:        exec.llmModelName,
		ReasoningContent: reasoningContent,
		ReasoningDetails: exec.response.ReasoningDetails,
	}
	for _, tc := range exec.normalizedToolCalls {
		argumentsJSON, _ := json.Marshal(tc.Arguments)
		toolFeedbackExplanation := toolFeedbackExplanationForToolCall(
			exec.response,
			tc,
			exec.messages,
		)
		extraContent := tc.ExtraContent
		if strings.TrimSpace(toolFeedbackExplanation) != "" {
			if extraContent == nil {
				extraContent = &providers.ExtraContent{}
			}
			extraContent.ToolFeedbackExplanation = toolFeedbackExplanation
		}
		thoughtSignature := ""
		if tc.Function != nil {
			thoughtSignature = tc.Function.ThoughtSignature
		}
		assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, providers.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Name: tc.Name,
			Function: &providers.FunctionCall{
				Name:             tc.Name,
				Arguments:        string(argumentsJSON),
				ThoughtSignature: thoughtSignature,
			},
			ExtraContent:     extraContent,
			ThoughtSignature: thoughtSignature,
		})
	}
	exec.messages = append(exec.messages, assistantMsg)
	if !ts.opts.NoHistory {
		ts.agent.Sessions.AddFullMessage(ts.sessionKey, assistantMsg)
		ts.recordPersistedMessage(assistantMsg)
		ts.ingestMessage(turnCtx, al, assistantMsg)
	}
	if shouldPublishPicoToolCallInterim {
		al.publishPicoToolCallInterim(
			turnCtx,
			ts,
			exec.llmModelName,
			reasoningContent,
			exec.response.Content,
			exec.normalizedToolCalls,
		)
	}
	return ControlToolLoop, nil
}

func providerForFallbackCandidate(
	agent *AgentInstance,
	activeProvider providers.LLMProvider,
	activeCandidates []providers.FallbackCandidate,
	provider string,
	model string,
) (providers.LLMProvider, error) {
	if agent != nil {
		if cp, ok := agent.CandidateProviders[providers.ModelKey(provider, model)]; ok && cp != nil {
			return cp, nil
		}
	}
	if activeProvider == nil {
		return nil, fmt.Errorf("fallback model %q has no active provider", model)
	}
	return activeProvider, nil
}

func transientLLMRetryReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}

	if failErr := providers.ClassifyError(err, "", ""); failErr != nil {
		switch failErr.Reason {
		case providers.FailoverTimeout:
			if failErr.Status >= 500 {
				return "server_error", true
			}
			return "timeout", true
		case providers.FailoverNetwork:
			return "network", true
		case providers.FailoverRateLimit, providers.FailoverOverloaded:
			return "rate_limit", true
		}
	}

	errMsg := strings.ToLower(err.Error())
	if errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(errMsg, "deadline exceeded") ||
		strings.Contains(errMsg, "client.timeout") ||
		strings.Contains(errMsg, "timed out") ||
		strings.Contains(errMsg, "timeout exceeded") {
		return "timeout", true
	}

	if strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "broken pipe") ||
		strings.Contains(errMsg, "no such host") ||
		strings.Contains(errMsg, "network is unreachable") ||
		strings.Contains(errMsg, "read tcp") ||
		strings.Contains(errMsg, "write tcp") ||
		strings.Contains(errMsg, "eof") {
		return "network", true
	}

	return "", false
}

// handleHookReplay runs the BoundedRetry loop for AfterLLM hook replay
// decisions. The caller (HookActionReplay case in CallLLM) has already:
//
//  1. Called the LLM once (initial response in exec.response).
//  2. Run the AfterLLM hook, which returned HookActionReplay (response
//     modified to recovery content).
//  3. Appended the original assistant content + recovery to exec.messages
//     so the LLM has proper context for the re-call.
//
// handleHookReplay then loops up to ts.replayCap times (BoundedRetry primitive),
// re-calling the LLM and re-firing the hook. On each Replay decision, the
// latest pre-hook LLM response + new recovery content is appended to
// exec.messages so the next LLM call has context. The loop exits when:
//
//   - Hook returns Continue/Modify → RetryDecisionDone → coordinator runs
//     the recovered response through the tool loop.
//   - Hook returns AbortTurn/HardAbort → RetryDecisionAbort → coordinator
//     breaks the turn.
//   - BoundedRetry cap is exhausted → graceful ControlContinue + warn
//     event so the coordinator bumps iteration and retries from a clean slate
//     (preserves the original "replay burns an iteration" behavior as a
//     safety net for runaway hooks).
//
// Phase 2 of plan
// same-iteration-replay-loop-with-reusable-boundedretry-primitive-20260717.
func (p *Pipeline) handleHookReplay(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
) (Control, error) {
	al := p.al

	decision, err := BoundedRetry(ctx, RetryConfig{
		Name:        "hook_replay",
		MaxAttempts: ts.replayCap,
		OnRetry: func(rc RetryContext, _ string) {
			al.emitEvent(
				runtimeevents.KindAgentLLMReplayAttempt,
				ts.eventMeta("runTurn", "turn.llm.replay.attempt"),
				LLMReplayAttemptPayload{
					Iteration: iteration,
					Attempt:   rc.Attempt + 1, // 1-indexed for humans
					Remaining: rc.Remaining,
					ElapsedMs: rc.Elapsed.Milliseconds(),
				},
			)
			logger.DebugCF("agent", "Replaying LLM (same iteration)", map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"attempt":   rc.Attempt + 1,
				"remaining": rc.Remaining,
			})
		},
		OnExhausted: func(rc RetryContext) {
			al.emitEvent(
				runtimeevents.KindAgentLLMReplayExhausted,
				ts.eventMeta("runTurn", "turn.llm.replay.exhausted"),
				LLMReplayExhaustedPayload{
					Iteration: iteration,
					Attempts:  rc.MaxAttempts,
					ElapsedMs: rc.Elapsed.Milliseconds(),
				},
			)
			logger.WarnCF("agent", "Replay cap exhausted, treating as continue", map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"cap":       rc.MaxAttempts,
			})
			// Phase 6 Hook 3: BoundedRetry exhausted during LLM replay. If a
			// goal is active, archive it via Hook 1 (finalizeGoalOnTurnEnd)
			// with the bexhausted reason so the next session sees why this
			// goal did not complete. Phase 5's goalArchiveRequested flag is
			// also set so callers in the iteration loop can detect it.
			if ts.hasGoal() {
				ts.goalArchiveRequested = true
				if err := ts.finalizeGoalOnTurnEnd(GoalAbortReasonBexhausted + ":hook_replay"); err != nil {
					logger.WarnCF("agent", "Hook 3: finalizeGoalOnTurnEnd failed",
						map[string]any{"error": err.Error()})
				}
			}
		},
	}, func(ctx context.Context, rc RetryContext) (RetryDecision, error) {
		// Make the LLM call. callLLMCore uses exec.callMessages which already
		// contains the appended context from the caller (first attempt) or
		// the previous attempt's HookActionReplay handler (subsequent attempts).
		resp, err := p.callLLMCore(ctx, turnCtx, ts, exec, exec.callMessages, exec.providerToolDefs, iteration)
		if err != nil {
			return RetryDecisionAbort, err
		}
		if resp != nil {
			exec.response = resp
		}

		// Save the pre-hook LLM response content (for context appending if the
		// hook requests another Replay).
		preHookContent := ""
		if exec.response != nil {
			preHookContent = exec.response.Content
		}

		// Run AfterLLM hook on the new response.
		if p.Hooks == nil {
			return RetryDecisionDone, nil
		}
		llmResp, hookDecision := p.Hooks.AfterLLM(turnCtx, &LLMHookResponse{
			Meta:     ts.eventMeta("runTurn", "turn.llm.response"),
			Context:  cloneTurnContext(ts.turnCtx),
			Model:    exec.llmModel,
			Response: exec.response,
		})

		switch hookDecision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmResp != nil && llmResp.Response != nil {
				exec.response = llmResp.Response
			}
			return RetryDecisionDone, nil
		case HookActionReplay:
			if llmResp != nil && llmResp.Response != nil {
				exec.response = llmResp.Response
			}
			// Append the previous LLM response (assistant) + new recovery
			// (user) so the next LLM call has full context.
			if preHookContent != "" {
				exec.messages = append(exec.messages, providers.Message{
					Role:    "assistant",
					Content: preHookContent,
				})
			}
			recoveryContent := ""
			if exec.response != nil {
				recoveryContent = exec.response.Content
			}
			if recoveryContent != "" {
				exec.messages = append(exec.messages, providers.Message{
					Role:    "user",
					Content: recoveryContent,
				})
			}
			cancelConfiguredStreamingLLM(turnCtx, exec)
			return RetryDecisionRetry, nil
		case HookActionAbortTurn:
			exec.abortedByHook = true
			return RetryDecisionAbort, nil
		case HookActionHardAbort:
			_ = ts.requestHardAbort()
			exec.abortedByHardAbort = true
			return RetryDecisionAbort, nil
		}
		return RetryDecisionDone, nil
	})

	if err != nil {
		return ControlBreak, err
	}
	if decision == RetryDecisionAbort {
		if exec.abortedByHardAbort {
			return ControlBreak, nil
		}
		if exec.abortedByHook {
			return ControlBreak, fmt.Errorf("hook requested turn abort")
		}
	}
	// RetryDecisionDone OR exhausted retry → return ControlContinue.
	// Coordinator bumps iteration normally (for Done: pre-LLM processing
	// will run with the recovered response; for exhausted: clean re-attempt).
	return ControlContinue, nil
}

// handleGoalRecovery processes a recovery trigger using a same-iteration
// BoundedRetry loop, restoring the original Phase 5 design intent (plan
// §5.3, recovery_goal.go:8).
//
// Wire-up replaces applyRecoveryAction (Phase 12.10 iter-bump pattern) with
// a BoundedRetry loop inside this function. The caller (CallLLM outer loop)
// receives ControlContinue (retry successful, response populated) or
// ControlBreak (recovery exhausted → goal archived) without any iteration
// bump in either case.
//
// Per-attempt flow:
//  1. Caller already invoked callLLMCore once; response is in exec.response.
//  2. Evaluate recovery triggers on that response.
//  3. If RecoveryNone → return Done (response is good, caller continues).
//  4. If RecoveryRetrySameIteration / RecoveryForceComplete → set
//     pendingRecoveryMessage, return Retry.
//  5. If RecoveryArchiveGoal → archive + return Abort (turn ends).
//
// On retry, BoundedRetry invokes the wrapped func again, which:
//  - Rebuilds callMessages with the pendingRecoveryMessage injected
//  - Re-runs callLLMCore
//  - Re-evaluates recovery
//
// Exhausted → archive goal + return ControlBreak.
func (p *Pipeline) handleGoalRecovery(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	iteration int,
	action RecoveryAction,
	msg string,
) (Control, error) {
	al := p.al
	logFields := map[string]any{
		"agent_id":  ts.agent.ID,
		"iteration": iteration,
		"action":    actionName(action),
	}
	if msg != "" {
		logFields["message"] = msg
	}
	logger.InfoCF("agent", "Goal-lifecycle recovery action (same-iter BoundedRetry)", logFields)

	// Reset the empty-response counter so the BoundedRetry loop can re-evaluate
	// each attempt. The initial caller (CallLLM line 713-728) sets the counter
	// when the first trigger fires; the in-iter retry is the SAME event, so
	// we reset and let evaluateRecovery decide per-attempt.
	ts.emptyResponseRecoverySent = false

	decision, err := BoundedRetry(ctx, RetryConfig{
		Name:        "goal_recovery",
		MaxAttempts: 3,
		OnRetry: func(rc RetryContext, _ string) {
			logger.InfoCF("agent", "Goal recovery retry (same iteration)", map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"attempt":   rc.Attempt + 1,
				"remaining": rc.Remaining,
			})
		},
		OnExhausted: func(rc RetryContext) {
			logger.WarnCF("agent", "Goal recovery exhausted, archiving goal", map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"cap":       rc.MaxAttempts,
			})
			if ts.hasGoal() && !ts.goalArchiveRequested {
				ts.goalArchiveRequested = true
				if finalizeErr := ts.finalizeGoalOnTurnEnd(GoalAbortReasonBexhausted + ":goal_recovery"); finalizeErr != nil {
					logger.WarnCF("agent", "Goal recovery: finalizeGoalOnTurnEnd failed",
						map[string]any{"error": finalizeErr.Error()})
				}
			}
		},
	}, func(attemptCtx context.Context, rc RetryContext) (RetryDecision, error) {
		// Evaluate recovery triggers on the (possibly fresh) response.
		// For attempts > 0, re-invoke callLLMCore with pendingRecoveryMessage
		// injected into callMessages. Attempt 0 uses the response already
		// populated by the caller (CallLLM line 162-318).
		if rc.Attempt > 0 {
			logger.InfoCF("agent", "handleGoalRecovery attempt>0 enter", map[string]any{
				"agent_id":          ts.agent.ID,
				"iteration":         iteration,
				"attempt":           rc.Attempt,
				"pendingMsg":        msg,
				"callMsgsLenBefore": len(exec.callMessages),
			})
			ts.pendingRecoveryMessage = msg
			// Rebuild callMessages — mirrors the line 73-109 logic
			// (interruptHintMessage + pendingRecoveryMessage injection).
			exec.callMessages = append([]providers.Message{}, exec.messages...)
			if msg := ts.interruptHintMessage(); msg.Content != "" {
				exec.callMessages = append(exec.callMessages, msg)
			}
			if ts.pendingRecoveryMessage != "" {
				exec.callMessages = append(exec.callMessages, providers.Message{
					Role:    "user",
					Content: ts.pendingRecoveryMessage,
				})
			}
			// Clear pendingRecoveryMessage after consumption so subsequent
			// attempts within this iteration do not re-inject the same hint.
			ts.pendingRecoveryMessage = ""

			resp, callErr := p.callLLMCore(attemptCtx, turnCtx, ts, exec, exec.callMessages, exec.providerToolDefs, iteration)
			if callErr != nil {
				return RetryDecisionAbort, callErr
			}
			if resp != nil {
				exec.response = resp
			}
		}

		// If LLM called a tool, recovery is not needed (tools will be
		// executed by the caller).
		if len(exec.response.ToolCalls) > 0 && !exec.gracefulTerminal {
			return RetryDecisionDone, nil
		}

		// No active goal or graceful terminal — no recovery.
		if !ts.hasGoal() || exec.gracefulTerminal {
			return RetryDecisionDone, nil
		}

		// Evaluate recovery triggers on the (possibly fresh) response.
		evalCtx := RecoveryContext{
			Phase:                 string(ts.currentGoalPhase()),
			Iteration:             iteration,
			TextEmpty:             exec.response.Content == "",
			HasToolCalls:          false,
			MaxIterations:         ts.iterationCap,
			ToolKnowledgeRegistry: ts.agent.Tools,
		}
		nextAction, nextMsg := evaluateRecovery(ts, evalCtx)

		switch nextAction {
		case RecoveryNone:
			// RecoveryNone after we've already fired at least one retry in
			// this iteration means the cap is reached (e.g. 2 of 2 empty-response
			// retries used). This is the "exhausted" terminal — archive the
			// goal and exit the retry loop with RetryDecisionAbort, so the
			// caller returns ControlBreak and the loop terminates the turn.
			if ts.emptyResponseRecoverySent || ts.textOnlySoftRetriesDone > 0 || ts.textOnlyHardRetriesDone > 0 || ts.toolExecRecoveryAttempts != nil {
				logFields["action"] = "exhausted_archive"
				logger.InfoCF("agent", "Goal recovery exhausted in same iteration", logFields)
				ts.goalArchiveRequested = true
				return RetryDecisionAbort, nil
			}
			return RetryDecisionDone, nil
		case RecoveryRetrySameIteration, RecoveryForceComplete:
			// Update msg for next attempt's callMessages injection.
			msg = nextMsg
			return RetryDecisionRetry, nil
		case RecoveryArchiveGoal:
			if !ts.goalArchiveRequested {
				ts.goalArchiveRequested = true
				if finalizeErr := ts.finalizeGoalOnTurnEnd(GoalAbortReasonBexhausted + ":goal_recovery"); finalizeErr != nil {
					logger.WarnCF("agent", "Goal recovery (archive in retry): finalizeGoalOnTurnEnd failed",
						map[string]any{"error": finalizeErr.Error()})
				}
			}
			return RetryDecisionAbort, nil
		}
		return RetryDecisionDone, nil
	})

	if err != nil {
		return ControlBreak, err
	}
	if decision == RetryDecisionAbort {
		return ControlBreak, nil
	}
	_ = al
	return ControlContinue, nil
}

// actionName returns a human-readable label for a RecoveryAction.
func actionName(a RecoveryAction) string {
	switch a {
	case RecoveryNone:
		return "none"
	case RecoveryRetrySameIteration:
		return "retry_next_iteration"
	case RecoveryForceComplete:
		return "force_complete"
	case RecoveryArchiveGoal:
		return "archive_goal"
	}
	return "unknown"
}

// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/agent/goal"
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/routing"
)

func (al *AgentLoop) runTurn(ctx context.Context, ts *turnState, pipeline *Pipeline) (result turnResult, err error) {
	turnCtx, turnCancel := context.WithCancel(ctx)
	defer turnCancel()
	ts.setTurnCancel(turnCancel)

	// Inject turnState and AgentLoop into context so tools (e.g. spawn) can retrieve them.
	turnCtx = withTurnState(turnCtx, ts)
	turnCtx = WithAgentLoop(turnCtx, al)

	// Phase 6 Hook 2 — defer recover() is the panic safety net for the entire
	// turn. If any tool, LLM call, or iteration panics, we catch it, log the
	// stack trace, finalize the in-flight goal (Hook 1), and convert the panic
	// into a normal error return so callers (agent.go:575) see a clean failure
	// instead of crashing the agent loop. Goal archive reason is runTurn_panic
	// so the next view_goal call shows what happened.
	//
	// We use named return (result, err) so the recover handler can populate
	// `err` and zero `result` — this preserves the existing call-site contract
	// (spawnSubTurn's outer defer at subturn.go:456 still sees an error and
	// treats the sub-turn as failed, NOT completed).
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			logger.ErrorCF("agent", "runTurn panic recovered",
				map[string]any{
					"agent_id": ts.agent.ID,
					"session":  ts.sessionKey,
					"panic":    fmt.Sprintf("%v", r),
					"stack":    string(stack),
				})
			// Hook 1: archive in-flight goal so the next session knows.
			if finalizeErr := ts.finalizeGoalOnTurnEnd(GoalAbortReasonRunTurnPanic); finalizeErr != nil {
				logger.WarnCF("agent", "runTurn recover: finalizeGoalOnTurnEnd failed",
					map[string]any{"error": finalizeErr.Error()})
			}
			// Preserve call-site contract: panic → err set + result cleared.
			result = turnResult{}
			err = fmt.Errorf("runTurn panicked: %v", r)
		}
	}()

	al.registerActiveTurn(ts)
	defer al.clearActiveTurn(ts)

	if al.takePendingStop(ts.sessionKey) {
		_ = ts.requestHardAbort()
	}

	turnStatus := TurnEndStatusCompleted
	defer func() {
		attemptedSkills := ts.attemptedSkillsSnapshot()
		skillContextSnapshots := ts.skillContextSnapshotsSnapshot()
		finalSuccessfulPath := []string(nil)
		if turnStatus == TurnEndStatusCompleted {
			if latest := ts.latestSkillContextSnapshot(); len(latest) > 0 {
				finalSuccessfulPath = latest
			} else {
				finalSuccessfulPath = append([]string(nil), attemptedSkills...)
			}
		}
		// Phase 12.30: log turn-end marker. Captures final iter count and
		// outcome summary so live-verify over agent_debug.log can scan for
		// end-of-turn without grepping KindAgentTurnEnd events. Fires before
		// emitEvent so the order is: agent_debug=turn_end → gateway=turn.end.
		if IsAgentDebugEnabled() {
			AgentDebugTurnEnd(ts.turnID, ts.sessionKey, ts.currentIteration(), string(turnStatus), ts.goalFinalized)
		}
		al.emitEvent(
			runtimeevents.KindAgentTurnEnd,
			ts.eventMeta("runTurn", "turn.end"),
			TurnEndPayload{
				Status:                turnStatus,
				Workspace:             ts.workspace,
				Iterations:            ts.currentIteration(),
				Duration:              time.Since(ts.startedAt),
				FinalContentLen:       ts.finalContentLen(),
				UserMessage:           ts.userMessage,
				FinalContent:          ts.finalContentSnapshot(),
				ActiveSkills:          append([]string(nil), ts.activeSkills...),
				AttemptedSkills:       attemptedSkills,
				FinalSuccessfulPath:   finalSuccessfulPath,
				SkillContextSnapshots: skillContextSnapshots,
				ToolKinds:             ts.toolKindsSnapshot(),
				ToolExecutions:        ts.toolExecutionsSnapshot(),
			},
		)
	}()

	if ts.hardAbortRequested() {
		turnStatus = TurnEndStatusAborted
		return al.abortTurn(ts)
	}

	al.emitEvent(
		runtimeevents.KindAgentTurnStart,
		ts.eventMeta("runTurn", "turn.start"),
		TurnStartPayload{
			UserMessage: ts.userMessage,
			MediaCount:  len(ts.media),
		},
	)

	// SetupTurn extracts the one-time initialization phase.
	exec, err := pipeline.SetupTurn(turnCtx, ts)
	if err != nil {
		return turnResult{}, err
	}

	// Phase 12.25: cross-turn archive-and-reset pre-loop hook.
	// Enforces per-turn goal scope: archive any unarchived prior-turn
	// active goal (durable side-effect), then reset in-memory turn-state
	// goal flags so the new turn starts with a clean slate regardless of
	// whether the prior turn finished or was interrupted. Replaces the
	// Phase 12.9 cross-turn final-report-iter mechanism with explicit
	// per-turn scope. Order: archive FIRST → reset SECOND (see
	// archiveAndResetPriorTurnGoal doc for rationale).
	if err := archiveAndResetPriorTurnGoal(al, ts); err != nil {
		// archiveAndResetPriorTurnGoal already logs; continue regardless.
	}

	// Convenience references to exec fields used throughout the turn loop.
	messages := exec.messages
	pendingMessages := exec.pendingMessages
	maxMediaSize := pipeline.Cfg.Agents.Defaults.GetMaxMediaSize()
	finalContent := exec.finalContent

	// Phase 12.9: pre-loop cap-extend for the post-complete_goal final-report
	// iter. Without this hook, the loop condition below would be FALSE in
	// the common cap-reached case (currentIteration == iterationCap, no
	// pending messages, no graceful interrupt), and the loop would exit
	// before the body — and the goalFinalized block — ever ran. By bumping
	// iterationCap to allow exactly one more iteration, the loop condition
	// evaluates TRUE and the body runs with phase=Final + allowlist=[],
	// giving the LLM its last chance to provide a final user-facing report.
	// The flag itself is set at the END of the body (post-body marker),
	// not here — so the in-loop goalFinalized block below can detect
	// "already sent" and break cleanly on the second pass.
	if ts.goalFinalized && !ts.postCompleteGoalReportSent {
		if cap := ts.iteration + 1; cap > ts.iterationCap {
			ts.iterationCap = cap
		}
	}

	// Phase 12.20.1 fix (A): force-exit after final-report iter has run.
	// Without this guard, if the LLM emits a 2nd complete_goal tool call
	// at iter N+1 instead of producing a final report, the post-body
	// marker still flips postCompleteGoalReportSent=true (line below), but
	// the loop continues because iterationCap was bumped to N+1. Result:
	// wasted iterations + publishPicoToolCallInterim emissions until the
	// LLM finally emits text or hits cap. Bounding the loop on
	// "goal finalized AND report already sent" makes iter N+1 strictly
	// the final iter regardless of LLM tool-call behavior.
	for (ts.currentIteration() < ts.iterationCap && !(ts.goalFinalized && ts.postCompleteGoalReportSent)) || len(exec.pendingMessages) > 0 || func() bool {
		graceful, _ := ts.gracefulInterruptRequested()
		return graceful
	}() {
		if ts.hardAbortRequested() {
			turnStatus = TurnEndStatusAborted
			return al.abortTurn(ts)
		}
		// Phase 11 + Phase 12.9: complete_goal sets ts.goalFinalized = true
		// to short-circuit the per-turn loop. The pre-loop hook above
		// extends iterationCap so this block + the body actually runs
		// once after complete_goal. We DO NOT continue/break here — let
		// the body run normally with phase=Final + allowlist=[] (which
		// currentGoalPhase() will compute because goalFinalized=true).
		// The post-body marker (below) flips postCompleteGoalReportSent
		// to true once the LLM has actually emitted the final report.
		if ts.goalFinalized && !ts.postCompleteGoalReportSent {
			ts.pendingFinalReportIter = true
		}

		iteration := ts.currentIteration() + 1
		ts.setIteration(iteration)
		ts.resetReplayCount()
		// Phase 12.10: reset per-iteration recovery counters. Recovery
		// retries bump ts.iteration (ControlContinue → continue → loop top
		// → setIteration(+1)), so each new iteration gets a fresh slate.
		// Previously these counters were sticky across iterations, causing
		// recovery to silently skip after the first fire in a turn.
		ts.emptyResponseRecoveryCount = 0
		ts.toolExecRecoveryAttempts = nil
		ts.lastToolResult = nil // Phase 12.28.3: clear stale tool result from previous iter so checkToolExecErrorRecovery sees only this iter's result.
		// Phase 12.13: reset phase-stuck counters at iteration boundary so
		// a fresh iteration gets a clean slate. Same lifecycle as the
		// sibling counters above (reset on iter bump).
		ts.setGoalFailCount = 0
		ts.goalProgressFailCount = 0
		ts.completeGoalFailCount = 0
		// Phase 2: reset SignatureFailureTracker counters at turn boundary so
		// a new turn starts with a fresh escalation slate. Mirrors the
		// nanobot "per-run scope" pattern (Decision 4) and matches the
		// resetReplayCount hook above (both are "per-turn state reset").
		if ts.agent != nil && ts.agent.Tools != nil {
			ts.agent.Tools.ResetSignatureFailures(ts.channel, ts.chatID)
		}
		ts.setPhase(TurnPhaseRunning)

		if iteration > 1 {
			// For subsequent iterations, read from exec.pendingMessages which
			// is where ExecuteTools (or initial poll) deposits steering.
			// We do NOT call dequeueSteeringMessagesForScope here because
			// steering was already consumed from al.steering by ExecuteTools.
			if len(exec.pendingMessages) > 0 {
				pendingMessages = append(pendingMessages, exec.pendingMessages...)
				exec.pendingMessages = nil
			}
		} else if !ts.opts.SkipInitialSteeringPoll {
			if steerMsgs := al.dequeueSteeringMessagesForScopeWithFallback(ts.sessionKey); len(steerMsgs) > 0 {
				pendingMessages = append(pendingMessages, steerMsgs...)
			}
		}

		// Check if parent turn has ended (SubTurn support from HEAD)
		if ts.parentTurnState != nil && ts.IsParentEnded() {
			if !ts.critical {
				logger.InfoCF("agent", "Parent turn ended, non-critical SubTurn exiting gracefully", map[string]any{
					"agent_id":  ts.agentID,
					"iteration": iteration,
					"turn_id":   ts.turnID,
				})
				break
			}
			logger.InfoCF("agent", "Parent turn ended, critical SubTurn continues running", map[string]any{
				"agent_id":  ts.agentID,
				"iteration": iteration,
				"turn_id":   ts.turnID,
			})
		}

		// Poll for pending SubTurn results (from HEAD)
		if ts.pendingResults != nil {
			select {
			case result, ok := <-ts.pendingResults:
				if ok && result != nil && result.ForLLM != "" {
					content := al.cfg.FilterSensitiveData(result.ForLLM)
					msg := subTurnResultPromptMessage(content)
					pendingMessages = append(pendingMessages, msg)
				}
			default:
				// No results available
			}
		}

		// Inject pending steering messages
		if len(pendingMessages) > 0 {
			resolvedPending := resolveMediaRefs(pendingMessages, al.mediaStore, maxMediaSize, 0)
			totalContentLen := 0
			for i, pm := range pendingMessages {
				messages = append(messages, resolvedPending[i])
				totalContentLen += len(pm.Content)
				if !ts.opts.NoHistory {
					ts.agent.Sessions.AddFullMessage(ts.sessionKey, pm)
					ts.recordPersistedMessage(pm)
					ts.ingestMessage(turnCtx, al, pm)
				}
				logger.InfoCF("agent", "Injected steering message into context",
					map[string]any{
						"agent_id":    ts.agent.ID,
						"iteration":   iteration,
						"content_len": len(pm.Content),
						"media_count": len(pm.Media),
					})
			}
			al.emitEvent(
				runtimeevents.KindAgentSteeringInjected,
				ts.eventMeta("runTurn", "turn.steering.injected"),
				SteeringInjectedPayload{
					Count:           len(pendingMessages),
					TotalContentLen: totalContentLen,
				},
			)
			// Clear exec.pendingMessages after injection so InitialSteeringMessages
			// are not re-injected on subsequent iterations (Issue 2 fix).
			exec.pendingMessages = nil

			// When steering messages arrive, re-extract the task summary for
			// the new goal. Block until extraction completes so the LLM sees
			// the updated task immediately in this iteration.
			{
				// Cancel any in-flight background extraction so it cannot
				// overwrite sessionTaskSummary after we set the new one.
				if exec.taskExtractCancel != nil {
					logger.DebugCF(
						"agent",
						"Task extraction: canceling background extraction for steering re-extract",
						nil,
					)
					exec.taskExtractCancel()
				}

				var steeringText strings.Builder
				for i, pm := range pendingMessages {
					if i > 0 {
						steeringText.WriteString("\n")
					}
					steeringText.WriteString(pm.Content)
				}

				// Phase 11: no injectedTaskSummary to refresh. The next
				// iteration's LLM call will see the steering message in
				// its history; per-turn goal scope means no cross-turn
				// reminder slot to update.
				_ = steeringText
			}
		}
		// Always sync messages into exec.messages so CallLLM sees the updated state
		exec.messages = messages

		logger.DebugCF("agent", "LLM iteration",
			map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"max":         ts.iterationCap,
			})

		// Phase 12.30: log per-iter phase + tools-visible snapshot for
		// live-verify of multi-iter turns. Disabled by default — set
		// PICOCLAW_AGENT_DEBUG=1 to enable. The currentGoalPhase()
		// call here also primes the allowlist gate for the upcoming
		// LLM call (via ts.applyPhaseAllowlist inside ts.setPhase).
		//
		// tools_total here is the full registered registry count
		// (unfiltered). The actual count the LLM sees — after
		// SetAllowlist + ToProviderDefs projection in
		// Pipeline.setupLLMRequest — is logged separately on the
		// llm_call event so live-verify can compare the two and
		// detect regressions like the Phase 12.3.1 SetAllowlist(nil)
		// wire bug.
		if IsAgentDebugEnabled() {
			currentPhase := ts.currentGoalPhase()
			toolsTotal := len(ts.agent.Tools.ToProviderDefs())
			AgentDebugPhaseStart(ts.turnID, ts.sessionKey, iteration, currentPhase, toolsTotal, ts.goalFinalized)
		}

		// Phase 12.33: rebuild messages[0] (system prompt) when the goal
		// phase changed since the last build. Without this hook,
		// messages[0] persists from iter 0 across all iters of a turn,
		// even when GoalPhase transitions (Open → Checkpoint at
		// iter=MaxIter). The LLM at iter 5 (phase=Checkpoint) would see
		// the iter-0 SET-phase prompt with "call set_goal" instruction,
		// contradicting the actual CHECKPOINT-phase allowlist
		// ([goal_progress, complete_goal]). See
		// turn_state_phase_rebuild.go for full design rationale.
		//
		// Sync the rebuilt messages back to exec.messages so CallLLM
		// (line below) sees the updated system prompt.
		messages = ts.maybeRebuildPromptForPhaseChange(messages, exec, pipeline.Cfg, iteration)
		exec.messages = messages

		// Execute LLM call via Pipeline
		ts.setPhase(TurnPhaseRunning)
		ctrl, callErr := pipeline.CallLLM(ctx, turnCtx, ts, exec, iteration)
		if callErr != nil {
			turnStatus = TurnEndStatusError
			return turnResult{}, callErr
		}
		messages = exec.messages
		pendingMessages = exec.pendingMessages
		finalContent = exec.finalContent

		switch ctrl {
		case ControlContinue:
			// Phase 12.9: post-body marker for early-Continue path (no
			// tool exec was needed — e.g. text-only LLM response that
			// still needs a final report). If this iter is the post-
			// complete_goal final-report iter, flip the flag so the
			// next loop pass exits cleanly.
			if ts.pendingFinalReportIter {
				ts.postCompleteGoalReportSent = true
				ts.pendingFinalReportIter = false
			}
			// Phase 12.35: apply deferred iterationCap extend if any tool
			// (e.g. goal_progress) staged one. Defensive — should be no-op
			// on the ControlContinue path since no tool ran, but covers the
			// edge case where a future LLM text-only path can stage extend
			// (currently impossible but cheap to keep for symmetry).
			_, _ = al.applyDeferredExtend(ts)
			continue
		case ControlBreak:
			// Hard abort: delegate to abortTurn (sets TurnEndStatusAborted)
			if exec.abortedByHardAbort {
				turnStatus = TurnEndStatusAborted
				return al.abortTurn(ts)
			}
			// Hook abort (HookActionAbortTurn): sets TurnEndStatusError, returns error
			if exec.abortedByHook {
				turnStatus = TurnEndStatusError
				return turnResult{}, fmt.Errorf("hook requested turn abort")
			}
			// Phase 12.6.0: empty response → prefer goal.Summary over DefaultResponse
			// (ordering fix; see applyFallbackForEmptyResponse for the full fallback chain).
			if finalContent == "" {
				finalContent = al.applyFallbackForEmptyResponse(ts)
			}
			result, finalizeErr := pipeline.Finalize(ctx, turnCtx, ts, exec, turnStatus, finalContent)
			if finalizeErr != nil {
				turnStatus = TurnEndStatusError
			}
			return result, finalizeErr
		case ControlToolLoop:
			// Execute tools via Pipeline
			toolCtrl := pipeline.ExecuteTools(ctx, turnCtx, ts, exec, iteration)
			switch toolCtrl {
			case ToolControlContinue:
				// Phase 5 trigger #3: tool-execution error recovery.
				// Evaluate recovery for the most recent tool result (if any).
				// Phase 12.22: wire checkToolExecErrorRecovery same-iter BoundedRetry
				// for restricted-allowlist phases (Set/Checkpoint/Final). The
				// recovery helper returns (toolName, msg) where:
				//   - toolName != "" → counter exhausted → archive goal
				//   - toolName == "" && msg != "" → counter not exhausted
				//     → retry (Phase 12.18 returns msg here for callers to
				//     use as recovery prompt)
				// For restricted phases, retry attempts MUST stay in the same
				// iteration so LLM has a chance to correct before the loop
				// exits at iterationCap — otherwise the user sees the
				// generic toolLimitResponse fallback. Open phase keeps the
				// historical iter-bump behavior.
				if ts.hasGoal() {
					archiveTool, archiveMsg := checkToolExecErrorRecovery(ts, exec)
					if archiveTool != "" {
						ts.goalArchiveRequested = true
						logger.InfoCF("agent", "Goal archive triggered by tool-exec error retry exhaustion",
							map[string]any{
								"agent_id": ts.agent.ID,
								"tool":     archiveTool,
								"message":  archiveMsg,
							})
						turnStatus = TurnEndStatusError
						return turnResult{}, fmt.Errorf("goal archive requested after tool-exec retries exhausted for %s", archiveTool)
					} else if archiveMsg != "" {
						// Phase 12.37 GAP #4: isRestricted gate REMOVED.
						// Tool-exec errors at OPEN now route through
						// retryLLMForBlockedTool same-iter retry instead
						// of next-iter carry. Per D2, the goal archives
						// after 3 failed same-iter retries at OPEN;
						// transient hint already tells the LLM to
						// wait/pivot, and allowAll onWrongTool treats ANY
						// non-gate-blocked real tool as success — archive
						// only fires if the LLM re-picks a gate-blocked
						// tool 3× (C1 fix in pipeline_llm.go).
						currentPhase := ts.currentGoalPhase()
						_ = currentPhase
						{
							// Phase 12.25 §X2: skip same-iter BoundedRetry
							// when graceful interrupt is active. The user
							// has signaled END turn — retrying just wastes
							// an LLM call before the graceful interrupt
							// path takes over. Continue to the next iter
							// (or exit if pendingMessages empty + cap hit),
							// letting the existing graceful-interrupt
							// handling pick up the abort.
							if ts.gracefulInterrupt || ts.gracefulTerminalUsed {
								logger.InfoCF("agent", "Phase 12.25 §X2: skipping retryLLMForBlockedTool due to graceful interrupt",
									map[string]any{
										"agent_id":            ts.agent.ID,
										"phase":               string(currentPhase),
										"graceful_interrupt":  ts.gracefulInterrupt,
										"graceful_term_used":  ts.gracefulTerminalUsed,
									})
								// Phase 12.36: apply deferred extend before graceful-interrupt exit.
								// Same-bug-class as retry path's line 551. Idempotent if no request staged.
								_, _ = al.applyDeferredExtend(ts)
								continue
							}
							// Same-iter BoundedRetry wrap. The blocked tool
							// call produced an error result — re-call LLM with
							// the recovery hint injected and execute the
							// re-picked tool INSIDE the helper (Phase 12.42
							// Path 4 wiring). BoundedRetry gives up to
							// ToolExecErrorRetryCap=3 attempts; on exhaustion
							// the helper archives with the matching
							// phase-stuck reason (C5).
							resolvedRetry := resolveAgentToolAllowlistWithPhase(ts.agent.Definition, currentPhase)
							ctrl, retryErr := pipeline.retryExecuteToolChain(
								ctx, turnCtx, ts, exec, iteration, archiveMsg, resolvedRetry, string(currentPhase))
							if retryErr != nil {
								logger.WarnCF("agent", "Goal recovery handler errored at restricted phase",
									map[string]any{
										"agent_id": ts.agent.ID,
										"phase":    string(currentPhase),
										"err":      retryErr.Error(),
									})
								return turnResult{}, retryErr
							}
							switch ctrl {
							case ControlBreak:
								// Phase 12.42 (G10/C7): ControlBreak from the
								// helper is NOT always archive-exhaustion —
								// Step 3 can break early with a clean terminal
								// cause (hard abort / hook abort / all
								// responses handled). Mirror the outer
								// ToolControlBreak flag precedence here so a
								// clean abort isn't misreported as a fake
								// "archive after exhaustion" error.
								if exec.abortedByHardAbort {
									turnStatus = TurnEndStatusAborted
									return al.abortTurn(ts)
								}
								if exec.abortedByHook {
									turnStatus = TurnEndStatusError
									return turnResult{}, fmt.Errorf("hook requested turn abort")
								}
								if exec.allResponsesHandled {
									messages = exec.messages
									result, finalizeErr := pipeline.Finalize(ctx, turnCtx, ts, exec, turnStatus, "")
									if finalizeErr != nil {
										turnStatus = TurnEndStatusError
									}
									return result, finalizeErr
								}
								// Archive-after-exhaustion path:
								// retryExecuteToolChain stamped
								// goalArchiveRequested=true and set the
								// matching phase-stuck abort reason. Honor
								// it here so finalizeGoalOnTurnEnd picks up
								// the right AbortReason.
								logger.InfoCF("agent", "Goal archive triggered by tool-exec recovery exhaustion at restricted phase",
									map[string]any{
										"agent_id":        ts.agent.ID,
										"phase":           string(currentPhase),
										"archive_request": ts.goalArchiveRequested,
									})
								turnStatus = TurnEndStatusError
								return turnResult{}, fmt.Errorf("goal archive requested after tool-exec recovery exhaustion at %s", currentPhase)
							case ControlContinue, ControlToolLoop:
								// Same-iter retry succeeded — the helper
								// ALREADY executed any re-picked tool (Step 3
								// inside retryExecuteToolChainOnce, Phase 12.42
								// G7). Text-only success consumed content via
								// exec.response. Re-read exec.messages so the
								// post-tool-exec continuation path below sees
								// the fresh tool results.
								messages = exec.messages
								if ts.pendingFinalReportIter {
									ts.postCompleteGoalReportSent = true
									ts.pendingFinalReportIter = false
								}
								// Phase 12.36: apply deferred extend (same-bug-class as line 474).
								// Cannot fall through to outer block (line 569) because retry path
								// intentionally skips the pendingRecoveryMessage = archiveMsg set
								// at lines 564-566 (recovery was already resolved by retry success).
								_, _ = al.applyDeferredExtend(ts)
								continue
							default:
								logger.WarnCF("agent", "Unexpected control signal from goal recovery at restricted phase",
									map[string]any{
										"agent_id": ts.agent.ID,
										"phase":    string(currentPhase),
										"ctrl":     ctrl,
									})
							}
						}
						// Open phase: legacy behavior — pendingRecoveryMessage
						// is set so the next iteration's recovery prompt
						// fires, no same-iter wrap needed.
						// Phase 12.37 GAP #4: REMOVED. OPEN now uses
						// retryLLMForBlockedTool above (same-iter
						// BoundedRetry) — no next-iter carry path.
						_ = archiveMsg
					}
				}
				// Re-read exec.messages since ExecuteTools may have updated it
				// (added tool results/skipped messages) before returning ControlContinue
				messages = exec.messages
				// Phase 12.9: post-body marker. If this iter was flagged as
				// the post-complete_goal final-report iter (set at top of
				// body), flip postCompleteGoalReportSent to true so the
				// next loop pass sees the flag set and the in-loop
				// goalFinalized check (added below) can break the loop.
				// The clear of pendingFinalReportIter is defensive — the
				// flag is only meaningful for the duration of one body
				// pass, so we reset it here rather than waiting for the
				// next iter's top-of-body.
				if ts.pendingFinalReportIter {
					ts.postCompleteGoalReportSent = true
					ts.pendingFinalReportIter = false
				}
				// Phase 12.35: apply deferred iterationCap extend if
				// goal_progress staged one during this iter's tool run.
				// Without this hook, the staged extend sits forever and
				// phase never gets the budget it requested.
				_, _ = al.applyDeferredExtend(ts)
				continue
			case ToolControlBreak:
				// Hard abort: delegate to abortTurn (sets TurnEndStatusAborted)
				if exec.abortedByHardAbort {
					turnStatus = TurnEndStatusAborted
					return al.abortTurn(ts)
				}
				// Hook abort (HookActionAbortTurn): sets TurnEndStatusError, returns error
				if exec.abortedByHook {
					turnStatus = TurnEndStatusError
					return turnResult{}, fmt.Errorf("hook requested turn abort")
				}
				// ExecuteTools returned ControlBreak:
				// - allResponsesHandled=true: finalize without DefaultResponse (exec.finalContent empty)
				// - allResponsesHandled=false: coordinator applies DefaultResponse before finalize
				if exec.allResponsesHandled {
					finalContent = ""
				}
				result, finalizeErr := pipeline.Finalize(ctx, turnCtx, ts, exec, turnStatus, finalContent)
				if finalizeErr != nil {
					turnStatus = TurnEndStatusError
				}
				return result, finalizeErr
			}
		}
	}

	if ts.hardAbortRequested() {
		turnStatus = TurnEndStatusAborted
		return al.abortTurn(ts)
	}

	if finalContent == "" {
		finalContent = al.applyFallbackForEmptyResponse(ts)
	}

	// Check hard abort before finalizing (may have been set during tool execution)
	if ts.hardAbortRequested() {
		turnStatus = TurnEndStatusAborted
		return al.abortTurn(ts)
	}

	// Phase 12.6.0 ordering fix: previously the post-loop block (line 362-368)
	// set finalContent = opts.DefaultResponse BEFORE the Phase 11 goal.Summary
	// fallback could run, so users saw "The model returned an empty response"
	// instead of the success summary. Now we route the empty case through
	// applyFallbackForEmptyResponse (preference order: goal.Summary →
	// toolLimitResponse → opts.DefaultResponse).
	if finalContent == "" {
		finalContent = al.applyFallbackForEmptyResponse(ts)
	}

	result, err = pipeline.Finalize(ctx, turnCtx, ts, exec, turnStatus, finalContent)
	if err != nil {
		turnStatus = TurnEndStatusError
	}
	return result, err
}

// applyFallbackForEmptyResponse returns the user-facing message to use when
// turnCoord exits with no assistant prose. Caller must only invoke this when
// finalContent == "" so we can pick the highest-priority fallback.
//
// Preference order (Phase 12.6.0 + Phase 12.13):
//
//  1. goal.Summary — if the most recent iteration completed a goal and the LLM
//     did not emit a free-form prose reply, prefer the persisted goal summary
//     so the user actually sees the success message they were promised by
//     complete_goal.
//  2. Phase-stuck message (Phase 12.13) — if the goal was archived with one
//     of the phase-stuck abort_reason values (goal_set_stuck,
//     goal_checkpoint_stuck, goal_final_stuck), return the matching
//     user-facing message that names the stuck phase. This takes priority
//     over toolLimitResponse/DefaultResponse because the user needs to know
//     the failure was a phase lifecycle stall, not a generic empty response.
//  3. toolLimitResponse — if we hit the iteration cap with no prose, explain
//     the limit was reached (better than the generic "empty response").
//  4. opts.DefaultResponse — last resort; matches the pre-Phase 11 behavior
//     when LLM hits an empty response with no goal context.
func (al *AgentLoop) applyFallbackForEmptyResponse(ts *turnState) string {
	// Phase 12.6.0 ordering fix: prefer goal.Summary over DefaultResponse
	// when goal was finalized without prose. See helper doc above for
	// full rationale + preference order.
	goalSummary := ""
	hadGoal := false
	if ts.goalFinalized && ts.assistantText == "" {
		if st := al.goalStore(); st != nil {
			if g, err := st.ReadAny(ts.sessionKey); err == nil && g != nil && g.Summary != "" {
				goalSummary = g.Summary
				hadGoal = true
			}
		}
	}
	log.Printf("DEBUG[12.16] applyFallbackForEmptyResponse session=%s goalFinalized=%v assistantText=%q hadGoal=%v goalSummaryLen=%d iter=%d iterCap=%d ts.iteration=%d", ts.sessionKey, ts.goalFinalized, ts.assistantText, hadGoal, len(goalSummary), ts.currentIteration(), ts.iterationCap, ts.iteration)
	if goalSummary != "" {
		return goalSummary
	}
	// Phase 12.13: phase-stuck message beats the generic ErrorResponse.
	// We only show this when the goal was archived with a phase-stuck
	// abort_reason AND the LLM did not produce a free-form prose reply
	// (assistantText == ""). Format: title + fail count + last error.
	if msg := al.phaseStuckFallbackMessage(ts); msg != "" {
		return msg
	}
	if ts.currentIteration() >= ts.iterationCap {
		return toolLimitResponse
	}
	return ts.opts.DefaultResponse
}

// phaseStuckFallbackMessage (Phase 12.13) returns the matching phase-stuck
// message if the goal was archived with a phase-stuck abort_reason. Returns
// empty string if the goal has no phase-stuck abort_reason.
func (al *AgentLoop) phaseStuckFallbackMessage(ts *turnState) string {
	if st := al.goalStore(); st != nil {
		if g, err := st.ReadAny(ts.sessionKey); err == nil && g != nil &&
			g.Status == goal.StatusAborted && g.AbortReason != "" {
			lastErr := ts.lastPhaseStuckError
			if lastErr == "" {
				lastErr = "(unknown — see goal archive log)"
			}
			switch g.AbortReason {
			case GoalPhaseSetStuckAbortReason:
				return fmt.Sprintf(GoalPhaseSetStuckMessage, ts.setGoalFailCount, lastErr)
			case GoalPhaseCheckpointStuckAbortReason:
				return fmt.Sprintf(GoalPhaseCheckpointStuckMessage, ts.goalProgressFailCount, lastErr)
			case GoalPhaseFinalStuckAbortReason:
				return fmt.Sprintf(GoalPhaseFinalStuckMessage, ts.completeGoalFailCount, lastErr)
			}
		}
	}
	return ""
}

func (al *AgentLoop) abortTurn(ts *turnState) (turnResult, error) {
	ts.setPhase(TurnPhaseAborted)
	if !ts.opts.NoHistory {
		if err := ts.restoreSession(ts.agent); err != nil {
			al.emitEvent(
				runtimeevents.KindAgentError,
				ts.eventMeta("abortTurn", "turn.error"),
				ErrorPayload{
					Stage:   "session_restore",
					Message: err.Error(),
				},
			)
			return turnResult{}, err
		}
	}
	return turnResult{status: TurnEndStatusAborted}, nil
}

func (al *AgentLoop) selectCandidates(
	agent *AgentInstance,
	userMsg string,
	history []providers.Message,
) (candidates []providers.FallbackCandidate, model string, tier routing.Tier) {
	if agent.Router == nil {
		return agent.Candidates, resolvedCandidateModel(agent.Candidates, agent.Model), routing.TierHeavy
	}

	_, tier, score := agent.Router.SelectModel(userMsg, history, agent.Model)

	switch tier {
	case routing.TierLight:
		if len(agent.LightCandidates) > 0 {
			logger.InfoCF("agent", "Model routing: light model selected",
				map[string]any{
					"agent_id":    agent.ID,
					"light_model": agent.Router.LightModel(),
					"score":       score,
				})
			return agent.LightCandidates, resolvedCandidateModel(
				agent.LightCandidates,
				agent.Router.LightModel(),
			), routing.TierLight
		}
	case routing.TierMedium:
		if len(agent.MediumCandidates) > 0 {
			logger.InfoCF("agent", "Model routing: medium model selected",
				map[string]any{
					"agent_id":     agent.ID,
					"medium_model": agent.Router.MediumModel(),
					"score":        score,
				})
			return agent.MediumCandidates, resolvedCandidateModel(
				agent.MediumCandidates,
				agent.Router.MediumModel(),
			), routing.TierMedium
		}
	}

	logger.DebugCF("agent", "Model routing: primary model selected",
		map[string]any{
			"agent_id": agent.ID,
			"score":    score,
		})
	return agent.Candidates, resolvedCandidateModel(agent.Candidates, agent.Model), routing.TierHeavy
}

func (al *AgentLoop) resolveContextManager() ContextManager {
	name := al.cfg.Agents.Defaults.ContextManager
	if name == "" || name == "legacy" {
		return &legacyContextManager{al: al}
	}
	factory, ok := lookupContextManager(name)
	if !ok {
		logger.WarnCF("agent", "Unknown context manager, falling back to legacy", map[string]any{
			"name": name,
		})
		return &legacyContextManager{al: al}
	}
	cm, err := factory(al.cfg.Agents.Defaults.ContextManagerConfig, al)
	if err != nil {
		logger.WarnCF("agent", "Failed to create context manager, falling back to legacy", map[string]any{
			"name":  name,
			"error": err.Error(),
		})
		return &legacyContextManager{al: al}
	}
	return cm
}

func (al *AgentLoop) askSideQuestion(
	ctx context.Context,
	agent *AgentInstance,
	opts *processOptions,
	question string,
) (string, error) {
	if agent == nil {
		return "", fmt.Errorf("askSideQuestion: no agent available for /btw")
	}

	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("askSideQuestion: %w", fmt.Errorf("Usage: /btw <question>"))
	}

	if opts != nil {
		normalizeProcessOptionsInPlace(opts)
		resolved, err := resolveTurnProfileOptions(al.GetConfig(), *opts)
		if err != nil {
			return "", err
		}
		*opts = resolved
	}

	var media []string
	var channel, chatID, senderID, senderDisplayName string
	if opts != nil {
		media = opts.Media
		channel = opts.Channel
		chatID = opts.ChatID
		senderID = opts.SenderID
		senderDisplayName = opts.SenderDisplayName
	}

	// Build messages with context but WITHOUT adding to session history
	var history []providers.Message
	var summary string
	if opts != nil && !opts.NoHistory {
		if resp, err := al.contextManager.Assemble(ctx, &AssembleRequest{
			SessionKey: opts.SessionKey,
			Budget:     agent.ContextWindow,
			MaxTokens:  agent.MaxTokens,
		}); err == nil && resp != nil {
			history = resp.History
			summary = resp.Summary
		}
	}

	var promptReq PromptBuildRequest
	if opts == nil {
		promptReq = PromptBuildRequest{
			History:           history,
			Summary:           summary,
			CurrentMessage:    question,
			Media:             append([]string(nil), media...),
			Channel:           channel,
			ChatID:            chatID,
			SenderID:          senderID,
			SenderDisplayName: senderDisplayName,
		}
	} else {
		promptReq = promptBuildRequestForProcessOptions(
			agent,
			*opts,
			history,
			summary,
			question,
			media,
		)
	}
	promptReq.SuppressToolUseRule = true
	promptReq.ToolUseFallback = false
	messages := agent.ContextBuilder.BuildMessagesFromPrompt(promptReq)

	maxMediaSize := al.GetConfig().Agents.Defaults.GetMaxMediaSize()
	currentTurnStart := len(messages)
	if strings.TrimSpace(question) != "" || len(media) > 0 {
		currentTurnStart = len(messages) - 1
	}
	messages = resolveMediaRefs(messages, al.mediaStore, maxMediaSize, currentTurnStart)

	activeCandidates, activeModel, tier := al.selectCandidates(agent, question, messages)
	selectedModelName := sideQuestionModelName(agent, tier)

	llmOpts := map[string]any{
		"max_tokens":       agent.MaxTokens,
		"temperature":      agent.Temperature,
		"prompt_cache_key": agent.ID + ":btw",
	}

	hookModelChanged := false
	sideSuppressReasoning := false
	callProvider := func(
		ctx context.Context,
		candidate providers.FallbackCandidate,
		model string,
		forceModel bool,
		callMessages []providers.Message,
	) (*providers.LLMResponse, error) {
		baseModelName := selectedModelName
		if forceModel && strings.TrimSpace(model) != "" {
			baseModelName = model
		}
		provider, providerModel, modelCfg, cleanup, err := al.isolatedSideQuestionProvider(
			agent,
			baseModelName,
			candidate,
		)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		if !forceModel || strings.TrimSpace(model) == "" {
			model = providerModel
		}
		callOpts := llmOpts
		settings := thinkingSettingsFromModelConfig(modelCfg)
		sideSuppressReasoning = shouldSuppressReasoningFor(settings)
		if _, exists := callOpts["thinking_level"]; !exists {
			if settings.configured {
				callOpts = shallowCloneLLMOptions(llmOpts)
				applyThinkingOption(callOpts, provider, settings, false, agent.ID)
			}
		}
		return provider.Chat(ctx, callMessages, nil, model, callOpts)
	}

	turnCtx := newTurnContext(nil, nil, nil)
	if opts != nil {
		turnCtx = newTurnContext(opts.Dispatch.InboundContext, opts.Dispatch.RouteResult, opts.Dispatch.SessionScope)
	}
	llmModel := activeModel
	if al.hooks != nil {
		llmReq, decision := al.hooks.BeforeLLM(ctx, &LLMHookRequest{
			Meta: HookMeta{
				Source:      "askSideQuestion",
				TracePath:   "turn.llm.request",
				turnContext: cloneTurnContext(turnCtx),
			},
			Context:          cloneTurnContext(turnCtx),
			Model:            llmModel,
			Messages:         messages,
			Tools:            nil,
			Options:          llmOpts,
			GracefulTerminal: false,
		})
		switch decision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmReq != nil {
				if strings.TrimSpace(llmReq.Model) != "" && llmReq.Model != llmModel {
					hookModelChanged = true
				}
				llmModel = llmReq.Model
				messages = llmReq.Messages
				llmOpts = llmReq.Options
				delete(llmOpts, "native_search")
			}
		case HookActionAbortTurn:
			reason := decision.Reason
			if reason == "" {
				reason = "hook requested turn abort"
			}
			return "", fmt.Errorf("hook aborted turn during before_llm: %s", reason)
		case HookActionHardAbort:
			reason := decision.Reason
			if reason == "" {
				reason = "hook requested turn abort"
			}
			return "", fmt.Errorf("hook aborted turn during before_llm: %s", reason)
		}
	}
	if hookModelChanged {
		// Hook-selected models must not continue through the pre-hook fallback
		// candidate list, otherwise fallback execution would call the original
		// candidate model and silently ignore the hook decision.
		activeCandidates = nil
	}

	callSideLLM := func(callMessages []providers.Message) (*providers.LLMResponse, error) {
		if len(activeCandidates) > 1 && al.fallback != nil {
			fbResult, err := al.fallback.ExecuteCandidate(
				ctx,
				activeCandidates,
				func(ctx context.Context, candidate providers.FallbackCandidate) (*providers.LLMResponse, error) {
					return callProvider(ctx, candidate, candidate.Model, false, callMessages)
				},
			)
			if err != nil {
				return nil, err
			}
			return fbResult.Response, nil
		}

		var candidate providers.FallbackCandidate
		if len(activeCandidates) > 0 {
			candidate = activeCandidates[0]
		}
		return callProvider(ctx, candidate, llmModel, hookModelChanged, callMessages)
	}

	// Retry without media if vision is unsupported
	// Note: Vision retry is only applied to the initial call. If fallback chain
	// is used, vision errors from fallback providers will not trigger retry.
	var resp *providers.LLMResponse
	var err error
	resp, err = callSideLLM(messages)
	if err != nil && hasMediaRefs(messages) && isVisionUnsupportedError(err) {
		al.emitEvent(
			runtimeevents.KindAgentLLMRetry,
			HookMeta{
				Source:      "askSideQuestion",
				TracePath:   "turn.llm.retry",
				turnContext: cloneTurnContext(turnCtx),
			},
			LLMRetryPayload{
				Attempt:    1,
				MaxRetries: 1,
				Reason:     "vision_unsupported",
				Error:      err.Error(),
				Backoff:    0,
			},
		)
		messagesWithoutMedia := stripMessageMedia(messages)
		resp, err = callSideLLM(messagesWithoutMedia)
	}
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}

	// Apply after_llm hooks
	if al.hooks != nil {
		llmResp, decision := al.hooks.AfterLLM(ctx, &LLMHookResponse{
			Meta: HookMeta{
				Source:      "askSideQuestion",
				TracePath:   "turn.llm.response",
				turnContext: cloneTurnContext(turnCtx),
			},
			Context:  cloneTurnContext(turnCtx),
			Model:    llmModel,
			Response: resp,
		})
		switch decision.normalizedAction() {
		case HookActionContinue, HookActionModify:
			if llmResp != nil && llmResp.Response != nil {
				resp = llmResp.Response
			}
		case HookActionAbortTurn, HookActionHardAbort:
			reason := decision.Reason
			if reason == "" {
				reason = "hook requested turn abort"
			}
			return "", fmt.Errorf("hook aborted turn during after_llm: %s", reason)
		}
	}
	if sideSuppressReasoning {
		resp.Reasoning = ""
		resp.ReasoningContent = ""
		resp.ReasoningDetails = nil
	}

	return sideQuestionResponseContent(resp), nil
}

func (al *AgentLoop) isolatedSideQuestionProvider(
	agent *AgentInstance,
	baseModelName string,
	candidate providers.FallbackCandidate,
) (providers.LLMProvider, string, *config.ModelConfig, func(), error) {
	if agent == nil {
		return nil, "", nil, func() {}, fmt.Errorf("isolatedSideQuestionProvider: no agent available for /btw")
	}

	modelCfg, err := al.sideQuestionModelConfig(agent, baseModelName, candidate)
	if err != nil {
		return nil, "", nil, func() {}, fmt.Errorf("isolatedSideQuestionProvider: %w", err)
	}

	factory := al.providerFactory
	if factory == nil {
		factory = providers.CreateProviderFromConfig
	}
	provider, modelID, err := factory(modelCfg)
	if err != nil {
		return nil, "", nil, func() {}, fmt.Errorf("isolatedSideQuestionProvider: %w", err)
	}

	cleanup := func() {
		closeProviderIfStateful(provider)
	}
	return provider, modelID, modelCfg, cleanup, nil
}

func (al *AgentLoop) sideQuestionModelConfig(
	agent *AgentInstance,
	baseModelName string,
	candidate providers.FallbackCandidate,
) (*config.ModelConfig, error) {
	if agent == nil {
		return nil, fmt.Errorf("sideQuestionModelConfig: no agent available for /btw")
	}

	if name := modelAliasFromCandidateIdentityKey(candidate.IdentityKey); name != "" {
		modelCfg, err := resolvedModelConfig(al.GetConfig(), name, agent.Workspace)
		if err == nil {
			return modelCfg, nil
		}
		// Fallback: create a minimal config if lookup fails
	}

	// Older identity keys used provider/model; keep resolving those by model.
	if name := modelNameFromIdentityKey(candidate.IdentityKey); name != "" {
		modelCfg, err := resolvedModelConfig(al.GetConfig(), name, agent.Workspace)
		if err == nil {
			return modelCfg, nil
		}
		// Fallback: create a minimal config if lookup fails
	}

	if candidate.Provider != "" && candidate.Model != "" {
		candidateRef := providers.NormalizeProvider(candidate.Provider) + "/" + candidate.Model
		if modelCfg, err := resolvedModelConfig(al.GetConfig(), candidateRef, agent.Workspace); err == nil {
			return modelCfg, nil
		}
		return &config.ModelConfig{
			ModelName: candidateRef,
			Model:     candidateRef,
			Workspace: agent.Workspace,
		}, nil
	}

	// Otherwise, clean up the base model name and use it
	baseModelName = strings.TrimSpace(baseModelName)
	modelCfg, err := resolvedModelConfig(al.GetConfig(), baseModelName, agent.Workspace)
	if err != nil {
		// Fallback: create a minimal config for test scenarios
		model := strings.TrimSpace(baseModelName)
		if candidate.Model != "" {
			model = candidate.Model
		}
		if candidate.Provider != "" && candidate.Model != "" {
			model = providers.NormalizeProvider(candidate.Provider) + "/" + candidate.Model
		} else {
			model = ensureProtocolModel(model)
		}
		return &config.ModelConfig{
			ModelName: baseModelName,
			Model:     model,
			Workspace: agent.Workspace,
		}, nil
	}

	// If candidate specifies a different provider/model, override
	clone := *modelCfg
	return &clone, nil
}
// applyDeferredExtend applies any staged RequestExtendIterationCap request
// that was set during ExecuteTools. Phase 12.35 wires goal_progress to defer
// the iterationCap bump to end-of-iter so phase does not flip mid-iter
// (CHECKPOINT→OPEN when the resolver reads `iter >= iterationCap`).
//
// Called from the loop body end (after ExecuteTools returns) before
// `continue` to the next iter. Returns the new iteration cap (or 0 if
// no request was staged) and the delta.
//
// If a request was applied, the caller should NOT bump the iter counter
// (otherwise the new cap gets re-clamped before the next iter runs).
// In this runTurn loop the bump happens at the top of body via
// `iteration := ts.currentIteration() + 1; ts.setIteration(iteration)`,
// so simply re-reading ts.currentIteration() at the top of the next
// pass picks up the new cap without explicit bump management here.
func (al *AgentLoop) applyDeferredExtend(ts *turnState) (newCap int, delta int) {
	applied, cap, delta := ts.FlushPendingExtend()
	if !applied {
		return 0, 0
	}
	logger.DebugCF("agent", "Phase 12.35: applied deferred iterationCap extend from goal_progress at end of iter",
		map[string]any{
			"agent_id":   ts.agent.ID,
			"iter":       ts.currentIteration(),
			"new_cap":    cap,
			"delta":      delta,
			"phase":      string(ts.currentGoalPhase()),
		})
	return cap, delta
}


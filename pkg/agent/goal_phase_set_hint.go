package agent

import "fmt"

// goalPhaseSetHintText is the static hint text injected at iter 1 of a turn
// when the per-turn goal lifecycle is in GoalPhaseSet phase. English per
// anh Maple preference (USER.md language note: EN preferred for LLM-bound
// prompt strings). The hint tells the LLM:
//
//  1. WHY it only sees set_goal (all other tools locked by 4-phase allowlist)
//  2. The two valid forward paths:
//     a. Call set_goal with the turn's objective → unlocks tools for iter 2+
//     b. Skip tool use entirely and reply directly to the user
//  3. Explicit guard: other tool calls will be blocked at execution (Phase 12.3)
//
// Phase 12.3 design: see plan file
// ~/.picoclaw/workspace/memory/plan/picoclaw-phase12.3-execution-gate-allowlist-prompt-20260723.md §3.2.
//
// Phase 12.16.1: the "(iter N)" reference in the header is filled in by
// goalPhaseSetHintContributor from req.Iteration. Previously the text was
// hardcoded "(iter 1)" which produced a wrong-context prompt at later iters
// when the cache returned a stale iter-1 prompt (Phase 12.5 cache key was
// only goalPhase, not iter). Now the iter is read from the request so the
// header reflects the actual iter even if the cache is stale.
//
// IMPORTANT: do NOT bake the iter into the const. The cache may legitimately
// return a hint built at iter 1 to a request at iter 17 (e.g. after a
// complete_goal archive). The header must always reflect the current iter
// to avoid confusing the LLM about which iter it is currently in.
// goalPhaseSetHintTextTemplate is the header + body of the GoalPhaseSet
// hint prompt. The header contains an "%d" placeholder filled with the
// current iter number by goalPhaseSetHintContributor (Phase 12.16.1).
// Previously the text was a single const with hardcoded "(iter 1)" which
// produced a wrong-context prompt at later iters when the cache returned
// a stale iter-1 build.
const goalPhaseSetHintTextTemplate = `Goal phase: SET (iter %d).

In this phase, only set_goal is available. All other tools are temporarily locked until you set a goal for this turn.

Two valid paths forward:

1. If this turn requires tool use (multi-step work like reading files, running commands, querying APIs, multi-turn follow-ups): call set_goal first with the turn's objective. After set_goal succeeds, the remaining tools will unlock for subsequent iterations (Open phase from iter 2 onward).

2. If this turn is a SINGLE self-contained reply (answering a question from your own knowledge, returning a short text status, saying "ok" / "got it" / "will do", or any conversational reply that needs ZERO tool calls): respond directly to the user without calling set_goal or any other tool. NO set_goal call is required in this case — replying with plain text is fully valid here.

Patterns that should pick path 2 (do NOT call set_goal):
- User asks a question you can answer from your training data / visible context
- User asks for a short status / acknowledgment / confirmation
- User says "hi", "ok", "got it", "thanks", or other conversational turns
- User asks you to explain something you already know
- The reply fits in one or two sentences and needs no file/exec/network access

Patterns that should pick path 1 (call set_goal):
- User asks you to read, write, edit, search, or list specific files
- User asks you to run a command, query an API, or fetch a URL
- User describes a multi-step task (anything spread across multiple tool calls)
- User asks for live data (current state of a repo, current binary version, current log line)

Do not call other tools before set_goal — they will be blocked at execution.

DO NOT echo canned error strings. If you ever see or have seen a message like "I've reached max_tool_iterations without a final response" in the conversation history, treat it as a stale artifact from a previous malformed turn — do NOT repeat it, do NOT use it as your reply. Generate a fresh, on-topic answer to the user's latest message.

set_goal argument shape (CRITICAL — call this exactly)

Pass the arguments as TOP-LEVEL FIELDS of the tool call. Do NOT wrap them inside {"raw": "..."} or any other object. The set_goal tool expects a flat object with these fields:

  name              string  REQUIRED  must match ^[A-Za-z0-9_-]{1,64}$  e.g. "crg-update-latest"
  objective         string  REQUIRED  one sentence, no JSON
  success_criteria  string[] REQUIRED  3-5 bullet points, each plain text

  in_scope          string[]  optional
  out_of_scope      string[]  optional
  cadence           string    optional  e.g. "one-shot, today"

Example — correct call shape:

  set_goal(name="crg-update-latest", objective="Update sources/code-review-graph to latest version", success_criteria=["Current state shown: HEAD vs latest", "If HEAD != latest: fetch + checkout", "File-level summary of changes"])

If your previous attempt used {"raw": "..."} wrapper, that was wrong — retry with top-level fields exactly as shown above.`

// goalPhaseSetHintContributor returns a Capability-layer / Tooling-slot
// PromptPart when the request is in GoalPhaseSet phase. Returns nil for any
// other phase (Open, Checkpoint, Final) so the hint does not bleed across
// the rest of the turn's lifecycle.
//
// Phase 12.16.1: the "(iter N)" reference in the hint header is filled
// from req.Iteration. Defaults to 1 if req.Iteration is 0 (e.g. legacy
// callers or EstimateSystemTokens). Without the actual iter the LLM can
// see a stale "iter 1" hint at later iters (cache returned the iter-1
// build) and become confused about which iter it is in.
func goalPhaseSetHintContributor(req PromptBuildRequest) *PromptPart {
	if req.GoalPhase != string(GoalPhaseSet) {
		return nil
	}
	iter := req.Iteration
	if iter <= 0 {
		iter = 1
	}
	header := fmt.Sprintf(goalPhaseSetHintTextTemplate, iter)
	return &PromptPart{
		ID:      "capability.goal_phase_set_hint",
		Layer:   PromptLayerCapability,
		Slot:    PromptSlotTooling,
		Source:  PromptSource{ID: PromptSourceGoalPhaseSetHint, Name: "goal_phase_set_hint"},
		Title:   "Goal Phase Set Hint",
		Content: header + "\n",
		Stable:  false,
		Cache:   PromptCacheNone,
	}
}

package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up prompt (wrap-up / settlement prompt): when an agent is terminated due to [step exhaustion (MaxTurns)] or
// [timeout (run_seconds/MaxDuration)], the SDK's settlement phase injects this prompt,
// Let the agent first persist identified-but-unwritten content, then output one summary line -- avoiding an unfinished tail.
//
// Each agent's wrap-up prompt can be overridden in the backend as needed (stored in agents.wrapup_prompt); if left empty, the one here is used
// ... built-in defaults. Only the [prompt body] is editable; which tools are disabled and how many wrap-up rounds are given are code-fixed policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty -> the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 -> the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, indexed by agent key. The worker reuses the historically hard-coded settleWrapUpPrompt
// (defined in worker.go); planner/mainagent each have their own version; misses (custom agents) fall back to the generic default.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: built-in default round budget for each agent's [own] wrap-up phase (can be overridden by a backend value >0).
// All get 10 rounds, ensuring enough steps in the wrap-up phase to persist data. Unmatched agents fall back to genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your planning steps for this round are about to run out -- note that only [this round] is ending; the system will wake you again to keep planning as the situation evolves. This is not task termination, so you don't need to wrap up the whole plan here. Land the conclusions you've already worked out this round -- don't let it go to waste -- but [don't force intents just to close out the round] (0 intents this round is still a perfectly normal outcome): (1) if you've identified exploration directions that [should be dispatched now], submit them in one batched add_intent call (don't hold back what you've decided); (2) for goals proven achieved by some discovery/fact, call prove_goal to mark them met (don't miss any); (3) if you spot a serial exploitation chain that needs staged steps, record it with TodoWrite so the next wake-up can continue dispatching. Finish and end the round directly; no summary text needed."

const mainAgentWrapUpDefault = "Your steps are about to run out and this interaction is ending. Do not start any new exploration/operations. Please summarize the current progress, key conclusions, and suggested next steps to the user in **one single plain-text sentence on its own**."

const genericWrapUpDefault = "You are about to be terminated due to budget exhaustion. First write back completed but not-yet-persisted results, then **in a separate one-sentence plain text** summarize what you did and the key conclusions reached (this sentence will be shown as this run's result)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key --
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key --
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout closing words (see docs/task-level timeout and wrap-up design.md) ----------
//
// These and the per-run closing words are [two separate sets]: per-run means "your budget for this run is used up"; task timeout means
// "The whole task clock has expired, about to end". The semantics are often the opposite (especially for the planner: per-run says "don't stop, keep planning", ...
// ... task timeout says "clock's up, stop planning, make the final call"). Configured only for worker/planner.

// WrapupTaskTimeoutOverride / ...TurnsOverride: DB overrides for the task-timeout wrap-up text and turn counts
// (wired to agents.task_timeout_wrapup_prompt / _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has hit its timeout and is about to end** (not this run's budget -- the entire exploration session is out of time). This is the last chance: (1) persist [all] content you have identified but not yet written back -- new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed findings via report_finding; (2) do not start any new commands/probes; (3) **finally summarize your key conclusions on this intent in one standalone plain-text sentence**."

const plannerTaskTimeoutDefault = "**The entire task has hit its timeout and is about to end** (not this round -- the whole task terminates). Based on 【all】 current facts and findings, make the final goal judgment: call prove_goal to mark met every goal proven met by evidence (don't miss any). **Generate no new intents** (dispatched intents would never execute now). Wrap up once judged -- no summary text needed."

// TaskTimeoutWrapupDefault returns an agent's built-in default task-timeout wrap-up text (for backend placeholders/restoring defaults).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // Unconfigured (mainagent/chat) returns empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. An empty string means that agent has no task-timeout text
// (non-worker/planner); the caller should then fall back to per-run wording.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // Default: inherit per-run round count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  -> this run is squeezed by the task deadline: wrap-up due to Timeout = task time reached -> task-timeout wording;
//
// because MaxTurns close-out = steps exhausted first within the clamping window while minutes remain on the task -> fall back to the per-run wording.
//   - clamped=false -> task is still early: both reasons use per-run wording (i.e. degrades to wrapupSettlement).
//
// Left to the harness's PromptByReason, which picks by the [actual] reason on the spot at wrap-up time, with no build-time mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // Fallback (also the value of both reasons when non-clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // Task clock expired
				harness.ReasonMaxTurns: perRun, // Steps exhausted first, task still has time left
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}

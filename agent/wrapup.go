package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up / settlement prompt: when an agent is terminated due to step budget
// exhaustion (MaxTurns) or timeout (run_seconds/MaxDuration), the SDK's settlement
// phase injects this prompt so the agent can persist any identified but unwritten
// content to the store and output a brief summary, avoiding incomplete runs.
//
// Each agent's wrap-up prompt can be overridden in the admin backend (stored in
// agents.wrapup_prompt); leave empty to use the built-in default here. Only the
// prompt text is editable; which tools to disable and the turn budget for the
// wrap-up phase itself are fixed code-level policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, indexed by agent key. The worker reuses the
// historically hard-coded settleWrapUpPrompt (defined in worker.go); planner and
// mainagent each have their own version; unmatched (custom) agents fall back to
// the generic default.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: built-in default turn budget for each agent's wrap-up phase
// (can be overridden by a positive value in the admin backend). All set to 10 to
// ensure enough steps to persist data during wrap-up. Unmatched agents fall back
// to genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your step budget for this planning round is about to run out — note that only THIS ROUND is ending; the system will wake you again as the situation evolves, so this is NOT the end of the task and you do NOT need to wrap up the entire plan. Persist any conclusions you have already reached so this round is not wasted, but DO NOT force intents just for the sake of wrapping up (0 intents this round is a perfectly normal outcome): (1) If you have identified exploration directions that SHOULD be dispatched now, submit them in a single add_intent batch call (do not hold back ready intents); (2) For goals that have been proven met by a finding/fact, call prove_goal to mark them met (do not miss any); (3) If you have identified a sequential exploitation chain that requires multiple steps, use TodoWrite to note it down so you can continue on the next wake-up. When done, end this round directly — no summary text needed."

const mainAgentWrapUpDefault = "Your step budget is about to run out and this interaction is ending. Do not initiate any new exploration or operations. Please **summarize in a single plain-text sentence** the current progress, key conclusions, and recommended next steps for the user."

const genericWrapUpDefault = "You are about to be terminated due to budget exhaustion. First, persist any completed but unrecorded results to the store, then **summarize in a single plain-text sentence** what you did and what key conclusions you reached (this sentence will be displayed as the result of this run)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
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

// ---------- Task-level timeout wrap-up prompts (see docs/task-timeout-and-wrapup-design.md) ----------
//
// These are a SEPARATE set from the per-run wrap-up prompts: per-run means "your
// budget for this single run is exhausted"; task timeout means "the entire task has
// reached its deadline and is about to end". The semantics are often opposite
// (especially for the planner: per-run says "don't stop, keep planning", while task
// timeout says "stop planning, make your final judgments"). Only configured for
// worker/planner.

// WrapupTaskTimeoutOverride / ...TurnsOverride: DB overrides for the task-timeout
// wrap-up prompt and turn budget (wired to agents.task_timeout_wrapup_prompt /
// _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (this is not your per-run budget — the whole exploration session has reached its deadline). This is your last chance: (1) Persist ALL identified but unrecorded content to the store — new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed vulnerabilities via report_finding; (2) Do not start any new commands or probes; (3) **Summarize in a single plain-text sentence** your key conclusions for this intent."

const plannerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (this is not just this round — the whole task is terminating). Based on ALL current facts and findings, make one final goal assessment: call prove_goal to mark met for any goals that have been proven achieved by evidence (do not miss any). **Do not generate any new intents** (intents dispatched now will not be executed). Once the assessment is complete, wrap up — no summary text needed."

// TaskTimeoutWrapupDefault returns the built-in default task-timeout wrap-up prompt
// for an agent key (used by the admin UI as placeholder / restore-default value).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // unconfigured agents (mainagent/chat) return empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. An empty
// string means the agent has no task-timeout prompt (not worker/planner), in which
// case the caller should fall back to the per-run prompt.
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
	return resolveWrapupTurns(agentKey) // default: reuse per-run turn budget
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is constrained by the task deadline: settlement due
//     to Timeout = task deadline reached → use task-timeout prompt; settlement due
//     to MaxTurns = steps exhausted within the clamped window while task time
//     remains → fall back to per-run prompt.
//   - clamped=false → task deadline is far away: both reasons use the per-run
//     prompt (degenerates to wrapupSettlement).
//
// PromptByReason handed to the harness picks the prompt at settlement time based on
// the ACTUAL reason, avoiding any build-time mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // task deadline reached
				harness.ReasonMaxTurns: perRun, // steps exhausted first, task time remains
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}

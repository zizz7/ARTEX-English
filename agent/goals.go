package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration testing goal decomposer. Your job is to identify the **final outcomes to achieve** from user input, NOT to plan attack steps.

**Step 1 (do this BEFORE splitting goals): Extract operation constraints**
Identify any explicit rules the operator has set about what operations are allowed or prohibited from the task goal / task description, and register them one by one via set_constraints (if the description/goal contains no operation constraints, skip this extraction step):
- type=deny: Prohibited operations (e.g. "no port scanning", "no write/delete operations on production", "no brute-forcing", "do not touch a certain subdomain").
- type=allow: Explicitly allowed/limited operation scope (e.g. "passive reconnaissance only", "only target a specific domain").
- Constraints ≠ goals, and ≠ attack steps: they define the boundaries of permissible operations.
- **Constraints MUST be self-contained with specific targets hard-coded**: Replace pronouns like "the current target/current port/current IP/current domain/this site" with the **actual values** from the task goal/description. Constraints are injected separately into execution-phase prompts where, out of context, pronouns cannot be resolved.
  Example: If the target is https://abc.example.net → write "only test abc.example.net" instead of "only test the current target"; "only test target port 443, do not scan other ports" instead of "only test the current port". If the original text only says "the current target" but the target address is clear, fill in the address.
- **Only register constraints that are explicitly stated or emphasized in the goal/description — never fabricate**; when unsure about the type, use deny (more conservative).
- If the goal/description truly contains no operation constraints, **do not** call set_constraints.
After registering constraints (if any), proceed with goal decomposition below.

**Goal = final deliverable/verifiable outcome**

**NOT goals (must NOT be listed as sub-goals)**:
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and verification processes
- Attack steps, exploitation techniques
- Result verification steps

**Decomposition principles**:
- If the user describes only one final goal → output one
- If there are multiple **mutually independent** final deliverables → list them separately
- Annotate vulnclass when a clear vulnerability class can be mapped; leave empty for information-gathering/business-logic goals
- Never fabricate goals the user did not mention

Call set_goals to submit results.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: Register test asset scope**
In addition to decomposing goals, you must also identify the **explicitly provided test asset scope** from the task goal / task description and register it via add_task_scope (this is the denominator for asset test coverage, as well as the authorization boundary). **Minimum scope principle: only register the specific target the user explicitly named — never expand the scope on your own.**
- If the target is a URL or address with a hostname (e.g. https://xxx.example.com/path, app.example.com) → extract its **full hostname**, kind=subdomain, value=full hostname.
  Example: target https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**NOT** example.net).
  **Never** shorten a hostname with subdomains to the root domain — seeing xxx.example.com and registering the entire example.com would expand the scope beyond the user's target, violating the minimum scope principle.
- Only when the user provides a **bare root domain with no subdomains** (e.g. directly writes example.com), or explicitly says “the entire site / all subdomains / full domain” → use kind=root_domain, value=example.com.
- Pure IP or CIDR range → kind=ip / cidr, value=IP or CIDR.
- **Do not** register company scope (company) — the task was just created and the asset system typically does not have this company yet, so it cannot be registered; company-level scope is handled in a later planning phase.
Other rules:
- Only register scopes **explicitly stated in the goal/description**; never fabricate or infer unmentioned domains/IPs.
- reason should briefly note which sentence the basis comes from, for audit purposes.
- If the goal/description contains no explicit asset scope, **do not** call add_task_scope.
Call add_task_scope to register the scope first (if any), then call set_goals to submit goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target scope/flag count/engagement rules, etc.).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (no asset store dependency): the prompt body
	// already contains the "extract operation constraints first, then decompose goals"
	// step (wording can be edited in the agent edit page); we just wire up the tool here.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task goal:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background information, may contain target scope/flag count/engagement rules; for reference only, do not fabricate anything not mentioned):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 steps (extract constraints → register scope → decompose goals) each need one tool call; give enough turns to avoid missing set_goals before wrap-up.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // when this profile selects non-streaming, use Provider.Complete
		MaxTokens:    maxTokens,    // 0 = do not send cap, let server default decide
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}

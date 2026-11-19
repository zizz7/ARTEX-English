package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[YOUR PLANNING TODO (persisted across wake-ups, written by you in a previous round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed accordingly: only dispatch intents for the next step whose prerequisites are completed / whose dependent facts already exist; use TodoWrite to update the checklist (mark steps satisfied by facts as completed). Do not re-dispatch steps already pending/in_progress in the checklist.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    — the human (via main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = goal text added this time, 1+ items; set_goals supports batch).
//	"goal_deleted" — the human deleted a goal from the overview's goal management (Detail = deleted goal text).
//	"goal_edited"  — the human edited a goal from the overview's goal management (OldGoal→NewGoal text).
//	"cancelled" — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Goals    []string // Kind=="goal" only: goal text added by this set_goals call (one or more)
	OldGoal  string   // Kind=="goal_edited" only: goal text before editing
	NewGoal  string   // Kind=="goal_edited" only: goal text after editing
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[CHANGES THAT TRIGGERED THIS ROUND (look here first, then decide whether to add new directions)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a new goal: %s — a new goal to achieve; add exploration directions for it if no corresponding intent exists yet.", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d new goals: %s — all are new goals to achieve; for each goal without a corresponding intent, add exploration directions.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — this goal has been removed; re-evaluate remaining goals/directions accordingly (no need to dispatch intents for it).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal, changed from \"%s\" to \"%s\" — adjust exploration directions based on the new goal (stop dispatching if the original direction no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- Intent #%d (%s)'s worker reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; intent content: %s, deletion reason: %s. The intent has been stopped (no longer executing); the reason has been attached to it as a fact. Please re-plan accordingly.", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- Intent #%d (%s)'s worker finished, output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
		}
	}
	b.WriteString("\n(For full details, use node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to retrieve output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output records yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ...(truncated; use get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[CURRENT SITUATION (graph_overview pre-fetched, equivalent to calling that tool yourself; use node_detail/list_facts etc. for deeper details as needed)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate
// artifact output spec tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "Planner" of an ARTEX platform authorized penetration testing system, woken up frequently (triggered whenever the graph changes). Responsibilities: read the situation → judge goals → **add exploration intents ONLY when there are genuinely uncovered new directions**. You are the planner, not the executor: this round's only outputs can be [generating/articulating intents] or [judging goals] — never actually do the work inside a plan.

Task goal: {{.Goal}}

**How many intents should this round produce (think this through first)**:
- **Hard floor (highest priority)**: as long as the goal is NOT met AND there are NO open or running intents (frontier_open=0 and running_intents is empty), this round MUST produce at least one intent that advances toward the goal — when there are no running workers to wait for and no queued directions, producing 0 intents = task stall. Even if all known directions are only in recent_done, you must open a new one or redispatch based on the done/exhausted/blocked analysis below.
- Beyond the hard floor, **producing 0 intents is a normal outcome, but requires a legitimate reason** (not a default of "fewer is safer"): (1) **Already covered** — every direction you can think of is already handled by open/running intents (rephrasing and duplicating an existing intent is a serious error); (2) **Waiting on dependency** — the next step depends on output from a currently running worker that hasn't finished yet (force-dispatching would cause the downstream to spin without prerequisites; wait for the next wake-up after the graph updates).
- Conversely: if there ARE uncovered directions that don't depend on running workers, or the goal is unmet and there are still untested surfaces in scope, dispatch them — don't treat 0 intents as the lazy default.

**Decision flow for each wake-up**:

1. **The full situation is attached below this prompt** (this is the graph_overview return — no need to call it again): task (original title + goal/root node), asset counts, goals + status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, suggesting potentially unexplored directions), findings (confirmed vulnerability count), facts (exploration fact count, separate from vulnerabilities), recent_facts ({id, summary, confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) belong to this task only; **the asset graph is globally shared** (same across tasks; asset counts are global in-scope, not task-specific) — ignore assets unrelated to this task.
   - **Lineage**: each intent carries parents (upstream: which facts/intents it was derived from) and yields (downstream: which facts/findings it produced); each recent_fact carries from_intent. Use this to understand "which facts came from which direction and whether they can be combined into new directions."
   - **Negative/uncertain observations** ("port closed" / "not injectable" etc. in recent_facts) are worker observations, not conclusions: before accepting them, use node_detail(id) to check evidence — only treat a direction as temporarily closed when evidence is solid, confidence=observed, and all approaches have been exhausted; when evidence is missing, it's just "looks like it / only tested once", or confidence=inferred, treat it as [not yet determined]. If in scope and no other intent covers it, dispatch a verification intent to confirm or refute by default (**same negative direction gets at most one verification**; if still negative after verification with reasonable evidence, respect the conclusion and don't dispatch again).
   - **Call for deeper details as needed**: list_facts (paginated, newest first, default 20, filterable by q, paginate with before, includes total/has_more), list_findings (all vulnerabilities), node_detail(id) (full evidence/details; lists/recent_facts only show summaries), list_assets (pull: q search, type/company_id/task_id filter, pagination, or direct id/ids fetch), asset_neighbors. Assets are globally shared — don't pull everything by default.

2. **Judge goals (core responsibility)**: the goals field already contains goals and their status; for unmet goals that have been proven by a finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark them met. **When the goal you mark is the last unmet goal, the system automatically determines the entire task is complete** — closure is driven solely by individual prove_goal calls; there is no other "one-click complete" mechanism.
   - Warning: **Quantitative acceptance verification (strictly no premature sign-off)**: when a goal contains quantifiable conditions (coverage reaching X%, capturing N flags, obtaining a specific privilege), you MUST verify against the graph_overview's actual values (coverage.pct, findings count, etc.) BEFORE calling prove_goal: if the target is not met, DO NOT call prove_goal — dispatch intents to close the gap instead. Do not mark met on the basis of "mostly achieved / core objective accomplished." Example: required coverage 100% but actual coverage.pct=40% → not met, continue dispatching supplemental testing intents.
   - **Independent verification of high-impact findings**: before using a critical or high finding as prove_goal evidence (and before treating it as settled), check its evidence with node_detail. Unless the evidence is unambiguous on its own (captured flag, command output from achieved code execution, data that could only come from the claimed access), dispatch ONE verification intent that reproduces it independently — a fresh session or request sequence, ideally a different tool or technique — and confirms the claimed impact. Verify each finding at most once; if the reproduction fails, record that as a fact and do not use the finding as goal evidence.

3. **(Optional, initial phase only, extremely lightweight) Reconnaissance for understanding**: ONLY when the graph has almost no facts yet (recent_facts is essentially empty, task just started) and the situation alone cannot make initial intents specific enough, use Bash etc. for minimal, read-only probing of the target (e.g., 1-2 curl requests to check the homepage/fingerprint). **The only legitimate output is a more precise intent description** — never vulnerability discovery/validation/exploitation, and never endpoint/directory/parameter enumeration results (those are worker tasks — write them as intents and dispatch). Three hard boundaries:
   - If the graph already has worker-produced facts (facts>0 / recent_facts is non-empty) → DO NOT probe yourself; all judgments must be based on existing facts; this round's output can only be "dispatch new intents" or "done." Want to dig deeper into a lead → dispatch an intent for the worker to investigate, don't curl it yourself.
   - Even at the start, probe at most 3 times then stop — only to articulate initial intents clearly. The moment you find yourself "investigating in depth" rather than "quickly orienting" (enumerating endpoints/directories one by one, trying IDs one by one, decoding chains, repeatedly probing the same interface, any injection/authorization bypass/vulnerability testing — all heavy worker tasks), stop immediately and write it as an intent.
   - If you can judge from existing facts/situation, probing is not needed at all.

4. **Decide which new directions to add**: **"restraint" here means ONLY [don't duplicate existing intents], NOT "dispatch as few as possible"** — when the goal is unmet, the default question is "what deeper, more aggressive, uncovered approaches exist to get closer to the goal," not "can we wrap up." Intents are [open exploration directions] (not fixed types/menus); combine known facts, assets, and goals to judge directions yourself, then compare each against open + running + recent_done:
   - Already covered by open/running → don't generate (being handled).
   - Appeared in recent_done → **first check that intent's state (included with each entry) to understand how it stopped, then decide**:
     - **done (finished normally)**: covered → don't redispatch as-is; whether it's a dead end depends on the fact conclusions in its yields, not the state itself. Only redispatch when genuinely NEW material emerges (new facts/assets/parameters/clearly different approach), and the summary must clearly state the difference from last time. Rephrasing or "maybe it'll work if we try again" doesn't count — retrying is prohibited.
     - **exhausted (budget depleted, exploration cut short mid-way, only partial write-back) / blocked (model or network failure, barely explored)**: both are premature terminations with incomplete information — first use get_worker_trace / get_worker_output to see what it actually accomplished and where it got stuck, then choose: close to breakthrough but budget-cut → dispatch "continue from previous progress"; pure external failure with no real execution (blocked often is) → directly redispatch the same direction; stuck at the same point repeatedly → change approach/direction. The basis is always the actual progress in the trace, not the state itself.
   - Completely new direction with no intent coverage → generate.
   - All known directions are covered by still-open/running intents → don't generate, end directly (there are running/queued workers, wait for them to progress). But if only recent_done covers them and there are no open/running intents while the goal is unmet → per the hard floor above, you must open a new one or redispatch.
   - **Depth over breadth**: coverage is a floor/acceptance criterion, not the exploration goal itself. After discovering a high-value entry point (potentially leading to RCE/privilege escalation/data exfiltration), prioritize dispatching intents to push that path DEEP, rather than spreading wide to level up coverage by shallow-testing each asset.
   - **Maintain route diversity, don't converge prematurely**: when the goal is unmet and all existing intents are clustered on the same route/entry point, while fundamentally different uncovered directions exist (another attack surface/another asset type/another exploitation chain), prioritize adding that divergent direction rather than adding synonymous intents on the same line (look at substantive differences, not wording). If that divergent direction is already covered by existing intents, still don't generate. Ideally 2-3 mechanistically different routes coexist (e.g., "attack via upload chain" and "attack via authentication bypass"); only after one produces evidence of goal convergence do you concentrate resources there. **But diversity always defers to the top-level [OPERATION CONSTRAINTS]**: entry surfaces/ports/hosts/operations excluded by constraints must NEVER have intents generated, even if they are fundamentally different.

   **Sequential exploitation chains: dispatch step-by-step, don't split into parallel.** For strongly dependent sequential chains (step 1 → step 2 → step 3, where each step depends on the actual output of the previous one): do not dispatch all at once in parallel (downstream without non-existent prerequisites will just repeat/spin). Use TodoWrite to record the entire chain as a todo list (one item per step), dispatch only the step whose prerequisites are satisfied this round (usually the first step), and after it produces facts, dispatch the next step on the next wake-up (the prompt will include the todo list) and mark satisfied steps as completed. Don't split "the same thing" into two items ("confirm the trigger point" and "trigger the trigger point" are the same step); use multiple parallel intents ONLY for genuinely parallel, independent dimensions (e.g., enumerating multiple unrelated endpoints).

5. **Submit**: use a SINGLE add_intent call to batch-submit the filtered new directions (intents array, at most 4 highest-value ones — don't call multiple times with one each):
   - **summary**: a one-sentence natural-language description of the direction (full address of the test target + what to do + why), without fixed categorization. Deduplication relies mainly on comparing this against existing intents.
   - **asset_ids**: target asset IDs for this direction to test/attack (provide when possible, 0/1/multiple, from list_assets) — whenever the direction involves specific assets (sites/endpoints/parameters/hosts) always provide them, used for coverage deduplication and linking into the asset chain; provide all when spanning multiple assets. Leave empty only for pure global reconnaissance with no specific assets.
   - **parent_ids**: which upstream nodes this direction was derived from (optional, 0/1/multiple) — when multiple facts combine to produce an intent, provide all; when derived from an upstream intent/finding, provide its ID too. Leave empty for top-level brand-new directions.

No duplicates, no forced filler; but when the goal is unmet and there are uncovered, deeper approaches available, dispatch them. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	tsx := NewToolSet(ts, "planner")
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools + base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash).
	// When asset coverage is disabled, strip add_task_scope/list_untested_assets (not included in prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// Key situational data (just-completed intents + pre-fetched full graph) is now placed
	// in the per-round user input (see input below); the system prompt retains only the
	// static planning body. Moving it out keeps the system prompt stable per round for
	// better caching; the trade-off is that if a single round grows long, situational
	// data may be compressed by compaction (planner rounds are usually short, low risk).
	// situational is concatenated into the input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / final-round mode (injected via ctx, see taskclock.go). In the
	// final round, the task-timeout planner wrap-up prompt is concatenated into the user
	// input as a per-round operational directive (alongside situational), making it only
	// perform final goal judgment without producing new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[TASK FINAL WRAP-UP (special directive for this round, overrides the normal planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// This task's working directory <workDir>/tasks/<taskID>, created upfront.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // inject operation constraints (if any) into the system prompt to bound the exploration scope
	}
	system, boundary := deferredSystem(sysBody, def)
	// The planner has no wall-clock budget of its own; when a deadline exists, MaxDuration
	// is clamped to the remaining time so the running planning round enters wrap-up when
	// the task deadline arrives (timeout → task-timeout prompt; step limit → per-run prompt).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // routed through the recording proxy for audit trail; load proxy CA to verify MITM re-signed HTTPS certs
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), independent of the MITM recording proxy; empty means direct.
		EnableWebSearch:    p.webSearch.Enabled,
		WebSearchBackend:   p.webSearch.Backend,
		BraveSearchAPIKey:  p.webSearch.BraveKey,
		TavilySearchAPIKey: p.webSearch.TavilyKey,
		WebSearchProxy:     p.webSearch.Proxy,
		BashEnv:            proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommands default to proxy + trust CA
		WorkingDir:         taskDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:      cmdOutDir(taskDir),
		MaxTurns:           p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:        maxDur,     // 0=unlimited; with deadline=remaining time to deadline
		Compaction:         compactionConfig(p.compactionWindow()),
		// Cross-wake-up shared planning todo: keeps sequential chains across rounds (session is new, store is not).
		Todos: p.todoFor(ts.ID()),
		// When this round's step budget is hit → SDK runs wrap-up: persist conclusions already
		// reached this round (dispatch add_intent, prove provable goals via prove_goal, record
		// sequential chains via TodoWrite), rather than stopping planning — the planner will
		// continue to be woken up repeatedly afterward.
		// When clamped (constrained by task deadline), use PromptByReason (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // when this profile selects non-streaming, use Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = do not send cap, let server default decide
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Situational data (just-completed intents + full graph) is now concatenated into the
	// per-round user input (see input below). The user message also includes directives +
	// cross-wake-up todos (todos are the model's own planning notes, regenerable, fine in user).
	// The opening line varies by whether this round has concrete changes: changes present →
	// point to the [CHANGES] block below; no changes (heartbeat/timer/hint/resume etc.) →
	// don't falsely claim "the graph changed," instead prompt to also review running intents.
	lead := "There were concrete changes just now (see [CHANGES THAT TRIGGERED THIS ROUND] below); plan the next step accordingly:"
	if len(triggers) == 0 {
		lead = "This round is a **scheduled patrol (heartbeat timer) / no concrete change signal** wake-up — the graph may or may not have new changes. Also review running intents: use steer_work to course-correct those with no progress or drifting off-track, use kill_work to cut losses on completely wrong directions; then judge goals and decide whether to add directions:"
	}
	input := lead + situational + "\n\nBased on the situation above, judge goals. When a goal has been TRULY achieved (target outcome obtained / target vulnerability confirmed), use prove_goal to mark each one. **Hard floor: as long as the goal is NOT met AND there are NO open or running intents (frontier_open=0 and running_intents is empty), this round MUST produce at least one intent advancing toward the goal — there are no running workers to wait for and no queued directions, so producing 0 intents = task stall. Only when open/running intents are already advancing, or the goal is already met, may this round produce no new intents.**" +
		renderPlannerTodos(opts.Todos.List())
	// When clamped by a deadline, add a hard ctx backstop (soft budget + grace) to prevent a single round from getting stuck and bypassing the turn-boundary soft timeout.
	runCtx := ctx
	if maxDur > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeoutCause(ctx, maxDur+settleHardGrace, AbortRunHardTimeout)
		defer cancel()
	}
	_, _, err = captureRun(runCtx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // upstream: which nodes this intent derived from (multiple facts can jointly produce one intent)
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // downstream: which facts/findings this intent produced
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	as     *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs     *db.CompanyStore // company store (optional)
	ts     *db.ExplorationStore
	worker string
	taskID int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "user added N goals: ..." trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "facts1 assets25 findings0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("facts%d assets%d findings%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block,
// the insertAssets auto-scope hook a no-op, and CoverageTools reports the two
// coverage-only tools so callers can drop them from the agent's tool list.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
var coverageOnlyTools = map[string]bool{"add_task_scope": true, "list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// StripCoverageParams hides coverage-only parameters (currently insert_assets'
// per-item `related`, which only decides whether an asset enters the coverage
// denominator) from the model-facing schema when coverage is disabled — the param
// has no effect then, so showing it just pollutes the prompt. MUST run on the FINAL
// tool list (after AugmentTools/ToolResolve): the DB tools table is authoritative on
// schema, so stripping the code schema earlier would be overwritten. No-op when
// enabled or when the list has no insert_assets. The schema is deep-copied before
// editing so the shared/DB schema map is never mutated.
func (t *ToolSet) StripCoverageParams(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	for i, tool := range tools {
		if tool.Name() != "insert_assets" {
			continue
		}
		schema := deepCopyJSONMap(tool.InputSchema())
		if props, ok := nestedMap(schema, "properties", "assets", "items", "properties"); ok {
			delete(props, "related")
		}
		tools[i] = DecorateTool(tool, tool.Description(), schema)
	}
	return tools
}

// deepCopyJSONMap returns a JSON round-trip deep copy of a schema map so callers can
// edit it without touching the original (which may be shared/cached). Falls back to
// the input on any marshal error (edits then become best-effort no-ops upstream).
func deepCopyJSONMap(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return m
	}
	return out
}

// nestedMap walks a chain of string keys through nested map[string]any values,
// returning the final map and whether the whole path resolved to one.
func nestedMap(m map[string]any, keys ...string) (map[string]any, bool) {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return readTool("graph_overview",
		"(Exploration graph) Distilled situational summary: asset counts, sites without interfaces, frontier, findings, hints (strategic guidance from humans/main agent — must be considered when generating intents). Call this first when planning.",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("graph_overview requires task context (exploration store not initialized)"), nil
			}
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: strategic guidance posted to the graph by humans/main agent via add_hint;
	// folded in so the planner reads them every round when generating intents
	// (otherwise they would be write-only, never read).
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hsum = append(hsum, map[string]any{"id": h.ID, "state": h.State, "text": p["text"]})
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	fr, _ := t.ts.Frontier(100)
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if len(recentDone) < recentDoneWindow {
				recentDone = append(recentDone, n)
			}
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// done_intents_total: total count of finished intents (done/blocked/exhausted),
	// named in parallel with recent_done_intents — the latter is just a truncated
	// window (<=15) of the most recent ones. The two keys side by side are
	// self-describing: "what you see is N out of total", so the planner doesn't
	// mistake "not shown" for "never dispatched" during dedup.
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	out["frontier_open"] = len(fr)
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	out["findings"] = len(vulnNodes)                   // confirmed vulnerability count (used for goal assessment)
	out["facts"] = len(factNodes)                      // exploration facts/conclusions count (including negative results)
	recentFacts := make([]map[string]any, 0, 20)
	for _, n := range factNodes {
		if len(recentFacts) >= 20 {
			break
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // which intent produced this fact
		}
		// confidence brought into the overview so the planner can see at a glance
		// which conclusions are only inferred (especially negative results — don't
		// treat them as definitive); evidence is lengthy and left to node_detail(id).
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts // recent facts as {id, summary, from_intent, confidence?}; details/evidence via node_detail(id)
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage: rough asset testing coverage reference — the proportion of assets within
	// scope (task_scope) that have been touched by facts + by_type (total/tested per type).
	// To see specific untested assets, the agent calls list_untested_assets on demand.
	// Only present in task context.
	// When coverage is disabled (coverageDisabled): only scope/hosts are kept (scope
	// boundaries and target host awareness — company associations surface via scope here),
	// dropping denominator/tested/pct/by_type/note and other coverage metrics to avoid
	// context pollution and not induce use of the hidden add_task_scope/list_untested_assets.
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "Asset testing coverage (including interfaces and various related assets), rough estimate for reference only: includes scope and fact anchors from the current task and directly related tasks; related scope is read-only. Container assets / large enumerations may undercount, do not assume full coverage; use add_task_scope to extend this task's scope, list_untested_assets to view untested assets [usually no need to call list_untested_assets — just proceed with the task];"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "scope not anchored"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			// scope: the root assets of the current testing scope (task_scope raw rows), so
			// the agent knows exactly which targets this task has scoped (not all test assets,
			// but the scope boundaries themselves). Always provided regardless of coverage toggle.
			if rows, err := t.as.ListTaskScopeWithSources(t.taskID); err == nil && len(rows) > 0 {
				scope := make([]map[string]any, 0, len(rows))
				for _, r := range rows {
					e := map[string]any{"kind": r.Kind, "source": r.Source, "task_id": r.TaskID}
					if r.TaskID != t.taskID {
						inheritedMap(e, r.TaskID)
					}
					switch {
					case r.Domain != "":
						e["value"] = r.Domain
					case r.Net != "":
						e["value"] = r.Net
					case r.Value != "":
						e["value"] = r.Value
					case r.CompanyID != nil:
						e["company_id"] = *r.CompanyID
						if t.cs != nil {
							if company, err := t.cs.GetCompany(*r.CompanyID); err == nil && company != nil {
								e["company_name"] = company.Name
							}
							if rules, err := t.cs.GetScope(*r.CompanyID); err == nil {
								keywords := make([]string, 0)
								companyScope := make([]map[string]any, 0, len(rules))
								for _, rule := range rules {
									value := rule.Raw
									if value == "" {
										switch rule.Kind {
										case "domain":
											value = rule.Domain
										case "ip", "cidr":
											value = rule.Net
										default:
											value = rule.Value
										}
									}
									entry := map[string]any{"kind": rule.Kind, "value": value}
									if rule.Reason != "" {
										entry["reason"] = rule.Reason
									}
									companyScope = append(companyScope, entry)
									if rule.Kind == "keyword" && rule.Raw != "" {
										keywords = append(keywords, rule.Raw)
									}
								}
								if len(companyScope) > 0 {
									e["company_scope"] = companyScope
								}
								if len(keywords) > 0 {
									e["company_keywords"] = keywords
								}
							}
						}
					}
					scope = append(scope, e)
				}
				m["scope"] = scope
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// Only provide the host count, no longer flattening the host list into
				// graph_overview (in large-scope tasks it's a lot of strings repeated every
				// round with limited value for planning decisions); specific hosts can be
				// queried via list_assets on demand.
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes     = 48_000
	relatedOverviewMaxTextPerSource   = 8_000
	relatedOverviewMaxGoalsPerSource  = 8
	relatedOverviewMaxHintsPerSource  = 6
	relatedOverviewMaxFactsPerSource  = 12
	relatedOverviewMaxFindingsPerTask = 6
	relatedOverviewMaxIntentsPerTask  = 8
	relatedOverviewMaxScopePerSource  = 12
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDone := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return readTool("list_findings", "List confirmed vulnerabilities for this task and directly related tasks (compact: id+task_id+intent_id+vulnclass+severity+summary+state). Related task entries carry source_task_id/inherited=true and are read-only. This only contains vulnerabilities; for general exploration facts use list_facts, for details use node_detail(id).",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("list_findings requires task context (exploration store not initialized); run this agent inside a task, not a standalone chat"), nil
			}
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> the intent that produced it
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return readTool("list_facts", "Paginated listing of exploration facts/conclusions for this task and directly related tasks, newest first (compact: id+summary+state; long summaries are truncated, full text via node_detail(id)). All parameters are optional: limit (default 20, max 100), before (cursor — pass the previous page's next_before to get an older page; omit/0 = newest page), q (filter by summary keyword). Returns {facts, total, has_more, next_before}: total is the filtered count, use next_before to continue paging when has_more=true. Related task entries carry source_task_id/inherited=true and are read-only. For vulnerabilities see list_findings.",
		obj(map[string]any{
			"limit":  intp("Number of items to return, default 20, max 100"),
			"before": intp("Pagination cursor: only return facts with id less than this value (older); omit or 0 = newest page"),
			"q":      str("Filter by fact summary keyword (case-insensitive); omit = no filter"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("list_facts requires task context (exploration store not initialized)"), nil
			}
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // pass this back to get the next (older) page
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return readTool("node_detail", "Retrieve the full content of an exploration graph node by id, for this task or directly related tasks. Inherited nodes carry source_task_id/inherited=true and are read-only. Only accepts exploration node ids returned by list_facts/list_findings/graph_overview; for assets use list_assets/asset_neighbors.",
		obj(map[string]any{"id": idp("Exploration graph node id (not an asset id)")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("node_detail requires task context (exploration store not initialized)"), nil
			}
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id is required"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("Exploration node %d not found. If you are looking for an asset, use list_assets / asset_neighbors (assets and exploration nodes are in different id spaces — asset ids cannot be passed to node_detail).", id)), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence
		})
}

// --- planner write tools ---

// intentItem is a single exploration direction in an add_intent batch/single call.
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
}

// addOneIntent creates one intent node and links its upstream lineage, returning the id.
// Constraint: intents can only anchor on confirmed knowledge — each parent_id must be
// an existing fact/finding node (cannot attach to other intents/goals/hints). Top-level
// fresh directions leave parent_ids empty and are automatically linked to the origin fact.
// This enforces "every intent traces back to a fact node and is discovery-driven rather
// than planned out of thin air" at the creation path.
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary must not be empty")
	}
	// Validate anchor points first (before creating the node, to avoid bad anchors leaving orphan intents).
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d does not exist in this task or directly related tasks: parent_ids must be existing fact/finding node ids; for top-level fresh directions, leave parent_ids empty", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d is a %q node and cannot serve as an intent anchor: intents can only anchor on confirmed facts/findings, not on intents/goals/hints; for top-level fresh directions, leave parent_ids empty", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return writeTool("add_intent", "Create exploration intents and write them to the frontier, linking them into the exploration chain. An intent is an open exploration direction, not a fixed type — use summary to freely describe what to explore/verify/exploit in one sentence.\n"+
		"Prefer batch: submit multiple new directions from one round in the intents array at once (saves round trips vs. calling one by one). Returns an ids array, same length and order as intents (failed items have id=0, details in errors). For a single intent, omit intents and provide top-level summary directly.",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "[Preferred] Array of new exploration directions to add, processed in order. Each element has the same fields as the top-level ones below (summary/asset_ids/parent_ids/priority). Returns ids with the same length and order as this array.", "items": map[string]any{"type": "object"}},
			"summary":    str("[Single] One-sentence description of this exploration direction: what to do + why. The direction itself is sufficient; no dependency on asset ids."),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Target asset ids for this direction (**provide whenever possible**, 0/1/multiple; these are asset ids from list_assets, not exploration node ids): which assets (sites/endpoints/parameters/hosts etc.) this exploration direction targets. Always provide them when the direction involves specific assets — this is the structured marker for 'which targets this exploration hits', used for coverage dedup and linking intents into the asset chain. Only leave empty for pure global reconnaissance with no specific target assets."},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Upstream anchor ids (optional, 0/1/multiple): which confirmed facts/findings this direction is derived from. **Only existing fact/finding node ids are allowed — do not use intent/goal/hint ids** — intents must be anchored in confirmed knowledge, discovery-driven rather than speculative planning. Pass multiple when several facts jointly produce a new intent; leave empty for top-level fresh reconnaissance directions (they auto-link to the task's origin fact)."},
			"priority":   intp("Priority 0-10, default 5"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("add_intent requires task context"), nil
			}
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // single-item mode: top-level summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// Human submits intent via main agent → if task is already done (goalless branch
			// with no open goals), pull it back to running so a worker can claim and execute
			// this intent. resumeTask is wired only by MainAgent.Chat (SetResumeTask);
			// planner's ToolSet has nil, so planner calling add_intent is a no-op here and
			// does not affect its normal intent generation. The intent node was already
			// created above (open), so the resume check won't miscount it as drained.
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // single item: keep original return format
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return readTool("list_goals", "List this task's goal nodes and their states (open/met), for checking whether they have been achieved.",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("list_goals requires task context (exploration store not initialized)"), nil
			}
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return writeTool("prove_goal", "Call when you determine that a finding/fact proves a goal has been achieved: links the evidence node to the goal node and marks the goal as met.",
		obj(map[string]any{
			"goal_id":     idp("Goal node id"),
			"evidence_id": idp("Finding/fact node id that proves it"),
			"reason":      str("Why this evidence satisfies the goal"),
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("prove_goal requires task context"), nil
			}
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id and evidence_id are required"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id must be a goal node of this task (linked task goals are read-only)"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id must be a fact/finding node of this task or a directly linked task"), nil
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// Each time a goal is marked met, check whether ALL goals of this task are now
			// met; if so, auto-complete the task (set GoalMet) without relying on the model
			// to explicitly call goal_met.
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("all %d goals are met (last triggered by goal %d)", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d marked met; all goals of this task are achieved, task auto-completed", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "[Immediately ends the entire task] — call ONLY when you confirm that ALL of the task's goals have truly been achieved and the task is fully complete (note: the ENTIRE task must be complete; achieving just one goal/one flag/one vulnerability does NOT count — use prove_goal to mark individual goals). It is NOT for 'ending this planning round': if there are no new intents to dispatch or you are waiting for worker output, just end the current round without calling this tool (0 intents is perfectly normal). Prefer prove_goal to prove goals one by one; goal_met is only a shortcut to bypass per-goal proof and declare global task completion.",
		obj(map[string]any{"reason": str("Reason for completion (must be evidence that the goals are truly achieved, not a reason like 'no new directions this round')")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// recentDoneWindow is how many finished intents graph_overview shows the planner.
// Too small and long tasks re-dispatch work the planner can no longer see.
const recentDoneWindow = 30

// --- worker write tools ---

// minFindingEvidence is the shortest evidence text report_finding accepts; anything
// shorter cannot contain both the request/command and the proving output line.
const minFindingEvidence = 20

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "[Important] You MUST call this tool when a vulnerability is found and reproduced! Records a confirmed vulnerability finding. evidence is required: the exact request or command plus the key response/output line that proves it. Unverified suspicions belong in record_fact, not here. In task context intent_id is required (the id of the intent currently being executed); in conversation context intent_id may be omitted.",
		obj(map[string]any{
			"vulnclass": str("Vulnerability class (category, e.g. SQL Injection / IDOR / XSS)"),
			"name":      str("Vulnerability name (a specific readable title, e.g. 'IDOR in order API endpoint'; recommended — if empty the frontend falls back to displaying vulnclass)"),
			"severity":  str("critical|high|medium|low"),
			"summary":   str("Finding summary"),
			"intent_id": idp("Intent id that produced this finding (required in task context; optional in conversation context)"),
			"asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "[Provide whenever possible, otherwise the vulnerability location must be clearly stated in the summary] Affected asset ids (optional, 0/1/multiple): parameters/endpoints/sites etc. If a vulnerability affects multiple assets, include all; for pure observations you may omit."},
			"evidence":  str("Required. Reproducible proof: the exact request/command and the key response/output line that demonstrates the vulnerability"),
		}, "vulnclass", "severity", "summary", "evidence"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				VulnClass, Name, Severity, Summary, Evidence string
				IntentID                                     json.RawMessage   `json:"intent_id"`
				AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			}
			_ = json.Unmarshal(in, &a)
			if t.ts == nil {
				return actool.Errorf("report_finding requires task context"), nil
			}
			a.Severity = strings.ToLower(strings.TrimSpace(a.Severity))
			if !db.ValidSeverity(a.Severity) {
				return actool.Errorf(fmt.Sprintf("severity must be one of critical|high|medium|low (got %q); informational observations belong in record_fact", a.Severity)), nil
			}
			if len(strings.TrimSpace(a.Evidence)) < minFindingEvidence {
				return actool.Errorf("evidence is required: include the exact request or command you ran and the key response/output line that proves the vulnerability. If you could not reproduce it, record it with record_fact instead of report_finding"), nil
			}
			payload := map[string]any{"vulnclass": a.VulnClass, "name": a.Name, "severity": a.Severity, "summary": a.Summary,
				"evidence": map[string]any{"by": t.worker, "poc": a.Evidence}}
			var anchors []int64
			for _, raw := range a.AssetIDs {
				if p := pid(raw); p > 0 {
					anchors = append(anchors, p)
				}
			}
			var id int64
			if t.ts != nil {
				intent := pid(a.IntentID)
				if intent > 0 {
					node, err := t.ts.GetNode(intent)
					if err != nil || node == nil || node.Kind != db.KindIntent {
						return actool.Errorf("intent_id must be an intent of this task (linked task intents are read-only)"), nil
					}
				}
				if dup, derr := t.ts.FindDuplicateFinding(a.VulnClass, a.Name, anchors); derr == nil && dup > 0 {
					return actool.Text(fmt.Sprintf("duplicate: finding %d already records this vulnerability (same class, name and affected assets); no new finding was created. Continue with untested surfaces instead of re-reporting it.", dup)), nil
				}
				var err error
				id, err = t.ts.AddNode(db.KindFinding, payload, 9, "confirmed", t.worker, anchors)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if intent > 0 {
					_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> finding
				}
				_, _ = t.ts.AddStandaloneFinding(t.taskID, id, a.VulnClass, a.Name, a.Severity, a.Summary, a.Evidence, t.worker, anchors)
				// Confirmed finding saved → immediately wake this task's planner (don't wait for
				// worker to finish; debounce merges). Prefer contextual trigger (which intent +
				// finding summary); intent comes from tool arg, falls back to owner intent.
				if t.notifyFinding != nil {
					iid := pid(a.IntentID)
					if iid <= 0 {
						iid = t.ownerNode
					}
					t.notifyFinding(iid, a.Summary)
				} else if t.notify != nil {
					t.notify()
				}
			} else {
				// conversation context: no exploration store available, cannot record finding
				return actool.Errorf("report_finding requires task context (exploration store not initialized)"), nil
			}
			t.writes.Findings++
			return actool.Text(fmt.Sprintf("finding recorded: %d", id)), nil
		})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem is a single fact entry for record_fact's batch/single mode.
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // one line of key evidence (command + key output line), supports the conclusion for later verification
	Confidence string            `json:"confidence"` // observed (directly seen in output) | inferred (deduced from observations)
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// recordOneFact writes one fact node and links it to the intent (intent→yields→fact).
// defaultIntent is the batch-mode default intent (used when this item has no intent_id).
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary must not be empty")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	if c := strings.TrimSpace(it.Confidence); c != "" {
		payload["confidence"] = c
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, fmt.Errorf("intent_id must be an intent of this task (linked task intents are read-only)")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return writeTool("record_fact", "Write an exploration fact/conclusion into the exploration graph, linked to the intent that produced it (intent_id). Use this to record exploration results — including positive conclusions like fingerprinting/enumeration, and negative conclusions like 'port closed'/'parameter not injectable'/'no login page found'.\n"+
		"Multiple observations from one exploration should be consolidated into ONE fact, not split into many — merge what can be merged: summary = one-sentence overview of the conclusion, detail = relevant specifics (can contain multiple items). Example: fingerprint intent → one fact {summary:'Identified tech stack and response characteristics of site X', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'}. One intent typically produces one fact; splitting too finely causes the graph to bloat.\n"+
		"The facts array is for writing multiple DISTINCT conclusions at once (each may omit intent_id, defaulting to the top-level intent_id). Returns an ids array, same length and order as facts.\n"+
		"Only record conclusions you ACTUALLY SAW in tool output — do not fabricate. evidence and confidence prevent inaccurate conclusions from polluting the graph:\n"+
		"  - evidence = ONE LINE of key evidence (command + the one or two most conclusive output lines), **keep it concise** — details belong in detail, do not paste large output blocks here.\n"+
		"  - confidence = observed (directly seen in output) | inferred (deduced from observations).\n"+
		"  - For **negative conclusions** (not injectable/port closed/no entry point found), write only 'observation + tentative interpretation' — state what you actually saw; whether to abandon the direction is for the planner to decide globally. Always provide evidence; mark as inferred when methods are not exhausted or evidence is weak (including single attempt, looks-like), mark as observed only when methods are truly exhausted and you directly saw the result.",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "[For multiple distinct conclusions] Facts array; element fields are the same as the top-level fields below (summary/detail/evidence/confidence/intent_id/asset_ids); omitting intent_id defaults to the top-level intent_id. Returns ids with the same length and order as this array.", "items": map[string]any{"type": "object"}},
			"summary":    str("One-sentence summary of this exploration conclusion (an overview of the detail)"),
			"intent_id":  idp("Intent id that produced this fact (your claimed intent; serves as batch default)"),
			"detail":     str("Relevant details of this fact: put all observations from this exploration here"),
			"evidence":   str("ONE LINE of key evidence: command + the one or two most conclusive output lines. Keep it concise; do not paste large output blocks (details go in detail)."),
			"confidence": str("observed (directly seen in output) | inferred (deduced from observations). Be honest when marking negative conclusions."),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Related asset ids (optional, 0/1/multiple): which assets this fact pertains to"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("record_fact requires task context"), nil
			}
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // single-item mode + batch default intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // top-level intent_id = batch default

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // single item: keep original return format
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("fact recorded: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text     string            `json:"text"`
	AssetIDs []json.RawMessage `json:"asset_ids"`
}

// addOneHint attaches a hint node (active/human) to the exploration graph, optionally anchored to assets; returns id.
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text must not be empty")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	id, err := t.ts.AddNode(db.KindHint, map[string]any{"text": it.Text}, 0, "active", "human", anchors)
	if err == nil && t.notify != nil {
		t.notify() // wake the planner so the new hint is read promptly (debounced)
	}
	return id, err
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal attaches a goal node (open) to the exploration graph, linked to the task
// root (origin fact, rel spawns). Origin is t.worker (default "system"): goals written
// by the decomposer are recorded as "goals", goals added at runtime by main agent as
// "human". Waking the planner is done by setGoals after the full batch (see below);
// this function only handles persistence.
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text must not be empty")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals adds exploration goals (goal nodes) to this task. Serves both as the
// goals decomposer's submission tool and as the main agent's runtime tool for adding
// goals — same managed tool, editable in the web UI per agent binding.
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"Add exploration goals to this task. A goal = a final deliverable/verifiable result, not an attack step or reconnaissance action.\n"+
			"Prefer batch: submit multiple goals in the goals array at once; returns ids with the same length and order (failed items have id=0, details in errors). For a single goal, omit goals and provide top-level text directly.\n"+
			"vulnclass is optional: maps to a vulnerability class (e.g. SQLi/IDOR); leave empty for business-logic goals. Whether a goal is achieved is determined by the system marking it as met; this tool only adds new goals.",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "[Preferred] Array of goals to add, processed in order. Each element: text (required, an independent verifiable end goal) + vulnclass (optional). Returns ids with the same length and order as this array.", "items": map[string]any{"type": "object"}},
			"text":      str("[Single] An independent verifiable end goal"),
			"vulnclass": str("[Single] Corresponding vulnerability class (if applicable), e.g. SQLi/IDOR; leave empty for business-logic goals"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals not available: ExplorationStore not initialized"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // single-item mode: top-level text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// Wake the planner (once for the whole batch). Prefer notifyGoal: one set_goals
				// records a single "human added N goals: ..." trigger instead of flooding per
				// item; decomposer/worker lacks this callback → falls back to plain notify
				// (decomposer round-0 has no notify either, i.e. no-op since planner hasn't started).
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// Main agent adds goals at runtime → pull completed/paused task back to running
				// (the terminal-state gate swallows plain notify, so explicit resume is needed).
				// Only mainagent wires this callback; decomposer/worker have nil.
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // single item: keep original return format
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint persists one operation constraint to task_constraints. Origin is
// t.worker (default "system"): decomposer writes as "goals", main agent as "human".
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text must not be empty")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // default to deny: more conservative when type is unspecified
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type must be allow or deny")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints adds operation constraints (allow = what is permitted / deny = what is
// forbidden) to this task. Serves both as the goals decomposer's round-0 constraint
// extraction tool and as the main agent's runtime tool for adding constraints — same
// managed tool, editable in the web UI per agent binding. Constraints are injected into
// planner/worker system prompts to bound the exploration scope.
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"Add operation constraints to this task to bound the exploration scope: type=allow (permitted operations) or deny (prohibited operations).\n"+
			"A constraint = a rule about what operations can/cannot be done (e.g. 'only test this port, do not scan others', 'do not perform write operations on production database', 'passive reconnaissance only'), not a goal or attack step.\n"+
			"Prefer batch: submit multiple constraints in the constraints array at once; returns ids with the same length and order (failed items have id=0, details in errors). For a single constraint, omit constraints and provide top-level text/type directly.\n"+
			"Only register constraints explicitly stated in the task goal/description — do not fabricate; when unsure about the type, use deny (more conservative).",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "[Preferred] Array of constraints to add, processed in order. Each element: text (required, one constraint) + type (allow|deny). Returns ids with the same length and order as this array.", "items": map[string]any{"type": "object"}},
			"text":        str("[Single] Content of one operation constraint"),
			"type":        str("[Single] allow (permitted) or deny (prohibited); defaults to deny"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints not available: ExplorationStore not initialized"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // single-item mode: top-level text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // single item: keep simple return
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return writeTool("add_hint", "Attach a human/main-agent strategic hint to the exploration graph; the planner will see it the next time it generates intents.\n"+
		"Prefer batch: submit multiple hints in the hints array at once (saves round trips vs. calling one by one). Returns an ids array, same length and order as hints (failed items have id=0, details in errors). For a single hint, omit hints and provide top-level text directly.",
		obj(map[string]any{
			"hints":     map[string]any{"type": "array", "description": "[Preferred] Array of hints to add, processed in order. Each element has the same fields as the top-level ones below (text/asset_ids). Returns ids with the same length and order as this array.", "items": map[string]any{"type": "object"}},
			"text":      str("[Single] Hint content, e.g. 'focus on post-authentication endpoints'"),
			"asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Anchored asset ids (optional, 0/1/multiple)"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // single-item mode: top-level text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // single item: keep original return format
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return writeTool("kill_work", "Terminate a running intent (work). Used to stop off-track or pointless exploration; the terminated intent is marked as stopped and will not be auto-reclaimed. Check get_worker_output first to see what it is doing before deciding.",
		obj(map[string]any{"intent_id": idp("Intent id to terminate (= work handle)")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work is currently unavailable"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id is required"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id must be an intent of this task (linked task intents are read-only)"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("sent termination signal to work for intent %d", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("stop doing X, focus on Y"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return writeTool("steer_work", "Inject a real-time course-correction into a running intent (work) without killing it or discarding existing progress: the worker receives your instruction before its next action and adjusts accordingly. Use for in-intent nudges like 'stop doing X, focus on Y'; if the entire direction is wrong, use kill_work and create a new intent instead. Recommend checking get_worker_output first to see what it is doing.",
		obj(map[string]any{
			"intent_id": idp("Intent id to steer (= work handle)"),
			"message":   str("Course-correction instruction for the worker: clearly state what to stop and what to pivot to"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work is currently unavailable"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id is required"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id must be an intent of this task (linked task intents are read-only)"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message is required"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("injected course-correction into work for intent %d (takes effect on next step)", id)), nil
		})
}

// getWorkerOutput returns a work's final (or up-to-termination) conclusion text by intent id.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return readTool("get_worker_output", "Retrieve the final output/conclusion of an intent (work) in this task or a directly linked task. Linked task results carry source_task_id/inherited=true and are read-only. Normally finished works return their summary; terminated (stopped)/abnormal works return their last output up to the point of termination (terminated=true).",
		obj(map[string]any{"intent_id": idp("Intent id (= work handle)")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id is required"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id does not belong to this task or its directly linked tasks"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick, terminated := chosen, false
			if pick == nil {
				pick, terminated = fallback, true
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "(this work has no output yet)", "terminated": true,
					}, intentNode.SourceTaskID))
				}
				return actool.Text("(this work has no output yet)"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "worker_name": pick.Worker, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError, "terminated": terminated,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return readTool("get_worker_trace",
		"View the execution process of an intent (work) — unlike get_worker_output which only returns the final conclusion. Three usage modes:\n"+
			"1. intent_id only → returns a summary stream of every step in that work (summary <= 100 chars, with step_id; just the action outline, no full output);\n"+
			"2. intent_id + q → returns only steps whose summary/full output match the keyword (still only summaries; use mode 3 for content);\n"+
			"3. intent_id + step_ids → returns the full content (detail) of those steps; max 5 per call, excess returns only the first 5 with the rest listed in notice/omitted_step_ids.\n"+
			"Typical flow: use mode 1/2 to locate suspicious step_ids, then mode 3 to get their full output. Thinking steps are excluded. Supports trace history from directly linked tasks; their results carry source_task_id/inherited=true and are read-only.",
		obj(map[string]any{
			"intent_id": idp("Intent id (= work handle)"),
			"q":         str("Keyword: only return steps whose summary/full output matches it (optional; mutually exclusive with step_ids)"),
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "step_ids to retrieve full content for (from mode 1/2 results; max 5 per call, excess returns only the first 5 with the rest listed in omitted_step_ids)"},
			"limit":     intp("Return limit for summary stream/search (optional)"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id is required"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id does not belong to this task or its directly linked tasks"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids let the model decide programmatically
					// whether another call is worth it; the notice states the same in prose.
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"Max %d steps' full content per call; returned the first %d (%v), %d not fetched: %v. "+
							"If this is enough to locate the issue, no need to fetch the rest; if needed, call again with those step_ids.",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return readTool("search_all_worker_traces",
		"[Usually not recommended, as the system already provides most information] Keyword-search (q) across the execution process of ALL OTHER works in this task — for recovering something a worker saw but did not write as a fact (a path/token/error message etc.). "+
			"Steps from your own intent are automatically excluded (they are already in your context). "+
			"Returns only matching step summaries (summary <= 100 chars), each with intent_id; use get_worker_trace(intent_id, step_ids=[...]) to get full content.",
		obj(map[string]any{
			"q":     str("Keyword (searched in summaries + full output of all work steps)"),
			"limit": intp("Return limit, default 100 (optional)"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q is required"), nil
			}
			// Exclude the caller's own intent steps (the worker's own trace is already in its context).
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return readTool("list_worker_traces",
		"[Usually not recommended, as the system already provides most information] List an index of works (intents) that have been executed in this task: intent_id + one-line direction (summary) + state. "+
			"As a worker you cannot see the exploration graph; use this to discover which works are worth inspecting — then use get_worker_trace(intent_id) for their steps and get_worker_trace(intent_id, step_ids=[...]) for details. "+
			"Only lists executed works (running/done/exhausted/blocked/stopped), not unstarted open ones. Note: your task boundary is still the intent you claimed; viewing other works is only for reusing observations / avoiding duplicate effort.",
		obj(map[string]any{
			"q":     str("Filter by summary keyword (optional)"),
			"limit": intp("Return limit, default 50 (optional)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// report_finding: during planning situation assessment, if a vulnerability is already confirmed, the planner can record it directly (same tool as worker).
		t.addFinding(),
		// list_companies: view company list + scope + asset counts (get company_id / understand ownership scope).
		t.listCompanies(),
		// list_assets: search the full asset library by DSL during planning (complements
		// list_untested_assets' "in-scope untested" view with the ability to query the
		// entire library by domain/fingerprint/port/status code etc.).
		t.listAssets(),
		// add_company_scope: during planning, add domains/IPs/CIDRs/ICPs/keywords to a company's asset scope (auto-claims matching assets).
		t.addCompanyScope(),
		// add_task_scope: proactively add entire root domains/companies/subdomains/IPs into this task's test scope (coverage denominator).
		t.addTaskScope(),
		// list_untested_assets: query untested assets within this task's scope on demand (by type + pagination), decide what to test next.
		t.listUntestedAssets(),
	}
}

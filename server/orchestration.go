package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// This file implements P2 "cross-task orchestration toolset" (docs/benchmark-orchestration §2 P2). These are host tools -- they need
// access to the Manager (any task's Store), the Engine (pause), and the task-creation flow, so they live in the server layer.
// read-type tools redirect "existing per-task tools" to run against the target task's store (building a temporary ToolSet
// and Call the corresponding tool), thereby reusing exactly the same logic; control-type tools (spawn/pause) call the Manager/Engine directly.
// Like the traffic tools, they are seeded into the tools table and bound per agent (only visible when bound to the orchestration agent).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.platformTools()...) // platform operation tools (create/modify skills/tools/MCP, for Auto use)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] failed to load: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id is required"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task does not exist: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify) // hint writes wake this task's planner (no-op for read tools)
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"List all tasks (id/description/goal/status/runtime/parent task/LLM profile). The orchestration agent uses it to grasp the overall picture, see which tasks have been stuck for too long, and which LLM each uses. Runtime: running = creation->now, terminal state = creation->last activity (seconds). llm_profile: the profile name used by the task's planner/worker; (active profile) = follows the global active profile.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "((active profile))"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(deleted)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"List available LLM profiles: id, name, model, format, and whether it is the currently active profile. Use the id to specify a subtask-specific LLM through spawn_task's llm_profile_id parameter (e.g. a cheap model for reconnaissance, a strong model for exploitation). Does not include the API Key.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"Create a new subtask and start the exploration engine, returning task_id. Used to dispatch one piece of work (e.g. a challenge/a target) as an independent task. parent_ref is optional: fill in the parent task id of the current orchestration to link parent and child.",
		objSchema(map[string]any{
			"description":            strParam("Task description (short title)"),
			"goal":                   strParam("Task goal (what is to be achieved)"),
			"parent_ref":             strParam("Optional: parent task id (for parent-child linking)"),
			"llm_profile_id":         map[string]any{"type": "integer", "description": "Optional: LLM profile id to use for this subtask's planner/worker (see list_llm_profiles); if left empty, inherits from the parent task, then falls back to the global active profile"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "Optional: task-level timeout (seconds). When it expires, a graceful wrap-up is triggered and the task enters the timeout terminal state; leave empty or 0 = no time limit"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "Optional: planner heartbeat trigger interval (seconds). If this much time has passed since the end of the last planning round / the task start with no trigger in between -> trigger a planning round (fallback against deadlock + wake up to supervise in-flight workers). Leave empty or 0 = default 600 (10min);"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "Optional: can be enabled for simple tasks; on creation, a seed intent (content = description + goal) is dispatched directly so the worker can start testing without waiting for the first-round planner; default false (follows the standard plan-first-then-execute flow)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description, Goal, ParentRef string
				LLMProfileID                 json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds               int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds         int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent              bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "Unnamed task"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal is required"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM profile #%d does not exist or has no API Key", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			t, err := s.m.CreateTask(a.Description, a.Goal, pin, a.TimeoutSeconds, a.PlanHeartbeatSeconds)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// shared post-creation flow, reusing the same launchTask as HTTP task creation (server.go createTask):
			// seed + visibly decompose goals in the background (round 0/LLM step/goal by goal) + engine.Run.
			// seed_first_intent defaults to false (standard plan-first-then-execute); can be enabled for simple tasks to dispatch a work directly for testing.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "Pause the specified task (stops its planner/worker loop).",
		objSchema(map[string]any{"task_id": strParam("id of the task to pause id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task does not exist: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "Read the exploration graph overview of the specified task (same as graph_overview: asset counts/frontier/findings/coverage, etc.); use task_id to specify the task.",
		objSchema(map[string]any{"task_id": strParam("Task id id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "Read the confirmed vulnerabilities of the specified task (including flag/PoC; each entry carries id/task_id/intent_id/vulnclass/severity/summary/status); use task_id to specify the task.",
		objSchema(map[string]any{"task_id": strParam("Task id id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "Inject a strategic hint into the specified task (the task's planner will read it when generating intents in its next round).\n"+
		"★Prefer batching: put multiple hints into the hints array and submit them in one call (returns an ids array, same length and order as hints, failed items have id=0); for a single hint, omit hints and give the top-level text directly.",
		objSchema(map[string]any{
			"task_id":   strParam("Task id id"),
			"hints":     map[string]any{"type": "array", "description": "[Prefer this] Array of hints; each element has the same fields as the top level (text/asset_ids).", "items": map[string]any{"type": "object"}},
			"text":      strParam("[single] Hint content"),
			"asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Anchored asset ids (optional, 0/1/many; asset ids within that task)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"View the execution process of a work (intent) in the specified task: use get_task_worker_trace(task_id, intent_id) to see step summaries; then pass step_ids=[...] to get the full content of those steps (at most 5 at a time; if more are passed only the first 5 are returned).",
		objSchema(map[string]any{
			"task_id":   strParam("Task id id"),
			"intent_id": map[string]any{"type": "integer", "description": "Intent id (the work within that task work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Optional: ids of the steps whose full content to fetch (at most 5 at a time; if more are passed only the first 5 are returned, the rest are listed in omitted_step_ids)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "List the works (intents) that have run in the specified task + their step counts, used to find which works are worth looking into (then use get_task_worker_trace).",
		objSchema(map[string]any{"task_id": strParam("Task id id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "Search the execution process of all works in the specified task by keyword (returns matching step summaries + intent_id).",
		objSchema(map[string]any{"task_id": strParam("Task id"), "q": strParam("Search keyword")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"Read the full content of an exploration graph node in the specified task (finding/fact/intent/goal: summary + details/evidence/PoC). id is the exploration node id (e.g. returned by report_finding, or the id in list_task_findings). Use it to fetch the vulnerability's complete evidence before writing a vulnerability report.",
		objSchema(map[string]any{
			"task_id": strParam("Task id id"),
			"id":      map[string]any{"type": "integer", "description": "Exploration graph node id (not an asset id)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"Write/update the [detailed report] of a registered vulnerability (full Markdown text, overwrites the old content as a whole). For finding_id, pass the id returned by report_finding (the number in \"finding recorded: <id>\"). The report should include: vulnerability overview, impact and harm, reproduction steps, evidence/PoC, remediation advice.",
		objSchema(map[string]any{
			"finding_id": map[string]any{"type": "integer", "description": "Target vulnerability id (the id returned by report_finding)"},
			"report":     strParam("Full text of the detailed report, in Markdown format"),
		}, "finding_id", "report"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				FindingID json.RawMessage `json:"finding_id"`
				Report    string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // reuse the "number or numeric string" parsing
			if nodeID <= 0 {
				return actool.Errorf("finding_id is invalid"), nil
			}
			n, err := s.m.pg.SetFindingReportByNodeID(nodeID, a.Report)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("no vulnerability record found for finding_id=%d (register it with report_finding first))", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (it is inherently meant to
	// operate the platform). SeedTool takes effect on first insert; rows already seeded in old DBs are back-filled by seedAutoDefaultBindings.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()     // goals prompt gains an "extract operation constraints" step -> old DBs get a new default version appended (one-time)
	s.reseedMainAgentPrompt() // mainagent prompt gains "after the goal is achieved, add_intent asks whether to create a goal" (one-time)
	s.reseedPlannerPrompt()   // planner prompt: rewrite the "0 intents" justification + add quantitative acceptance check (one-time)
	s.reseedWorkerPrompt()    // worker prompt: add an evidence threshold for negative conclusions (one-time)
	s.seedReporterAgent()     // preset "report writing" agent + tool bindings + finding trigger (one-time)
	// Note: pentest's default tool bindings need no migration -- on a fresh initialization BuiltinToolSeeds already seeds
	// list_assets/insert_assets/report_finding/list_findings/list_companies together with
	// pentest (the project has no legacy DB yet, so no migration is done).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task's llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// also refresh some built-in agent tools to the code defaults:
	//   - goal_met: the description seeded in old DBs carries the misleading "end this round of planning" wording, which makes the planner treat it as
	//     a means of "ending an empty round" and wrongly judge the whole task complete right at the start.
	//   - insert_assets: added the related input parameter (marks whether an asset is relevant to the current task and decides whether it counts toward coverage),
	//     SeedTool is first-insert-only, so schemas already seeded in old DBs would otherwise not receive this new parameter.
	//   - list_facts: changed to pagination, added limit/before/q input parameters; the empty schema already seeded in old DBs would otherwise
	//     show "no parameters" on the tool management page, and the model would not get these parameter descriptions either.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] refreshed orchestration/platform tool schemas to code defaults (one-time)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met failed to unbind from planner: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt refreshes the prompt of the goals goal decomposer to the [current code default] -- because the default body added
// the step "extract operation constraints (set_constraints) first, then decompose goals", while SeedPromptIfEmpty is first-insert-only and the old DB's
// existing version 1 does not receive this step. Here version management is used to [append a new version] and switch to it (ResetPromptToDefault),
// the old version is still kept in history, and if the user customized it, it can be recovered from the version records. Guarded by a settings flag -> runs only once;
// whenever the default changes again, bump this flag. Fresh DBs need no handling (SeedPromptIfEmpty has already seeded the latest default).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // on a fresh DB with no agent row yet, seedPrompts seeds the latest default directly, so this migration is unnecessary
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// on a fresh DB seedPrompts has already seeded the latest default -> the current version already equals the code default, so there is no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals prompt refresh to the new default failed: %v", err)
		return
	}
	log.Printf("[prompts] goals prompt new default version appended (added the extract-operation-constraints step, one-time)")
}

// reseedMainAgentPrompt refreshes the mainagent prompt to the [current code default] -- the default body added a "once all goals
// are achieved, when add_intent injects an intent directly, ask the human whether to register it as a formal goal" guidance, while SeedPromptIfEmpty is first-insert-
// only, existing versions in old DBs do not receive it. Version management is used to [append a new version] and switch to it (ResetPromptToDefault); the old version is still
// kept in history, and if the user customized it, it can be recovered from the version records. Guarded by a settings flag -> runs only once. Fresh DBs need no handling
// (SeedPromptIfEmpty has already seeded the latest default). Structurally identical to reseedGoalsPrompt.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // on a fresh DB with no agent row yet, seedPrompts seeds the latest default directly, so this migration is unnecessary
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// on a fresh DB seedPrompts has already seeded the latest default -> the current version already equals the code default, so there is no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent prompt refresh to the new default failed: %v", err)
		return
	}
	log.Printf("[prompts] mainagent prompt new default version appended (added asking whether to create a goal after goal achievement, one-time)")
}

// reseedPlannerPrompt refreshes the planner prompt to the [current code default] -- the default body was streamlined and refactored, and "restraint" was downgraded to
// dedup only, added "depth over coverage" and "hard floor: when the goal is unmet and no intent is running, one must be produced", and put an upper bound on negative-conclusion re-verification.
// whenever the default changes substantively, bump the flag below (currently v2) so existing old DBs refresh once more. SeedPromptIfEmpty is first-insert-only and existing versions in old DBs do not receive it, so version management is used to
// [append a new version] and switch to it (ResetPromptToDefault); the old version is still kept in history, and if the user customized it, it can be
// recovered from the version records. Guarded by a settings flag -> runs only once. Fresh DBs need no handling (SeedPromptIfEmpty has already seeded the latest default). Structurally identical to
// reseedGoalsPrompt in every respect.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // on a fresh DB with no agent row yet, seedPrompts seeds the latest default directly, so this migration is unnecessary
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// on a fresh DB seedPrompts has already seeded the latest default -> the current version already equals the code default, so there is no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner prompt refresh to the new default failed: %v", err)
		return
	}
	log.Printf("[prompts] planner prompt new default version appended (streamlined refactor + restraint downgraded to dedup + depth-first + negative re-verification upper bound, one-time)")
}

// reseedWorkerPrompt refreshes the worker prompt to the [current code default] -- the default body was streamlined and refactored, and negative conclusions were changed to
// "write only observations + tentative readings", and added "write cross-intent leads into a fact for the planner instead of chasing them yourself". Bump the flag to v2 so existing old DBs refresh once more. SeedPromptIfEmpty is first-insert-only,
// existing versions in old DBs do not receive it, so version management is used to [append a new version] and switch to it; the old version is still kept in history and can be recovered.settings flag
// guard -> runs only once. Fresh DBs need no handling. Structurally identical to reseedGoalsPrompt.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // on a fresh DB with no agent row yet, seedPrompts seeds the latest default directly, so this migration is unnecessary
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// on a fresh DB seedPrompts has already seeded the latest default -> the current version already equals the code default, so there is no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker prompt refresh to the new default failed: %v", err)
		return
	}
	log.Printf("[prompts] worker prompt new default version appended (streamlined refactor + negatives changed to observational style + cross-intent lead reporting, one-time)")
}

// seedReporterAgent presets a custom "report writing" agent (builtin=false, editable/deletable in the UI):
// binds update_finding_report + the task query tools, and attaches a "triggered whenever report_finding is called"
// trigger -- every time a vulnerability is registered, it is woken up to write a detailed report. One-time (guarded by a settings flag): not recreated after the user deletes it.
// Dependency: the orchestration tools have already been inserted via SeedTool above this function, so the binding works.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // attempt only once regardless of success

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key key is already taken (the user created it manually) -- not overwritten
	}
	a, err := s.m.pg.CreateAgent("reporter", "Report Writing",
		"Detailed vulnerability report writing: triggered automatically when a vulnerability is found; after fetching the evidence and execution process, writes a Markdown report and writes it back.")
	if err != nil {
		log.Printf("[reporter] failed to create agent: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt failed: %v", err)
	}
	// trigger run policy: parallel + none -- one report per vulnerability, multiple findings are processed concurrently each writing its own.
	// merge must be none: otherwise (default all) a wave of findings would be merged into a single run, making parallelism pointless.
	// maxParallel=5: at most 5 report sessions at the same time, to avoid too many LLM calls at once.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] failed to set trigger run policy: %v", err)
	}
	// bind the tools it needs: write reports + read evidence/execution process/situation.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] failed to bind tools: %v", err)
	}
	// trigger: fires whenever report_finding is called (the tool returns "finding recorded: <id>" carrying finding_id,
	// the task id is also in the trigger message).
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:   "reporter",
		Enabled:    true,
		OnToolCall: true,
		ToolNames:  []string{"report_finding"},
		ToolCallMessage: "A vulnerability was just registered above via report_finding. Please extract finding_id from the trigger context" +
			"(the number in the tool's return value \"finding recorded: <id>\") and the task id, and per your duties write a detailed report for this vulnerability, " +
			"and finally call update_finding_report(finding_id, report) to save it.",
	}); err != nil {
		log.Printf("[reporter] failed to create trigger: %v", err)
	}
	log.Printf("[reporter] preset report-writing agent + finding trigger")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker→planner default binding switch
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope default binding failed: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope unbind failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: replace the old asset tool names, add insert_assets/add_company_scope
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// asset tools: when Auto operates the platform it often needs to view/register assets and manage the company scope.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

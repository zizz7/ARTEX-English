package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns "built-in tools" from pure code into an enumerable, DB-overridable
// catalog:
//   - BuiltinToolSeeds(): expands the three execution agents' built-in tool sets into
//     seed records (key + description + parameter schema + default-bound agents) for
//     the server to idempotently seed into the tools table on startup.
//   - ToolResolve hook: at runtime, applies per-agent filtering + description/schema
//     overrides + parameter default injection based on DB tools rows.
//     key/handler remain in the code layer; the DB only changes "prose and defaults".
// The handler (Call behavior) always comes from code — the DB cannot change it, only
// the description and default parameters that the model sees.

// ToolSeed is a seedable snapshot of a built-in tool: Key is CoreTool.Name() (bound
// to the handler, read-only in the UI), Desc/Schema come from the code-level tool
// definition, and Agents lists which agents the code binds this tool to by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, immutable
	Desc   string         // top-level description (can be overridden in UI)
	Schema map[string]any // parameter JSON-Schema (structure is read-only; description/default can be edited in UI)
	Agents []string       // default-bound agent keys (worker/planner/mainagent)
}

// builtinToolsByAgent constructs each execution agent's domain tool set using a
// read-only shell ToolSet (nil stores). Tool constructors only capture closures in
// the Spec without dereferencing stores at construction time, so nil is safe — these
// tools are only used here to read Name()/Description()/InputSchema(), never Called.
//
// Deliberately EXCLUDES SDK generic tools actool.DefaultTools() (Read/Write/Edit/
// MultiEdit/LS/Glob/Grep/Bash): every agent always has them with no per-agent binding
// decision, and their descriptions are mostly in Prompt() (this table only overrides
// Description(), which would cause misleading partial overrides). No seed → no DB
// row → ToolResolve passes them through unchanged, same behavior as before. Only
// ARTEX's own domain tools are entered into the catalog for management.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (goal decomposer) default-binds set_goals + set_constraints: these write
		// the decomposed goals and extracted operation constraints to the store. Shares
		// the same managed tools as mainagent; the web UI can edit descriptions/schemas
		// and toggle per-agent binding.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto default-binds vulnerability reporting + asset management tools; other
		// domain tools can be toggled in the UI as needed. New DBs are seeded from here;
		// legacy DBs are migrated via seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (standalone pentest agent) default-binds: list assets / insert assets /
		// report findings / list findings / list companies. New DBs are seeded from here;
		// legacy DBs are migrated via seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools are still entered into the catalog (visible in
// the web UI, can be manually bound per agent), but are default-bound to NO agent —
// ToolResolve drops tools with empty bindings for all agents; explicit opt-in is
// required. They remain in an agent's base tool set (e.g. goal_met in PlannerTools)
// so that: (1) the seed can construct them to obtain desc/schema, and (2) when
// manually re-bound, ToolResolve finds them in the base set and keeps them.
//
// goal_met: bypasses per-goal prove_goal to globally declare the ENTIRE task complete;
// high impact with risk of false positives, and redundant with the "prove_goal marks
// the last goal → auto wrap-up" flow, so it is not bound to any agent by default —
// bind it manually when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds deduplicates and merges the built-in tool sets of all agents into a
// seed list: tools with the same name (e.g. list_assets used by multiple agents) are
// merged into one entry with Agents as the union; tools in defaultUnbound are forced to
// have empty agent bindings.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // entered into catalog and can be manually bound, but default-bound to no agent (store [] not null, consistent with other tools)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// default parameters get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only default values are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}

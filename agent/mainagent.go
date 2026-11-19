package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	prov           llm.Provider
	model          string
	tx             *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window         int                                    // context window in tokens (for compaction)
	windowFn       func() int                             // optional dynamic task-chain minimum
	maxTurns       int                                    // max agent turns per run (0 = unlimited)
	proxyAddr      string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert    string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch      WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir        string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork      func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	maxTokensFn    func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (section [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate
// artifact output spec tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `You are the "main agent" of an authorized penetration testing system — the human operator's interface. You do not explore on your own, nor do you autonomously generate a continuous stream of intents (that is the planner's job). Your responsibilities:

1. Observe: Use graph_overview / list_findings / list_facts / list_assets / get_worker_output to answer the human's questions about current progress.
2. Steer (translate the human's intent into system actions):
   - Human wants to "change direction / emphasize a certain vulnerability class / focus on a certain area" → use add_hint to write a hint (the planner will read it on the next wake-up).
   - Human wants to "immediately test a specific target" → use add_intent to directly inject a high-priority intent (priority 8-10). The system will automatically pull a completed task back to running state, let a worker claim this intent and execute it, then return to the completed state when done.
     **When all task goals have been achieved** (all goals in graph_overview are met): before dispatching, first assess whether this intent implies a "new outcome to achieve". If it does, restate the inferred goal in one sentence and **ask the human whether to register it as a formal goal** — if yes → use set_goals to register it (the task then enters regular planning and the planner will autonomously drive it forward); if no / they just want to do a quick probe → only dispatch this single intent via add_intent, and the task returns to completed state after the worker finishes (no autonomous continuation). If the intent is clearly a one-off verification with no implied new goal, just use add_intent directly — no need to ask every time.
   - Human wants to "course-correct a running intent (work) in real time (stop doing X, focus on Y)" → use steer_work (non-disruptive, preserves existing progress, takes effect before the worker's next action); first use get_worker_output to see what it's doing. If the direction is entirely wrong, use add_intent to dispatch a new intent instead.
   - Human wants to "add a new final goal to achieve" → use set_goals to add the goal. The system writes the goal into the task graph and **automatically pulls completed/paused tasks back to running state** (the planner will then re-evaluate whether goals are met) — no need for the human to manually resume.
   - Human wants to "add/modify test constraints (allow/prohibit certain operations, e.g. 'only test the current port', 'no brute-forcing', 'passive reconnaissance only')" → use set_constraints to register (type=allow for allowed / type=deny for prohibited). Constraints are injected into the planner/worker prompts on the next planning round to bound the exploration scope; they can also be added/edited/removed in the overview's "Constraint Management" section.
3. Reply concisely in plain language, explaining what you did.

Current task goal: {{.Goal}}

Do not fabricate findings; only answer based on real data returned by tools.`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // add_hint wakes this task's planner (debounced)
	tsx.SetResumeTask(resume)     // set_goals adds new goal → pull completed/paused task back to running
	tsx.SetNotifyGoal(notifyGoal) // set_goals adds new goal → record a "human added goal: ..." trigger for planner
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// Domain tools + base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash).
	// When asset coverage is disabled, strip add_task_scope/list_untested_assets (not included in prompt).
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	tools = tsx.StripCoverageParams(tools) // when coverage is disabled, hide insert_assets' related param
	defer cleanup()
	// This task's working directory <workDir>/tasks/<taskID>, created upfront.
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // routed through the recording proxy for audit trail; load proxy CA to verify MITM re-signed HTTPS certs
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), independent of the MITM recording proxy; empty means direct.
		EnableWebSearch:    m.webSearch.Enabled,
		WebSearchBackend:   m.webSearch.Backend,
		BraveSearchAPIKey:  m.webSearch.BraveKey,
		TavilySearchAPIKey: m.webSearch.TavilyKey,
		WebSearchProxy:     m.webSearch.Proxy,
		BashEnv:            proxyEnv(m.proxyAddr, m.proxyCACert), // Bash subcommands default to proxy + trust CA
		WorkingDir:         mainDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:      cmdOutDir(mainDir),
		MaxTurns:           m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:         compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:              actool.NewTodoStore(),                  // session-level temporary todos (TodoWrite), planning only, discarded on exit
		// budget hit (steps) → SDK runs wrap-up: output a one-sentence progress summary to the user. Prompt and wrap-up turn count are admin-editable (default 10 turns).
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // when this profile selects non-streaming, use Provider.Complete
		MaxTokens:    m.maxTokens(),   // 0 = do not send cap, let server default decide
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per task
		opts.Transcript = m.tx
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
	}
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}

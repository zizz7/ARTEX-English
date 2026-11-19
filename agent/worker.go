package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/memory"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), or "tavily"
// (TavilyKey required). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
}

type Worker struct {
	prov        llm.Provider
	model       string
	workDir     string
	proxyAddr   string
	proxyCACert string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch   WebSearchOpts     // web_search tool backend selection (off by default)
	mem         *memory.Store     // cross-engagement tradecraft memory (G4)
	tx          *transcript.Store // raw LLM conversation persistence (nil = off)
	window      int               // context window in tokens (for compaction)
	windowFn    func() int        // optional dynamic task-chain minimum
	maxTurns    int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleHardGrace is how far past the soft wall-clock budget (runTimeout) the hard
// ctx deadline sits — a backstop for a hung turn; the soft budget handles the
// normal case at the turn boundary.
const settleHardGrace = 90 * time.Second

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "You are about to be terminated due to budget exhaustion. Do not run any more commands or probes. Proceed in order: (1) Persist all identified but unrecorded content one by one — new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed vulnerabilities via report_finding; (2) **End with a single plain-text sentence** summarizing what you did and what key conclusions you reached (this sentence will be displayed as the result of this run — you must output it)."

// SetMemory enables cross-engagement tradecraft memory (RecallMemory/RecordMemory
// tools + auto-injection of relevant memories, docs §8 G4).
func (w *Worker) SetMemory(m *memory.Store) { w.mem = m }

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (section [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the artifact output
// spec are NOT here — they are code-owned and appended by workerSystem after rendering
// (sections [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `You are a "worker" agent of the ARTEX platform's authorized penetration testing system. You have been assigned ONE intent (a one-sentence exploration direction), and your sole responsibility is: **complete this single intent, write your findings back to the knowledge graph, then stop and return.**

**Boundaries (hard rules)**:
1. **Do only this one intent.** The intent boundary is the hard line: a fingerprinting intent means only fingerprinting — do not also enumerate endpoints, brute-force directories, scrape JS for APIs, or test for vulnerabilities — those are other intents, dispatched by the planner to other workers. You are also not responsible for generating exploration directions. **If while exploring this intent you spot leads worth investigating that fall outside this intent** (error-disclosed paths, points that may correlate with other assets, suspected entry points for another exploitation chain), **mention them briefly in a fact's summary for the planner** (it will see them in the recent_facts overview and plan accordingly) — do not pursue them yourself.
2. **Exhaust all approaches within this intent before concluding.** Initial resistance (payload filtered / 404 / blind injection) does not mean the exploration is complete — try alternative encodings/methods/parameters/paths to exhaust all reasonable approaches within this intent; but exhaustion is limited to WITHIN this intent only, never expand to other intents. Once truly exhausted or all reasonable approaches have been tried, write back immediately and return; do not continue because "the overall goal is not yet achieved," and do not spin idly on an already-exhausted direction just to fill steps.
3. Operate only within the authorized scope. If [OPERATION CONSTRAINTS] are attached at the top of the system prompt, those are the highest-priority hard rules: self-check before every command/probe, and do not proceed if it violates them (even if it falls within your assigned intent).
4. **A tool failure is not a negative result.** If a tool times out, is not installed, gets rate-limited, or returns unusable output, retry once with adjusted options (shorter timeout, smaller wordlist, lower rate, different flags) or an equivalent tool (e.g. ffuf / feroxbuster / gobuster, httpx / curl, nuclei templates / a manual request) before drawing any conclusion. If it still fails, record the failure itself as an inferred fact ("surface X untested: tool Y failed with ...") so the planner knows the surface is untested, not clean.

**Write back as you discover** (only what is written to the graph counts — anything only in your head or text does not; write each result immediately, do not batch them until the end only to lose them when steps run out). Three types of write-back — do not mix them up:
- **New assets/resources → insert_assets (asset graph)**: endpoints / parameters / tech fingerprints / services / credentials / subdomains, etc. You only pass raw information; keys and parent-child relationships are computed by code: for new endpoints pass the full url+method (code auto-creates domain→site→endpoint, extracts URL query params, body/header params go in params); for fingerprints pass type=tech, name=technology name, on_url=site address, props={version, category}. Structured attributes go on the asset's own props (status code/title/body length/content_type go in props.http); exploration conclusions do not go here. Use the assets array to batch-register multiple assets at once.
- **Exploration conclusions/facts → record_fact (exploration graph, pass intent_id)**: use this for both positive conclusions (fingerprints/enumerations) and negative/uncertain observations (port closed, parameter not injectable, no entry point found). **Consolidate multiple observations into ONE fact** (summary in one sentence + detail with tech stack/status codes/response characteristics), not one fact per attribute — one intent typically produces only one fact; splitting them out causes unbounded graph growth; use the facts array only when there are genuinely DISTINCT conclusions. **Write only incremental findings**: scan the recent_facts above before writing; only record what is NEWLY discovered this time — do not rephrase and re-record existing facts (if you only confirm what exists with no new info, no need to record). **Write only what you actually observed**: provide evidence (one line: command + the one or two most probative lines of output, concise, details go in detail), mark confidence (observed = directly seen / inferred = deduced from phenomena). **For negative conclusions, write only "observation + tentative interpretation"**: state what you actually saw + at most one sentence of tentative inference; whether to abandon the direction is the planner's call based on the full picture (e.g.: "Tried X/Y/Z on this parameter with no response or error (evidence:...), no apparent injection point for now, not all techniques exhausted"). Mark inferred if approaches are not fully exhausted, or evidence is "looks like / probably / tested only once"; mark observed only when you have truly exhausted this intent's approaches (alternative encodings/params/paths/methods) and the evidence is directly observed; especially in early stages, prefer inferred to let the planner review.
- **Confirmed vulnerability → report_finding (exploration graph, with PoC, pass intent_id)**: **use ONLY when you have actually triggered it this time and obtained reproducible evidence (request/response or command output)**. It is strictly forbidden to treat "version/fingerprint matched a CVE," "parameter looks injectable," or "inferred from external vulnerability databases/changelogs/code diffs" as confirmed. Do not substitute actual triggering with CVE database lookups or patch version comparisons. If you cannot trigger it but there is suspicion → use record_fact to record an inferred fact (suspected point + why not triggered) for the planner — do not force it into a finding.

**Query context (as needed)**: list_assets / asset_neighbors / list_facts (exploration facts, paginated newest-first, default 20, filterable with q, paginate with before) / list_findings / node_detail (exploration node id, not asset id).

After completing this intent, summarize in one sentence what you did and what facts you wrote back. Be pragmatic, restrained, and focused on this single intent.`

// workerTrafficBlock is section [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**Traffic tools**:\n- traffic_search / traffic_get / traffic_blob: review responses and find previously visited resources. **Check traffic first — do not re-curl the same URL**. traffic_search **requires specifying host**; by default returns only 3 lightweight index entries (id/method/url/status/resp_len, no response body). For more results, explicitly increase limit. Use body_contains for full-text search within request/response bodies (minimum 3 characters, supports substrings, e.g. finding passwords/keys/errors/internal addresses). To view a specific raw entry use traffic_get(id); oversized bodies display as @blob sha256:<hash> — use traffic_blob(hash) to retrieve the full text in segments."
}

// artifactSpec is section [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**Artifact output convention**: All intermediate artifacts — scripts, payloads, captured response bodies, temporary data, etc. — **must be written to this task's working directory " + dir + "** (relative paths land here, or use the absolute path) — **do not write to /tmp or any other absolute path**."
}

// workerArtifactSpec is the worker's section [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**Artifact output convention**: All intermediate artifacts — scripts, payloads, captured response bodies, temporary data, etc. — **must be written to this intent's dedicated working directory " + runDir + "** (already created automatically; just use relative paths here, no need to create directories manually) — **do not write to /tmp or any other absolute path**."
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n[YOUR ASSIGNED INTENT (your sole task this run: do only this one, produce only facts, stop when done)]:\n%s\nIntent id: %d (pass this when writing back via record_fact / report_finding)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage is a signal for the planner to judge "which categories are under-tested /
	// whether to expand scope" — this conflicts with the worker's responsibility boundary
	// of "only do the assigned intent, do not pursue uncovered points" → remove it from
	// the worker view. data is a new map exclusive to this worker; deleting keys does
	// not affect the planner.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n[GLOBAL EXPLORATION SITUATION (for your awareness only, NOT your task list)]:\n" +
		"Below is the current exploration overview for the entire task. The **sole purpose** of showing you this is situational awareness.\n" +
		"**It does NOT expand your responsibility boundary**: you still do only the one intent assigned above. If you see other open intents / uncovered points / other exploitable directions here, **do NOT pursue them yourself** — those are other workers' tasks, dispatched by the planner. If you discover related new leads, at most write them into a fact to inform the planner — do not pursue them yourself.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // when report_finding is persisted, immediately wake the planner with "which intent + finding"
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	base = append(base, actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	tools = tsx.StripCoverageParams(tools) // when coverage is disabled, hide insert_assets' related params
	defer cleanup()

	// Intent + global situation now go in the LAUNCH USER MESSAGE (see input below);
	// the system prompt keeps only the static role text (sections [A]/[B]/[C] + deferred
	// block). Consistent with the planner: move volatile runtime data out of system so
	// it stays stable per session and is more cache-friendly; the tradeoff is that this
	// user message may be compressed by compaction in long runs. This intent's dedicated
	// working dir <workDir>/tasks/<taskID>/i<intentID> is pre-created by the engine.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // operation constraints (if any) injected into system prompt; worker strictly obeys during execution
	}
	system, boundary := deferredSystem(sysBody, def)
	// Task-level deadline (injected via ctx) clamps this run's wall-clock budget + determines wrap-up prompt (see taskclock.go).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch goes through the recording proxy; its HTTP traffic is captured just like
		// curl. Loading the proxy CA lets MITM re-signed HTTPS certs pass normal
		// verification (instead of disabling verification). Empty proxy = direct.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), independent of the MITM recording proxy; empty = direct.
		EnableWebSearch:    w.webSearch.Enabled,
		WebSearchBackend:   w.webSearch.Backend,
		BraveSearchAPIKey:  w.webSearch.BraveKey,
		TavilySearchAPIKey: w.webSearch.TavilyKey,
		WebSearchProxy:     w.webSearch.Proxy,
		// Bash subcommands' HTTP defaults to recording proxy + trust its CA (tools need no -x/-k).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// Wall-clock budget, checked at turn boundaries, does not interrupt mid-turn; 0 = unlimited.
		// When a task-level deadline exists, clamped to min(own budget, time remaining to deadline)
		// so this run naturally enters wrap-up when the task reaches its deadline (see taskclock.go).
		MaxDuration: maxDur,
		// Budget hit (turns OR duration) → SDK runs one wrap-up turn (Bash hidden), persisting
		// identified results to avoid incomplete runs. When clamped (by task deadline),
		// PromptByReason is used: timeout = task deadline reached → task-timeout prompt;
		// max_turns = steps exhausted within clamped window → fall back to per-run prompt.
		// Not clamped → pure per-run behavior.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. Truncation limit uses SDK default (30000 chars).
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // session-level temporary todos (TodoWrite), planning only, discarded on exit
		NonStreaming:  w.nonStreaming(),                       // when this profile selects non-streaming, use Provider.Complete
		MaxTokens:     w.maxTokens(),                          // 0 = do not send cap, let server default decide
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.mem != nil {
		opts.Memory = &agentcore.MemoryOptions{Store: w.mem, AutoInject: true, MaxInject: 3}
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// Intent + global situation + launch instruction + intent-anchored asset raw data
	// all go in this launch user message. Intent is placed first and most prominent;
	// overview is for situational awareness only. Raw asset JSON is attached directly,
	// without extraction/formatting, saving an opening list_assets call. Note: these
	// runtime data now live in the user message and risk being compressed by compaction
	// in long runs (the intent is the worker's entire job — if compressed, it may need
	// to be pinned separately; to be evaluated).
	input := renderIntentTask(intent) + overview + "\n\nBegin executing the intent above: do only this one, produce only facts, stop when done."
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					input += "\n\nTarget assets corresponding to this intent's asset_ids:\n" + string(b)
				}
				// Assets explicitly targeted by this intent → automatically added to the task's
				// test scope (same conservative granularity as insertAssets). upsertTaskScope's
				// ON CONFLICT DO NOTHING + uq_task_scope unique index guarantees no duplicates;
				// reruns/retries are also idempotent no-ops.
				// When asset coverage is disabled, do not accumulate test scope (denominator).
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}

	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "Continue execution."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "Continue executing the new intent from the previous human conversation input. Do not repeat already completed actions."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n[HUMAN CONVERSATION INPUT - NEW INTENT]\n" + message +
				"\n\nExecute this human input immediately. After completion, decide based on context whether the original task needs to continue."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n[HUMAN CONVERSATION INPUT - NEW INTENT]\n" + message +
				"\n\nPrioritize executing this human input."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// We only add a HARD ctx backstop past the soft wall-clock budget, for the rare
	// case a single turn hangs so the turn-boundary check never runs — the soft
	// budget handles the normal case gracefully (no mid-stream abort). pause /
	// planner kill cancel ctx; the engine distinguishes those and re-queues/stops.
	// backstop uses the EFFECTIVE (deadline-clamped) budget, not the raw runTimeout,
	// so a task-timeout run's hard deadline sits just past its clamped soft budget.
	runCtx := ctx
	if maxDur > 0 {
		var runCancel context.CancelFunc
		runCtx, runCancel = context.WithTimeoutCause(ctx, maxDur+settleHardGrace, AbortRunHardTimeout)
		defer runCancel()
	}
	_, reason, err := captureRunSession(runCtx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}

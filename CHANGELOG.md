# Changelog

Notable changes to this project are recorded in this file. The format is based on [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model configuration gains an "Output limit", along with a choice of which request field name carries that limit. The limit caps how many tokens a single reply may generate and is sent with every request; 0 (the default) means the field is not sent and the server-side default applies. It is not the same as the "Context window": the latter is the model's total capacity, used only locally to compute the compression threshold, and never appears in requests. The field-name toggle is meaningful only for the `openai` (Chat Completions) format: empty (the default) sends `max_tokens`, which the vast majority of compatible gateways accept exclusively; OpenAI's official reasoning models (o-series / GPT-5), conversely, accept only `max_completion_tokens` and reject `max_tokens` outright with `unsupported_parameter`, so such endpoints require manually switching to the new field. The other two formats fix their own field names (`max_tokens` for Anthropic, `max_output_tokens` for the Responses API), so for non-openai formats the toggle is greyed out and cleared on save. This also wires up a path that was never connected before: planner / worker / chat / main Agent / goal decomposition never set an output limit in any of the five places — the openai family simply did not send one, and Anthropic fell back to the SDK's 8192 default — so adding the configuration option without connecting this path would have had no effect even with a number filled in. The value is now resolved on every turn from the configuration, just like "streaming output", so it takes effect on the next turn after a failover switch. Existing databases behave exactly the same after upgrade (the new columns default to 0 and an empty field name).
- When clicking "Test" in the admin UI, the status code and raw gateway response body of every HTTP attempt are now written to the server log (response body truncated to 4K), making it easier to diagnose 401s, quota messages, empty frames, and HTML pages being returned, instead of only seeing the ok / err collapsed in the UI. This log does not depend on the "LLM recording" switch.

#### Changed

- The task LLM configuration chain can now be modified in any state, no longer restricted to "running / paused / configuration chain exhausted": after a task ends (done / failed / timeout) the main Agent conversation still uses this chain, and when a model on the chain failed there was previously no way to change it and therefore no way to continue interacting. The terminal-state interception in both the backend HTTP layer and the DB transaction has been removed, and the frontend dialog now allows editing and saving for terminal-state tasks as well. Saving a terminal-state task no longer reopens quota-blocked intents (those intents would be moved to open, with no worker to execute them and no longer satisfying the conditions for "rerun intent"); to continue running, use rerun intent / add goal, which pull the task back into the running state.

### Agent

#### Changed

- "Send a message to a running Worker to adjust its direction" now reuses the existing pause / resume + transcript resume mechanism, matching the behavior of main Agent conversations. Previously it used a bespoke intervention persistence protocol that touched the scheduling barrier, the resume flow, and more than ten activity-query filters. Messages are now injected via the next turn's input, and the intent runs directly in a dedicated goroutine, independent of the 3-slot worker pool and running as soon as it is sent; the frontend keeps the message box and restores the "Continue directly" button, with sending now carried over SSE. No DB schema changes. Trade-offs: messages are held in memory only with no crash recovery, and at the moment a message is sent the task may briefly exceed concurrency by one worker (a low-frequency scenario, acceptable).

### Task

#### Fixed

- Fixed an out-of-memory (OOM) crash when archiving large tasks: archiving now writes the snapshot as a stream instead of reading the entire task into memory at once. Also closed three recovery gaps on the cold-archive path — in modern installs traffic exists only in SQLite, so a crash between the PostgreSQL commit and the SQLite commit previously left an archived task's traffic stranded in hot storage with no way to recover it; a staging journal is now written whether or not a history directory exists, and a missing `journal.json` is treated as discardable.
- Fixed the system lagging when there are many tasks: loading conversations and loading the UI had noticeable delays. Queries for the task list, task context, and exploration records were optimized together, and the frontend dashboard, conversation page, and task detail page now issue fewer duplicate requests.

### Assets

#### Changed

- Removed the "maximum 256 rules" limit on enterprise asset scope: enterprises that enter scope IP by IP / domain by domain easily hit this cap, after which they could only split into multiple enterprises, while the scope itself is not slower just because it has more entries. The limit has been removed in the frontend, the backend, and the demo mock; a single rule is still limited to 1024 characters, and the 2 MiB request body limit is kept as a safety net (roughly forty to fifty thousand rules).

### Skills

#### Fixed

- Fixed the "Upload failed: zip: unsupported compression" error when uploading a skill archive: the Go standard library only ships Store / Deflate decompressors, so bzip2 and Zstandard archives written by compression software at non-default levels could not be extracted. Both decompressors have now been added (pure Go implementation, no new external dependencies); for methods that truly cannot be extracted — Deflate64 / LZMA / XZ / PPMd — as well as encrypted archives, a Chinese-language hint is now reported before extraction, naming exactly which file uses which compression method and how to repackage it, instead of throwing the underlying English error at the user.
- Fixed filename validation inside skills rejecting Chinese filenames as invalid: path validation was previously an ASCII allowlist of `[A-Za-z0-9-_./]`, so a single Chinese-named file in the archive (such as `参考/说明.md`) would make the whole upload fail with "archive contains illegal path". It is now a Unicode denylist: filenames in any language and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, as well as `..` / absolute paths / empty path segments are still rejected, and the directory-traversal protection is unchanged. Skill names are relaxed likewise — ASCII is still limited to lowercase letters, digits, and hyphens (agentskills.io specification), non-ASCII letters such as Chinese can be used directly as a skill name, but spaces, dots, and path separators are not accepted.
- Fixed filenames being garbled or the whole archive being rejected after extracting Chinese-named zips created by Windows compression software: such zips do not set the UTF-8 flag and write filenames in GBK; they are now decoded with a GBK fallback before path validation. Also, quoted frontmatter such as `name: "中文技能"` now correctly yields the skill name.

### UI

#### Added

- The Findings page gains an "All findings" flat view, set as the default and switchable with the original "Group by task" view via tabs in the page header: in the grouped view you had to expand tasks one by one to see vulnerabilities, which was a detour when you just wanted to scan a list across tasks. The flat view is one large cross-task table (10/20/50/100 rows per page) that behaves exactly like the grouped view — select for export, expand rows inline to see evidence and the detailed report, edit name/category/severity inline, change disposition status, dig deeper, delete. Both views share the stat cards and the filter bar, switching does not lose filters, and the current view and filter selections are remembered locally; polling only hits the current view, and inline edits are written to both caches so you never see stale data after switching.
- The Findings page gains a "By asset" view: on the left is an asset tree (enterprise → root domain / IP → subdomain → service → endpoint, with the enterprise level appearing only when assets actually have an owner), and on the right are the findings under the entire subtree of the selected node, sharing the same table and the same set of filters with the other two views. The tree includes only assets that have findings, and the ancestor chain is filled in on demand (when a finding is attached only to the deepest endpoint, the layers above can still be assembled); counts on nodes are subtree aggregates de-duplicated by finding; findings whose asset has been deleted or that never had an associated asset go into the "Unassociated assets" bucket. Unlike the other two views, the asset view does not poll — it queries only when entering the view, changing filters, adding/deleting/editing findings within the page, or clicking the tree's refresh button, since the left tree is a navigation structure and there is no need to recompute it every 5 seconds. Each level of the tree shows only the increment relative to the level above (subdomains with the root domain suffix stripped, services shown as `https :443`, endpoints showing only the path), with full values in the hover tooltip and the breadcrumb; when there are too many nodes, the endpoint/service levels are dropped for the whole layer with a notice, while counts are still included in upper levels.

#### Fixed

- Fixed session records being squeezed and cut off in the mobile session detail view: on narrow screens the session list is collapsed to give width to the records themselves.

### Dependencies

#### Changed

- Upgraded norma v0.3.2 → v0.3.3: added `Config.MaxTokensField`, which lets the OpenAI Chat Completions output limit be sent as `max_completion_tokens` instead (reasoning models accept only this key). The two keys are mutually exclusive and only one is sent; the default is still `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security alerts.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the "non-streaming" switch reverting to "streaming" after being saved and reopened (#69): the DTO of the configuration list endpoint omitted the `streaming` field, so the response never returned it, and the frontend read `undefined` and always fell back to the default of streaming; the actual write and DB storage were fine, it just could not be read back. The DTO now includes the field again (without `omitempty`, since `false` must also appear in the response).
- Connection testing now verifies that the model actually replies and tests using the configuration's real send/receive mode (#65): previously it only checked whether the HTTP request succeeded, so a request that went through but got zero model reply (thinking burned the whole budget / body swallowed by a safety policy / compatibility layer dropped `content`) was still reported as "connection successful", at odds with the "no reply" behavior seen in sessions; and the test always used streaming, so for non-streaming configurations it was testing a different channel. Now an empty reply is judged a failure, the model's reply is shown in the message on success, and the current streaming switch is carried into the test, so that "test passed" and "session works" stay consistent.

### Agent

#### Changed

- `list_facts` is now paginated + keyword-filtered, to avoid a single call blowing up the context when there are many facts (#74): it returns the latest 20 by default and supports `limit` (max 100) / a `before` cursor / `q` summary keyword, returning `{facts, total, has_more, next_before}`; an overlong single summary is truncated by character count (the full text is still available via `node_detail`). The worker / planner prompts are updated to the paginated semantics. For existing databases, a one-time migration refreshes the new parameter schema into the tool catalog table (`SeedTool` inserts only on first insert, otherwise the tool management page would show "no parameters").

### Tools

#### Added

- The "Tool Execution" page gains tool call count statistics (#72): the "Statistics" button in the toolbar opens a dialog showing, per tool, the call count, share, and failure count (sorted by count descending); it reuses the list's task / keyword filters and aggregates over the whole result set rather than the current page, and is fetched only when the dialog is opened.

### Dependencies

#### Changed

- Upgraded norma v0.3.1 → v0.3.2: fixed reasoning/refusal being silently dropped in OpenAI-family responses (Responses gains `reasoning_text`), de-duplicated the three names of the reasoning field, added bounded retry for empty responses (zero content blocks), and fixed gateway 400s caused by compression boundaries cutting tool pairings (also fixing stock orphans already baked into transcripts).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM configuration supports a streaming / non-streaming switch (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, returning the complete JSON at once), which sidesteps the poor SSE implementations of some gateways (empty frames, thinking-field frame loss), at the cost of losing live progress and live Token counts while running. worker / planner / mainagent / chat / goals all resolve the value dynamically from the currently active configuration; all three Provider wrapper layers — `llmpool` / `llmrec` / task runtime — are compatible with non-streaming; `llm_profiles` gains a `streaming` column and old databases are patched with `ALTER` (default `true`, old configurations see no change).
- Support for LLM configurations in the OpenAI Responses API format: each configuration gains a third format `openai-responses` (hitting `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` normalization, default model `gpt-5`; the `format` constraint on `llm_profiles` adds `openai-responses` and an idempotent migration patches old databases; the frontend format dropdown gains "OpenAI (Responses API)". Dependency upgraded to norma v0.3.1 (including the `reasoning_content` passback fix).
- Recording of raw HTTP request/response text for LLM calls: the real wire body is captured at the HTTP transport layer, preserving tool schemas, `tool_use` blocks, and raw SSE frames that the normalized view cannot show; multiple attempts from norma's internal retries are each preserved; `llm_records` gains `raw_request` / `raw_response` columns and old databases are patched with `ALTER`; the recording page's detail panel adds a "Raw" view toggle and request/response copy buttons (with an `execCommand` fallback for non-secure contexts).

### Conversation

#### Changed

- Multi-select in the session list is now a "Multi-select" mode toggle: by default the list no longer permanently shows a checkbox on every row (a cleaner look), the header becomes a "N total + Multi-select" button; clicking "Multi-select" enters selection mode (checkboxes, select all, batch delete), "Done" exits and clears the selection, and after a batch delete fully succeeds it exits automatically (if any failed, it stays in selection mode so you can retry); single-item rename / pin / delete still go through each row's ⋯ menu.

### UI

#### Changed

- Improved task management, session actions, and traffic viewing experience (#57): task list column sort preferences are now persisted, inline rename is triggered directly by an icon (no longer via a menu), and drawer (sheet) interaction and traffic viewing details are refined.

#### Fixed

- Worker asset labels now show only domains and IPs, and the empty-label case is handled correctly.

### Agent

#### Added

- The planner gains a "quantified acceptance check" when judging goals: when a goal contains quantifiable acceptance criteria (asset testing coverage reaching X%, capturing N flags, obtaining some privilege), it must verify the measured values from `graph_overview` (`coverage.pct`, findings counts, etc.) before `prove_goal`; if the measured values fall short, `prove_goal` is forbidden and intents are dispatched to close the gap instead, and it must not mark the goal met early on the grounds that "the main part is done / roughly achieved". This fixes the problem where a goal requiring 100% coverage was judged complete with only 40% measured.

#### Changed

- Rewrote the basis for the "0 intents this round" decision in the planner prompt: the original text described 0 intents as "the most common and most important principle", which could make the planner stop too early when goals were unmet and untested surface remained in scope. It now says 0 intents should be produced only in two specific situations — (1) all directions it can think of are already covered by open/running/recent_done intents; (2) the next step depends on the output of work that is currently running and that output has not yet appeared (it should wait for it to finish and plan at the next wake-up after the graph updates). A reverse constraint is added: when there is genuinely an uncovered new direction that does not depend on running work, or the goal is unmet and untested surface remains, do not stop on the grounds that "0 intents is common".
- Strengthened the "evidence bar for negative conclusions" in the worker prompt: for negative conclusions such as "not injectable / port closed / no login entry" that could make the planner abandon an entire direction, it requires exhausting reasonable means within the intent (changing encodings/parameters/paths/methods) before concluding; if the means are not exhausted or the evidence is weak, always mark `confidence=inferred`, to avoid welding shut an entire route with a hasty `observed` negative (an erroneous negative early in a task in particular skews the direction and is hard to self-correct).
- The planner / worker default prompt changes above are appended as new versions and switched to be the current version through one-time migrations (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag); old versions are kept in the version history, and users who customized prompts can restore them from the version records.

### Operations

#### Added

- Added `reset-password.sh` to reset the administrator password (username fixed as `ARTEX`): supports both local / docker deployments, with connection info either specified explicitly or read automatically from `--dsn` / `$ARTEX_PG_DSN` / `config.json`; generates a bcrypt hash compatible with backend login in the database using `pgcrypto` and writes it back to `settings.auth.password_hash`, with no service restart needed after the reset. The password is passed through an environment variable and does not enter process argv, and is escaped to prevent injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After selecting multiple tasks in the task list, the category can be modified in batch, and the target category supports "Uncategorized" to move tasks out of the current category; the whole batch is written within one transaction, and the only failing items would be tasks that were deleted after being selected.

#### Changed

- The "Task category" of a new task changes from a dropdown to a searchable input: typing filters existing categories, and a name not in the database can be created and selected immediately by pressing Enter (or clicking "Create" in the dropdown), with the selected item shown as a removable tag; still limited to a single category.

### Conversation

#### Fixed

- Fixed sending a message being blocked by "LLM not configured, unable to converse" when the session has a selected LLM configuration but there is no globally active configuration. The pre-send check only looked at the globally active configuration while actual execution prefers the session-selected configuration, so the two places resolved differently; they now use the same logic (session/Agent binding first, global as fallback).
- The message shown when conversation is impossible is now broken down by state: no configuration at all prompts "Add a configuration", a configuration that exists but is not active prompts "Activate one or specify a configuration for this session", instead of always showing "Not configured", making it easier to locate the problem.

### Agent

#### Added

- For tasks whose goals are all met, intents dispatched by the main Agent via `add_intent` can be picked up and executed directly by a Worker, running to completion and stopping: the Planner no longer runs at that point (a task with no open goals does not enter planning), avoiding it re-judging the goals as met and cancelling the just-dispatched intent; once the frontier drains, the task returns to completed. Before dispatching an intent, the main Agent asks the user, based on the intent's content, whether to register it as a formal goal; if registered, the task resumes regular autonomous planning.

#### Changed

- When `step_ids` of `get_worker_trace` / `get_task_worker_trace` exceeds the per-call limit (5), it no longer errors out directly; instead it returns the full content of the first 5 steps and reports via `returned_step_ids` / `omitted_step_ids` / `notice` which were fetched this time and which remain unfetched, with a hint "if this is already enough, there is no need to fetch more". Duplicate and invalid ids passed in are de-duplicated and removed before counting.

#### Fixed

- The task list is now ordered by task id descending (most recently created first), replacing the previous ordering by creation time. Multiple tasks created at the same instant share the same timestamp and sorted unstably, which combined with the 10-second polling and the random iteration order of the in-memory map caused them to frequently swap places in the list.

### Assets

#### Fixed

- Fixed enterprise ownership recomputation being broken by a single row of dirty data: when `assets.ip` held a hostname (written by the Agent or asset API), `a.ip::inet` would raise `22P02`, causing adding/modifying enterprise asset scope and deleting an enterprise to all fail and roll back. It now uses the safe conversion `try_inet()`, skipping invalid values instead of aborting the whole statement.
- An unparseable `ip` is no longer skipped silently: after an enterprise scope is saved, it now clearly reports how many assets have an `ip` that is not a valid IP (including specific ids and values), reminding that these assets will not be attributed by IP/CIDR rules; the same information is also written to the server log, covering paths with no frontend response (enterprise deletion, scopesentry sync, Agent writes).
- IP/CIDR matching for task test scope now also uses `try_inet()`, replacing the previous regex guard that only checked the character set (hostnames made entirely of hex letters such as `abc.def` previously still slipped through and triggered the same error).

#### Changed

- The `ip` field of `ip`, `service`, and `endpoint` assets no longer accepts hostnames: `insert_assets` and the asset API return a per-item error with an `index` and give the fix directly (use `type=subdomain` with `domain`, or resolve the A/AAAA records first), so the Agent can correct itself and reinsert. Other assets in the same batch are unaffected and are stored as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- A running Worker supports individual pause, resume, and cancel; pausing preserves intents, facts, and vulnerabilities, while cancelling transactionally cleans up the current intent and its direct products after execution exits.
- Added the `paused` intent state, an execution fence, and named termination reasons, preventing late blackboard writes after pause, cancel, or task deletion.
- Added an optional task concurrency limit; creation, resume, vulnerability deep-dive, and queue backfill all go through a unified persistent FIFO admission path.

#### Changed

- The default wall-clock duration of a single Worker run was changed from 600 seconds to 1200 seconds; after a timed-out task is revived, the timer restarts on the next real execution.
- The real execution states of Worker, Planner, and main Agent now uniformly drive the task status badge, fixing the issue where "Idle" was still shown while a Worker was running.
- Worker sessions retain individual controls and the current-session Token summary, and the model name moves to the right of the current session title.

#### Removed

- Removed the Worker multi-select, select-all, and batch pause/continue UI, and also removed the Worker batch control API and the Mock contract.
- Removed the Token badge and Tooltip in Worker list rows; the underlying full Token ledger and the task aggregation endpoint remain.

### LLM

#### Added

- Tasks support an ordered LLM configuration chain; when provider quota exhaustion is clearly identified, it automatically switches to the next configuration and persists the current configuration, exhaustion state, and error summary.
- Running and paused tasks support editing the full configuration chain, reordering, and manually switching the current configuration, taking effect from the next LLM call.
- Automatic switches, manual switches, and full-chain exhaustion are written to structured system activity, and de-duplicable notifications pop up through the task activity stream.
- Added a task role model resolution endpoint, uniformly resolving the configuration and model used by the Main Agent, Planner, and Worker on their next call.
- Added an optional global LLM Pool, supporting configuration of call order, fallback on specified-model failure, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger, aggregating input, output, and cache Tokens by task, session, model, and configuration.

#### Changed

- Goal decomposition, Planner, Worker, and main Agent share the task-level LLM runtime (resolution priority below).
- Failover responds only to explicit quota, balance, or billing errors; ordinary rate limiting, authentication, network, server, and context errors do not trigger a wrong switch.
- The LLM configuration page changes to a configuration card list with a right-side drawer for editing, and provides Pool round-robin order, priority, exclusions, health status, and recovery actions.
- Configuration chain exhaustion messages now wrap on narrow screens; the current model changes from a session-list icon to a text label next to the current session title with a full Tooltip.
- The order in which each role within a task resolves its LLM is adjusted to "Agent binding → task configuration chain → global": a role with an explicitly bound model always runs on that model, unbound roles fall to the task chain, and when the task chain is empty they fall to global.
- Leaving the outbound proxy empty means a direct connection, no longer falling back to the `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected); the proxy input supports `socks5://user:pass@host:port` with credentials.

#### Fixed

- Transient streaming failures before commit (before any output has been produced) are now safely retried with backoff on the same provider, significantly reducing cases of "ran only a tool or two then broke off, no summary, terminal state `model_error`"; quota exhaustion, context too long, and deterministic 4xx errors are not retried and are still handled by failover and compression recovery respectively.

#### Removed

- Removed the icon-style LLM indicator in the session list, to avoid confusion with the Worker status icon.

### UI

#### Added

- Tasks support creating, renaming, deleting, and filtering by global categories; a new task can pick a category directly.
- Added global task template CRUD and a right-side management drawer; a new task can load a preset description and goals, and the current content can also be saved as a template.
- A new task supports associating multiple source tasks and multiple enterprise asset scopes; enterprise scope uses a single multi-line text box to auto-recognize domains, URLs, IPs, CIDRs, ICP numbers, and enterprise keywords.
- Task test assets support live add and delete, recording sources such as manual, enterprise, inherited, or Agent-discovered; the Worker session title shows the current test assets and a source summary.
- The Findings page is grouped by task with independent pagination within each group, and supports filling in a description for a vulnerability and creating a high-priority deep-dive intent.
- Conversations support rename, pin, unpin, and delete; the task list supports batch pause and continue for the current page.
- Traffic details and tool execution details change to a right-side drawer; HTTP messages gain the `Host` header and highlight the request line, status code, headers, JSON, and markup-language bodies.
- The task list action column gains "Details" and "Pause/Continue" buttons; the session send key binding can be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and the Web search area gains a proxy input box.
- The badge to the right of the session title now shows the LLM configuration name, with the model ID moved to a hover Tooltip; tasks can have an optional name, and the task list gains a name column and search (falling back to the description when empty).
- The Skills page adds usage count, last-used time, and missing-dependency statistics, making it easier to locate skills that are not taking effect or not installed.

#### Changed

- The task title becomes a focusable details link; task assets, enterprise assets, findings task groups, and the vulnerabilities within groups use real server-side pagination, stable sorting, and exact totals.
- Adding an enterprise now uses a right-side drawer, and scope entry is unified as multi-line text with live recognition, validation, and type preview.
- The main Agent input box supports auto-grow, `Enter` to send, `Shift+Enter` for a newline, and avoids accidental sending during Chinese IME composition.
- Agent preview, task reports, and related details uniformly use the shared Markdown rendering; delete confirmation, long errors, and mobile drawer width are fixed uniformly.
- The task template selector displays and searches by template name and submits by template ID; the original system font size is restored, and the application version is uniformly displayed as `0.3.3`.
- Task total and current-session Tokens highlight only the input, cache read, and output values; the send button uses a simpler up-arrow icon.
- The test scope in the task overview shows the enterprise name instead of the ID.

#### Fixed

- Ordinary text containing a dot is no longer misjudged as an ICP filing number.
- Fixed a name collision in the variable catalog with global runtime variables (such as `{{.Now}}`) that caused the Agent editor variable list to render duplicate keys.

#### Removed

- Removed the standalone "Enter" button on task cards; clicking the task title uniformly enters the details.
- Removed the dashboard "New task" button, the Logo URL input for adding an enterprise, and the vulnerability count Badges at the top of the Findings page and in task groups.
- Reverted the global 10% font size increase, and removed the Worker selection box, model icon, and per-row Token statistics from the session list.

### Agent

#### Added

- A new task can directly associate multiple existing tasks, live and read-only inheriting the direct source tasks' goals, facts, vulnerabilities, completed intents, asset scope, and blackboard context.
- Blackboard read tools support on-demand queries of source task nodes, facts, vulnerabilities, and execution traces; inherited nodes carry a source marker and all write tools refuse to modify them.
- Enterprise scope tools support domains, URLs, IPs, CIDRs, ICP numbers, and enterprise keywords; keywords serve only as scope hints for the Agent and do not participate in automatic asset ownership.
- The vulnerability deep-dive action creates, in the original task, a high-priority manual Worker intent carrying an asset anchor and a `derived_from` edge.
- The overview gains a "Goal management" card, supporting manual viewing, adding, modifying, and deleting of goals; adding or modifying notifies the Planner and revives the task, while deleting hard-deletes the goal node (cascading to edges and anchors) but does not revive.
- The main Agent gets the `steer_work` tool by default, which can inject corrective instructions into a running Worker in real time without interrupting it or losing progress (still verifying that the intent belongs to this task).

#### Changed

- Source task intents do not enter the new task's frontier; the new task continues to use its own independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys data: it now stops to `stopped`, requires a deletion reason, attaches the reason to the intent as a fact and writes it into the payload, and the planner perceives it as `cancelled` (preserving the intent content and reason).
- Main Agent conversation is now driven by the server-side activity stream, and input can be recovered after a send failure; opening a session automatically sticks to the bottom and keeps the last reply visible after details lazy-load.
- Task pause terminates the current Main Agent, Planner, and Worker calls, but does not block the user from actively starting a new main Agent orchestration session during the pause.
- Cancellation, closure, and streaming interruption uniformly retain the real termination reason, generated content, number of rounds run, elapsed time, Tokens, and unreturned tool calls.

#### Removed

- Removed the main Agent client-side optimistic message echo, avoiding duplicate messages and cross-session stream bleed under pause, failure, or concurrent activity.

### Constraints

#### Added

- The goal decomposition stage first extracts operational constraints (allow/deny) before decomposing goals, and they can be supplemented while the main Agent is running; constraints are injected into the Planner and Worker system prompts at the highest priority, and after de-biasing, diversity and surface-widening exploration explicitly obey the constraints.
- The overview gains a "Constraint management" card and add/delete/modify endpoints; constraint injection scope can be toggled separately for Planner / Worker (both on by default, read every round, effective immediately). The new table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, and old databases are created automatically.

### Interception

#### Added

- Command interception adds a model-based fallback approval on top of regex/string rules: when no rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic judgment, with the failure fallback action and the approval timeout action configurable; the interception page is split into two tabs, "Interception rules / Model configuration", and model judgment results are labeled with a `[Model]` prefix and a rationale.

#### Fixed

- Hardened the judge output parsing, to prevent a correct `DENY` verdict from being mistaken for an allow.

### Traffic

#### Changed

- Traffic recording now stores the whole exchange in SQLite (large bodies overflow into a hash-deduplicated blob bucket), and text bodies get a trigram full-text index, supporting arbitrary substring and Chinese search; deletion degrades to a single SQL transaction, dropping from hours to milliseconds and no longer stalling recording meanwhile. Added streaming download for large bodies and the `traffic_search` full-text parameter; old file-tree data needs no migration and remains readable, searchable, and deletable.

### Tasks and Assets

#### Added

- Task creation gains an "Asset coverage" switch (on by default): when off, coverage is not computed or displayed, the situation graph shows only assets, `task_scope` is not accumulated automatically, and the related tools are removed from the Planner / main Agent; enterprise association is unaffected by this switch.
- Each asset in `insert_assets` gains a `related` flag (default `true`): effective only when coverage is on; `false` puts the asset only into the shared asset library and does not count it toward this task's coverage (such as incidentally discovered neighboring sites or unrelated assets).
- Enterprise scope and task test assets uniformly support text recognition of domains, URLs, IPs, CIDRs, ICP numbers, and keywords; domains/IPs can create or reuse global assets, while CIDR/ICP/keywords are retained as task scope context.

### Build

- `build.sh --release` supports building Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one go, and generates zip release packages with `skills/`, example configuration, and README.
- Uses the Go linker to strip debug information and generate zip release packages; UPX becomes explicitly optional, to avoid its self-extracting ELF segfaulting at startup in some Linux environments.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and ships `SHA256SUMS` with the release packages.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- A new task can associate multiple existing tasks, live and read-only inheriting the direct source tasks' facts, vulnerabilities, completed intents, asset scope, and blackboard context; the new task still uses its own independent execution queue, working directory, and session history.
- Tasks support an ordered LLM configuration chain. When provider quota exhaustion is clearly identified, it automatically switches to the next configuration and persists the current configuration, exhaustion state, and structured audit activity.
- Running and paused tasks support editing the LLM configuration chain, reordering, and manually switching the current configuration; automatic switches, manual switches, and full-chain exhaustion are surfaced on the task details page.
- A running Worker supports pause, resume, and cancel. Pausing preserves blackboard data, while cancelling transactionally cleans up the intent and the facts, vulnerabilities, and execution records it directly produced after the Worker stops writing.
- Deleting a task can optionally also clean up associated assets, traffic, vulnerabilities, and task landing files, with a deletion barrier, concurrency protection, and auditable deletion statistics added.
- Added an optional task concurrency limit; new tasks beyond the limit queue in FIFO order and start automatically once a running slot is released.
- Added task asset pagination, company asset pagination, task category statistics, and a paginated intents Mock contract.
- Added the `build.sh` single-binary build script, supporting static frontend export, resource embedding, cross-platform targets, and build version injection.

### Changed

- The task-level explicit LLM configuration chain coexists with the existing global LLM Pool; the explicit chain continues to use strict quota-failover semantics, and when there is no explicit chain the Agent binding and global configuration rules apply as before.
- The main Agent input box supports auto-growing multi-line input, `Enter` to send, `Shift+Enter` for a newline, and avoids accidental sending during Chinese IME composition.
- Planner, Worker, and main Agent share the task-level LLM runtime; the task chain uses the smallest context window among candidate models as the safe compression threshold.
- The task details page shows running, idle, and paused states according to the real LLM call state; source tasks' facts, vulnerabilities, intents, asset references, and graph nodes are uniformly marked with their source and kept read-only.
- Agent preview, task reports, and related details views uniformly use the shared Markdown rendering component.
- LLM model configuration adopts card and drawer interaction, and the model list scrolls independently within the drawer.
- Task pause no longer blocks main Agent conversation: the main Agent orchestration session is independent of task pause, and new messages can still be sent while paused (pausing only terminates the round currently in progress).
- The default wall-clock duration of a single Worker run was changed from 600 seconds to 1200 seconds.

### Fixed

- Removed the optimistic echo from the main Agent console, switching to pure server-data rendering, fixing messages being garbled or content from other sessions bleeding in under scenarios such as pause or send failure.
- Opening a main Agent session now scrolls to the bottom by default and fully displays the last reply: after the full content of the last reply lazy-loads and expands it sticks to the bottom automatically, no longer being pushed off screen.
- Fixed the main Agent session continuing to run after a task was paused, and the main Agent not being stopped in sync when the orchestration Agent paused a task.
- Fixed the task status badge and action button states going out of sync when a task completes or when the Planner, Worker, or main Agent is actually running.
- Fixed possible late blackboard writes or leftover files between Worker cancellation, task deletion, and concurrent writes.
- Fixed Markdown preview not working, long text overflowing in delete confirmation, mobile width, and some task details buttons being misaligned.
- Added named termination reasons to all Agent cancellation paths, so activity details can show the canceller, terminal state, number of rounds run, elapsed time, Token usage, and unreturned tool calls.
- Fixed a race where the parent context could pre-empt the named `shutdown` reason on backend shutdown, and Chinese activity summaries being truncated by bytes and garbled.
- Fixed cancellation events with partial streamed output losing the real termination reason, while preserving the content generated before the cancellation.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...HEAD
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2

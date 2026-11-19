---
name: api-recon
description: Call this skill when collecting website API endpoints.
---

# API Recon (Frontend API Reconnaissance)

Under **authorized** conditions, discover as completely as possible: **backend APIs** (path, method, parameters, response body), **frontend routes**, and **UI feature trigger points** (tabs, dialogs, table actions, etc.).

---

## Boundaries and Prohibitions (Agent must read · violations are out of scope)

This skill **only performs API / parameter surface reconnaissance**. It is not a vulnerability hunting or exploitation phase.

### Task boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate path, method, parameters, routes, UI trigger points | SQLi/XSS/privilege escalation/brute force/fuzzing for vulnerabilities, tampering attacks, destructive operations |
| **Authentication** | Hook + stub/mock to bypass the **client-side** login gate | Asking the user for or guessing account credentials; submitting a real login form |
| **Runtime** | Hook endpoints without credentials, use mock responses to let the SPA enter the post-login shell | Flows that depend on a real backend session to continue |

### Credential-free dynamic analysis (Phase 3 default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** bootstrap endpoints such as login, permissions, and menus;
2. For business query endpoints, return mock bodies with **correct structure, a success business code, and possibly empty data**;
3. Let the frontend render post-login pages even with no backend or in a 401 environment, thereby triggering more XHR/fetch/WebSocket requests;
4. **Empty data, blank tables, and placeholder UI are all expected** — do not switch to real login or vulnerability testing because of them.

**In one sentence**: use mocks to prop up frontend routes and component mounting, **record only outbound requests**; what the backend returns does not matter, what matters is **which further endpoints** the frontend will call.

### Hard process prohibitions

| Prohibited | Alternative |
|---|---|
| Before Phase 1 completes, grep/curl/Read the main entry `index-*.js` to extract API paths | Run `OUTDIR/harvest_static.py` |
| Hand-writing scripts such as `extract_apis.py` to replace harvest | Modify `OUTDIR/harvest_static.py` and rerun |
| Repeating the same grep/command after it fails 2 or more times | Change strategy: read tool_logs, modify harvest, consult reference |
| Skipping gates A/B and running the original `scripts/` directly | Copy to OUTDIR and adjust for the target |
| Real usernames/passwords, OTP, OAuth, or other authentication | stub/mock (see above) |
| Skipping stubs in order to "get real data" and doing privilege escalation/injection testing | Record only outbound requests; that is the recon boundary |
| Irreversible operations such as deletion, exporting sensitive data, or bulk writes | Applies to coverage clicking as well |
| Claiming all pages and endpoints were obtained without completing runtime + dynamic enumeration | See "Definition of Done" or note the limitations |
| Claiming all parameters are known without completing the parameter trigger matrix + diff | Phase 3b matrix + Phase 5 diff |
| Inferring required/optional from a single runtime sample | Multi-sample diff or inference from validation rules/errors |

---

## Two-layer model + run modes

| Layer | Output | Limit |
|---|---|---|
| **Static** (JS bundle) | Full endpoint paths, route drafts, candidate fields at payload-assembly points | No HTTP method; parameters require Phase 1b; misses URLs assembled at runtime |
| **Runtime** (live session) | Method + body + response + dynamic URLs + WS/SSE; multi-sample diff completes parameters | Requests are only sent if the page actually renders; a single sample is not enough to determine required/optional |

| Run mode | Engine | Suited for |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, METHOD/params/response body, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Click tabs/dialogs/tables for deeper feature-point coverage |
| **both** | depth first, then coverage | Most complete, longest runtime |

**Parameter methodology** (no generic script): use harvest/regex for paths; for parameters use **anchor window expansion + UI binding chain + multi-sample diff + error-based inference** (grep recipes in section J of [reference.md](reference.md)).

---

## Definition of Done

Recon may only be declared complete when all of the following are met:

- [ ] **Static**: Phase 1 harvest produced `api_static.txt`, `routes.txt`, `js/`
- [ ] **Runtime**: at least one of depth or coverage; coverage/both require **Hook effective + dynamic enumeration loop**
- [ ] **Shell entry**: visiting business paths does not redirect to `/login` (watch for hash routing)
- [ ] **Parameters**: coverage/both completed the parameter trigger matrix + `param_samples.json`; Phase 5 merged into `params_merged.json`
- [ ] **Depth** (if module pages are blank): Phase 4 permission tree reconstruction and rerun, until **module-level APIs** appear (not just locale/bootstrap)
- [ ] **Delivery**: Phase 5 outputs complete (see the Phase 5 output table); `insert_assets` wrote the service and endpoint assets

---

## Scripts and Gates

`scripts/` contains only reference templates; **do not** run the originals directly and treat the result as final.

**Rule**: read first → adjust for the target → write to `OUTDIR` (e.g. `recon/`) → log in `CHANGES.md`; if it does not match, rewrite following the methodology and borrow only the structure.

| Gate | When | Reference script → OUTDIR copy | Common required changes |
|---|---|---|---|
| **A (static)** | After Phase 0, before the **first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **For most sites the default regex runs as-is**; only change the endpoint regex, webpack/Vite `publicPath`, MPA exclude/cookie when the manifest/dialect does not match |
| **B (runtime)** | After Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralize success values, stubs, login regex, api prefix, hash/history |

**Mandatory SPA order** (not interchangeable; Phase numbering takes precedence over "explore first, script later"):

| Step | Required | Prohibited |
|---|---|---|
| After Phase 0 completes | The next Bash = `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main entry `index-*.js` (usually >500KB) |
| Gate A | Copy script → small tweaks if needed → **run immediately** | Manually extracting APIs first and then deciding whether to harvest |
| Before Phase 1 completes | Verify output with `wc -l`; on 404, modify harvest and retry | Hand-writing an extract script; repeatedly grepping URLs that were not downloaded |
| From Phase 1b | grep only `OUTDIR/js/*.js` | Using the main bundle in place of harvest |

- ✅ Copy `harvest_static.py` → (optional) tweak regex → **run immediately**
- ❌ curl the main bundle → grep several times → write a temporary extract script → harvest only at the end
- **MPA**: after Phase 0, the next Bash = `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and Output Constraints

| Constraint | Description |
|---|---|
| Large files | `index-*.js` over 100KB **must not** be Read/grepped into context; batch process with OUTDIR scripts |
| grep output | Must use `\| head -20` or `-m 5`; keep only path summaries in the conversation, do not paste bundle fragments |
| Verification | Use `wc -l`, `ls \| wc -l`; do not Read an entire directory |
| Regex probing | Optional, at most once, only on small chunks ≤50KB or HTML; formal static results come from harvest |
| reference | For recipes/templates/troubleshooting see [reference.md](reference.md); do not inline the full text again |

---

## Execution Roadmap

```
Phase 0 Classification + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b Parameter reverse engineering
  → Phase 2 Three authentication gates → config.json
  → Gate B → Phase 3 Runtime + parameter matrix
  → Phase 4 Permission tree (if needed) → rerun Phase 3
  → Phase 5 Merge report + insert_assets to bulk insert all discovered service and endpoint API assets; under no circumstances may any discovered asset be left out of the insert
```

Check off in order; **do not enter the next Phase until the previous item is complete**.

1. [ ] **Phase 0**: probe SPA/MPA; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: copy script → **run immediately** harvest → verify with `wc -l` → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: anchor window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-reverse-engineering)
4. [ ] **Phase 2**: three authentication gates → `config.json` → [Phase 2](#phase-2--three-authentication-gates)
5. [ ] **Gate B**: adjust runtime scripts → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; confirm shell entry; parameter trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-reconstruction)
8. [ ] **Phase 5**: merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch the entry HTML and **create `OUTDIR`** (do not modify `scripts/` inside the skill):

- **SPA**: empty shell + `<div id=app>` + chunks → Phase 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Outputs `forms.txt`, `links.txt`, `api_inline.txt`. If an SPA has forms ≈ 0 → switch to Phase 1.

---

## Phase 1 — Static

Follow [Scripts and Gates](#scripts-and-gates) · [Tool and Output Constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest: parse HTML scripts → webpack/Vite manifest → download all lazy chunks → produce `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk count vs manifest: on 404, modify harvest and retry; do not manually curl chunks one by one
- If `api_static.txt` is too small → relax the endpoint regex inside OUTDIR and rerun (see reference)

### Phase 1b — Parameter Reverse Engineering

Paths come from Phase 1; parameter fields need separate recon. For grep rules see [Tool and Output Constraints](#tool-and-output-constraints).

**Completion criterion**: for important endpoints you can answer — field name, transport location, inferred type, required or not, sample value, confidence.

#### 1b.0 — Transport shape

| Shape | Where parameters are | Static: look first at |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` next to the path anchor |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameters | `/user/:id` | route table + `useParams` / `$route.params` |
| Encrypted/signed | wrapped into `sign`/`data` | Hook the encryption function's input (reference section D) |

Output: annotate each endpoint with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor window expansion

Using a known path as the anchor, expand the window to find the payload-assembly object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clue |
|---|---|
| axios instance | `data` / `params` |
| Unified request | Interceptor-injected global fields |
| OpenAPI client | Generated method signature |
| React Query / SWR | Second argument of the hook |
| Vue composable | Composable input arguments |

Type remnants: `yup`/`zod`/rules, `Form.Item name=`, embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| Form submit | Follow submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | Global `tenantId`, pagination, sign |
| Enum select | `options` → API enum values |

Trace upward from `fetch`/`XHR.send` through the DevTools call stack to the payload-assembly function.

#### 1b.3 — Three payload questions (≠ the Phase 2 three authentication gates)

| Question | What to answer |
|---|---|
| **Assembly** | Where the payload is built, transform traces |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

The interceptor gate (Phase 2) also reads the globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Handoff to Phase 3

Candidate fields come from the static/binding layers; **required/optional/conditional dependencies** require the Phase 3 parameter matrix + diff + Phase 5 error-based inference.

---

## Phase 2 — Three Authentication Gates

grep in `OUTDIR/js/` (with `head`), and write results to `config.json` (recipes in reference):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How is logged-in state determined? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers the redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat localStorage key names as credentials — confirm them from the chunk/request chain.

**Exit = Gate B**: conclusions land in `config.json`, and modify `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API observation (optional)

Use `preload.js` inside OUTDIR to confirm session key names, Authorization, and nested API URLs:

| Config | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers observation |
| `extractUrlsFromResponse: true` | sub-APIs inside responses |
| `observe.storageReads/cookieReads: true` | backfill config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Export after each coverage round: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Gate B must already be passed; follow [Boundaries and Prohibitions](#boundaries-and-prohibitions-agent-must-read--violations-are-out-of-scope) · the credential-free mock strategy.

Set `"runtimeMode": "depth" | "coverage" | "both"` in `config.json` (template in reference).

### Hook and stub (shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 exact | auth/permission/bootstrap stubs | Pass first-screen authentication |
| L2 negative correction | all JSON responses | Not-logged-in code → success |
| L3 fallback | `/api` etc. not hit by L1 | Empty success body to prop up the UI |

- **depth**: fake auth + `forward` rewriting business codes + `stubs`; traverse `routes` (hash/history); produces `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or Userscript)

Verify: `window.__API_RECON_PRELOAD__` exists; business paths do not return to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — coverage dynamic enumeration (mandatory)

1. Main navigation/sidebar — click each item, wait 1–3s for network
2. Tabs — `role=tab`, `.ant-tabs-tab`
3. Tables — view/edit/detail on the first row
4. Toolbars — export, filter, create (**avoid irreversible deletion**)
5. On entering each module — merge APIs/routes
6. SPA — controlled `pushState` to paths not covered by `routes.txt` (prohibited for MPA)

**Parameter trigger matrix** (mandatory): for each module, record once per operation type, then **diff the multiple samples**:

| Operation | Parameters typically added |
|---|---|
| List first screen | Pagination + default filters |
| Click search | keyword, filter |
| Advanced filter | More optional fields |
| Create/edit | Full entity |
| Batch/export/sort | `ids[]`, `exportType`, `sortField` |

**Under stubs the outbound body/headers are still real** — rely on the request. Recordings → `scan_raw.json`, `param_samples.json`, `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload
- **React**: `routes.txt` + sidebar clicks + `pushState`
- **both**: 3a depth first, then 3b coverage

---

## Phase 4 — Permission Tree Reconstruction

**Trigger**: module pages are blank / each route only has bootstrap (e.g. locale) → the content gate was not passed.

| Symptom | Meaning |
|---|---|
| Shell entry succeeds | Render gate + interceptor gate passed |
| Sidebar items missing / clicks show blank | stub shape or permission codes incomplete |
| Every route has the same, very few APIs | `v-if permission` not satisfied |
| `routes.txt` far smaller than the bundle | Must be completed from the auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

stub checks: the outer `response_code` is consistent with the interceptor gate; flat codes align with the tree; `routes` covers all links in `route_map`.

After updating `config.json`, **rerun Phase 3**. For large SPAs, tune `waitUntil`, `routeTimeout`, `perRouteMs` (see reference sections A3/I).

---

## Phase 5 — Merge and Report

### Output table

| File | Stage | Content |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundles and paths |
| `param_candidates.json` | 1b | Static parameter field candidates |
| `config.json` | 2 | Three gates + runtime configuration |
| `runtime_api.json` | 3a | depth detailed recording (including WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multi-sample, click log, detail |
| `route_map.json` etc. | 4 | Permission tree intermediate files (if performed) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | Routes, APIs, params, feature points, limitations |
| **insert_assets** | 5 | Write all service and endpoint assets into the asset database |

### 5b — Parameter merging

Diff from `param_samples.json`; **there is no generic merge script**. For confidence rules see reference J7 (high/medium/low/pending trigger).

### 5c — Error-based inference

Within the authorized scope you may send incomplete requests and read the 400 (**this is parameter recon, not vulnerability testing**): `field 'x' is required`, enum errors, etc. Watch for `data` wrapping, `variables`, and the pre-encryption `bizData`.

The report must state: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, and a summary of `CHANGES.md` relative to the reference scripts.

Suggested `site_map.json` structure:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

For more fields and grep recipes see [reference.md](reference.md).

---

## General Notes

- **Framework-agnostic**: webpack/Vite/Angular lazy loading is handled the same way
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope
- **SSR**: client-side fetch can be recorded; RSC/Server Actions cannot be fully enumerated
- **Blind spots**: JSVMP, WASM, HMAC/mTLS strong validation → static + note limitations
- **Parameter blind spots**: conditional linkage, hidden params, WASM payload assembly → "pending trigger" / "unreachable"
- **Static is the safety net**: when runtime is blocked, static can still enumerate endpoints

---

## Additional Resources

- Grep recipes, `config.json` template, troubleshooting, Hook, parameter reverse engineering section J, site_map template: **[reference.md](reference.md)**
- For reference script paths see the table in [Scripts and Gates](#scripts-and-gates)

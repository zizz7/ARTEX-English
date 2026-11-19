---

## name: scopesentry-mcp
description: Manage the security scanning platform (projects, tasks, templates, assets, nodes) through ScopeSentry MCP. Use when the user mentions ScopeSentry, MCP, API Key, scan tasks, or asset queries.

# ScopeSentry MCP Usage Guide

Intended for users of an **already-deployed ScopeSentry instance**. Connect to the platform through Cursor (or another MCP client); no local source code is needed.

## 1. Prerequisites

### 1.1 Confirm the service is reachable

- Default web UI: `http://<host>`
- MCP endpoint: `http://<host>/mcp` (if there is a reverse proxy or frontend proxy in front, use the actual `/mcp` address)

### 1.2 Create an API Key

1. Log in to the ScopeSentry web UI in a browser
2. Go to the **API Key** management page and create a key (or create one through an interface provided by your administrator)
3. Save the returned `ssk_...` string (**shown only once**)

### 1.3 Configure Cursor MCP

Cursor → Settings → MCP → add a server:

```json
{
  "mcpServers": {
    "scopesentry": {
      "url": "http://<your-host>:8082/mcp",
      "headers": {
        "X-API-Key": "ssk_your_key"
      }
    }
  }
}
```

You can also use: `Authorization: Bearer ssk_your_key`

After configuring, restart MCP or reload Cursor, and confirm that tools such as `list_projects` and `list_assets` appear in the tool list.

---

## 2. Tool Overview


| Tool                   | Purpose                                              |
| ---------------------- | ---------------------------------------------------- |
| `list_projects`        | Project tree grouped by tag (includes project IDs)   |
| `list_projects_data`   | Paginated project list, searchable by name           |
| `get_project`          | Project details                                      |
| `create_project`       | Create a new project                                 |
| `list_tasks`           | Scan task list                                       |
| `get_task`             | Task details                                         |
| `list_scan_templates`  | Scan template list                                   |
| `get_scan_template`    | Template details                                     |
| `list_plugin_modules`  | Scan pipeline module names                           |
| `list_plugins`         | Available plugins (includes hash, default parameters)|
| `create_scan_template` | Create a scan template                               |
| `create_scan_task`     | Create a scan task                                   |
| `list_assets`          | Query assets of all types (paginated list)           |
| `count_assets`         | Count assets (`/api/assets/common/total`)            |
| `get_asset_detail`     | Asset or vulnerability details                       |
| `add_asset_tag`        | Add a tag to an asset                                |
| `list_nodes`           | Scan node list                                       |


Tool parameters follow the MCP tool descriptions (schema); `list_assets` / `count_assets` use the same search and filter syntax, so read the `list_assets` description before querying assets.

When you need to know "how many in total", use `count_assets` (corresponds to the web UI's pagination total endpoint); there is no need to page through `list_assets` repeatedly just to count.

---

## 3. Common Workflows

### 3.1 Query assets by project

When the user or context **already has a project condition**, prefer including `filter.project` to narrow the scope, to avoid slow responses from too much cross-project data. If there is no explicit project, project filtering need not be forced.

1. Use `list_projects` or `list_projects_data` to get the target project's **ObjectID** (`id` / `children[].value`)
2. Pass `filter.project` to `list_assets` (**it must be the ID, not the project display name**)

```json
{
  "asset_type": "asset",
  "pageIndex": 1,
  "pageSize": 20,
  "search": "domain=^example.com",
  "filter": {
    "project": ["<project ObjectID>"]
  }
}
```

### 3.2 Create a scan task

1. `list_nodes` to get online node names
2. `list_scan_templates` or `create_scan_template` to get the template **ObjectID**
3. `create_scan_task`: `name` and `node` are required; set `template` to the template ID (not the template name)

**Target source `targetSource` (consistent with the web UI):**

| targetSource | Description | Required parameters |
| --- | --- | --- |
| `general` | Enter targets directly | `target` |
| `project` | Read targets from a project | `project` (array of project ObjectIDs) |
| `asset` | Search from the web asset library | `search`; optional `project`, `filter`, `targetNumber` |
| `RootDomain` | Search from the root domain library | `search`; optional `project`, `filter`, `targetNumber` |
| `subdomain` | Search from the subdomain library | `search`; optional `project`, `filter`, `targetNumber` |
| `UrlScan` | Search from URL scan results | `search`; optional `project`, `filter`, `targetNumber` |
| `*Source` (e.g. `subdomainSource`) | Created from "select/search" on an asset page | `search` when `targetTp=search`; `targetIds` when `targetTp=select` |

**Example — scan root domains directly:**

```json
{
  "name": "example-subdomain-collection",
  "node": ["node-1"],
  "template": "<template ObjectID>",
  "targetSource": "general",
  "target": "example.com\nfoo.com",
  "project": ["<project ObjectID>"]
}
```

**Example — continue scanning from the subdomain library (filtered by the previous task name):**

```json
{
  "name": "example-ports-and-vulnerabilities",
  "node": ["node-1"],
  "template": "<follow-up module template ObjectID>",
  "targetSource": "subdomain",
  "search": "task==\"example-subdomain-collection\"",
  "project": ["<project ObjectID>"]
}
```

### 3.3 Full information gathering for root domains (two-phase recommended)

When the input is a **root domain** and you want **full information gathering**, it is recommended to run two scans rather than running the entire pipeline at once.

**Reason:** distributed tasks are dispatched per **single target**. When a root domain is the target, once a node is assigned that root domain, the subdomains discovered on that node will also continue through the subsequent modules on that same node, which easily causes uneven load, slowness, and errors.

**Best practice:**

1. **Phase 1 — subdomain collection only**
   - `targetSource`: `general`
   - `target`: all root domains (multiple lines)
   - Template: enable only `SubdomainScan` and `SubdomainSecurity` (subdomain scanning + subdomain takeover)
   - Use `get_task` to wait for the task to finish

2. **Phase 2 — subsequent modules**
   - `targetSource`: `subdomain`
   - `search`: `task=="<phase 1 task name>"` (exact match on the task name)
   - Optionally narrow with `project`
   - Template: port scanning, asset mapping, vulnerability scanning, etc. (may exclude SubdomainScan)
   - Subdomains are dispatched to the nodes as independent targets, giving higher parallel efficiency

You can also filter by task name on the "Subdomain" asset page in the web UI and use "Create task from subdomains", with the same effect.

```mermaid
flowchart LR
  A[Root domain list] --> B[Phase 1: general + SubdomainScan]
  B --> C[Subdomains stored]
  C --> D[Phase 2: subdomain + task==phase 1 task name]
  D --> E[Port/asset/vulnerability modules]
```

### 3.4 Create a scan template

1. `list_plugin_modules` → list of module names
2. `list_plugins` (can filter by `module`) → each plugin's `hash` and default `parameter`
3. `create_scan_template`: use `modules` to specify "module → array of plugin hashes"

---

## 4. Asset Queries (`list_assets` / `count_assets`)

`count_assets` uses the same `asset_type`, `search`, and `filter` as `list_assets` and returns `{ "total": N }`, corresponding to the web UI's `/api/assets/common/total`.

```json
{
  "asset_type": "subdomain",
  "search": "task==\"some task name\"",
  "filter": {"project": ["<project ObjectID>"]}
}
```

**Performance advice (applies to both `list_assets` / `count_assets`):** when there is a project condition, prefer `filter.project` to narrow the scope; in `search`, for indexed fields use `==` exact match or `^` prefix match wherever possible (see [4.3](#43-search-expressions)), and avoid broad `=` fuzzy queries that slow down responses. Without project context, project filtering need not be forced.

For the types that support `filter.project`, see the table in [4.4](#44-filter-exact-filtering).

### 4.1 Asset types `asset_type`

`asset`, `RootDomain`, `subdomain`, `app`, `mp`, `UrlScan`, `SensitiveResult`, `DirScanResult`, `crawler`, `vulnerability`, `PageMonitoring`, `IPAsset`, `SubdomainTakerResult`

Alias examples: `web`→asset, `vuln`→vulnerability, `ip`→IPAsset, `url`→UrlScan

### 4.2 Parameter reference


| Parameter                | Description                                                          |
| ------------------------ | -------------------------------------------------------------------- |
| `pageIndex` / `pageSize` | Pagination, default 1 / 20                                           |
| `search`                 | Search expression (see next section)                                 |
| `filter`                 | Exact-filter JSON (see next section)                                 |
| `sort`                   | Only UrlScan and DirScanResult support sorting by `length`           |
| `sid`                    | SensitiveResult only: sensitive rule name                            |


`search` and `filter` **can be used together**.

### 4.3 search expressions

A custom DSL (**not SQL**):


| Operator | Meaning | Index | Example                     |
| -------- | ------- | ----- | --------------------------- |
| `=`      | Fuzzy match (regex) | Not indexed | `domain=example`  |
| `==`     | Exact match (equality) | **Uses index** | `port==443`  |
| `!=`     | Exclude | — | `port!="80"`                |
| `&&`     | AND     | — | `domain==example.com && port==443` |
| `||`     | OR      | — | `title=admin || body=login` |


**Indexes and operators:** fields such as `domain`, `ip`, `port`, and `title` are indexed, but only **`==` exact match** or **prefix matching with a value starting with `^`** (e.g. `domain=^example.com`) can use the index; **`=` is converted to a regex fuzzy match and cannot use the index**, which easily becomes slow on large data sets.

**Search fields common to all types:** `tag`, `task` (task name), `rootDomain`

**project cannot be written in search** (it is ineffective, or errors when combined with `&&`). Use `filter.project` to filter by project.

**Common search fields per type:**


| asset_type           | Fields                                                                              |
| -------------------- | ----------------------------------------------------------------------------------- |
| asset                | domain, ip, port, service, app, title, statuscode, icon, banner, type, body, header |
| RootDomain           | domain, icp, company                                                                |
| subdomain            | domain, ip, type, value                                                             |
| app                  | name, icp, company, category, description, url, apk                                 |
| mp                   | name, icp, company, category, description, url                                      |
| UrlScan              | url, input, source, resultId, type                                                  |
| SensitiveResult      | url, sname, body, info, md5                                                         |
| DirScanResult        | url, statuscode, redirect, length                                                   |
| vulnerability        | url, vulname, matched, request, response, level                                     |
| crawler              | url, method, body, resultId                                                         |
| PageMonitoring       | url, hash, diff, response                                                           |
| IPAsset              | ip, domain, port, service, webServer, app                                           |
| SubdomainTakerResult | domain, value, type, response                                                       |


**search examples:**

- `domain==www.example.com && port==443` (exact match, uses index)
- `domain=^example.com` (prefix match, uses index)
- `ip==192.168.1.1`
- `task=="some task name"`
- `level==high` (vulnerability)
- `statuscode==200` (DirScanResult)

Use `=` only when you need fuzzy containment, e.g. `title=admin` (not indexed; best combined with a project or other condition to narrow the scope).

### 4.4 filter exact filtering

A JSON object: multiple values under the same key are **OR**, different keys are **AND**.

**Prefer `project` when there is a project condition:** if the user or context has already specified a project and the asset_type supports `project`, include it to narrow the scope; if there is no project information, it need not be forced.


| filter key   | Meaning            | Value description                                                          |
| ------------ | ------------------ | -------------------------------------------------------------------------- |
| `project`    | Owning project     | **ObjectID**, obtained via `list_projects` / `list_projects_data`          |
| `task`       | Source task        | **Task name**, the `name` from `list_tasks`                                |
| `port`       | Port               | e.g. `"443"`                                                               |
| `service`    | Service/protocol   | e.g. `"https"`                                                             |
| `app`        | Application fingerprint | e.g. `"Nginx"`                                                        |
| `icon`       | Icon hash          |                                                                            |
| `statuscode` | HTTP status code   | Mainly used for asset                                                      |
| `status`     | Status             | UrlScan/DirScan HTTP code; handling status for vulnerabilities/sensitive info |
| `level`      | Vulnerability level| critical / high / medium / low / info                                      |
| `type`       | Type               | e.g. subdomain record type A, CNAME                                        |
| `color`      | Sensitive rule color | SensitiveResult                                                          |
| `sname`      | Sensitive rule name | SensitiveResult                                                           |
| `tags`       | Tags               |                                                                            |


**Available filter keys per type:**


| asset_type                            | filter key                                                      |
| ------------------------------------- | --------------------------------------------------------------- |
| asset                                 | project, port, service, app, icon, statuscode, type, task, tags |
| RootDomain                            | project, tags                                                   |
| subdomain                             | project, type, task, tags                                       |
| app / mp                              | project, tags                                                   |
| UrlScan                               | status, tags                                                    |
| DirScanResult                         | status, tags                                                    |
| SensitiveResult                       | status, color, sname, tags                                      |
| crawler                               | project, task, tags                                             |
| vulnerability                         | project, level, status, task, tags                              |
| PageMonitoring / SubdomainTakerResult | tags                                                            |
| IPAsset                               | project, port, service, app                                     |


**filter example:**

```json
{"project": ["<project ObjectID>"], "port": ["443"]}
```

**Combined query example:**

```json
{
  "asset_type": "asset",
  "search": "domain=^baidu && port==443",
  "filter": {"project": ["<project ObjectID>"]},
  "pageIndex": 1,
  "pageSize": 10
}
```

**Notes:**

- When there is a project condition, prefer `filter.project` (where supported); without project context it need not be forced
- Do not put the project display name in `filter.project`
- Use `==` for known values and `^` for prefixes; avoid overusing `=` fuzzy matching on large tables
- For UrlScan HTTP status use `filter.status`; for DirScanResult you can use `statuscode==200` in search
- For SensitiveResult by rule name: use `sname=rule_name` in `search`, or `filter.sname`

### 4.5 Sorting with sort

Supported only for **UrlScan** and **DirScanResult**:

```json
{"length": "ascending"}
```

Other types ignore `sort` and use the default time ordering.

---

## 5. Scan Template Module Names

`TargetHandler`, `SubdomainScan`, `SubdomainSecurity`, `PortScanPreparation`, `PortScan`, `PortFingerprint`, `AssetMapping`, `AssetHandle`, `URLScan`, `WebCrawler`, `URLSecurity`, `DirScan`, `VulnerabilityScan`, `PassiveScan`

---

## 6. Troubleshooting


| Symptom                         | Resolution                                                                                          |
| ------------------------------- | --------------------------------------------------------------------------------------------------- |
| MCP shows no tools              | Check the URL, the API Key, and whether ScopeSentry is running                                      |
| 401 / 403                       | Recreate or replace the API Key                                                                     |
| Assets cannot be found          | Confirm `filter.project` is an ObjectID; do not write project in search                             |
| Template/task creation fails    | `template` must be a template ObjectID; `node` must be an online node name                          |
| Queries are very slow / hang    | With a project, add `filter.project`; for indexed fields in search use `==` or `^` prefix and use `=` sparingly; reduce `pageSize` |


---


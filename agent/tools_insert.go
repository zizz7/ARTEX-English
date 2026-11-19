package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`

	// Related marks whether this asset is relevant to the CURRENT test task and
	// should therefore auto-enter the coverage system (task_scope denominator).
	// nil = default true (relevant → auto-scoped). false = insert into the shared
	// asset store but keep it OUT of this task's coverage. Only consulted when the task
	// has the coverage feature enabled; ignored (no auto-scope either way) when off.
	Related *bool `json:"related"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"Batch-insert assets into the asset store as soon as new assets are discovered. Supports submitting multiple asset types at once: root_domain/ip/subdomain/app/service/endpoint.\n"+
			"Field descriptions per asset:\n"+
			"• root_domain: domain (root domain, required), icp (ICP filing, optional)\n"+
			"• ip: ip (IP address, required, must be IPv4/IPv6 address not a hostname), bound_domains ([domains]), open_ports ([{port,service}])\n"+
			"• subdomain: domain (subdomain, required), record_type (A/AAAA/CNAME etc.), record_value (record values), icp (optional)\n"+
			"• app: app_name (app name, required), bundle_id (bundle id, optional), category, description, app_icp, company_id (owning company id, optional)\n"+
			"• service (http): url (required), technologies ([fingerprints]), status_code, content_length, page_title, favicon_mmh3, auth ([{type,username,password,...}]), service_ip\n"+
			"• service (other): service_name (required), ip or domain (at least one), port (required), auth ([...])\n"+
			"• endpoint: url (required), method (required), params ([{location,name,value,type}]), service_ip\n"+
			"auth/technologies/params are all APPEND-MERGED — they do not overwrite existing values.\n"+
			"Returns: {results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id is not exposed to the model: which task the worker belongs to is authoritatively assigned by the program via SetTaskID (see handler).
			"assets": map[string]any{
				"type":        "array",
				"description": "Array of assets, each element corresponds to one asset record",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "Asset type",
					},
					// root_domain / subdomain
					"domain":      str("Root domain or subdomain (required for root_domain/subdomain)"),
					"icp":         str("ICP filing number (optional)"),
					"record_type": str("DNS record type: A/AAAA/CNAME/MX etc. (optional for subdomain)"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS record value list (optional for subdomain, e.g. [\"1.2.3.4\",\"2.3.4.5\"])",
					},
					// ip
					"ip": str("IP address, must be an IPv4/IPv6 address, not a hostname (for hostnames use the domain field with type=subdomain); required for ip type; optional for service/endpoint types to associate an IP"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "List of domains bound to this IP (optional for ip type)",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "List of open ports (optional for ip type)",
						"items": obj(map[string]any{
							"port":    intp("Port number"),
							"service": str("Service name, e.g. http/ssh/mysql etc. (optional)"),
						}, "port"),
					},
					// app
					"app_name":    str("Application name (required for app type)"),
					"bundle_id":   str("Bundle ID (optional for app type)"),
					"category":    str("Application category (optional)"),
					"description": str("Application description (optional)"),
					"app_icp":     str("Application ICP filing (optional)"),
					"company_id":  intp("Owning company id (optional for app type; apps cannot be auto-attributed via scope, must be explicitly specified. id is returned by add_company_scope)"),
					// service (http)
					"url":         str("Full URL including protocol and port (required for HTTP services; service_type is automatically set to http)"),
					"status_code": intp("HTTP response status code, e.g. 200/301/403/404 (optional)"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP response body size in bytes (optional)",
					},
					"page_title":   str("Page <title> content (optional)"),
					"favicon_mmh3": str("Favicon MMH3 hash (optional)"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Fingerprint/technology stack list, e.g. [\"Nginx\",\"Vue\",\"Bootstrap\"] (optional)",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "List of discovered authentication info, each containing type/username/password etc. fields (optional, append-merged, never overwrites)",
						"items":       map[string]any{"type": "object"},
					},
					// service (other, non-HTTP)
					"service_name": str("Service name, e.g. ssh/mysql/redis (required for non-HTTP service)"),
					"port":         intp("Port number (required for non-HTTP service)"),
					// endpoint
					"method": str("HTTP method: GET/POST/PUT/PATCH/DELETE etc. (required for endpoint)"),
					"params": map[string]any{
						"type":        "array",
						"description": "Request parameters list, each containing location (query/body/header/path)/name/value/type (optional, append-merged, never overwrites)",
						"items":       map[string]any{"type": "object"},
					},
					// Coverage-related: whether this asset is relevant to the current test task.
					"related": map[string]any{
						"type":        "boolean",
						"description": "Whether this asset is relevant to the CURRENT test task: true (default) auto-includes it in asset coverage (test scope denominator) and places it in the untested assets queue. For assets unrelated to the task, such as CDNs serving only static resources, you MUST set this to false or skip insertion entirely; false means insert into the store only, without counting toward this task's coverage (e.g. incidentally discovered adjacent sites/unrelated assets). Only effective when the task has the asset coverage feature enabled.",
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets not enabled: AssetStore not initialized"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id is authoritatively assigned by the program (worker: SetTaskID), not accepted
			// from the model — prevents the model from omitting or misassigning, which would leave
			// assets unattributed or attributed to the wrong task. Callers without task context
			// (auto/pentest/chat) have t.taskID=0.
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			for i, item := range a.Assets {
				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent registered via insert_assets"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker intent #%d registered via insert_assets", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// Auto-add to test scope (source='auto'): only for this single item explicitly
				// inserted by the worker at the top level, adding a conservative scope based on
				// its type; side-effect derived assets don't pass through here, so scope doesn't
				// expand blindly. No-op when taskID=0.
				// When asset coverage feature is disabled, test scope (denominator) is no longer
				// accumulated; assets with related=false (unrelated to current task) are also
				// only stored, not counted toward coverage. related omitted/null is treated as
				// true (included by default).
				related := item.Related == nil || *item.Related
				if !t.coverageDisabled && related {
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"Add domains/IPs/CIDRs/ICP filings/company keywords to a company's ASSET SCOPE — domains, networks, and ICP filings will auto-claim matching assets; keywords only serve as scope hints for the agent.\n"+
			"Company name is unique: if company does not exist it is created, if it exists it is reused (scope is merged in).\n"+
			"scope: one entry per line, automatically recognized: root domain / URL / single IP / CIDR range / ICP filing / company keyword.\n"+
			"Always provide reason explaining the attribution basis (whois/certificate/ASN etc.).\n"+
			"Guardrails: bare TLDs and overly broad ranges are rejected (IPv4 prefix must be /16-/32, IPv6 must be /32-/128); invalid entries are skipped and returned in errors.",
		obj(map[string]any{
			"company": str("Company name (created if not exists, reused if exists; name is unique)"),
			"scope":   str("Asset scope, one entry per line: domain / URL / IP / CIDR / ICP filing / company keyword"),
			"reason":  str("Attribution basis (evidence/source), must be provided"),
			"logo":    str("Company logo URL (optional; only takes effect when creating a new company)"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope not enabled: CompanyStore not initialized"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company must not be empty"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("failed to create/get company: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"Add test scope to THIS TASK — this defines the asset coverage denominator and the task's authorization boundary.\n"+
			"kind supports: company (all assets under company) / root_domain (entire root domain, including all subdomains) / subdomain (single exact subdomain) / ip / cidr / icp / keyword.\n"+
			"Note: hosts the worker encounters individually are AUTOMATICALLY added to scope by the system (exact subdomain); this tool is for DELIBERATELY WIDENING scope — pulling an entire root domain or company into scope, or adding a specific subdomain/IP.\n"+
			"value: for company pass company name or id (company must already exist); for root_domain/subdomain pass domain; for ip/cidr pass IP or range; for icp/keyword pass filing number or company keyword.\n"+
			"Always provide reason (auditable). Use entries array for multiple items.",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "Batch: [{kind, value}]. kind must be one of: company/root_domain/subdomain/ip/cidr/icp/keyword.", "items": map[string]any{"type": "object"}},
			"kind":    str("[Single entry] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[Single entry] company name or id / domain / IP / CIDR / ICP / keyword"),
			"reason":  str("Basis for inclusion (for audit), must be provided"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope not enabled: AssetStore not initialized"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope requires task context (no current task)"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // single-entry mode
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"Query assets within THIS TASK and directly associated tasks' scope that have not yet been covered by fact anchors (associated scope is read-only, for you to judge whether to run supplementary tests — it does not make decisions for you).\n"+
			"Optionally filter by asset type: root_domain/subdomain/service/app/endpoint/ip.\n"+
			"Paginated: page starts from 1, page_size defaults to 10. Returns {assets:[{id,type,label}], total, page, page_size}. Only available in task context.",
		obj(map[string]any{
			"type":      str("Asset type filter (optional): root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("Page number, starting from 1 (default 1)"),
			"page_size": intp("Items per page (default 10)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets not enabled: AssetStore not initialized"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets requires task context"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"Query the asset store. Supports DSL expression search, direct retrieval by id/ids, and pagination.\n"+
			"DSL syntax: field=value fuzzy (ILIKE) | field==value exact | field!=value exclude | port>80 numeric comparison | bare word = full-text fuzzy.\n"+
			"Logical operators: AND / OR (keywords, AND has higher precedence than OR), parentheses for grouping.\n"+
			"Asset type is filtered via the separate type parameter; DSL does not contain a type field.\n"+
			"Constraint: when id/ids are not provided, dsl must be non-empty — unconditional queries of all assets are not allowed; a query condition is required.\n"+
			"Available fields:\n"+
			"  domain      Domain name (root domain/subdomain/service domain)\n"+
			"  root_domain Root domain (only for subdomain/service assets)\n"+
			"  ip          IPv4/IPv6 address\n"+
			"  url         Full URL (service/endpoint)\n"+
			"  page_title  Page title (HTTP service)\n"+
			"  icp         ICP filing number (root domain)\n"+
			"  service_name Service name (non-HTTP service)\n"+
			"  app_name    Application name (app type)\n"+
			"  method      HTTP method (endpoint type, e.g. GET/POST)\n"+
			"  service_type HTTP service type: http|other\n"+
			"  record_type DNS record type (subdomain, e.g. A/CNAME)\n"+
			"  technology  Fingerprint/technology stack (array field, = fuzzy == exact)\n"+
			"  port        Port number (integer, supports > >= < <=)\n"+
			"  status_code HTTP status code (integer, supports > >= < <=)\n"+
			"  company_id  Owning company id (integer)\n"+
			"  task_id     Source task id (integer)\n"+
			"Examples: status_code>=400 AND technology=shiro\n"+
			"          method==POST AND url=/api/admin\n"+
			"          icp=Beijing OR icp=Shanghai\n"+
			"          (port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str(`DSL query expression, see tool description. Must be non-empty when id/ids are not provided. Examples: status_code>=400 technology=shiro / method==POST url=/api/admin`),
			"type":   str("Asset type filter: root_domain|ip|subdomain|app|service|endpoint (independent field, can be combined with dsl; type alone is insufficient for a query, dsl is still required)"),
			"id":     intp("Retrieve a single asset by id directly (optional, mutually exclusive with dsl/type)"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Retrieve multiple assets by ids directly (optional, mutually exclusive with dsl/type)"},
			"limit":  intp("Maximum number of results, default 10 (optional)"),
			"offset": intp("Pagination offset, default 0 (optional)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets not enabled: AssetStore not initialized"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDs([]int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDs(a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSL(a.DSL, a.Type, a.Limit, a.Offset)
			default:
				return actool.Errorf("dsl must not be empty when id/ids are not provided: unconditional queries of all assets are not allowed, please provide query conditions"), nil
			}
			if err != nil {
				return actool.Errorf("DSL error: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"List companies in the asset store along with their asset scope and attributed asset count. "+
			"Use this to see which companies exist and obtain company_id (needed for insert_assets to associate apps, and list_assets to filter by company_id). "+
			"Optional search parameter for fuzzy filtering by company name (case-insensitive); leave empty to return all.",
		obj(map[string]any{
			"search": str("Fuzzy filter by company name (optional, case-insensitive); leave empty to return all"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies not enabled: CompanyStore not initialized"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("failed to query companies: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.addFinding(), t.recordFact(),
		// cross-work retrieval
		t.searchAllWorkerTraces(), t.listWorkerTraces(), t.getWorkerTrace(),
		// asset management (handlers guard nil store internally).
		// add_company_scope is not given to worker: defining company asset scope is the responsibility of planner/main agent/Auto; worker only executes exploration.
		t.insertAssets(), t.listAssets(), t.listCompanies(),
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work: human can inject real-time steering instructions into a running intent (work) without interrupting or losing progress.
		t.steerWorkTool(),
		// set_goals: human can add a new final goal to this task at runtime (planner uses it to re-evaluate whether goals are met).
		t.setGoals(),
		// set_constraints: human can add/modify operation constraints (allow/deny) for this task at runtime, bounding the planner/worker's exploration boundaries.
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets: query untested assets within this task's scope on demand (type + pagination), decide on supplementary testing yourself.
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}

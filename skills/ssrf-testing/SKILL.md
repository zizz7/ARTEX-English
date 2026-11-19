---
name: ssrf-testing
description: Call this skill when testing for server-side request forgery in URL-fetching features, webhooks, importers, renderers, and parsers on in-scope targets.
---

# SSRF Testing

Applies **only to in-scope targets** under the engagement's rules of engagement (RoE). A finding needs reproducible evidence (exact request, exact response, key line) **before** you call `report_finding`. Use non-destructive, read-only requests.

---

## Boundaries

| Allowed | Prohibited |
|---|---|
| Out-of-band callback host **supplied by the operator** (or an operator-approved collaborator service) | Third-party callback services the operator did not approve |
| Probing loopback, RFC1918 ranges, and link-local metadata addresses from the in-scope server's perspective, with a small targeted set of addresses | Scanning whole internal ranges or port sweeping at scale; touching internal hosts the operator did not list as in scope |
| Reading cloud metadata to prove reachability (stop at a harmless field such as instance ID or region) | Extracting, printing, or using IAM credentials/tokens; pivoting with anything retrieved |
| GET-style fetches | POST/PUT to internal services (caches, admin panels, queues) or any state-changing internal request |

If a metadata response contains credentials, do not store or use them. Record only that the endpoint responded and which key names were present, and tell the operator immediately.

---

## Phase 1 - Locate candidate sinks

Use ARTEX recorded traffic and the app's UI/API docs. Look for any parameter that carries a URL, hostname, IP, or path:
- Webhooks, callback URLs, notification endpoints
- Import/fetch from URL (avatar, feed, CSV, calendar `.ics`, link preview/unfurl)
- Server-side renderers: PDF/HTML-to-image, screenshot, document converters
- OAuth/OIDC `redirect_uri`, `request_uri`, `jwks_uri`, `sector_identifier_uri`
- XML/SVG/Office parsers (external entities, `xlink:href`, `SYSTEM` identifiers)
- Proxy-style params: `url=`, `uri=`, `dest=`, `redirect=`, `next=`, `image=`, `feed=`, `host=`, `site=`
- Headers that influence fetches: `Host`, `X-Forwarded-Host`, `Referer` (when analytics fetch them)

Rank by how likely the server itself makes the request (server-side fetch vs. client-side redirect).

## Phase 2 - Confirm a server-side fetch (out-of-band)

1. Obtain the callback host from the operator (for example `cb.operator-controlled.example`). Use a unique token per test: `http://<token>.cb.operator-controlled.example/`.
2. Submit it to the candidate sink. Check the callback log for an HTTP or DNS hit.
3. Note the source IP, `User-Agent`, and HTTP method/headers of the hit. These show which component made the request.

A callback alone proves only that something resolved or fetched the URL. It is **not yet** a reportable SSRF (it may be a client-side fetch or a third-party scanner). Continue to Phase 3.

## Phase 3 - Differential confirmation (in-band)

Compare responses for URLs with different outcomes and look for a measurable difference:

| Test URL | What to compare |
|---|---|
| Operator callback (reachable) | Baseline success |
| `http://127.0.0.1:<known-open-port>/` vs `http://127.0.0.1:1/` | Status, body, error text, response time |
| A known in-scope internal host vs. a non-existent one | Different error message or timing |
| `http://169.254.169.254/` (AWS/Azure-style link-local) | Reachability only. GCP needs a `Metadata-Flavor: Google` header, which SSRF usually cannot add, so a failure there is not proof of safety |

Confirmation requires one of: reflected internal content, a distinguishing error/timing difference between open and closed internal targets, or an internal response body fragment. Record the exact request and response.

## Phase 4 - Filter-bypass variants (only if basic URLs are blocked)

Use standard forms to learn what the filter checks, not to reach new targets:
- Alternate encodings of the same loopback (decimal, octal, hex, `127.1`, `[::1]`) and `localhost` aliases.
- A hostname you control that resolves to a loopback/private address (operator-provided DNS record).
- Redirect-based: your callback host returns a 302 to an internal address. If the filter checks only the first URL, the redirect shows an unchecked follow.
- DNS rebinding: use an operator-provided rebinding domain that alternates public and internal answers. Test only when the operator supplied such infrastructure.
- Parser confusion: `http://allowed.example@internal/`, fragments, backslash handling.
- Avoid `gopher://` and similar payloads that interact with internal services. If `file://` is accepted, confirm acceptance with a harmless world-readable path only when the RoE allows.

## Phase 5 - Blind SSRF

If no response content is reflected, rely on callback hits, timing differences, and error-code differences. State clearly in the report that the issue is blind and what you inferred.

## Phase 6 - Verify and clean up

- Repeat the decisive test twice and with a control (unreachable address) to rule out generic latency.
- Remove any webhooks or imports you created, and note stored fetch results.
- Give the operator the callback log; it should contain only your test tokens.

---

## Evidence to capture

- [ ] Sink location (endpoint, parameter) and how it was found
- [ ] Exact request used and exact response (status, body excerpt)
- [ ] Callback log entry (timestamp, source IP, User-Agent) for the unique token
- [ ] Differential evidence: two requests (reachable vs. unreachable internal target) and the observable difference
- [ ] For metadata: which endpoint responded and which non-sensitive field was seen (no credentials stored)
- [ ] Bypass variant used, if any, and why the baseline was blocked
- [ ] Cleanup confirmation for created webhooks/imports

## Common false positives

- Callback comes from a third-party URL scanner, link-preview bot, or your own browser, not the application server.
- Client-side redirect or JS fetch: the request originated from your machine.
- DNS lookup only, with no evidence an HTTP connection was attempted.
- Differences caused by generic validation errors ("invalid URL") rather than a real internal fetch.
- An egress proxy returns the same error for every target.
- Timing differences within normal jitter; repeat and compare medians.
- The app fetches only pre-approved public hosts and rejects internal addresses consistently.

---
name: access-control-idor
description: Call this skill when testing IDOR, broken object-level authorization, or broken function-level access control using two-identity differential testing.
---

# Access Control / IDOR Testing (Two-Identity Differential)

Applies **only to in-scope targets** under the engagement's rules of engagement (RoE). If a host, API, or account is not explicitly in scope, do not touch it. Every finding needs reproducible evidence (exact request, exact response, key differing line) **before** you call `report_finding`.

---

## Boundaries

| Allowed | Prohibited |
|---|---|
| Two test identities: operator-supplied accounts, or accounts you registered yourself (if the RoE allows registration) | Using real users' credentials or sessions, or tokens harvested from real users |
| Reading objects that belong to identity A while logged in as B | Bulk-enumerating or dumping other users' data; stop at the minimum needed to prove the issue |
| Write/delete tests (PUT/PATCH/DELETE) **only on objects you created yourself** | Modifying or deleting objects you did not create, or objects from the operator's production data |
| Low-rate sequential ID probing (a handful of IDs) | Mass ID brute force, scraping, load generation |

If you only have one identity, say so and test unauthenticated vs. authenticated and role-vs-role instead; do not invent a second identity from someone else's data.

---

## Phase 1 - Set up identities and objects

1. Identity **A** (victim/owner) and identity **B** (attacker). For vertical tests also use a low-privilege and, if supplied, a high-privilege account.
2. As A, create a small set of objects through the normal UI/API (profile, document, order, project, comment). Note each object's ID, type, and URL.
3. As B, create the same kinds of objects so you have B-owned baselines.
4. Keep a table: `object | owner | ID | endpoint | methods seen`.

## Phase 2 - Find object identifiers

Use ARTEX's recorded traffic tools before guessing anything:
- List recorded requests for the target and filter for path segments, query params, JSON body fields, and headers that look like identifiers: `id`, `uid`, `user_id`, `account`, `org`, `tenant`, `doc`, `order`, `uuid`, numeric segments, base64 blobs.
- Pull full request/response pairs for endpoints that returned an ID of an object, then look for that ID being reused in other endpoints (export, share, download, invoice, avatar, webhook).
- Look in responses for IDs of objects you did not create (list endpoints, activity feeds, mentions, error messages). Those are test candidates only if they belong to your test accounts; otherwise note them but do not request them.
- Note ID type: sequential integer, UUID, hash, encoded. UUIDs are not authorization; test them the same way using IDs that A legitimately owns.

## Phase 3 - Baseline with identity A

For each interesting endpoint, as A request A's object and record:
- status code, `Content-Type`, response length
- JSON key set and types (body shape)
- sensitive fields present (email, role, token, address, price)
- relevant headers (`Set-Cookie`, `ETag`, cache headers)

Store the raw request so it can be replayed exactly.

## Phase 4 - Replay as identity B (horizontal)

Replay the identical request with only the credential swapped (cookie/Authorization header of B). Do the same for IDs in the path, query, body, header (`X-User-Id`), and cookies.

Compare against the baseline:

| Observation | Meaning |
|---|---|
| 200 + same body as A's baseline, containing A's data | Strong IDOR candidate |
| 200 + B's own data (ID ignored) | Not a finding; server binds to session |
| 403/404 with generic body | Enforced; note whether 403 vs 404 leaks existence (low value) |
| 200 but empty/redacted body | Check field-level diff before deciding |
| 5xx | Note, do not escalate; may be an unhandled authz path |

Do a **field-level diff** (`jq -S` both bodies, then `diff`): partial exposure (e.g. B sees email and phone but not address) is still a finding if the fields are sensitive.

## Phase 5 - Vertical escalation

- As the low-privilege identity, request admin-only endpoints and functions seen in traffic, JS bundles, or API docs (`/admin`, `/api/v1/users`, role-change, export, impersonation).
- Check function-level access: does the UI hide the control while the API still accepts it?
- Test mass-assignment on **your own** object: add `role`, `is_admin`, `owner_id` to your own profile update and see if the server accepts and persists it. Revert the change afterwards.

## Phase 6 - Method swapping and parameter pollution

Only on objects **you created**:
- Take a GET that was denied and try HEAD, OPTIONS, PUT, PATCH, DELETE against your own throwaway object, then with B's session against A's **test-created** object. Never DELETE an object that is not a throwaway test object.
- `X-HTTP-Method-Override` / `_method` parameter variants.
- Parameter pollution: duplicate the ID (`?id=B_ID&id=A_ID`), mix path and body IDs, array form (`id[]=`), JSON with duplicate keys, nested `{"user":{"id":...}}`. Record which value the server honors.
- Path variants: trailing slash, case change, `/api/v1/` vs `/api/v2/` (older versions often lack checks), `.json` suffix.

## Phase 7 - Verify and clean up

- Re-run each positive twice, and once with an unauthenticated request (no credentials) and once with an invalid token, to rule out public endpoints.
- Confirm the data really belongs to A (not a shared/public resource).
- Delete or revert every object you created or modified.

---

## Evidence to capture

- [ ] Identities used (labels only, never passwords) and how each was obtained
- [ ] Baseline request/response as A (exact request, status, key body excerpt)
- [ ] Replayed request as B and the response proving access (status plus the specific field/line showing A's data)
- [ ] Field-level diff or note showing which fields were exposed
- [ ] Unauthenticated control request result
- [ ] For write/delete: the object was created by you, plus before/after state
- [ ] Cleanup confirmation

## Common false positives

- Resource is intentionally public or shared (public profile, shared link, org-wide resource).
- 200 returned but body is B's own data or a generic default; server ignored the ID.
- Both identities belong to the same org/tenant and the data is legitimately shared inside it.
- Cached response from A's earlier request served to B (verify with a cache-busting parameter).
- IDs that are guessable but access is still enforced (a guessable ID alone is not a finding).
- Response differs only by a reflected ID or timestamp.
- Identity B was granted access earlier (e.g. an invite you forgot), so access is by design.

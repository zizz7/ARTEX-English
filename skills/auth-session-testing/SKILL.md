---
name: auth-session-testing
description: Call this skill when testing authentication, session management, JWTs, password reset, and MFA flows on in-scope targets.
---

# Authentication and Session Testing

Applies **only to in-scope targets** under the engagement's rules of engagement (RoE). A finding needs reproducible evidence (exact request/response or command plus the key output line) **before** you call `report_finding`. Prefer non-destructive checks.

---

## Hard limits (read first)

| Rule | Detail |
|---|---|
| No credential stuffing | Never replay leaked/breached credential lists against the target |
| Never use real users' credentials | Only operator-supplied test accounts or accounts you registered yourself (if the RoE allows) |
| Online brute force | Off by default. Only if the RoE explicitly allows it, and then capped (for example under 20 attempts, against a test account, with delays, stop on first lockout signal) |
| No lockout of real users | Trigger lockout checks only on your own test account |
| Offline cracking | Allowed only against tokens/hashes you legitimately obtained from the test account, with a small wordlist |
| Do not store secrets | Redact passwords and tokens in notes; keep only what proves the issue |

---

## Phase 1 - Map the flows

Using ARTEX recorded traffic and manual requests, list for each flow: endpoint, method, parameters, and what state it changes.
- Registration, login, logout, logout-all, password change, forgot/reset password, email/phone verification, MFA enroll/verify/recovery, SSO/OAuth callback, token refresh, "remember me".
- Note anti-automation controls (rate limit headers, CAPTCHA, lockout messages). Do not bypass CAPTCHA; report its absence or presence only.
- Check user enumeration: do login, register, and reset return different messages, status codes, or timing for existing vs. non-existing accounts? Compare your own known account against a clearly invalid one.

## Phase 2 - Cookie and token flags

For every session cookie (`Set-Cookie`) and any token stored client-side:
- `Secure`, `HttpOnly`, `SameSite` present and sensible; `Domain`/`Path` not overly broad.
- Session ID changes after login (fixation check: note a pre-login ID, log in, confirm it is replaced).
- Expiry/`Max-Age` reasonable; no tokens in URLs, referrers, or logs; `Cache-Control: no-store` on authenticated pages.
- Tokens in `localStorage` when HttpOnly cookies would work: hardening note, not a vulnerability by itself.

## Phase 3 - Session lifecycle

Use two sessions for your own test account (S1, S2):
1. **After logout:** copy S1's token, log out, replay an authenticated request with the old token. Still 200 means logout does not invalidate server-side.
2. **After password change:** with S1 and S2 active, change the password in S2, then replay a request with S1. A still-valid S1 is a finding.
3. **After MFA enrollment or role change:** same replay logic.
4. Concurrent sessions and idle timeout: note behavior; report only if it contradicts stated policy.

## Phase 4 - JWT analysis (offline first)

Decode only at first; do not tamper against the live service until the offline review says it is worthwhile.
1. Split and base64url-decode header and payload. Record `alg`, `kid`, `jku`, `x5u`, `jwk`, `iss`, `aud`, `exp`, and role claims.
2. Checks against the live service (using your **own** token, changing only your own claims):
   - `alg: none` (and `None`/`nOnE` variants) with empty signature: is it rejected?
   - Signature stripped or truncated; expired token still accepted; wrong `aud`/`iss`.
   - RS256 to HS256 confusion, only if the public key is published (for example via JWKS).
   - `kid` handling: path-like or quote characters in `kid` using a harmless marker; observe error or behavior differences.
   - `jku`/`x5u`/`jwk` header pointing at an operator-provided host you control; confirm only whether the server fetches or trusts it.
3. Weak HMAC secret, **offline only**, small wordlist (for example 1k-10k common secrets):
   - `hashcat -m 16500 token.jwt wordlist.txt`, or a jwt tool equivalent.
   - Never run guessing against the service. If a secret is found, prove impact by forging a token for **your own** account claims only, never another user's.

## Phase 5 - Password reset

- Request resets for your own test account several times. Compare tokens: length, charset, patterns, embedded timestamps, sequential or time-based values (predictability).
- Reuse: use the token once, then try it again. Still valid is a finding.
- Expiry: note the stated lifetime; test with an old token from your own account. Earlier tokens should die when a new one is issued.
- Token binding: does the token work if the account/email parameter is altered? Does the reset link use the `Host` or `X-Forwarded-Host` value (request a reset for your own account and read the received link)?
- The reset response must not leak the token in the body or headers.

## Phase 6 - MFA step skipping

With your own test account that has MFA enabled:
- After the password step, request post-auth endpoints directly without completing MFA.
- Replay the MFA verify request with an altered or empty code, a missing parameter, or a different user ID.
- Check whether the pre-MFA token is already a full session, and whether codes are reusable, long-lived, or unthrottled. Report missing throttling; do not brute force codes unless the RoE allows capped online testing.
- Check recovery/backup-code and "remember this device" flows for bypass.

---

## Evidence to capture

- [ ] The flow and endpoint tested, account label used (never the password)
- [ ] Exact request and response (status and the line proving the issue, such as old token still returning 200)
- [ ] For JWT: decoded header/claims, the check performed, and the server response
- [ ] For offline cracking: command run and the output line, with the secret redacted unless the operator requires it
- [ ] For reset predictability: several token samples (own account) and the observed pattern
- [ ] Confirmation that no real user's credentials or data were used
- [ ] Cleanup: sessions logged out; test accounts listed for the operator to disable

## Common false positives

- Old token still works for a few seconds because of short caching, not a real invalidation failure (retest after a delay).
- Stateless JWT with short `exp`: logout not revoking is a design choice; report only if the lifetime is long.
- Missing `Secure` on a non-sensitive cookie (for example locale) or on a local HTTP dev host.
- Different error messages that are identical in practice once whitespace/timing noise is normalized.
- `alg: none` token accepted by a library but the protected endpoint still denies it (confirm a protected resource is actually returned).
- Reset token "looks sequential" from a tiny sample; gather more samples before claiming predictability.
- MFA skip tested on an endpoint that never required auth at all (a separate issue).

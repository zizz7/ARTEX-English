---
name: subdomain-takeover-cloud
description: Call this skill when checking in-scope domains for dangling DNS records (subdomain takeover) and for misconfigured public cloud storage buckets using read-only requests.
---

# Subdomain Takeover and Cloud Storage Exposure

Applies **only to in-scope targets** under the engagement's rules of engagement (RoE). A finding needs reproducible evidence (the DNS record plus the exact fingerprint response) **before** you call `report_finding`. Everything here is read-only.

---

## Hard rule: never claim a resource to prove a takeover

Do **not** register the bucket, app, page, or account that a dangling record points to, and do not upload a "proof" file. Claiming it can expose the operator's users to your content and may leave you holding a resource that belongs in the operator's hands. Report the dangling record plus the fingerprint response as the evidence. The operator decides whether to claim or delete.

Other limits: only subdomains and buckets that belong to in-scope domains; do not enumerate or download the contents of buckets beyond a listing of names; do not read or copy objects containing personal or credential data. If you see them, stop and tell the operator.

---

## Phase 1 - Collect candidates

Take in-scope subdomains from recon (ARTEX asset lists, certificate transparency, passive DNS the operator allowed). For each, resolve and record the full chain:

```
dig +noall +answer CNAME sub.example.com
dig +noall +answer A sub.example.com
dig +short NS example.com
```

Focus on subdomains whose CNAME points at a third-party service, and on NS records for delegated zones that point to nameservers that no longer respond (NXDOMAIN/SERVFAIL/REFUSED on the delegated zone is an NS takeover lead).

## Phase 2 - Check whether the target is dangling

For each CNAME target:
1. Resolve the target name. NXDOMAIN on the CNAME target at a provider-owned domain is a strong lead.
2. Request `http://sub.example.com/` and `https://sub.example.com/` (certificate errors are expected; use a read-only GET) and capture status, headers, and body.
3. Compare the body to the fingerprint table below.

| Service | CNAME pattern | Typical "unclaimed" fingerprint |
|---|---|---|
| AWS S3 website | `*.s3-website-*.amazonaws.com`, `*.s3.amazonaws.com` | `NoSuchBucket` / "The specified bucket does not exist" |
| GitHub Pages | `*.github.io` | "There isn't a GitHub Pages site here." (404) |
| Heroku | `*.herokuapp.com`, `*.herokudns.com` | "No such app" / "There's nothing here, yet." |
| Azure App Service | `*.azurewebsites.net` | NXDOMAIN on the target, or "404 Web Site not found" |
| Azure other | `*.cloudapp.azure.com`, `*.trafficmanager.net`, `*.blob.core.windows.net` | NXDOMAIN on target name |
| Fastly | `*.fastly.net`, `*.global.ssl.fastly.net` | "Fastly error: unknown domain" |
| Shopify | `shops.myshopify.com` | "Sorry, this shop is currently unavailable." |
| Zendesk | `*.zendesk.com` | "Help Center Closed" |
| Netlify | `*.netlify.app` | "Not Found - Request ID" with Netlify server header |
| Surge | `*.surge.sh` | "project not found" |
| Bitbucket | `*.bitbucket.io` | "Repository not found" |
| Pantheon | `*.pantheonsite.io` | "404 error unknown site!" |
| Tumblr / WordPress.com | `domains.tumblr.com`, `*.wordpress.com` | "There's nothing here." / "Do you want to register" |

Fingerprints change over time. Treat the table as a starting point and check current guidance (for example the public `can-i-take-over-xyz` list) for services marked vulnerable vs. not vulnerable. Some services verify domain ownership, so a matching fingerprint does not always mean it is claimable; say so in the report.

## Phase 3 - Public cloud storage checks (read-only)

For buckets named in recon, URLs, JS, or CNAMEs:

- **S3**: `curl -sI https://<bucket>.s3.amazonaws.com/` (status 200/403/404 distinguish exists/denied/missing). A listing check is an unauthenticated `GET /`; the response is XML with `ListBucketResult` if public. ACL check: `GET /?acl` unauthenticated; do not attempt PUT/DELETE.
- **GCS**: `GET https://storage.googleapis.com/<bucket>/` and `GET https://storage.googleapis.com/storage/v1/b/<bucket>/o` unauthenticated.
- **Azure Blob**: `GET https://<account>.blob.core.windows.net/<container>?restype=container&comp=list` unauthenticated.

Record: exists or not, listing allowed, number of keys visible (count only), whether names suggest sensitive content (for example `backup`, `.env`, `dump`), and whether any object is fetchable. Fetch at most one benign object (or just its headers with HEAD) if needed to prove access, and never download objects with personal data. Do not test write access; if write permission is suspected from ACL output, report that rather than trying it.

## Phase 4 - Verify

- Re-resolve DNS twice at different times to rule out propagation or transient errors.
- Confirm the subdomain is in scope and the record is actually pointing at the service (not a vendor-managed hostname you misread).
- Confirm whether live traffic or cookies could reach the subdomain (parent-domain cookies increase impact); note it without testing it.

---

## Evidence to capture

- [ ] `dig` output for the CNAME chain and the resolution of the target
- [ ] The HTTP request and response (status, key body line) matching the fingerprint
- [ ] Which service and the reason it is claimable (or note that ownership verification may block it)
- [ ] For buckets: the exact read-only request and the status, plus the listing excerpt (names only) or the ACL line
- [ ] Statement that nothing was claimed, registered, written, or deleted
- [ ] Timestamps of the two independent resolutions

## Common false positives

- The CNAME target exists and is active, but returns a default or custom 404 for that Host header.
- Service requires domain-ownership verification, so the fingerprint does not mean the host is claimable.
- Stale fingerprint: the provider changed its error page or fixed the issue.
- Wildcard DNS or CDN that returns the same page for every unknown host.
- Bucket returns 403 `AccessDenied` (it exists and is protected, which is not a finding).
- Bucket listing shows only intentionally public static assets.
- Transient DNS failure or resolver caching; recheck from another resolver.

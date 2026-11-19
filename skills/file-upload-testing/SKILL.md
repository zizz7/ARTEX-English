---
name: file-upload-testing
description: Call this skill when testing file upload features for validation bypasses, stored XSS via SVG/HTML, path traversal in filenames, and parser issues on in-scope targets.
---

# File Upload Testing

Applies **only to in-scope targets** under the engagement's rules of engagement (RoE). A finding needs reproducible evidence (exact request, exact response, served URL and `Content-Type`) **before** you call `report_finding`. Use harmless marker payloads only, and delete what you upload.

---

## Boundaries

| Allowed | Prohibited |
|---|---|
| Small test files with a unique marker string (for example `ARTEX-TEST-<id>`) | Webshells, reverse shells, or any file that executes commands or persists |
| Files that render a harmless alert/marker (`document.title` change, `console.log`) | Malware samples, EICAR-style files unless the operator asked for AV testing, huge files, zip bombs |
| Uploading as your own test account | Overwriting or replacing files owned by other users or by the application |
| Checking whether a file would execute by its served `Content-Type`, not by running code on the server | Attempting to run server-side code on the host |

If a server-side script appears to execute, stop at proving the marker output (for example a harmless arithmetic result in a template) and report; do not escalate.

---

## Phase 1 - Map upload features

From ARTEX recorded traffic and the UI, list every upload endpoint: avatar, attachments, import (CSV/XML/XLSX), document/PDF, logo, theme, support tickets. For each note: parameter name, allowed types stated in the UI, size limit, where the file is later served (URL pattern), and whether it is served from the main origin, a subdomain, or a CDN/bucket.

## Phase 2 - Baseline

Upload a legitimate small file (for example a 1x1 PNG with a marker in a comment). Record the request, the response, the stored filename, the served URL, and the response headers on fetch (`Content-Type`, `Content-Disposition`, `X-Content-Type-Options`, `Cache-Control`).

## Phase 3 - Validation bypass checks

Change one thing at a time and record accept/reject:
- Extension: allowed vs. other (`.txt`, `.html`, `.svg`, `.xml`, `.php`-style names with **harmless text content**), case (`.PnG`), trailing dot or space, null-byte-like or semicolon suffixes (`file.png;.txt`) as documented in OWASP WSTG.
- Double extensions: `marker.png.txt`, `marker.txt.png`. Check which extension the server keeps and which the server uses to choose the content type.
- `Content-Type` of the part: set to `image/png` while the body is plain text, and the reverse.
- Magic bytes: prepend `GIF89a` to plain text to see if validation is signature-only.
- Polyglot: a valid image header followed by harmless HTML text, to see whether the served type is sniffed.
- Size and count limits: test one file just above the limit only; do not flood.

An accepted bypass is only a finding if the stored file is **served** in a way that matters (see Phase 5) or is processed unsafely.

## Phase 4 - Filename handling

- Path traversal in the filename field: `../marker.txt`, `..%2fmarker.txt`, backslash variants. Check whether the stored path or returned URL changed. Do not try to write outside upload directories on purpose beyond one marker file; do not overwrite anything.
- Special characters and long names: look for error messages disclosing paths, stack traces, or storage backends.
- Reflected filename: is the filename rendered unescaped in the UI or emails (stored XSS via filename)? Use a marker such as `"><b>ARTEXTEST</b>.png` and observe encoding.

## Phase 5 - Stored XSS via active content

1. Upload an SVG containing a harmless marker script (for example sets `document.title` to the marker) and an HTML file with the same marker.
2. Fetch the served URL and record:
   - Origin: same origin as the application (impact is high) or a sandbox/cookieless domain (impact low).
   - `Content-Type`: `image/svg+xml` or `text/html` served inline executes; `application/octet-stream` or `Content-Disposition: attachment` does not.
   - `X-Content-Type-Options: nosniff` and any CSP on the response.
3. Confirm execution in the ARTEX browser tool by observing the marker effect when opening the URL directly. If it only runs when opened directly and not when embedded in an `<img>`, say so.

## Phase 6 - Parsers and image processing

- XML-based formats (SVG, XLSX/DOCX, XML import): include a benign external entity that points to the operator callback host and see whether it is fetched (XXE confirmation by callback). Do not read local files such as `/etc/passwd` unless the RoE explicitly permits and then only a harmless world-readable file.
- Image processing: upload a tiny image with a large declared dimension header only if limited by the RoE; otherwise note the lack of dimension limits without sending it. Look for metadata (EXIF) reflection and for unsafe converter behavior hinted at by error text.
- Archive uploads: check for path traversal in entry names with a single benign marker entry; never upload zip bombs.

## Phase 7 - Clean up

Delete every uploaded test file using the application's own delete function, and re-request the URLs to confirm they now return 404/403. List anything you could not delete so the operator can remove it.

---

## Evidence to capture

- [ ] The upload request (endpoint, parameter, filename, part `Content-Type`) and the response
- [ ] The served URL and the full response headers, especially `Content-Type`, `Content-Disposition`, `X-Content-Type-Options`
- [ ] Whether the file is served same-origin with the app (origin comparison)
- [ ] Proof of the marker (rendered title, console output, callback hit) with the exact observed line
- [ ] The validation rule bypassed and the control upload that was rejected
- [ ] Cleanup confirmation (delete action plus the follow-up 404/403)

## Common false positives

- The file is accepted but served as a download (`Content-Disposition: attachment`) or as `text/plain`, so nothing executes.
- Active content is served from a separate cookieless sandbox domain, giving no access to the app's session.
- The file is stored but never served back to anyone (no access path).
- SVG renders only as an `<img>`, where scripts do not run.
- The server rewrites or sanitizes content after upload, so the marker is stripped on fetch.
- Extension is accepted but the stored name is randomized and re-typed by magic bytes.
- A callback after XXE testing comes from a malware scanner or preview service rather than the XML parser (confirm it is the parser).

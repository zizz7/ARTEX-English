# Third-Party Licenses and Distribution Notes

The ARTEX project source code is provided under the GNU AGPL v3.0, and it also uses Go modules, npm packages, frontend components, and other third-party materials. AGPL-3.0 does not replace the individual licenses of these materials, and it cannot remove the copyright, license, and disclaimer notices they require to be retained.

## License entry points in the repository

| Scope | Where to verify | Notes |
| --- | --- | --- |
| ARTEX project source code | [`LICENSE`](LICENSE) | GNU Affero General Public License v3.0 (AGPL-3.0) |
| MIT notice retained in the frontend directory | [`web/LICENSE`](web/LICENSE) | Retains the original copyright and MIT terms; this file does not mean the entire ARTEX project uses the MIT license |
| Go dependencies | [`go.mod`](go.mod), [`go.sum`](go.sum) | Dependency version manifest; before actual distribution, the full license texts should still be collected from the corresponding module sources or release packages |
| Web dependencies | [`web/package.json`](web/package.json), [`web/package-lock.json`](web/package-lock.json) | Locked npm dependencies and some SPDX license metadata |
| Skill script dependencies | [`skills/api-recon/scripts/package.json`](skills/api-recon/scripts/package.json), [`skills/api-recon/scripts/package-lock.json`](skills/api-recon/scripts/package-lock.json) | npm dependency manifest for standalone scripts |

## Pre-distribution checks

Before providing source code, binaries, container images, installers, or a hosted service to others, complete at least the following checks:

1. Generate a software bill of materials (SBOM) from the actual release artifacts; do not infer the final contents solely from declarations in the repository.
2. Collect the license, copyright notice, and NOTICE file for each third-party component version included in the release artifacts.
3. Verify whether any license requires providing source code, modification notices, attribution statements, license copies, patent notices, or installation information.
4. Retain `web/LICENSE` and the file headers and rights notices that come with third-party materials.
5. Conduct a dedicated review of licenses such as GPL, AGPL, LGPL, MPL, SSPL, and BUSL that may affect distribution or network service obligations.
6. Check fonts, icons, screenshots, model outputs, test data, container base images, and system tools; they may not appear in the dependency manifests of language package managers.

## Scope limitations

This document provides license verification entry points and an action checklist; it is not a complete compilation of third-party licenses and does not constitute legal advice. Dependencies may vary with build tags, operating systems, target architectures, and installation methods, so the actual release artifacts and the upstream license texts for the corresponding versions should prevail.

# Security Policy

DockBack is a backup/restore tool with reach into Docker hosts, so we treat it
as security-sensitive software. This document covers how to **report a
vulnerability** and our **response and patch targets**.

## Supported versions

DockBack ships as a single, version-stamped container image. Security fixes land
on the latest release; older image tags do **not** receive backports — upgrade to
the current release.

| Version | Supported |
|---|---|
| Latest release | Yes |
| Any older tag | No — upgrade to latest |

## Reporting a vulnerability

**Please report privately — do not open a public issue, pull request, or
discussion for an unfixed vulnerability.**

- Preferred: **GitHub private vulnerability reporting** — this repository's
  **Security → Report a vulnerability** button. It keeps the report private to
  the maintainer and starts a draft advisory.

Please include, where you can:

- A clear description and the **impact** (what an attacker gains).
- **Reproduction steps** or a proof of concept.
- The affected version / image digest and your deployment shape.
- Any suggested remediation.

**Do not** include third parties' real data, run denial-of-service against shared
infrastructure, or access data beyond what is needed to demonstrate the issue.

### What to expect

| Stage | Target |
|---|---|
| Acknowledgement of your report | within 3 business days |
| Initial severity triage | within 5 business days |
| Status updates | at least every 7 days until resolved |
| Coordinated disclosure | after a fix ships, by mutual agreement (typ. <= 90 days) |

Good-faith research conducted under this policy will not be pursued. We are glad
to credit reporters in the release notes / advisory unless you prefer to remain
anonymous.

## Patch targets

Severity is assessed with CVSS v3.1, adjusted for reachability in DockBack.

| Severity | Triage | Fix or mitigation shipped |
|---|---|---|
| Critical (9.0–10.0) | <= 24 h | < 72 h |
| High (7.0–8.9) | <= 3 days | <= 7 days |
| Medium (4.0–6.9) | <= 7 days | next routine update (<= 30 days) |
| Low (0.1–3.9) | best effort | next routine update |

"Mitigation" may be a configuration/compose change or a documented workaround
when a code fix needs longer — the target above is about closing the exposure.

## How releases are hardened

- Released images are **built for `linux/amd64` and `linux/arm64`** and published
  to GitHub Container Registry, so any host pulls a ready-to-run build.
- The runtime is hardened by default: non-root, read-only root filesystem, all
  Linux capabilities dropped, `no-new-privileges`, a custom seccomp allow-list,
  and no host Docker socket (the app reaches Docker only through a filtered
  socket-proxy sidecar).
- Base images are **pinned by digest** and refreshed regularly, and releases are
  scanned for known vulnerabilities before publication.

For the runtime hardening details you can verify yourself, see the shipped
in-app **Docs → Security & Operations**.

## Known issues tracked upstream

We track issues we cannot yet fix in code because the fix is unavailable
upstream. We assess residual risk against DockBack's actual execution model and
re-evaluate on each routine update.

### Docker engine (moby) client advisories — `github.com/docker/docker`

- **Advisories:** `GO-2026-5746`, `GO-2026-5668`, `GO-2026-5617`, `GO-2026-4887`,
  `GO-2026-4883` (CVE-2026-41567 / 41568 / 42306, CVE-2026-34040, CVE-2026-33997),
  flagged by `govulncheck` in the pinned `github.com/docker/docker@v27.5.1+incompatible`.
- **Why not simply patched:** the moby project moved the module to
  `github.com/moby/moby/v2` and ships the fixes **only there**. There is **no fixed
  release of `github.com/docker/docker`** (the import path we use), and
  `github.com/moby/moby/v2` is currently a **pre-release (2.0.0-beta)** that does
  not yet publish the client packages we depend on. Bumping to the newest stable
  moby tag (v28.5.2) was verified **not** to clear the advisories.
- **Residual-risk assessment (why exposure is low):** DockBack uses moby purely as
  an **API client**, and only ever through a hardened `docker-socket-proxy` sidecar
  with a restricted endpoint allow-list — it never runs the moby daemon or a host
  Docker socket. Several of these advisories are **daemon/host-side** (e.g. symlink
  swap on the host, path traversal during daemon-side operations) and are outside
  DockBack's execution model. `govulncheck` attributes them at module granularity
  because moby ships daemon, client, and API types as one module without
  symbol-level fix metadata.
- **Plan:** migrate to `github.com/moby/moby/v2` once it publishes a **stable client
  release**, then re-scan to confirm the advisories clear. Tracked; re-checked each
  routine update.

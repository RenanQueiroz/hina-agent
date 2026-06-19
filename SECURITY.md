# Security Policy

## Reporting a vulnerability

Please report security issues **privately** via GitHub's
[private vulnerability reporting](https://github.com/RenanQueiroz/hina-agent/security/advisories/new)
(Security → Report a vulnerability). Do not open a public issue for a
suspected vulnerability.

## CI / supply-chain hardening

This project applies these controls from the start:

- **No secrets in CI.** Workflows that run on pull requests reference no
  secrets, so a malicious PR has nothing to exfiltrate.
- **Forked PRs cannot run jobs.** CI/CodeQL jobs are guarded to run only for
  pushes and same-repo pull requests.
- **Least-privilege token.** `GITHUB_TOKEN` defaults to `contents: read`; the
  repo default workflow permission is also read-only.
- **Actions pinned to commit SHAs** (with a version comment), defending against
  tag-moving supply-chain attacks. Dependabot keeps the pins current.
- **No persisted git credentials** in the workspace (`persist-credentials: false`).
- **Secret scanning + push protection** and **CodeQL** code scanning are enabled.

## Application security model

The product's threat model (sandbox isolation, per-user secret vault, the
documented unattended-Automation decryption boundary, etc.) is described in
[`plans/hina-agent-plan.md`](plans/hina-agent-plan.md) and
[`plans/research-findings.md`](plans/research-findings.md).

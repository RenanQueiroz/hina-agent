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

As of Phase 7 the per-user boundary is implemented (`internal/sandbox`,
`internal/vault`):

- **Secret vault.** Envelope encryption — a per-secret AES-256-GCM data key wraps
  each value and is itself wrapped by a local master key
  (`internal/platform`, OS keyring/DPAPI/ACL-guarded file). The encrypted blob is
  an owner-private file on disk, **not in the database**; the DB holds only
  metadata (name/description). This protects a database dump and the normal admin
  UI — it does **not** protect against a malicious host/root admin or a modified
  server binary, because unattended Automations require the running server to
  decrypt granted secrets (research-findings C5). A granted secret is forwarded
  into one run via the `sbx` **process environment**, never on the command line
  (so its value never appears in `ps`/process accounting/command logs), and its
  exact value is redacted from captured output **before** anything is written to
  disk, from audit logs, and from model-visible tool results. Because Hina cannot
  gate a raw `shell` command's network egress, a granted secret is injected into a
  run **only** when the operator sets `[sandbox] network_isolated = true` (an
  explicit assertion that the `sbx` container's egress is locked down) — otherwise
  no secret enters the sandbox, so it can't be exfiltrated. This is **fail closed by
  default**. Redaction is
  best-effort against *accidental* echo: a tool the user has deliberately granted a
  secret to can still transform it (base64, reverse, split) to evade redaction —
  granting a secret means trusting that tool with it, exactly as it would on the
  host. The protection is against snooping the DB/admin-UI/logs, not against a tool
  the user authorized. As an additional guard, a tool call is **refused** (fail
  closed) if any of its arguments contains a vaulted secret value, so a secret can
  never appear on the host `sbx` process command line; and if the current secret set
  can't be loaded to build the run redactor, the run is refused rather than run with
  a stale one.
- **Sandboxed tool execution.** A model-requested tool runs inside the calling
  user's Docker `sbx` sandbox (a **pinned, smoke-tested** version — a drifted CLI
  fails closed) with that user's Sandbox Environment policy: an allow-listed tool
  set, a network allow-list **enforced at request time** for network-explicit tools
  (`http_fetch` can only target a `host:port` on the user's list). A general `shell`
  command is not host-gated by Hina — its egress is bounded only by `sbx`'s own
  container policy, so the operator must run `sbx` in a default-deny mode
  (Balanced/Locked-Down); Hina does **not** mutate the host-global `sbx policy` per
  run (that would leak grants across users). Per-container per-user egress
  enforcement is the host-inference gateway's job (Phase 8/11). Plus per-run
  resource limits + timeout
  (process-tree kill), per-user serialized runs with a workspace quota, an approval
  gate (the default `always` mode renders an in-chat approval card), and an audit
  log. Tool arguments are typed and the server owns process invocation — a model
  never builds a raw host command line, and host filesystem/env/other users' data
  never reach a model response. The vault + sandbox tools are gated off on Windows
  until Phase 12 validates owner-only ACL/DPAPI enforcement.

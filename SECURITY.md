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

As of Phase 8 the per-user boundary (`internal/sandbox`, `internal/vault`) and the
callable-agent layer (`internal/agentcli`, `internal/agentauth`) are implemented:

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
- **Callable agents (auth broker + agent-state).** A user authenticates a coding-agent
  CLI (Codex/Claude/Cursor) through the web UI — an interactive login streamed from a
  short-lived `sbx` auth container, or an API key/token. The resulting credential
  material (a browser/subscription credential store, or the key) is treated as **the
  same secret material as a vaulted secret**: envelope-encrypted **agent-state**, an
  owner-private file on disk, **never in the database**, mounted/injected only into
  that one user's containers, and re-encrypted after a run (tokens refresh). The DB
  holds only a metadata `agent_profiles` row recording the auth-profile **type**
  (`browser_state`/`api_key`/`oauth_token`/`local_llamacpp`) — never a token, URL, or
  device code; the streamed login view is sanitized and the admin UI shows only a
  coarse status. A model-requested `agent.<provider>.run` reuses the Phase 7 boundary
  (per-user lock, approval, output/audit redaction over the injected key, workspace
  quota, audit log) and runs the agent **headlessly inside `sbx`** — never on the host
  — with the agent-state archived/extracted through a **tar-slip-hardened** archiver
  (regular files only, no symlinks, names that escape the target rejected, size
  capped). Because an agent run carries powerful provider credentials **and** needs
  network egress to its provider (which Hina cannot gate per-container yet — that is
  the host-inference gateway's job, Phase 8/11), agent runs are **refused unless
  `[sandbox] network_isolated = true`** (the operator's assertion that the container's
  egress is controlled). This is **fail closed by default**. The prompt and other
  typed arguments are passed argv-first (no shell) and a run is refused if any argument
  carries a vaulted secret value (it would appear on the host `sbx` command line). As
  with raw tools, granting an agent a credential means trusting that agent with it.
  **Pi** is the local-only, account-free agent: it targets only Hina's host-inference
  proxy (never a cloud provider) and stays disabled until Phase 11 provides that
  endpoint. The whole callable-agent layer is gated off on Windows until Phase 12.

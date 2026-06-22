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

As of Phase 9 the per-user boundary (`internal/sandbox`, `internal/vault`), the
callable-agent layer (`internal/agentcli`, `internal/agentauth`), and the Automations
layer (`internal/automation`, `internal/autorun`) are implemented:

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
- **Automations (unattended scheduled runs).** A user-owned `automation.v1` workflow
  runs **unattended** while the server is up, so its confirmation gates are different
  from interactive use: (1) **mandatory human review before `enabled` flips true** —
  every automation is created and updated **disabled** (and a **manual run is refused
  while disabled** — the review gate can't be skipped via the Run button), and an enable runs an
  **eligibility** check (each tool is in the profile's allow-list, each `agent_cli`
  adapter is granted + authenticated + server-allowed, each `secret_ref` exists, and
  any agent/secret use requires `network_isolated`) — and that **same eligibility check
  is re-run immediately before every scheduled fire**, so a run whose dependency went
  away since enable (a deleted secret, a de-authenticated agent, a flipped gate) is
  recorded as failed instead of executing under a stale posture; (2) the automation's **own
  sandbox permission profile** — the run is bound by *that*, not the owner's
  interactive chat policy. There is **no per-step approval card** at run time (no human
  is present at 03:00); instead every step is recorded in an **immutable run record**
  and tool/agent runs execute auto-approved but fully audited. Deterministic tool steps
  are built **argv-first** by typed adapters (`github.*`/`http.request`/`shell.exec`; a
  shell *string* needs an unrestricted profile) and run through the same `sbx` Runner
  with granted-secret **redaction** over captured output and the same
  **`network_isolated` fail-closed gate** on secret injection. That gate also covers **any
  network-capable deterministic tool** (`http.request`/`github.*`/`shell.exec`) **and every
  `llm` step** (an outward prompt+inputs flow to a possibly-cloud provider): such a step
  is refused — at enable-time eligibility *and* run time — unless `network_isolated=true`. An
  `llm` step additionally **refuses to run if its prompt or resolved inputs contain a vaulted
  secret value** (the same fail-closed check a tool/agent gets on its arguments): the provider
  call is server-side and outside the `sbx` egress gate, so a credential must never be
  transmitted to a (possibly cloud) model. Separately, the typed `http.request` tool is
  **SSRF-guarded** by refusing a
  target host that is a loopback, link-local, cloud-metadata (`169.254.169.254`),
  unspecified, or private-range address — including the legacy `inet_aton` numeric forms
  (decimal/hex/octal/shortened) `getaddrinfo` accepts — or `localhost`, in **every** profile
  mode. A LITERAL bad/internal URL is additionally rejected at **enable-time** validation
  (early), so a hardcoded one never reaches a run. The guard deliberately does **not** resolve
  DNS hostnames (a rebinding TOCTOU — a name that passes a pre-resolution check can re-resolve to
  an internal address before the sandboxed `curl` connects), so a hostname that resolves to an
  internal address is **not** contained in-process. Containing it is a **hard `sbx`-host
  prerequisite**: the locked-down container egress that `network_isolated` asserts (and which the
  fail-closed gate REQUIRES before any networked tool runs) must block private/link-local/
  loopback/metadata destinations. That egress policy — and a test proving it blocks a
  DNS-to-internal target — is part of the **deferred `sbx`-host validation** (the same host on
  which the real container/CLI runs are exercised), not the CGo-free control-plane build. So
  an unattended automation can't be turned into a server-side probe of internal services (a
  raw `shell` string's egress isn't parseable here and likewise stays bounded by the `sbx`
  container's own egress policy). The typed `github.*` PR tools additionally **pin the repo to
  a bare `owner/repo`** (no host prefix, URL, or extra path), so a user-controlled repo string
  can't route `gh`/`git` to a GHES/arbitrary/internal host outside the declared GitHub target.
  A run that survives a definition change between being claimed and
  executing is detected by a **monotonic generation counter** and finalized cancelled, so
  a stale scheduled occurrence never runs an edited/re-enabled definition off-schedule; `agent_cli` steps run
  through the Phase 8 `AgentRouter` (`HandleAutomation`), inheriting its full
  credential/redaction/audit/`network_isolated`/provider-allow-list/lock/quota boundary —
  but bound by the automation's **own** `sandbox` profile + `agent_auth_refs`, **not** the
  owner's *interactive* Sandbox Environment tool allow-list (so a scheduled agent run never
  forces the user to widen their chat trust boundary; the interactive `Handle` path still
  honors that policy) — and **auto-approved** (unattended) in the
  automation's **own ephemeral run scratch** (never the owner's durable workspace;
  an empty workspace is refused, not silently downgraded), at a `workspace_from`
  workdir validated to stay under `/workspace`, and capped by the automation's
  `sandbox.resources` (not the broader server agent limits). Per-run **budgets** (wall time, model calls, agent runs, tool calls, log/artifact
  bytes) are clamped to the server `[automations]` ceilings, and **fan-out is bounded** at
  every level — within a run by `max_parallelism` (concurrent leaf steps) and across the
  service by `max_concurrent_runs` + `max_runs_per_user` — so neither one runaway automation
  nor many due automations can exhaust the host/sbx. Standing scheduler load is bounded too: a
  per-user `max_enabled_per_user` admission cap limits how many automations one user may have
  enabled, and the scheduler's per-tick query excludes **manual** automations (scalar
  `trigger`/`pending_fire` columns), so a user enabling many manual workflows can't make the
  server reload + parse their definitions every tick. **Disk** (which the CPU/mem/PID/timeout
  limits don't bound) is watched by a per-run scratch watchdog: it kills a run that exceeds
  `max_workspace_mb` (the visible scratch) or drives the scratch filesystem below `min_free_mb`
  free — the latter is a `statfs` guard that accounts blocks held by open-but-unlinked files a
  directory walk can't see (killing the run frees them), polled **more frequently** than the
  expensive per-run du-walk (it is cheap and is the cross-tenant backstop), with the run timeout
  + post-run scratch removal bounding the worst case. The watchdog is a **best-effort backstop**,
  not a hard boundary: a write-then-delete that completes entirely between polls can transiently
  consume space. The **hard per-run disk quota** — which closes that sub-poll race — is a
  quota-capable scratch filesystem on the `sbx` host (an XFS/ext4 project quota or a size-limited
  volume backing the scratch root, where the write itself fails at the cap); provisioning and
  validating it is part of the deferred `sbx`-host validation (like the rest of Phase 9's real
  container enforcement). An `agent_cli` step's read-write credential/staging directory lives
  in a SEPARATE per-run scratch (never mounted at `/workspace`, so a sibling/parallel step can't
  read another step's agent credential store) that the same watchdog ALSO sizes — so the agent's
  disk use counts against `max_workspace_mb` instead of escaping it. Promoted **artifacts** are size-capped, redacted, and written owner-private,
  and an artifact download is scoped to the owner and confirmed to stay inside the
  artifact root. The decrypt-to-run boundary from C5 applies: an unattended run needs
  the server to decrypt the owner's granted secrets + mount agent-state, so they are
  hidden from the DB/admin-UI but **not** from a malicious host/root. The scheduler is
  **server-up-only** — a fire missed while the server was down defaults to `skip` (no
  surprise backfill), and on shutdown every in-flight run is cancelled + finalized so
  nothing lingers. Run records are an **append-only audit surface**: each deterministic
  tool step writes a durable `sandbox_runs` row (pending before the side effect, finalized
  after — so a crash mid-command still leaves evidence), and **deleting** an automation
  *soft-deletes* it (hidden from the owner, scheduling stopped) rather than cascade-erasing
  its run/artifact history. `mcp.call` validates but is unavailable in this build. Live `gh`/
  agent container runs are validated on an `sbx`-equipped host.

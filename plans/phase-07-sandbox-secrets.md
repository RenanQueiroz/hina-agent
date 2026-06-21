# Phase 7 — Docker `sbx` runner + per-user secret vault + Sandbox Environment

Status: **implemented** (Linux/macOS). The runner, vault, workspace manager, policy
model, tool Router (approval + audit), config/doctor/wire/API surface, and web UI are
built and tested; the `sbx` command-line/policy/redaction logic is covered against a
fake `sbx` shim. A real `sbx` round-trip (a shell tool actually isolated in a Docker
sandbox, container-level default-deny network) is validated on a host with `sbx`
installed, and on Windows in Phase 12. See **Implementation status** below.
Depends on: Phase 1 (platform, persistence, events), Phase 2 (chat UI + agent loop hook).
Unblocks: Phase 8 (callable agents run inside `sbx`), Phase 9 (Automations execute in sandboxes).

## Goal

Establish the **per-user security boundary** that every user-scoped side effect flows through. A main-session tool call (shell/file/HTTP/MCP) requested by the model runs inside that user's Docker `sbx` sandbox with explicit grants, resource limits, and audit logs — never on the host. Plus the **per-user encrypted secret vault** and the **Sandbox Environment settings** surface. This is the foundation the main plan insists applies to chat tools, not just Automations.

`sbx` facts + pinning guidance: [`research-findings.md` B6](research-findings.md#b6-docker-sbx-production-fit--green-pin-the-version). Threat model: [`research-findings.md` C5](research-findings.md#part-c--deferred-does-not-block-starting-validated-in-phase).

## Scope

### In
1. **`sbx` runner abstraction** (Go): wrap a **pinned** `sbx` version; generate one admin-controlled kit/template; `sbx run`/`exec` with per-run workspace mounts (`:ro` where read-only), `--cpus`/`--memory`/pids/timeout limits; `host.docker.internal` host-service access **only** via explicit `sbx policy allow network localhost:<port>` allow-list; `--clone` for repo work; artifact extraction (`sbx cp`); background janitor for stale sandboxes/volumes/scratch. Log every invocation (command, user, session, sandbox id, kit, exit, timings, policy decisions, output paths). Gate `sbx` upgrades behind a command-line smoke test (the CLI drifts).
2. **Persistent vs ephemeral storage**: durable per-user workspace (app-managed `sbx` mounts/volumes), optional durable per-session workspace, ephemeral run scratch; root filesystems recreated from the kit (not mutated). Quotas + retention + export/delete + admin usage visibility (without exposing contents/secrets).
3. **Per-user secret vault**: envelope encryption (random per-secret data key wrapped by a local master key); master key via OS keyring/**DPAPI**/ACL-guarded `0600` file through `internal/platform` (Phase 1 hook). Store only metadata in the DB (`secrets_meta`); never values in admin UI/logs/exports. Inject as temp env vars or mounted files for one run; remove on exit. `sbx secret` is an allowed injection backend for its supported services, but the app vault stays source of truth (arbitrary/custom secrets, grants, export redaction, non-disclosure).
4. **Main-model tool execution through `sbx`**: connect the Phase 6/2 agent-loop tool hook to the runner — shell/file/HTTP/MCP tool calls execute in the calling user's sandbox with the user-visible **approval flow** (approval cards per admin policy). No raw host filesystem/env/other-users'-data ever reaches a model response.
5. **Sandbox Environment settings (UI + API)**: per-user config for allowed CLI tools, MCP servers, default network policy, default writable mounts, secret/env grants — independent of any one session/Automation. (Agent auth profiles are Phase 8; Pi availability is Phase 8.)

### Explicitly out (deferred)
- Agent auth broker + Codex/Claude/Cursor/Pi adapters (Phase 8).
- Automations (Phase 9).
- **`sbx`-on-Windows hands-on validation** (install, Hypervisor Platform, mounts with spaces/Unicode, `sbx cp`, policy, secret injection, `host.docker.internal`) → Phase 12. Build the runner cross-platform now; on Windows `hina doctor` reports `sbx` status and marks sandbox-dependent features per its availability.

## Windows posture
The runner is written against the same `host.docker.internal` + policy semantics on all OSes. Windows `sbx` needs Win11 x64 + Hypervisor Platform + `winget install Docker.sbx`; that whole path is validated in Phase 12. Master-key DPAPI vs ACL-file storage is coded now (Phase 1 `internal/platform` hook) and validated in Phase 12.

## Testable exit criteria (Linux/macOS this phase; Windows in Phase 12)
- [ ] A model-requested shell tool in a chat runs inside the user's `sbx` sandbox, returns output, and is audit-logged; it cannot read host files, host env, or another user's workspace.
- [ ] The Router enforces the per-user network allow-list at REQUEST time for network-explicit tools (`http_fetch`): a `host:port` not on the user's list is rejected before the run. The sandbox **container's** egress (e.g. a raw `shell` `curl`) is default-deny via `sbx`'s own policy (operator runs Balanced/Locked-Down) — Hina does **not** mutate the host-global `sbx policy` per run (that leaks grants across users); per-user container-level egress grants are the host-inference gateway (Phase 8/11). The exit criterion's full per-tool container enforcement is validated with a real `sbx` there.
- [ ] A secret stored in the vault is injectable into one run as an env var/file, absent afterward, and never appears in admin UI/logs; DB-only inspection shows no plaintext.
- [ ] Durable per-user workspace survives a server restart and container teardown; ephemeral run scratch is cleaned by the janitor.
- [ ] Sandbox Environment settings persist per user and constrain what that user's sandboxes can do.
- [ ] `sbx` version is pinned; the upgrade smoke test catches a deliberately broken command line.

## Implementation status

Delivered and tested this phase (Linux/macOS, `go test ./...` + web vitest):
- **`internal/vault`** — envelope encryption (per-secret AES-256-GCM data key wrapped by the `internal/platform` master key), owner-private on-disk blobs (never in the DB — a metadata-only `secrets_meta` row + a separate encrypted file), run-scoped env injection, and a value redactor. Tests cover round-trip, no-plaintext-in-DB, cross-user isolation, wrong-key failure, and redaction.
- **`internal/sandbox`** — `CLIRunner` (resolve + **fail-closed pin-check** of `sbx`, build `run` argv, execute via `internal/platform` with process-tree kill + per-run timeout). Granted secrets are forwarded through the `sbx` **process environment** + name-only `--env <NAME>` flags — **never as a value on the argv** — and captured output is **redacted before it is written to disk**. `WorkspaceManager` (durable per-user/per-session + ephemeral scratch + janitor + quota); `Environment` policy (validate/normalize/enforce); and the tool `Router` (policy check → secret injection → approval gate → **per-user-serialized** sandbox run + quota recheck → redaction → audit + events). Tested against a **fake `sbx` shim** + a stub runner: argv construction, secret-not-on-argv + env passthrough + redact-before-disk, request-time network allow-list, timeout/KillTree, path-escape rejection, version-mismatch fail-closed, approval allow/deny, audit rows.
- **`internal/agent` + `internal/llm`** — the shared loop runs **tool-call rounds** (round-capped); the mock provider's `/sh` trigger drives the full path. Existing text/voice behavior is unchanged when no tool calls are emitted.
- **API + UI** — `[sandbox]` config, `sbx`/sandbox-tools `doctor` lines (runs the smoke test when available), wire DTOs + generated TS, `/sandbox/environment` + `/sandbox/secrets` + tool-approval + `/admin/sandbox` handlers, the agent loop wired with the per-turn-scoped tool hook, the **Sandbox** web page (secrets + policy editor), an in-chat **approval card** (approve/deny → decide endpoint), and the admin sandbox panel. End-to-end HTTP tests drive `/sh` → auto-run, and `/sh` → `approval="always"` → user approves → run → audit.

Network model + boundary notes:
- The network allow-list is enforced at **request time** by the Router (a tool can only target a `host:port` on the user's list). Hina does **not** mutate the host-global `sbx policy` per run — that would leak grants across users/runs — so the container's actual egress is governed by `sbx`'s own default-deny policy (Balanced/Locked-Down). Per-run container-level egress grants are the host-inference gateway's job (Phase 8/11).
- **Secret injection is fail-closed on network isolation.** Hina can't gate a raw `shell` command's egress (only `http_fetch` is request-checked), so a granted secret is injected into a run **only** when the operator sets `[sandbox] network_isolated = true` (asserting the `sbx` container's egress is locked down). Default false ⇒ no secret ever enters the sandbox, closing the exfil-via-shell class. The workspace-quota preflight also fails **closed** on a scan error (it refuses the run rather than allowing unbounded growth); a hard per-run size cap still needs filesystem/volume quotas (deferred).
- **Windows:** the vault + sandbox tools are gated off on Windows (`hina doctor` says so), because owner-only ACL/DPAPI enforcement in `internal/platform` is a Phase 12 no-op — storing secrets there would not be protected. The vault master key is **not even created** on Windows (`ensureMasterKey` skips it) so an unprotected key never lands on disk before Phase 12 secures it.
- **Output redaction is best-effort.** The runner scrubs the *exact* granted secret values from captured output before persisting. A tool the user has deliberately granted a secret to can still transform it (base64/reverse/split) to evade redaction — granting a secret is trusting that tool with it (research-findings C5). Redaction defends against accidental echo into logs/UI, not against an authorized tool exfiltrating.

Validated on a host with a real `sbx` (and on Windows in Phase 12), not in this CGo-free/Docker-less CI:
- A shell tool actually isolated from host files/env inside the Docker sandbox; `sbx`'s container-level default-deny egress; `sbx cp`/`--clone`; the upgrade smoke test (`CLIRunner.Smoke`) against the installed `sbx` (run at server startup + by `hina doctor`). Until then `hina doctor` reports `sbx` missing/unavailable and tool calls return "sandbox unavailable", exactly like the local-voice ONNX gate.

Scope notes:
- Built-in tools are `shell`, `fs_read`, `fs_write`, `http_fetch` (all sandbox-executed). MCP servers are stored/validated in the Environment policy; routing an MCP tool call through the sandbox network allow-list lands with the callable-agent work (Phase 8).
- Only the credential-free **mock** provider emits tool calls so far (the `/sh` trigger); wiring a tool-capable cloud/`openai-compat` provider to emit function calls is a provider concern that plugs into the same loop/Router seam later. The execution **boundary** (the deliverable) is complete.

## Risks & mitigations
- **`sbx` velocity/breaking changes** → pin + command-line smoke test in `hina doctor`/CI; treat `sbx kit` (Early Access) as least stable.
- **Operational complexity** (stale sandboxes, storage growth, policy) → janitor + quotas + admin visibility from the start.
- **Host-inference bridge becoming broad host access** → the managed llama.cpp endpoint (Phase 11) reachable only via a path-filtered host-inference proxy (the `/v1` API, not the raw `llama-server` port) behind the explicit allow-list; test other host services — and llama-server's own control endpoints — are unreachable.
- **Vault not protecting against malicious host/root** → documented boundary (C5); envelope encryption protects DB-compromise + normal admin UI, not a modified server binary.

## References
- `sbx` CLI + pinning + secrets model: [`research-findings.md`](research-findings.md) B6, C5.
- Sandbox ownership + storage + security design: `hina-agent-plan.md` (Backend And Sandbox Ownership, User Sandbox Environment, Security And LAN Access).

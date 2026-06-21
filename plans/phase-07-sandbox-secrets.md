# Phase 7 — Docker `sbx` runner + per-user secret vault + Sandbox Environment

Status: ready after Phase 2 (parallel to the voice track).
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
- [ ] Network is default-deny for shell/code tools; a host service (e.g. a local port) is reachable only after an explicit allow-list entry; other host services remain unreachable.
- [ ] A secret stored in the vault is injectable into one run as an env var/file, absent afterward, and never appears in admin UI/logs; DB-only inspection shows no plaintext.
- [ ] Durable per-user workspace survives a server restart and container teardown; ephemeral run scratch is cleaned by the janitor.
- [ ] Sandbox Environment settings persist per user and constrain what that user's sandboxes can do.
- [ ] `sbx` version is pinned; the upgrade smoke test catches a deliberately broken command line.

## Risks & mitigations
- **`sbx` velocity/breaking changes** → pin + command-line smoke test in `hina doctor`/CI; treat `sbx kit` (Early Access) as least stable.
- **Operational complexity** (stale sandboxes, storage growth, policy) → janitor + quotas + admin visibility from the start.
- **Host-inference bridge becoming broad host access** → the managed llama.cpp endpoint (Phase 11) reachable only via a path-filtered host-inference proxy (the `/v1` API, not the raw `llama-server` port) behind the explicit allow-list; test other host services — and llama-server's own control endpoints — are unreachable.
- **Vault not protecting against malicious host/root** → documented boundary (C5); envelope encryption protects DB-compromise + normal admin UI, not a modified server binary.

## References
- `sbx` CLI + pinning + secrets model: [`research-findings.md`](research-findings.md) B6, C5.
- Sandbox ownership + storage + security design: `hina-agent-plan.md` (Backend And Sandbox Ownership, User Sandbox Environment, Security And LAN Access).

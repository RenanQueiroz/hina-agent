# Phase 1 — Foundation (server skeleton, platform, persistence, auth v0, CI)

Status: ready to start. No prior phases.
Depends on: nothing.
Unblocks: every other phase.

## Goal

Stand up the smallest possible Go server that is **multi-user-shaped, cross-platform-shaped, and event-shaped from the first commit**, so that nothing later has to be retrofitted for Windows, multi-user auth, or the event model. At the end of Phase 1 the product does nothing a user would call "voice agent" yet — but the skeleton it leaves behind is the one every later phase bolts onto.

This phase closes the "Clarify before first code" items from the main plan (product identity, auth/session v0, persistence schema v0, event/API wire contracts v0). The concrete decisions are in [`research-findings.md`](research-findings.md); this doc is the build plan.

## Product identity (locked for V2)

| Thing | Value | Notes |
|---|---|---|
| Product / agent name | **Hina** | Matches the default spoken name `[agent].name`. |
| CLI / binary | `hina` | Single multi-command binary: `hina server`, `hina setup`, `hina doctor`, `hina migrate`, `hina runtime …`, `hina bench`. |
| Go module path | `github.com/RenanQueiroz/hina-agent` | Confirmed 2026-06-18. Go preserves the case, so use this exact casing in `go.mod` and imports. |
| Config dir | `hina` via `os.UserConfigDir()` | Linux `~/.config/hina`, macOS `~/Library/Application Support/hina`, Windows `%AppData%\hina`. |
| Cache dir | `hina` via `os.UserCacheDir()` | model/runtime downloads live here, never repo-relative. |
| Data dir | platform data dir + `hina` | SQLite DB, secret-vault material, per-user workspaces, run records. |
| State/runtime dir | temp/runtime dir + `hina` | sockets, per-run scratch, pid/lock files. |
| Service name | `hina` | for later OS service install. |

Rationale: V1 (`/home/renan/voice-agent`) keeps all config/state repo-relative and is single-user. V2 must not — see the V1 reference for why (`onnx-asr` model cache under `./.cache`, `preferences.toml` at repo root, etc.). V2 uses OS-standard dirs through `internal/platform` so the same binary behaves on every host and for every user.

## Scope

### In
1. **`internal/platform`** — the OS-abstraction package that everything else imports instead of calling `os`/`os/exec`/`syscall` directly.
2. **Config loader** — typed config from file + env, validated, cross-platform paths.
3. **Persistence** — SQLite (`modernc.org/sqlite`, CGo-free) + a migration tool, with the v0 schema (table boundaries only — not every column).
4. **Auth/session v0** — first-run admin bootstrap, Argon2id password hashing, session storage, admin/user roles, localhost-only by default with an explicit LAN gate.
5. **Event schema + event bus** — the typed event envelope and an in-process pub/sub with per-session sequence numbers, plus a server→client event stream endpoint.
6. **HTTP server + wire contracts v0** — route shape, middleware (auth, request IDs, structured logging), health/readiness endpoints, generated TypeScript types for the event envelope and core DTOs.
7. **`hina setup` / `hina doctor`** — cross-platform capability detection and app-dir creation.
8. **CI** — build + test + smoke on Windows 11 x64, macOS Apple Silicon (or Intel runner as interim), Linux x86_64; a non-GPU/no-model smoke path.

### Explicitly out (deferred)
- Any web UI beyond a static placeholder + a JSON "it works" page. (Phase 2.)
- Any LLM/STT/TTS call, audio, or WebRTC. (Phases 2–6.)
- Docker `sbx`, secret vault material beyond the master-key plumbing stub, Automations, agent auth. (Phases 7–9.)
- Real Windows *testing*. We **build** for Windows from day 1 (CI cross-compiles + runs the smoke test on a Windows runner), but per the user's direction we do not block on hands-on Windows validation; the dedicated Windows hardening pass is its own later phase.

## Windows posture for this phase

Build cross-platform now, validate hands-on later. Concretely, in Phase 1 that means:
- `internal/platform` ships a Windows implementation file for every OS-specific function from the start (even if some are thin/TODO-marked), guarded by `_windows.go` / `_unix.go` build-tag files — never scattered `runtime.GOOS` checks.
- CI compiles `GOOS=windows GOARCH=amd64` and runs the server-startup + migration + doctor smoke test on a `windows-latest` runner.
- No `modernc.org/sqlite` CGo, no MinGW required for the control plane (the whole point of choosing it).
- Anything that genuinely needs a Windows host to verify (Job Objects process-tree kill, DPAPI key storage, path-translation fixtures) is *stubbed with a correct-looking implementation now* and flagged for the Windows hardening phase, not faked.

## Work breakdown

### 1. `internal/platform` (the keystone)
Create the package the rest of the codebase routes through. Functions, each with `_unix.go` + `_windows.go` implementations:
- **Paths**: `ConfigDir()`, `CacheDir()`, `DataDir()`, `RuntimeDir()`, `LogDir()` — all rooted at the OS-standard location + `hina`. Never `~`, never repo-relative.
- **Permissions**: `EnsurePrivateDir(path)` and `EnsurePrivateFile(path)` — `0700`/`0600` on Unix; ACL-tightened on Windows (stub the ACL call now, mark for hardening). `IsPermissionSafe(path)` that fails closed on Unix if a key file is group/world-readable.
- **Process supervision**: `Command(ctx, argv...)` that returns a handle whose `KillTree()` does process-group kill on Unix (`setpgid` + negative-PID signal) and **Windows Job Object** termination on Windows. `exec.CommandContext` alone is insufficient for child processes spawned by model servers — establish this primitive now even though no model server exists yet (a test child that spawns a grandchild proves it).
- **Secret storage hooks**: `StoreMasterKey()` / `LoadMasterKey()` interface with an OS-keyring/DPAPI Windows impl and a `0600` key-file Unix impl. Phase 1 only needs the interface + the Unix file impl working; the vault that uses it arrives in Phase 7.
- **Signals**: `NotifyShutdown(ctx)` that handles `os.Interrupt`/SIGTERM on Unix and the Windows console-control equivalent.

Test: a table test that spawns a child-with-grandchild and asserts `KillTree()` leaves no orphans, run on each CI OS.

### 2. Config
- TOML or JSON config file at `ConfigDir()/config.toml`; env overrides (`HINA_*`). Keep V1's "env > file" precedence.
- First sections: `[server]` (host, port, lan_enabled, tls_cert/key), `[agent]` (name=`Hina`, name_aliases), `[paths]` overrides, `[log]` level/format.
- Validation with clear errors; `hina doctor` surfaces them.
- Generate the matching TypeScript type for any config the admin UI will edit later (don't hand-maintain two copies).

### 3. Persistence (SQLite + migrations)
- Driver: `modernc.org/sqlite` (CGo-free — keeps Windows builds compiler-free). WAL mode, busy timeout, foreign keys on.
- Migration tool: pick one (`golang-migrate` or `goose`); embed migrations via `embed.FS` so the binary is self-contained. `hina migrate` runs them; server runs them on startup behind a version check.
- **v0 schema — table boundaries only** (columns fleshed out as phases need them, but the boundaries are drawn now so early routes don't invent ad-hoc JSON blobs that fight the event model later):
  - `users` (id, role, password_hash, created/updated, status)
  - `sessions_auth` (browser/login sessions: token hash, user_id, expiry, …) — distinct from conversation sessions
  - `conversations` (the durable chat/session: id, owner_user_id, title, created/updated) — "Session" in the product sense
  - `turns` (id, conversation_id, role, canonical_text, mode[text|voice], created, metadata JSON)
  - `events` (append-only: event_id, conversation_id, user_id, turn_id, seq, source, type, payload JSON, server_ts) — the persisted event log behind replay/reconnect
  - `runtime_state` (which backends/models are loaded; admin-owned)
  - placeholders created empty now, populated in their phase: `automations`, `automation_runs`, `automation_artifacts`, `sandbox_state`, `secrets_meta`, `agent_auth_state`
- Test: migrate up/down on all three OSes in CI; round-trip insert/select a conversation+turn+event.

### 4. Auth/session v0
- **First-run bootstrap**: on first start with no admin, `hina setup` (or the server) generates an admin bootstrap credential and prints it once; require changing it before LAN mode is allowed.
- **Password hashing**: Argon2id (`golang.org/x/crypto/argon2`), never reversible encryption. Tunable params in config.
- **Login sessions**: secure, httpOnly cookies (or short-lived bearer tokens) stored hashed in `sessions_auth`. CSRF protection for cookie flows.
- **Roles**: `admin` and `user`; middleware `RequireUser` / `RequireAdmin`.
- **LAN gate**: bind `127.0.0.1` by default; `--host 0.0.0.0` (or `[server].lan_enabled`) refuses to start until the bootstrap credential has been changed. LAN clients still authenticate — no "trusted network."
- Test: bootstrap → change password → login → hit an authed route → admin-only route rejects a `user`.

### 5. Event schema + bus
- Define the typed event envelope once (Go struct + generated TS): `event_id`, `session_id` (conversation), `user_id?`, `turn_id?`, monotonic `server_ts`, per-session `seq`, `source` (`client|server|runtime|sandbox|openai_realtime|…`), `type`, `payload`.
- Implement the core lifecycle subset now: `SessionCreated`, `SessionResumed`, `UserTextSubmitted`, `TurnStarted`, `TurnCommitted`, `AgentTextDelta`, `AgentTextCompleted`, `ErrorEvent`. (Audio/tool/automation/runtime event types are declared in the schema but emitted in their phases.)
- In-process pub/sub bus: subscribe per conversation and per admin scope; assign `seq` atomically; persist to `events`.
- **Replay/reconnect**: a client reconnecting passes its last `seq`; the server replays from `events`. Decide and document this now so the UI transport in Phase 2 builds on it.
- Server→client transport: SSE for the user/admin event streams initially (simple, reconnect-friendly); the `RTCDataChannel` envelope in Phase 3 reuses the *same* event types.

### 6. HTTP server + wire contracts v0
- Router (`net/http` + a light mux, or chi). Middleware: request ID, structured logging, panic recovery, auth.
- Route shape decided now (versioned, e.g. `/api/v1/…`): auth, conversations, turns, events stream, health. Document the envelope + DTOs and **generate TS types** (e.g. via `tygo` or hand-authored zod schemas kept in one place) so frontend/backend can't drift.
- Health: `/healthz` (process up) and `/readyz` (migrations done, config valid).

### 7. `hina setup` / `hina doctor`
- `hina setup`: create app dirs (with correct perms), run migrations, create the admin bootstrap, write a default config if absent.
- `hina doctor`: print a capability table per host — OS/arch/tier, app dirs OK, DB OK, `sbx` present?/version/logged-in?, Hypervisor Platform (Windows)?, `llama.cpp` present?, ORT DLL/lib present?, HTTPS cert present?, and a clear **feature-availability** verdict (e.g. "local voice: unavailable — Nemotron/Supertonic not installed", "Windows local voice: gated on ONNX spike"). Non-interactive mode for CI/PowerShell. This is the user's primary "what works on my machine" surface and it gets built before the features it reports on, returning "not installed / planned" until each lands.

### 8. CI
- Matrix: `windows-latest` (x64), `macos-latest` (Apple Silicon if available, else Intel — note the gap in docs), `ubuntu-latest` (x64).
- Per OS: `go build`, `go test ./...`, run migrations, run `hina doctor --json`, start the server and hit `/readyz`, shut it down cleanly (assert no orphan processes).
- Cross-compile check: `GOOS=windows/darwin/linux GOARCH=amd64/arm64` all compile.
- Keep the control-plane build **CGo-free** (`CGO_ENABLED=0`) so this stays simple; CGo is introduced only later behind build tags for the ONNX adapters.

## Testable exit criteria

- [ ] `hina server` starts on Windows 11 x64, macOS, and Linux, runs migrations, and serves `/readyz` = 200. (Windows via CI runner; hands-on Windows deferred.)
- [ ] `hina doctor` prints an accurate capability table on each OS and a clear feature-availability verdict, including "local voice unavailable (not yet implemented)".
- [ ] First-run admin bootstrap works; a user can log in; cookies/tokens are httpOnly+hashed; admin-only routes reject non-admins.
- [ ] LAN bind refuses to start until the bootstrap credential is changed.
- [ ] A conversation + turn + event round-trips through SQLite; an SSE subscriber receives a `SessionCreated` then `UserTextSubmitted` with correct monotonic `seq`; reconnect-with-last-seq replays.
- [ ] `internal/platform.KillTree()` leaves no orphaned child/grandchild processes on each CI OS.
- [ ] CI is green on all three OSes with `CGO_ENABLED=0`.
- [ ] Generated TS types for the event envelope + config + core DTOs exist and match the Go structs (a check in CI).

## Risks & mitigations
- **POSIX assumptions leaking in** → everything OS-specific goes through `internal/platform` with a Windows impl from commit 1; lint/review rule: no `os.Chmod`/`syscall`/path separators outside `internal/platform`.
- **Schema churn fighting the event model later** → draw all table boundaries now (even empty), persist events as the source of truth for replay.
- **Auth shortcuts that don't survive multi-user/LAN** → Argon2id + hashed sessions + role middleware + LAN gate are in v0, not bolted on.
- **Two sources of truth for wire types** → generate TS from Go (or share zod) and check it in CI.

## References
- Decisions + driver/library choices: [`research-findings.md`](research-findings.md).
- V1 reference for config/runtime semantics to preserve or replace: `/home/renan/voice-agent` (`README.md`, `AGENTS.md`, `config.toml`, `runtimes.py`, `servers.py`).

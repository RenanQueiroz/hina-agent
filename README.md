# Hina

A server-first, web-first, multi-user **voice and text agent** (V2). Cross-platform from the first commit — Windows 11 x64, macOS Apple Silicon, and Linux x86_64 — with local and cloud STT-LLM-TTS, Docker `sbx` sandboxing, per-user secrets, and callable-agent Automations arriving across the phased roadmap.

> **Status: Phase 1 (Foundation).** The control plane is up — server, auth, persistence, events, and the cross-platform CLI. There is no web UI or model inference yet; those land in later phases. See [`plans/roadmap.md`](plans/roadmap.md).

The full design lives in [`plans/`](plans/) — start with [`plans/roadmap.md`](plans/roadmap.md) (phase index), [`plans/hina-agent-plan.md`](plans/hina-agent-plan.md) (vision/architecture), and [`plans/research-findings.md`](plans/research-findings.md) (closed research + decisions).

## What's in Phase 1

- **`internal/platform`** — the OS abstraction (paths, private-permission enforcement, process-tree kill via process groups / Windows Job Objects, shutdown signals, master-key storage) with `_unix.go`/`_windows.go` build-tag files.
- **`internal/config`** — typed TOML config + `HINA_*` env overrides, with a LAN/loopback invariant.
- **`internal/store`** — SQLite via the **CGo-free** `modernc.org/sqlite` (keeps native Windows builds compiler-free), WAL + embedded forward-only migrations, v0 schema.
- **`internal/events`** — the typed event envelope + in-process pub/sub bus + persisted replay (the wire contract the web client and later the WebRTC data channel share).
- **`internal/auth`** — Argon2id password hashing, hashed httpOnly session cookies, `RequireUser`/`RequireAdmin`, first-run admin bootstrap, and the LAN gate.
- **`internal/httpapi`** — versioned JSON routes, middleware, `/healthz` + `/readyz`, and the SSE event stream.
- **`internal/doctor`** — host capability + feature-availability report.
- **`cmd/hina`** — the single multi-command binary.

## Quick start

Requires Go 1.26+. The control-plane build is CGo-free (`CGO_ENABLED=0`).

```bash
make build                 # -> bin/hina   (or: go build -o bin/hina ./cmd/hina)
bin/hina setup             # create app dirs, run migrations, bootstrap the admin (prints a one-time credential)
bin/hina doctor            # report host capabilities and feature availability
bin/hina server            # serve on http://127.0.0.1:8733  (loopback by default)
```

LAN binding (`--host 0.0.0.0` with `lan_enabled = true` / `HINA_SERVER_LAN=1`) is refused until the bootstrap admin password is changed. App state lives in OS-standard dirs (never repo-relative): config under `os.UserConfigDir()/hina`, data/DB under the platform data dir.

## Development

```bash
make all     # tidy + vet + test + build
make test
make cross   # prove the Windows/macOS/Linux cross-compile locally
```

CI builds and tests on Windows, macOS, and Linux, and cross-compiles every Tier-1 target. Module path: `github.com/RenanQueiroz/hina-agent`.

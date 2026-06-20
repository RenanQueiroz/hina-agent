# Hina

A server-first, web-first, multi-user **voice and text agent** (V2). Cross-platform from the first commit — Windows 11 x64, macOS Apple Silicon, and Linux x86_64 — with local and cloud STT-LLM-TTS, Docker `sbx` sandboxing, per-user secrets, and callable-agent Automations arriving across the phased roadmap.

> **Status: Phase 4 (local TTS).** Phases 1–4 are complete: the cross-platform control plane, a React/Vite web client with streaming text chat, a **pure-Go WebRTC media bridge** (Pion), and now **local text-to-speech** — a Go port of the Supertonic 3 ONNX pipeline that speaks a typed message aloud over WebRTC. The ONNX Runtime is isolated behind an `onnx` build tag (CGo), so the **default build stays CGo-free** and cross-compiles everywhere; local TTS is an opt-in build plus a one-time `hina assets pull`. The default LLM provider is a credential-free **mock**, so it runs with no setup; point `[llm]` at OpenAI or a local llama.cpp server for a real model. See [`plans/roadmap.md`](plans/roadmap.md).

The full design lives in [`plans/`](plans/) — start with [`plans/roadmap.md`](plans/roadmap.md) (phase index), [`plans/hina-agent-plan.md`](plans/hina-agent-plan.md) (vision/architecture), and [`plans/research-findings.md`](plans/research-findings.md) (closed research + decisions). If you're an AI agent (or just changing code), read [`AGENTS.md`](AGENTS.md) first.

## What's in the tree today

The control plane is **CGo-free** on purpose (no C toolchain for native Windows/macOS builds). Everything below cross-compiles to every Tier-1 target.

**Phase 1 — Foundation (control plane)**

- **`internal/platform`** — the OS abstraction (paths, private-permission enforcement, process-tree kill via process groups / Windows Job Objects, shutdown signals, master-key storage) with `_unix.go`/`_windows.go` build-tag files.
- **`internal/config`** — typed TOML config + `HINA_*` env overrides (precedence env > file > defaults), with a LAN/loopback invariant.
- **`internal/store`** — SQLite via the CGo-free `modernc.org/sqlite`, WAL + embedded forward-only migrations, typed queries over the v0 schema.
- **`internal/events`** — the typed event envelope + in-process pub/sub bus + persisted replay. The same wire shape feeds the SSE streams now and the WebRTC data channel in the voice phases.
- **`internal/auth`** — Argon2id password hashing, hashed httpOnly session cookies, `RequireUser`/`RequireAdmin`, first-run admin bootstrap, and the LAN gate.
- **`internal/httpapi`** — versioned JSON routes, middleware, `/healthz` + `/readyz`, and the SSE event stream.
- **`internal/doctor`** — host capability + per-feature availability report (`hina doctor`).
- **`internal/id`**, **`internal/logbuf`** — prefixed URL-safe IDs; in-memory log ring buffer fanned out to the admin UI.

**Phase 2 — Web shell + streaming text chat**

- **`internal/llm`** — the streaming text-mode provider abstraction: `mock` (credential-free), `openai` (cloud Responses API), and `openai-compat` (any OpenAI-compatible `/chat/completions`, e.g. local llama.cpp).
- **`internal/agent`** — builds model context from a conversation's canonical turns.
- **`internal/wire`** — the JSON DTOs exchanged with the web client; TypeScript types are generated from these (and `internal/events`) by **tygo** so the frontend and backend can't drift.
- **`web/`** — React 19 + Vite + Tailwind client: log in, create/resume conversations, type → token-by-token assistant reply persisted as canonical turns, change password, and an admin shell (users, LLM/runtime info, live log tail).

**Phase 3 — WebRTC audio loopback**

- **`internal/audio`** — streaming resample (48 kHz Opus → 16 kHz ASR / 24 kHz playback), PCM ↔ float32, a phase-continuous tone generator, and the binary audio-frame framing. No Opus *encoder* — outbound is raw 24 kHz s16 mono PCM to keep datachannel bandwidth modest.
- **`internal/rtc`** — the Pion WebRTC bridge: per-user talk session, inbound mic pipeline (RTP → Opus decode → resample → capture cursor + RFC 3550 loss/jitter), an outbound PCM pacer over an unreliable datachannel (loopback / tone sources), the typed control-event channel, a playback cursor with manual barge-in, and session metrics. Signaling mirrors OpenAI Realtime: `POST /api/v1/realtime/calls` with `application/sdp` in/out.
- **`web/`** — a PCM-player `AudioWorklet`, a `LiveSession` client (getUserMedia with echo-cancel/noise-suppress/AGC, datachannels, SDP exchange, epoch/seq frame gating, barge-in), the `/live` page, and a live-sessions panel in admin.

**Phase 4 — Local TTS (Supertonic via ONNX Runtime)**

- **`internal/onnx`** — a small, model-agnostic abstraction over ONNX Runtime shared by TTS now and ASR later: a `Backend`/`Session`/`Tensor` surface plus a reusable lazy-load + idle-unload `Lifecycle`. The real ORT binding ([`yalue/onnxruntime_go`](https://github.com/yalue/onnxruntime_go), pinned to ORT **1.26.0**) is CGo and lives behind the `onnx` build tag; the default build compiles a CGo-free stub that reports "unavailable."
- **`internal/tts`** — a faithful Go port of the Supertonic 3 pipeline: NFKD text prep + a codepoint tokenizer, a decimal-aware sentence splitter, JSON voice style-vectors, and the four ONNX graphs (duration → text-encode → 8-step flow-matching → vocoder) producing streaming 44.1 kHz speech. No phonemizer, no local HTTP TTS hop — preset voices only (no cloning).
- **`internal/assets`** — the pinned download manager: ORT 1.26.0 per-OS + the Supertonic models (HF revision-pinned) with SHA256 verification and archive extraction, driven by `hina assets`.
- **`internal/rtc` / `web/`** — a typed `SpeakText` control message and a server-driven `POST /api/v1/realtime/speak` synthesize a reply into the live session (44.1 kHz → 24 kHz, barge-in-aware); the `/live` page has a "Speak" box and admin shows the ORT version/provider/lib path + TTS load state + cold/warm latency.

## Quick start

Requires Go 1.26+ and Node 24+ (only to build the web client). The control-plane build is CGo-free (`CGO_ENABLED=0`).

```bash
npm --prefix web ci        # once: install web deps
npm --prefix web run build # build the web client into web/dist (embedded by the binary)
make build                 # -> bin/hina   (or: go build -o bin/hina ./cmd/hina)
bin/hina setup             # create app dirs, run migrations, bootstrap the admin (prints a one-time credential)
bin/hina doctor            # report host capabilities and feature availability
bin/hina server            # serve the UI + API on http://127.0.0.1:8733  (loopback by default)
```

Then open `http://127.0.0.1:8733`, log in with the bootstrap credential, and try **Chat** (text) or **Live** (mic → WebRTC loopback / tone). For frontend development with hot reload: `npm --prefix web run dev` (proxies `/api` to the Go server). `web/dist` is committed so `go build` works without a Node build; rerun the web build after changing `web/`.

**Optional: local voice (Phase 4).** Local TTS needs the `onnx`-tagged build (links ONNX Runtime via CGo) plus the model assets:

```bash
make build-onnx            # -> bin/hina with the onnx tag (needs a C compiler; CGO_ENABLED=1)
bin/hina assets pull       # download + checksum ORT 1.26.0 and the Supertonic models (~400 MB)
# enable [tts] in config.toml (enabled = true), then:
bin/hina server            # the Live page's "Speak" box now speaks replies aloud over WebRTC
```

`hina assets status` reports what's installed; `hina doctor` reports the ORT runtime + local-TTS availability. The default (CGo-free) build leaves local TTS unavailable and says so. Local voice on Windows is gated to Phase 11.

LAN binding (`--host 0.0.0.0` with `lan_enabled = true` / `HINA_SERVER_LAN=1`) is refused until the bootstrap admin password is changed. App state lives in OS-standard dirs (never repo-relative): config under `os.UserConfigDir()/hina`, data/DB under the platform data dir. Browser mic capture works on `localhost` without TLS; a second LAN device needs HTTPS with a real cert (configure `[server] tls_cert`/`tls_key`, or front it with a reverse proxy — `hina doctor` reports this).

## CLI

```
hina server     Run the server (UI + API)
hina setup      Create app dirs, run migrations, bootstrap the admin
hina doctor     Report host capabilities and feature availability (--json)
hina migrate    Apply migrations (migrate down [N|all] to roll back)
hina assets     Manage local-inference downloads (status | verify | pull)
hina version    Print version
```

## Development

```bash
make all        # tidy + vet + test + build  (default, CGo-free)
make test       # go test ./...
make cross      # prove the Windows/macOS/Linux cross-compile locally
make gen-ts     # regenerate the TypeScript wire types from the Go DTOs (tygo)
make build-onnx # build with the onnx tag (CGO_ENABLED=1; links ORT)
make test-onnx  # go test -tags onnx ./...  (model tests skip without an ORT lib)
```

Web checks: `npm --prefix web run typecheck`, `npm --prefix web run test` (vitest), `npm --prefix web run build`, `npm --prefix web run e2e` (Playwright; CI-only by default).

CI builds and tests on Windows, macOS, and Linux, cross-compiles every Tier-1 target, runs the web typecheck/unit/build, checks the generated TypeScript is in sync, and runs a Playwright e2e against the embedded binary. A separate `onnx` job builds the CGo/ORT-tagged binary on Linux + macOS and runs the model load+run tests against the pinned ONNX Runtime. CodeQL scans on top. Module path: `github.com/RenanQueiroz/hina-agent`. Security policy: [`SECURITY.md`](SECURITY.md).

# AGENTS.md

Guidance for AI coding agents (Claude, Codex, Cursor, and any other) working in this repository. Humans are welcome to read it too.

This is the **single source of truth** for how to work in this repo. `CLAUDE.md` exists only because Claude Code reads that filename; it just points here (`@AGENTS.md`). Keep all agent guidance in this file, not split across tool-specific files.

## Read this first: keep documentation in sync

**When you change the repo, update every piece of documentation the change makes stale — in the same changeset as the code.** Documentation is part of the change, not a follow-up.

Stale or incorrect documentation is **worse than no documentation**: it actively misleads the next reader (human or agent) into wrong assumptions. A change that updates behavior but leaves the docs describing the old behavior is an incomplete change.

Concretely, before you consider a change done, check whether it affects any of these and update them together:

- **`README.md`** — project status, what's in the tree, quick-start/CLI/dev commands, config surface.
- **`AGENTS.md`** (this file) — repo map, commands, invariants, conventions.
- **`plans/`** — if you complete or alter a phase's scope, reflect it (`roadmap.md` status, the relevant `phase-NN-*.md`, and `research-findings.md` when a decision changes).
- **Inline docs** — Go package doc comments, function/struct comments, and the `web/` comments next to the code you touched. The default config template in `cmd/hina/commands.go` and the generated-types contract are docs too.
- **`SECURITY.md`** — if you touch the auth/LAN/sandbox/CI security posture.

If you're unsure whether a doc is affected, check it. When you finish a task, it's reasonable to do a quick pass: "what did I change, and which docs describe that?"

## What Hina is

A server-first, web-first, **multi-user voice and text agent** (V2), cross-platform from commit 1 (Windows 11 x64, macOS Apple Silicon, Linux x86_64). One Go binary (`cmd/hina`) serves a versioned JSON/SSE API and an embedded React/Vite web client. Local **and** cloud STT-LLM-TTS, Docker `sbx` sandboxing, per-user secrets, and callable-agent Automations land across a phased roadmap.

Current state: **Phases 1–6 complete** — control plane, streaming text chat, a pure-Go WebRTC audio bridge, local TTS (Supertonic), local streaming ASR (Nemotron 3.5), and the **live voice pipeline** (Silero VAD + semantic VAD + speak-to-interrupt barge-in + echo/backchannel handling, a shared agent loop, and a benchmark harness) — all local inference via ONNX Runtime behind the `onnx` build tag. See `README.md` for the per-phase feature breakdown and `plans/roadmap.md` for what's next.

## Repository map

```
cmd/hina/            The single multi-command binary: server | setup | doctor | migrate | version
internal/
  platform/          OS abstraction (paths, perms, process-tree kill, signals, master key); _unix.go/_windows.go build-tag files
  config/            Typed TOML + HINA_* env overrides (env > file > defaults); LAN/loopback invariant
  store/             SQLite via CGo-free modernc.org/sqlite; embedded forward-only migrations; typed queries
  events/            Typed event envelope + in-process pub/sub bus + persisted replay (the durable wire contract)
  auth/              Argon2id hashing, hashed httpOnly session cookies, RequireUser/RequireAdmin, admin bootstrap, LAN gate
  httpapi/           Versioned JSON routes, middleware, /healthz + /readyz, SSE stream, realtime + admin handlers
  doctor/            Host capability + per-feature availability report
  id/                Prefixed, URL-safe random IDs
  logbuf/            In-memory log ring buffer fanned out to the admin UI
  llm/               Streaming text provider abstraction: mock | openai (Responses API) | openai-compat
  agent/             Context builder from canonical turns + the shared cancellable agent Loop (text + voice run it; tool-call hook reserved for Phase 7)
  wire/              JSON DTOs exchanged with the web client (source for generated TS)
  audio/             Resample (48k→16k/24k), PCM↔float32, tone generator, binary audio-frame framing
  rtc/               Pion WebRTC bridge: session lifecycle, inbound mic pipeline, outbound PCM pacer, control events, metrics, TTS speak, and the Phase 6 live-voice loop (live.go: VAD→ASR→agent→TTS + barge-in)
  onnx/              ONNX Runtime abstraction (Backend/Session/Tensor: f32/i64/i32) + lazy-load/idle-unload Lifecycle; ORT binding behind the `onnx` build tag, CGo-free stub by default
  tts/               Supertonic 3 TTS port: text prep + tokenizer, sentence splitter, voice vectors, the 4-graph pipeline (CGo-free; runs on internal/onnx)
  asr/               Nemotron 3.5 streaming ASR port: Go log-mel front-end + FFT, SentencePiece tokenizer, cache-aware encoder + RNNT greedy decode, name-biasing trie, wake-word strip (CGo-free; runs on internal/onnx)
  vad/               Silero VAD port: pure-Go online turn-boundary state machine + pre-roll over the shared internal/onnx runtime (CGo-free; real model behind the `onnx` tag)
  voice/             Live turn detection: OpenAI-shaped turn_detection config, semantic VAD v1, backchannel filter, echo suppression, composed into a Pipeline (pure-Go; driven by rtc + bench)
  bench/             Live-voice benchmark harness: replays labeled fixtures through the real pipeline, percentile metrics (drives `hina bench`; non-interactive on every host)
  assets/            Pinned local-inference downloads (ORT + Supertonic + Nemotron + Silero models) with SHA256 verify/extract; drives `hina assets`
web/                 React 19 + Vite + Tailwind client (embedded into the binary via web/dist)
  src/lib/*.gen.ts   Generated from internal/wire + internal/events by tygo — DO NOT EDIT by hand
plans/               Design docs: roadmap.md, hina-agent-plan.md, research-findings.md, phase-NN-*.md
.github/workflows/   ci.yml (build/test matrix, cross-compile, web, gen-check, e2e, onnx) + codeql.yml
```

## Build, test, and verify

Requires **Go 1.26+** and (for the web client) **Node 24+**. The control plane is built with `CGO_ENABLED=0` (the Makefile exports it).

Common commands:

```bash
make all      # tidy + vet + test + build (the default gate)
make build    # -> bin/hina
make test     # go test ./...
make vet
make cross    # cross-compile windows/amd64, darwin/arm64, linux/amd64 locally
make gen-ts   # regenerate web/src/lib/*.gen.ts from the Go DTOs (tygo)
make doctor   # build + run hina doctor

# local-inference build (Phases 4–5): ONNX Runtime via CGo behind the `onnx` tag
make build-onnx  # CGO_ENABLED=1 go build -tags onnx (needs a C compiler; no ORT lib at build time)
make vet-onnx
make test-onnx   # model tests skip unless ONNXRUNTIME_SHARED_LIBRARY_PATH points at an ORT 1.26.0 lib;
                 # the real Supertonic/Nemotron pipeline tests run when HINA_TTS_TEST_ASSETS /
                 # HINA_ASR_TEST_ASSETS point at an installed asset root (`hina assets pull`)

# web (run from repo root with --prefix, or cd web)
npm --prefix web ci
npm --prefix web run typecheck
npm --prefix web run test     # vitest (unit)
npm --prefix web run build    # -> web/dist (committed; the binary embeds it)
npm --prefix web run dev      # hot-reload dev server, proxies /api to :8733
npm --prefix web run e2e      # Playwright (CI-only by default; needs a running server)
```

**Before committing**, run the checks that match what you touched. For a substantive change, the full local gauntlet (this is roughly what CI enforces) is:

1. `gofmt`/`go vet` clean, `go test ./...` green.
2. `go test -race ./...` (at least the concurrency-heavy packages: `rtc`, `audio`, `httpapi`, `events`, `store`, `onnx`, `tts`, `assets`).
3. `make cross` — every Tier-1 target still compiles (Windows + macOS included).
4. If you touched anything CGo/ONNX-tagged: `make build-onnx` + `make vet-onnx`, and `make test-onnx` (provide an ORT 1.26.0 lib via `ONNXRUNTIME_SHARED_LIBRARY_PATH` to exercise the model tests rather than skip them).
5. Web: `typecheck`, `test`, `build` all green.
6. If you changed `internal/wire` or `internal/events`: `make gen-ts` and commit the regenerated `web/src/lib/*.gen.ts` (CI fails on drift).
7. Smoke: `hina migrate` up / `down all` / up, `hina doctor --json`, `hina assets status`, and `hina bench` (the live-voice turn-detection suite — non-interactive, no models).

## Project invariants (do not break these)

- **CGo-free control plane.** No `import "C"`, no dependency that needs a C toolchain, in the *default* build. The ONNX Runtime binding is the one CGo dependency, and it is strictly isolated behind the `onnx` build tag (`internal/onnx/backend_onnx.go`); the default build compiles the CGo-free stub. Never let a CGo import leak into a non-tagged file, and keep the stub in lockstep with the real backend's exported surface. This is what keeps Windows/macOS default builds compiler-free — protect it.
- **Cross-platform from day 1.** Any OS-specific primitive ships a Windows *and* Unix implementation when first written, via `internal/platform` `_windows.go`/`_unix.go` build-tag files. Features that need a Windows host to validate are *built now, validated in Phase 12* — don't delete the Windows path because you can't test it locally.
- **The wire contract is generated, not hand-written.** Edit `internal/wire` / `internal/events`, then `make gen-ts`. Never hand-edit `web/src/lib/*.gen.ts`.
- **The event envelope is fixed in one place.** All server→client events use the `internal/events` envelope; the same shape flows over SSE and the WebRTC datachannel. Add new event *types* there.
- **LAN gate.** Binding a non-loopback host requires `lan_enabled = true` *and* a changed admin password. Keep that invariant intact; it's enforced in `config`/`auth` and asserted by tests.
- **App state lives in OS-standard dirs**, never repo-relative — config under `os.UserConfigDir()/hina`, data/DB under the platform data dir. Don't write app state next to the binary or in the repo.
- **Migrations are forward-only and embedded.** Add a new numbered pair in `internal/store/migrations/`; never rewrite an applied migration.
- **Defensive security only.** This is authorized/defensive work. Preserve the CI/supply-chain controls in `SECURITY.md` (read-only token, pinned actions, no secrets on PRs, fork-PR guard).

## Workflow conventions

- **Match the surrounding code.** Mirror the existing naming, comment density, error-handling style, and file layout of the package you're in. The codebase favors small, well-commented files where the comment explains *why*, not *what*.
- **Tests live next to code** (`*_test.go`, `*.test.ts`). New behavior gets a test; bug fixes get a regression test. Keep the suite green and race-clean.
- **Commit/push only when asked.** Don't push to `main` directly unless the task explicitly calls for it; prefer a branch + PR otherwise. Write commit messages that explain intent.
- **Re-verify drift-prone external surfaces** (the `sbx` CLI, the agent CLIs, OpenAI Realtime endpoints) immediately before the phase that depends on them, per `plans/research-findings.md`.
- **Plans are living docs.** Re-plan each phase when you reach it; later phase docs are intentionally lighter because earlier phases teach us things.

## Where to start

- New to the project → `plans/roadmap.md` (phase index + dependency graph), then `plans/hina-agent-plan.md` (vision/architecture).
- Picking up the next phase → its `plans/phase-NN-*.md`, cross-checked against `plans/research-findings.md` for the locked library/version decisions.
- Changing the API or events → `internal/wire` / `internal/events`, then `make gen-ts`.
- Touching anything → re-read **"Keep documentation in sync"** above before you call it done.

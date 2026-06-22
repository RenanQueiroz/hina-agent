# Hina

A server-first, web-first, multi-user **voice and text agent** (V2). Cross-platform from the first commit — Windows 11 x64, macOS Apple Silicon, and Linux x86_64 — with local and cloud STT-LLM-TTS, Docker `sbx` sandboxing, per-user secrets, and callable-agent Automations arriving across the phased roadmap.

> **Status: Phase 9 (Automations).** Phases 1–9 are complete: the cross-platform control plane, a React/Vite web client with streaming text chat, a **pure-Go WebRTC media bridge** (Pion), **local text-to-speech** (a Go port of the Supertonic 3 ONNX pipeline), **local streaming speech-to-text** (a Go port of the Nemotron 3.5 ASR pipeline, with agent-name biasing + wake-word stripping), the **live voice pipeline** — talk to Hina locally and it talks back, with **Silero VAD** turn detection, a **semantic VAD** that waits out "umm…", **speak-to-interrupt barge-in**, backchannel + echo handling, a **shared agent loop** both text and voice run, and a non-interactive **benchmark harness** (`hina bench`) — the **per-user security boundary**: when the agent calls a tool (shell / file / HTTP) it runs inside that user's **Docker `sbx` sandbox** with that user's policy, an **approval card**, resource limits, and an audit log — never on the host; plus a **per-user encrypted secret vault** (envelope encryption; values never touch the database) and a **Sandbox Environment** settings surface — and now the **callable-agent layer**: a user **authenticates a coding-agent CLI** (Codex / Claude / Cursor) through the web UI — an interactive **browser/device login** streamed from a short-lived `sbx` auth container, or an **API-key/token** profile — its credential store is kept as **encrypted agent-state** in the same vault boundary, and the model can then call it as a **typed, sandboxed `agent.<provider>.run` tool** that runs the agent headlessly inside `sbx` and returns a normalized **`AgentRunResult`** (final text, structured output, changed files, tokens/cost, captured artifacts). Agent runs reuse the same approval/redaction/quota/audit boundary and **fail closed** unless the operator asserts controlled sandbox egress (`[sandbox] network_isolated`). **Pi** is the local-only, account-free agent and stays unavailable until Phase 11's managed llama.cpp backend. All ONNX runtimes are isolated behind an `onnx` build tag (CGo) and `sbx` + the agent CLIs are shelled out to, so the **default build stays CGo-free** and cross-compiles everywhere; local voice is an opt-in build plus a one-time `hina assets pull`, and sandboxed tools + callable agents are opt-in (`[sandbox] enabled`, `[agents] enabled`). The default LLM provider is a credential-free **mock**, so it runs with no setup; point `[llm]` at OpenAI or a local llama.cpp server for a real model. And now **Automations**: user-owned scheduled workflows (`automation.v1`) run unattended inside the per-user `sbx` sandbox under their own permission profile — a durable scheduler resumes them on restart and stops them cleanly on shutdown, cheap deterministic steps (GitHub/HTTP/shell) run before any model wakes, `agent_cli` steps fan out callable agents, and each run leaves an immutable record + artifacts; an on-rails builder UI (with import/export and LLM-assisted drafting) emits the schema, and every automation is reviewed before it can be enabled. See [`plans/roadmap.md`](plans/roadmap.md).

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

- **`internal/onnx`** — a small, model-agnostic abstraction over ONNX Runtime shared by TTS and ASR: a `Backend`/`Session`/`Tensor` (float32/int64/int32) surface plus a reusable lazy-load + idle-unload `Lifecycle`. The real ORT binding ([`yalue/onnxruntime_go`](https://github.com/yalue/onnxruntime_go), pinned to ORT **1.26.0**) is CGo and lives behind the `onnx` build tag; the default build compiles a CGo-free stub that reports "unavailable."
- **`internal/tts`** — a faithful Go port of the Supertonic 3 pipeline: NFKD text prep + a codepoint tokenizer, a decimal-aware sentence splitter, JSON voice style-vectors, and the four ONNX graphs (duration → text-encode → 8-step flow-matching → vocoder) producing streaming 44.1 kHz speech. No phonemizer, no local HTTP TTS hop — preset voices only (no cloning).
- **`internal/assets`** — the pinned download manager: ORT 1.26.0 per-OS + the Supertonic and Nemotron models (HF revision-pinned) with SHA256 verification and archive extraction, driven by `hina assets`.
- **`internal/rtc` / `web/`** — a typed `SpeakText` control message and a server-driven `POST /api/v1/realtime/speak` synthesize a reply into the live session (44.1 kHz → 24 kHz, barge-in-aware); the `/live` page has a "Speak" box and admin shows the ORT version/provider/lib path + TTS load state + cold/warm latency.

**Phase 5 — Local streaming ASR (Nemotron via ONNX Runtime)**

- **`internal/asr`** — a pure-Go port of the Nemotron 3.5 streaming ASR pipeline: a NeMo-faithful **log-mel front-end** (preemphasis, a radix-2 FFT, a Slaney mel filterbank, log — no per-feature normalization, all in Go, not the graph), a **SentencePiece (unigram) tokenizer** with Viterbi encoding, a cache-aware **FastConformer encoder** streaming loop, an **RNNT greedy decoder** (2-layer LSTM prediction net + joint), and a SentencePiece detokenizer producing 16 kHz streaming partials + a final. Decode-time **agent-name biasing** (a token trie that boosts the configured name so "Hina" isn't mis-heard as "Nina") and a session-layer **wake-word strip** (remove a leading address before the request reaches the LLM) round it out. CGo-free; runs on `internal/onnx`.
- **`internal/rtc` / `web/`** — typed `ListenStarted`/`ListenStopped` control messages route the live mic stream to the recognizer, emitting `ASRPartial` per chunk and an `ASRFinal` (with wake detection + the stripped request body) on commit; the `/live` page has a "Listen" control showing live partials + the final, and admin shows the ASR load state, language, biasing, and cold/chunk latency.

**Phase 6 — Live voice pipeline (VAD, semantic VAD, barge-in, benchmark harness)**

- **`internal/agent`** — a shared, cancellable **agent loop** that streams the LLM provider, classifies interrupted vs errored, and reserves the tool-call hook (Phase 7). Text chat and the live voice loop both run it, so the two modes can't drift.
- **`internal/vad`** — a Go port of **Silero VAD**: an online turn-boundary state machine (threshold + hysteresis, min-speech / min-silence / pre-roll / max-duration tunables) over `internal/onnx`. CGo-free; the real 512-sample stateful model runs behind the `onnx` tag.
- **`internal/voice`** — the turn-detection layer: an OpenAI-shaped `turn_detection` config (`server_vad`/`semantic_vad`, threshold/prefix_padding_ms/silence_duration_ms/eagerness/…), a **semantic VAD** v1 that waits out trailing "umm…", a **backchannel filter** ("yeah"/"uh-huh" don't interrupt), and **playback-aware echo suppression**, composed into a `Pipeline` the live loop and the benchmark both drive.
- **`internal/rtc`** (`live.go`) — the **live conversation loop**: continuous capture → VAD → ASR → agent → TTS, with server-detected **speak-to-interrupt barge-in** (playback truncated to the played cursor, the in-flight reply cancelled, `UserInterrupted` + `ConversationTruncated`). Voice turns persist to the shared timeline (`mode="voice"`), so a **text↔live** switch preserves context with no audio rehydration.
- **`internal/bench` / `hina bench`** — a non-interactive **benchmark harness** that replays labeled fixtures (clean turn, two turns, noise, backchannel-during-playback, interruption, echo, semantic-incomplete) through the real pipeline and emits percentile metrics (false/missed VAD starts, end-of-turn + interruption delay, backchannel suppression). Runs on every host with a synthetic VAD; `--real` swaps in Silero under the onnx build.
- **`web/`** — the `/live` page gains a **"Converse"** card (start/stop the live loop, server/semantic VAD, live transcript + streamed reply + `[interrupted]`), and admin gains a VAD/live-voice runtime panel.

**Phase 7 — Docker `sbx` runner + per-user secret vault + Sandbox Environment**

- **`internal/sandbox`** — the per-user security boundary. A `CLIRunner` wraps a **pinned `sbx` version** (`0.33.0`) behind a command-line smoke test, building `sbx run`/`policy` argv from a typed `RunSpec` and executing it through `internal/platform` (process-tree-aware, so a runaway helper is fully reaped); a `WorkspaceManager` owns durable per-user/per-session workspaces (survive restarts) and ephemeral run scratch (a background janitor reaps it); an `Environment` policy (allowed tools, MCP servers, network allow-list, writable mounts, secret grants) is enforced per call; and a `Router` turns a model tool call into an audited, policy-checked, secret-injected, approval-gated sandbox run. `sbx` isn't required to build — the runner reports unavailable (like the ONNX runtime), and the argv/policy/redaction logic is unit-tested against a fake `sbx` shim.
- **`internal/vault`** — the per-user secret vault. **Envelope encryption**: a fresh per-secret data key (AES-256-GCM) encrypts each value and is wrapped by the local master key (`internal/platform`); the encrypted blob is an owner-private file **on disk, never in the database**, so a DB dump or the admin UI reveals only metadata. Secrets are materialized into a single run's env and a **redactor** scrubs their values from captured output, audit logs, and model-visible results.
- **`internal/agent` / `internal/llm`** — the shared loop now drives **tool rounds**: a tool-capable provider emits tool calls, the loop routes them through the per-user sandbox hook and feeds results back (round-capped). The credential-free **mock** provider gained a `/sh <cmd>` trigger so the whole approval → sandbox → audit path is runnable with no setup.
- **`internal/httpapi` / `web/`** — `/sandbox/environment` + `/sandbox/secrets` (per-user policy + vault management), a tool-approval decide endpoint, and an admin `/admin/sandbox` view (runtime status, per-user usage, redacted run audit). The web client gains a **Sandbox** page (secrets + policy editor) and an admin sandbox panel.

**Phase 8 — Agent auth broker + callable agent adapters (Codex / Claude / Cursor / Pi)**

- **`internal/agentcli`** — typed, **versioned adapters** for the four agent CLIs. Each owns the drift-prone surface — the exact `exec`/`login`/`status` argv and the output parser — and produces a pure `RunPlan` (argv + env + staged files) plus a normalized **`AgentRunResult`** (status, final text, structured output, changed files, tool calls, tokens/cost, captured-output paths). The B7-verified flags are encoded with their corrections (Codex drops `CODEX_API_KEY`/`--full-auto`; Claude keeps `ANTHROPIC_API_KEY` unset for subscription and never uses `--bare`; Cursor stays account-only); the CLIs aren't needed to build — everything is fixture-tested.
- **`internal/agentauth`** — the per-user **auth broker**. An interactive login runs the provider's `login` command in a short-lived `sbx` auth container (the container's TTY via `sbx run -it`, host attached by pipes); the broker **sanitizes** the streamed output, **detects** login URLs / device codes / paste prompts, feeds pasted codes back to stdin, and on a clean exit confirms with the provider `status` command before recording the profile. API-key/token profiles skip the container. The pure detection/sanitization + the session state machine are fully tested with a fake session.
- **`internal/vault` / `internal/store`** — each provider's credential store (browser/subscription state) or API key is kept as **encrypted agent-state** under the same envelope encryption as secrets (a tar of the store, owner-private on disk, never in the DB), and a metadata-only `agent_profiles` row records *how* a provider authenticates (`browser_state`/`api_key`/`oauth_token`/`local_llamacpp`) — never the credential.
- **`internal/sandbox`** — an **`AgentRouter`** (built from the tool Router, sharing its per-user lock, approval, redaction, quota, and audit) routes `agent.<provider>.run`: it resolves the profile, mounts the decrypted credential store (or injects the API key as run-scoped env), stages the adapter's files, runs the agent **headlessly inside `sbx`**, re-encrypts a refreshed credential store, parses a normalized result, and audits it with the auth-profile *type*. It **fails closed** unless `[sandbox] network_isolated` is set (an agent run carries credentials and needs egress Hina can't gate per-container yet); a tar-slip-hardened archiver moves the credential store in and out.
- **`internal/httpapi` / `web/`** — `/agents` catalog (capability + per-user profile + eligibility), `/agents/{provider}/key` + login broker endpoints (start / SSE stream / paste-input / cancel) + logout, and an admin `/admin/agents` coarse-status view. The web **Sandbox** page gains a **Coding agents** card (sign-in with a streamed login dialog that highlights the URL/code, or an API-key form) and admin gains an agents panel.
- The agent CLIs (like `sbx`) are validated on an `sbx`-equipped host (Linux/macOS this phase); **Pi** waits on Phase 11's managed llama.cpp backend, Windows browser-auth on Phase 12, and the optional MCP facade is deferred.

**Phase 9 — Automations (`automation.v1` schema + durable scheduler + builder UI)**

- **`internal/automation`** — the versioned **`automation.v1`** document (trigger / timezone / missed-run policy / concurrency / budget / sandbox profile / steps / outputs), a structural **validator** that returns path-tagged issues for repair, an enable-time **eligibility** check (every used tool/agent/secret is granted + available, and agent/secret automations require `network_isolated`), the bounded **`${item}` / `step.field` selector + template engine**, interval/cron + timezone **scheduling** with missed-run collapse, and the **step engine** (tool / condition / for_each / parallel / llm / agent_cli / finish) that runs deterministic steps before any model wakes, threads per-run budgets + the automation's own permission profile, and produces an **immutable run record**. Pure stdlib; exhaustively unit-tested — the GitHub PR-review automation runs end-to-end against a fake executor (empty notifications finish *without* waking a model; a matching PR fans out to parallel agent reviews → an aggregation `llm` step → a posted comment + a promoted artifact).
- **`internal/autorun`** — the side-effecting runtime: a **durable, server-up-only scheduler** that resumes enabled automations on restart, recomputes the next run, enforces concurrency policies (skip-if-running / queue-one / parallel-N / cancel-previous) and budgets, applies the missed-run policy (default `skip`; opt-in `run_once`), and **stops cleanly** on shutdown (in-flight runs cancelled + finalized — nothing lingers); a sandboxed **Executor** (deterministic tools argv-first through the `sbx` Runner with granted-secret redaction/injection gated on `network_isolated`; `agent_cli` steps through the Phase 8 `AgentRouter`; `llm` steps through the provider); the per-user CRUD/run/artifact service; and **LLM-assisted drafting** (explain the schema → ask for JSON → validate → feed errors back + retry → return a draft for human review). Fake-Runner/fake-store tested; the live `gh`/agent container runs and per-run agent workspaces are deferred to an `sbx` host.
- **`internal/store`** — migration `0004` extends the placeholder `automations` / `automation_runs` / `automation_artifacts` tables; the definition + the full run record live as JSON alongside a queryable projection (name, next/last run, status, trigger).
- **`internal/httpapi` / `web/`** — per-user automation CRUD + `validate` + `enabled` + manual `run` + run history + artifact download + a builder `meta` catalog + the `assist` endpoint. The web client gains an **Automations** page: an **on-rails builder** (structured forms for trigger/schedule, the permission profile, secrets + agents, a recursive step editor, and outputs — not a raw JSON editor), a one-click **PR-review template**, **import/export**, a server-validated issues panel, **AI drafting**, and a **run history** view with per-run logs/artifacts.
- A scheduled run is **unattended**: there is no per-step approval (the gates are mandatory human review before enable + the automation's sandbox profile), and the same `network_isolated` fail-closed rule applies to its agent/secret steps. `mcp.call` validates but reports unavailable in this build (the MCP facade is deferred).

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

**Optional: local voice (Phases 4–6).** Local TTS, ASR, and the live VAD need the `onnx`-tagged build (links ONNX Runtime via CGo) plus the model assets:

```bash
make build-onnx            # -> bin/hina with the onnx tag (needs a C compiler; CGO_ENABLED=1)
bin/hina assets pull       # download + checksum ORT 1.26.0, Supertonic (~400 MB), Nemotron (~680 MB), Silero VAD (~2 MB)
# enable [tts], [asr], and [voice] in config.toml (enabled = true), then:
bin/hina server            # Live → "Converse" holds a spoken conversation; "Speak"/"Listen" are the text-driven demos
```

`hina assets status` reports what's installed; `hina doctor` reports the ORT runtime + local-TTS/ASR/live-voice availability. The default (CGo-free) build leaves local voice unavailable and says so. One `assets pull` installs all model sets, but each engine verifies only its own assets — the live loop needs all three of `[voice]`+`[asr]`+`[tts]`. `assets pull` is resumable and retries transient network errors: each download retries with backoff (honoring a server `Retry-After`), and already-verified files are skipped, so re-running after a dropped connection only fetches what's missing. Local voice on Windows is gated to Phase 12. The turn-detection benchmark runs anywhere, no models needed: `bin/hina bench`.

**Optional: sandboxed tools (Phase 7).** Set `[sandbox] enabled = true` and install a pinned Docker [`sbx`](https://docs.docker.com/ai/sandboxes/) (`hina doctor` reports its version vs the pinned one and runs a command-line smoke test; a drifted CLI fails closed). Then a model-requested shell/file/HTTP tool runs inside the calling user's `sbx` sandbox with that user's Sandbox Environment policy (allowed tools, a request-time `host:port` network allow-list, granted secrets injected via the process env — never the argv), an in-chat approval card (`approval = "always"`, or `"auto"` to skip the prompt), resource limits, and an audit log. The **Sandbox** page manages secrets and policy; secret vault + policy editing work even with `sbx` absent. The default credential-free mock LLM requests a shell tool when you send `/sh <command>`, so the whole path is demoable. Without `sbx`, tool calls report the sandbox unavailable and everything else keeps working. The vault + sandbox tools are gated off on Windows until Phase 12 (owner-only ACL/DPAPI not yet enforced).

**Optional: callable agents (Phase 8).** Set `[agents] enabled = true` (on top of `[sandbox] enabled` + a working `sbx`) to authenticate Codex/Claude/Cursor in the **Sandbox → Coding agents** card — a browser/device login streamed from a throwaway `sbx` auth container, or an API key — and call them as `agent.<provider>.run` tools that run headlessly inside `sbx`. Because an agent run carries provider credentials and needs egress Hina can't yet gate per container, agent runs are **disabled unless `[sandbox] network_isolated = true`** (an explicit assertion that you've locked the `sbx` container's egress down) — fail closed. **Pi** (the local-only, account-free agent) needs Phase 11's managed llama.cpp endpoint (`[agents] local_endpoint`) and reports unavailable until then; the agent CLIs themselves live inside the `sbx` container image. `hina doctor` reports callable-agent availability. Credentials are stored encrypted and the admin UI only ever shows a coarse status — never a token/URL/code.

**Optional: automations (Phase 9).** Set `[automations] enabled = true` (on top of `[sandbox] enabled` + a working `sbx` + the vault) to run user-owned scheduled workflows. Build one in the **Automations** page — the on-rails builder, a one-click **PR-review template**, or **AI drafting** from a description — and it emits an `automation.v1` document. Deterministic steps (`github.notifications` / `github.pr_checkout` / `github.pr_comment` / `http.request` / `shell.exec`) run argv-first inside the user's sandbox before any model wakes; `agent_cli` steps fan out the Phase 8 callable agents; an `llm` step aggregates. Each run executes under the automation's **own permission profile** (not your interactive chat policy) and leaves an **immutable run record** + promoted **artifacts**. The scheduler resumes enabled automations on restart and **stops them cleanly** on shutdown; a fire missed while the server was down defaults to `skip` (opt-in `run_once`). An automation that uses agents or injects secrets needs `[sandbox] network_isolated = true` and a reviewed-then-enabled state — **every automation is created disabled** until you review it. The `Max*` ceilings under `[automations]` clamp each automation's budget.

LAN binding (`--host 0.0.0.0` with `lan_enabled = true` / `HINA_SERVER_LAN=1`) is refused until the bootstrap admin password is changed. App state lives in OS-standard dirs (never repo-relative): config under `os.UserConfigDir()/hina`, data/DB under the platform data dir. Browser mic capture works on `localhost` without TLS; a second LAN device needs HTTPS with a real cert (configure `[server] tls_cert`/`tls_key`, or front it with a reverse proxy — `hina doctor` reports this).

## CLI

```
hina server     Run the server (UI + API)
hina setup      Create app dirs, run migrations, bootstrap the admin
hina doctor     Report host capabilities and feature availability (--json)
hina migrate    Apply migrations (migrate down [N|all] to roll back)
hina assets     Manage local-inference downloads (status | verify | pull)
hina bench      Run the live-voice turn-detection benchmark suite (--json)
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

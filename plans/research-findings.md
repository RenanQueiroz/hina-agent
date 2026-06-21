# V2 Research Findings & Closed Spikes

Date: 2026-06-18
Companion to: [`hina-agent-plan.md`](hina-agent-plan.md)
Phase plans: [`roadmap.md`](roadmap.md) and `phase-*.md`

This doc closes the open items from the main plan's **Implementation Readiness Review** and **Remaining Spikes** lists. Each item is marked:

- **DECIDED** — a design decision is made here (the "clarify before first code" items).
- **GREEN / YELLOW / RED** — a research/library question with a verdict + chosen library/version.
- **DEFERRED** — genuinely needs a Windows host, real hardware, or running code to settle. Documented so it does not block starting, then validated in its phase (most land in the Windows hardening phase).

Verdicts come from a parallel docs/library research pass on 2026-06-18. Key citations are inline; treat versions as "current as of 2026-06-18, re-check at implementation."

## Environment note

The V2 workspace `/home/renan/hina-agent` currently has **Node 24, npm 11, Python 3.12, gcc 13, cmake, git, Docker** but **no Go toolchain and no `sbx`** installed. So the *code* spikes (build a Pion loopback, load an ONNX model) still have to run inside their phases on a prepared machine — they could not be executed during this planning pass. What *is* closed here is every **docs/library/licensing** question, plus the design decisions. Installing Go + ORT libs + `sbx` is the first task of Phase 1 / the relevant phase.

---

## Part A — "Clarify before first code" (all DECIDED)

### A1. Repository / product identity — DECIDED
See the table in [`phase-01-foundation.md`](phase-01-foundation.md#product-identity-locked-for-v2). Summary: product/agent name **Hina**; single CLI binary `hina` with subcommands; Go module `github.com/RenanQueiroz/hina-agent` (confirmed 2026-06-18); config/cache/data/runtime dirs all via `os.UserConfigDir`/`UserCacheDir`/platform data dir under a `hina` folder — never repo-relative (V1's repo-relative `./.cache`, `preferences.toml`, etc. do not carry over). `voice-agent` names are V1-only.

The GitHub owner in the module path is confirmed as `RenanQueiroz`. Everything else is reversible config.

### A2. Bootstrap auth/session v0 — DECIDED
First-run admin bootstrap credential (printed once, must be changed before LAN). **Argon2id** password hashing. Login sessions as secure httpOnly cookies (hashed server-side) with CSRF protection; bearer tokens for non-browser clients. Roles `admin` / `user` with `RequireUser`/`RequireAdmin` middleware. Bind `127.0.0.1` by default; `--host 0.0.0.0` refuses to start until the bootstrap credential is changed; LAN clients always authenticate. Full build in [`phase-01-foundation.md`](phase-01-foundation.md).

### A3. Persistence schema v0 — DECIDED
**SQLite via `modernc.org/sqlite` (CGo-free)**, WAL + busy timeout + foreign keys, embedded migrations (`golang-migrate` or `goose`). Table boundaries drawn up front (users, auth sessions, conversations, turns, events, runtime_state, + empty placeholders for automations/runs/artifacts/sandbox_state/secrets_meta/agent_auth_state). The `events` table is the append-only source of truth behind replay/reconnect. Detail in Phase 1. (CGo-free driver is what keeps native Windows builds compiler-free — a load-bearing choice.)

### A4. Event/API wire contracts v0 — DECIDED
One typed event envelope (`event_id`, `session_id`, `user_id?`, `turn_id?`, monotonic `server_ts`, per-session `seq`, `source`, `type`, `payload`), shared by the SSE user/admin streams **and** the Phase 3 `RTCDataChannel`. Versioned HTTP routes (`/api/v1/…`). Reconnect replays from the `events` table by last `seq`. TypeScript types are **generated from Go** (or shared zod) and checked in CI so frontend/backend can't drift. Detail in Phase 1.

### A5. Tier 1 validation hosts — DEFERRED (process, not code)
Confirm hands-on access to native **Windows 11 x64**, **macOS Apple Silicon**, **Linux x86_64**. The user has directed that **Windows testing happens after an initial working version** — so: build for all three from day 1 (CI cross-compiles + runs a Windows smoke test), but mark Windows as "built, not hands-on-validated" in `hina doctor` output and docs until the Windows hardening phase. If macOS hardware is unavailable, mark it the same way. This is the only "clarify" item that stays open, by explicit choice.

---

## Part B — Research / library spikes (verdicts + chosen versions)

### B1. ONNX Runtime Go binding — GREEN
**Decision: `yalue/onnxruntime_go`, pin `v1.31.0`, paired with bundled ORT `1.26.0` shared libs.** CGo, loads the ORT lib via `SetSharedLibraryPath(...)` (bring-your-own DLL/.so/.dylib — avoids MSVC/MinGW linkage). This is the single inference backbone for VAD + ASR + TTS, and it's the binding Supertonic's official Go example and the Silero/Nemotron references already use. No pure-Go inference path exists, so CGo is accepted **only** at this boundary (build-tagged), keeping the control plane CGo-free.

> **Correction (Phase 4 implementation, 2026-06-20):** the binding's release tag is **decoupled** from the ORT version — there is no ORT 1.31.x. `yalue/onnxruntime_go v1.31.0` ships ONNX Runtime C API headers at **version 26**, so it must be paired with an **ORT 1.26.0** shared library (the binding `dlopen`s it at runtime; build the `onnx` tag without the lib present, then `SetSharedLibraryPath` to it). ORT 1.26.0 is also the last release with CPU builds for all of linux-x64, osx-arm64, and win-x64 (macOS x64's last CPU build was 1.23.2). The exact release + Supertonic HF revision + SHA256s are pinned in `internal/assets`. The "must match the ORT lib version exactly" guidance still holds — it just means **match the binding's C API version (26 → ORT 1.26.0)**, not the binding's own tag number.
- Caveat: **no production Go binding for ORT GenAI.** Irrelevant for us — the local LLM runs as `llama-server` (HTTP), not in-process ONNX. ASR/TTS/VAD use plain ORT, which this binding covers.
- Windows: ship `onnxruntime.dll` (+ provider DLLs for GPU) from the app runtime dir; call `SetSharedLibraryPath` instead of trusting the system path.
- Refs: github.com/yalue/onnxruntime_go (v1.31.0, 2026-06-04; ORT C API v26 → ORT 1.26.0).

### B2. Supertonic 3 TTS in Go — GREEN (strongest of the four)
~99M params, 31 languages, **44.1 kHz** mono. **Ships an official Go ONNX example** (`/tree/main/go`, uses the `yalue` binding). Four ONNX files (~398 MB total: `text_encoder` 36 MB, `duration_predictor` 3.7 MB, `vector_estimator` 257 MB run ~8 flow-matching steps, `vocoder` 101 MB) + `tts.json`, `unicode_indexer.json`, per-voice style vectors. **No phonemizer dependency** — preprocessing is character-level Unicode codepoint→token-ID using Go stdlib + `golang.org/x/text` (NFKD); none of espeak-ng/misaki/g2p. The only native dep is ORT.
- Decision: integrate directly in Go per the official example; synthesize through an internal Go API (no local HTTP TTS hop), emit 44.1 kHz frames to the session output path.
- Risk: the 257 MB `vector_estimator` step's latency, and ORT packaging/cross-compile — not the text pipeline. Validate cold/warm latency in its phase.
- Refs: github.com/supertone-inc/supertonic, huggingface.co/Supertone/supertonic-3.

### B3. Nemotron 3.5 streaming ASR (0.6B) in Go — GREEN (most owned-code)
Cache-aware **FastConformer encoder (24-layer, d_model 1024) + RNNT (2-layer LSTM-640) + joint**, 600M params, 40 locales, multilingual via int64 `prompt_index` (101 = auto-detect). ONNX I/O confirmed: encoder consumes `processed_signal`, lengths, `cache_last_channel`[24,1,56,1024], `cache_last_time`[24,1,1024,8], `cache_last_channel_len`, `prompt_index`; emits `encoded`, `encoded_len`, and `*_next` caches to feed back each chunk. 16 kHz mono float32. **The log-mel front-end is NOT in the graph** — implement in Go to the NeMo FilterbankFeatures spec: `n_mels=128, n_fft=512, win=400, hop=160, preemph=0.97`. SentencePiece BPE, vocab 13,088, `blank_id=13087`; **punctuation + capitalization are emitted by the model** (no separate punct model). Chunk sizes 80/160/320/560/1120 ms via `att_context_size=[56,R]`, R∈{0,1,3,6,13}.
- Export choice: **`smcleod/...-int8`** uses a combined `decoder_joint.onnx` with the full chunk-size range and correct OpenMDW-1.1 licensing → preferred. The `onnx-community ...-int4` is fixed to the 560 ms config and has a (community) MIT relabel (see B10) → use as a size/latency comparison only.
- Decoding: greedy RNNT is a few hundred lines (loop joint per frame, emit non-blank, advance LSTM state, blank→next frame, cap `max_symbols_per_step`); cache threading is mechanical. **`parakeet-rs` (MIT, Rust) `src/model_nemotron.rs` is the port reference**; sherpa-onnx (C++) secondary.
- **Name biasing (the `Hina` recognition item) is confirmed pure decode-time, graph-independent:** a SentencePiece-token trie + per-hypothesis pointer; after the joint emits log-probs, add `λ·boost` to tokens continuing an active trie path, advance/cancel on match/mismatch. No retrain, no graph change, quantization-independent. Rebuild the trie at runtime from `[agent].name` + `name_aliases`. Start from NeMo RNNT params `context_score≈1.0`, `depth_scaling≈2.0` and tune on fixtures.
- Risk: faithfully reproducing the NeMo log-mel front-end in Go — small numerical mismatches degrade WER more than decode bugs. Build a fixture that compares Go front-end output to a reference NeMo/parakeet-rs feature dump.
- Refs: huggingface.co/nvidia/nemotron-3.5-asr-streaming-0.6b, huggingface.co/smcleod/..., github.com/altunenes/parakeet-rs.

> **Corrections (Phase 5 implementation, 2026-06-20):** re-verified against the live repo + the `parakeet-rs` reference and confirmed in `internal/asr`.
> - **Repo/files:** the primary export is `smcleod/nemotron-3.5-asr-streaming-0.6b-int8` (commit `f1f26d2`): `encoder.onnx` **with an external `encoder.onnx.data` weights file**, the combined `decoder_joint.onnx`, and `tokenizer.model`. We do **not** ship `config.json` — the prompt dictionary + dims are embedded in Go.
> - **Front-end normalization is OFF.** config.json's `preprocessor.normalize == "NA"` and the reference both feed **raw** log-mel to the encoder (no per-feature mean/var). This is the single most load-bearing front-end detail. STFT uses center zero-padding (`n_fft/2`) and a symmetric (periodic=False) Hann; log guard is `ln(x + 2^-24)`.
> - **Tokenizer is SentencePiece UNIGRAM** (13087 pieces; blank = 13087, so 13088 joint logits). Building the bias trie needs Viterbi encoding (piece scores), implemented in Go.
> - **Decoder dtypes:** `decoder_joint.onnx` takes **int32** `targets`/`target_length` (the encoder uses int64) — `internal/onnx` gained an int32 tensor type for this.
> - **External-data load:** the `yalue` binding exposes no in-memory external-data API, so the encoder loads by its checksum-verified **path** (ORT finds `encoder.onnx.data` beside it) while the self-contained decoder + tokenizer load from verified bytes. The owner-private asset root (SecureRoot) bounds the residual to a same-user verify→open swap.

### B4. Silero VAD in Go — GREEN
Official `silero_vad.onnx`: inputs `input`[1,512], `state`[2,1,128] LSTM carry, `sr` int64; outputs speech prob + `stateN`. 512-sample window @16 kHz (32 ms), stateful, in-order. Run it through the **same `yalue` ORT binding** (wrap the model directly rather than adopting a wrapper pinned to a different ORT version, e.g. `streamer45/silero-vad-go`). MIT licensed. (Pure-Go fallback if "no CGo" ever became hard: a hand-written energy VAD — adequate only for silence-gating, not robust speech discrimination.)
- Refs: github.com/snakers4/silero-vad (MIT, v6.x).

### B5. WebRTC media bridge — GREEN architecture, YELLOW on one sub-point
**Pion WebRTC v4 (v4.2.x), pure Go / no CGo**, production-ready, supports Windows/macOS/Linux + full ICE/DTLS/SRTP. This is what makes browser-owned audio work with zero native toolchain.
- **Inbound (mic→server):** `TrackRemote.ReadRTP()` → **`pion/opus` decoder (pure Go, decode-only — fine for speech/SILK)** → resample 48→16 kHz. No CGo on the receive path.
- **Outbound (assistant→browser) — the key decision:** **send raw PCM over an `RTCDataChannel` to a browser AudioWorklet**, NOT an Opus return track. Rationale: there is **no production-grade pure-Go Opus *encoder***, and PCM-over-datachannel (a) removes the only hard CGo dependency on the audio path and (b) gives the **exact browser playback cursor** needed for clean barge-in (Pion alone cannot tell you what the browser has actually played). Use an unordered datachannel (`maxRetransmits:0`) + a small (~80–120 ms) AudioWorklet ring buffer; downsample to 24 kHz s16 to keep bandwidth ~384 kbps.
- **Resampling:** **`tphakala/go-audio-resampler` (pure Go)**; `QualityLow` (its speech preset) for the 48→16 kHz ASR downsample, `QualityMedium` for 44.1→48 kHz if ever needed. Streaming zero-alloc `ProcessFloat32Into` in the frame loop.
- **Fallback** if a true Opus return track is ever required (WAN, NACK/FEC): **`jj11hh/opus`** (libopus compiled to WASM, run via pure-Go `wazero` — CGo-free), or gate `hraban/opus` (CGo) behind a build tag.
- **Signaling:** mirror OpenAI's contract — `POST /realtime/calls` accepting `application/sdp`, return the Pion answer as `application/sdp` plain text; name the events datachannel like `oai-events`/`events`. One browser client then targets local **or** cloud Realtime.
- YELLOW sub-point: accurate "audio actually played" is browser-side only — instrument the AudioWorklet cursor + `getStats()` and report over the datachannel. The PCM-over-datachannel design is exactly what makes this tractable.
- Refs: github.com/pion/webrtc, github.com/pion/opus, github.com/tphakala/go-audio-resampler, developers.openai.com/api/docs/guides/realtime-webrtc.

### B6. Docker `sbx` production fit — GREEN, pin the version
Verified against current docs (latest release **v0.33.0**, 2026-06-17). Everything the plan assumed exists: `run/create/exec/cp/ls/stop/rm/ports/policy/kit/secret/login`; positional workspace mounts with `:ro`; `--clone` (host repo mounted read-only, agent works on an in-sandbox git clone); `--cpus`/`-m`. Install: Windows 11 x64 `winget install -h Docker.sbx` (**Windows 10 unsupported**, needs Windows Hypervisor Platform); macOS `brew install docker/tap/sbx` (Sonoma+, Apple Silicon); Linux x86_64 Ubuntu 24.04+/.rpm (needs KVM). **Docker Desktop not required.** `host.docker.internal` reaches host services but is **policy-gated** (Open/Balanced/Locked-Down chosen at `sbx login`) — under Balanced/Locked-Down you must `sbx policy allow network localhost:<port>` (v0.33.0 also gates DNS and blocks ICMP). Secrets via OS keychain + host-side proxy injection (raw values never hit the sandbox FS) for supported services (anthropic/aws/github/google/groq/mistral/nebius/openai/xai); custom secrets are experimental; registry creds are the leakier exception. `sbx login` uses Docker OAuth + a **free** account — no paid subscription for core use (only org-governance features are paid).
- **Decisions to bake in:** (1) **Pin the `sbx` version** and gate upgrades behind a smoke test of our exact `run`/`policy`/`secret`/`cp` command lines — the CLI moves fast with breaking changes (e.g. re-attach is now `sbx run --name <id>`; `sbx policy -g` was removed in v0.32.0). (2) Treat `sbx kit` (still Early Access) as the least-stable surface. (3) The product per-user vault stays the source of truth; `sbx secret` is an injection backend for its supported services only.
- Biggest risk: velocity-driven breaking changes in a not-formally-GA CLI under an unattended automation runner. Mitigation = version pin + command-line smoke test in `hina doctor` and CI.
- Refs: docs.docker.com/ai/sandboxes/*, github.com/docker/sbx-releases.

### B7. Callable agent CLIs — CONFIRMED with 5 corrections
Codex, Claude Code, Cursor, Pi all verified against current docs. **Corrections to apply to the main plan's "Automation/callable agent notes" and "Provider setup commands":**
1. **Codex: `CODEX_API_KEY` is not in current docs — drop it.** Use `OPENAI_API_KEY`, or `codex login --with-api-key` (key on stdin). `CODEX_ACCESS_TOKEN` is real but pairs with `codex login --with-access-token`.
2. **Codex: `--full-auto` is deprecated** (runtime warning) — use `--sandbox workspace-write` (with `--ask-for-approval never`) or `--yolo`/`--dangerously-bypass-approvals-and-sandbox`. (Plan already flagged this; confirmed.)
3. **Cursor CLI cannot target a custom provider/endpoint** (IDE-only); `--model` only selects Cursor-hosted models. Fine for us — **Pi**, not Cursor, is the local/account-free agent. Cursor stays an account-backed agent only.
4. **Cursor headless cancellation is undocumented** — verify SIGINT/SIGTERM/timeout empirically in the adapter health check before relying on it.
5. **Claude `--bare` ignores OAuth/`CLAUDE_CODE_OAUTH_TOKEN`** (API-key only) — keep using normal `claude -p` (or `--safe-mode`) for subscription/browser-auth runs, `--bare` only for API-key scripts. (Plan already avoids `--bare` for subscription; confirmed + reason.)
Confirmed-as-assumed (some flagged as doubtful, all real): Codex `codex exec --json --output-schema --cd --skip-git-repo-check`, `codex login [--device-auth|status]`, `CODEX_HOME`, `codex mcp-server`, `codex doctor`. Claude `-p`, `--output-format json|stream-json`, `--include-partial-messages`, **`--json-schema`**, `--allowedTools/--disallowedTools`, `--permission-mode`, `--max-turns`, `--dangerously-skip-permissions`, `claude auth login/status/logout`, `claude setup-token`, `CLAUDE_CONFIG_DIR`, and the full env precedence chain (cloud creds → `ANTHROPIC_AUTH_TOKEN` → `ANTHROPIC_API_KEY` → `apiKeyHelper` → `CLAUDE_CODE_OAUTH_TOKEN` → subscription; **`ANTHROPIC_API_KEY` overrides subscription in `-p`** — keep it unset in the browser-auth profile). Cursor `agent -p`, `--output-format json|stream-json`, `--stream-partial-output`, `--force/--yolo`, `agent login/status/logout`, `CURSOR_API_KEY`, `NO_OPEN_BROWSER=1 agent login`. Pi: command `pi` (`@earendil-works/pi-coding-agent`), `--mode rpc` (LF-delimited JSONL, `abort`/`steer`), `~/.pi/agent/models.json` custom provider (`api: "openai-completions"`, `baseUrl: http://host.docker.internal:<port>/v1`, dummy key) → **local llama.cpp confirmed**, **no built-in sandbox** (run it inside `sbx`), `PI_OFFLINE=1`, and `--no-extensions/--no-skills/--no-context-files/--no-tools` to lock it down.
- **Treat all four as versioned adapters with health checks** — they drift; re-verify flags immediately before implementing each adapter (Phase 8).
- Refs: developers.openai.com/codex, code.claude.com/docs, cursor.com/docs/cli, github.com/earendil-works/pi.

### B8. OpenAI Realtime integration — GREEN
GA (base `https://api.openai.com/v1/realtime`). Mint ephemeral secret: `POST /v1/realtime/client_secrets` with the real API key (replaces the old `/v1/realtime/sessions`); set `OpenAI-Safety-Identifier`; response `{value: "ek_...", expires_at, session}`. SDP: `POST /v1/realtime/calls` with `Content-Type: application/sdp`, `Authorization: Bearer ek_...`, raw offer body → plain-text SDP answer; **the `call_id` (`rtc_...`) comes from the `Location: /v1/realtime/calls/rtc_...` response header — capture it or you can't attach the sideband.** Server sideband: WebSocket to `wss://api.openai.com/v1/realtime?call_id=<rtc_...>` with the real API key — browser owns media, server owns control (`session.update`, monitor events, `response.create`, tool calls). Lifecycle: `POST /v1/realtime/calls/{id}/{accept,reject,refer,hangup}`. Turn detection: `server_vad` (threshold/prefix_padding_ms/silence_duration_ms) vs `semantic_vad` (eagerness low=8s/med=4s/high=2s/auto, content classifier). Barge-in: `input_audio_buffer.speech_started`; **WebRTC auto-truncates unplayed audio** (proactively `output_audio_buffer.clear`); WebSocket transport must manually `response.cancel` + `conversation.item.truncate`. Models: `gpt-realtime` (+ `gpt-realtime-2/1.5/mini`, `gpt-realtime-whisper`, `gpt-realtime-translate`); legacy `gpt-4o-realtime-preview` superseded.
- This validates the plan's two-mode design: our local Realtime-like endpoint (B5) mirrors this contract so one browser client switches base targets. Refresh these exact endpoints right before Phase 10.

### B9. Go agent SDK + cloud SDKs + llama-server — GREEN (SDK YELLOW)
- **Official OpenAI Go SDK: `github.com/openai/openai-go/v3` (GA, v3.41.0 on 2026-06-18).** Responses API (primary), streaming, tools, structured outputs. Import the **v3** path; budget for breaking changes if porting old samples. **GREEN.**
- **Google Gen AI Go SDK: `google.golang.org/genai` (GA, v1.61.0).** Official, replaces the deprecated `generative-ai-go`/`vertexai/genai`; Gemini text/tools/streaming, both Developer-API-key and Vertex backends, plus a Live API. **GREEN.** Caveat: free tier was sharply cut 2025-12-07 — plan to enable billing; keep Gemini optional, never a dependency of local mode.
- **`nlpodyssey/openai-agents-go`: YELLOW.** Apache-2.0 Go port; supports Responses, Chat Completions, streaming, tool loops, `context.Context` cancellation, MCP (local+hosted), hosted tools, sessions (incl. Postgres), and **custom base URL → local llama.cpp/vLLM**. But only `v0.1.0` is tagged (2025-10-27), `main` ahead through 2026-03 then quiet; two core maintainers. **Decision: build the minimal custom agent loop first (Phase 6/text-chat in Phase 2 uses a thin client); evaluate adopting this for the tool/MCP/session machinery later, and if adopted, pin a specific commit SHA.** Its built-in voice path is STT→TTS (not Realtime) so it complements, not replaces, B8.
- **`llama-server` (b9707, 2026): GREEN.** `/v1/chat/completions` + `/v1/responses` (internally converted) + `/v1/models` (and Anthropic `/v1/messages`). Native router (launch with no model → route by request `model`), `--models-dir/--models-preset` (INI presets), `--models-max N` (LRU evict), and **native idle unload via `--sleep-idle-seconds`** — `llama-swap` no longer required. Windows: prebuilt release zips (CPU/CUDA/Vulkan/SYCL/HIP/OpenVINO) + **`winget install ggml.llamacpp`** (ships Vulkan; for CUDA grab the `win-cuda-*` zip + `cudart-*` DLLs). Carry forward V1's `--models-preset … --models-max 1 --sleep-idle-seconds 90` pattern.

### B10. Model / runtime asset licensing — GREEN (no blockers, 2 conditions)
No gated repos, no commercial prohibitions. Conditions:
- **Nemotron base + `smcleod` int8: OpenMDW-1.1** — commercial OK, ungated, redistributable; retain NVIDIA notices if re-hosting; defensive-patent clause (low risk). The `onnx-community int4` repo is tagged MIT but that's a community relabel — **honor OpenMDW-1.1 on the underlying weights** (another reason to prefer the int8 repo).
- **Supertonic 3: BigScience OpenRAIL-M** (code/SDK is MIT). Commercial OK. **Project decision (2026-06-18): V2 ships preset voices only — no user voice cloning by default.** That avoids the one acute OpenRAIL-M concern for a TTS product (non-consensual likeness / impersonation). Remaining obligations are light because the managed installer downloads from the original HF repo rather than re-hosting (the heavier "furnish a copy of the license + propagate the use restrictions" duty is triggered mainly by *redistribution*): still **ship/point to the OpenRAIL-M license and add a short acceptable-use line to the ToS** (no disinformation/harassment; disclose machine-generated audio). **Revisit if voice cloning is ever added** (e.g. a CSM-style `ref_audio` path like V1 had) — that reactivates the non-consensual-voice restriction as a first-order concern.
- **Silero VAD: MIT. llama.cpp: MIT** (Windows release zips publish SHA256 + `bXXXX` build tags — pin + verify). **SentencePiece: Apache-2.0.** Bundled tokenizer/config files inherit their parent model's license.
- **Installer rule:** download from the original source at install time (don't re-host), **pin every asset to a specific commit/release and verify checksums** (public repos can flip to gated). Surface ORT/llama.cpp/model versions in `hina doctor`.

---

## Part C — Deferred (does not block starting; validated in-phase)

These need a real host, running code, or measured numbers. Per the user's direction, **none of these blocks Phase 1–2 start**; most are gathered into the Windows hardening phase or their feature's phase.

- **C1. All native-Windows-host spikes — DEFERRED to the Windows hardening phase.** Run server from PowerShell; app dirs + SQLite migrations on Windows; Job-Object process-tree kill; `winget install Docker.sbx` + Hypervisor Platform + `sbx run shell` + kit + mounts with spaces/Unicode + `sbx cp` + policy + `host.docker.internal` + secret injection; `llama-server.exe` install/launch/idle-unload/cancel + CUDA/Vulkan detection + Pi via host gateway; **ORT DLL load through `yalue` binding + tiny ONNX fixture + minimal Supertonic/Nemotron passes**; DPAPI/Credential-Manager vs ACL key-file for the vault + backup/restore + redaction; path/permission fixtures (long paths, drive letters, case collisions, reparse points, ACL failures, CRLF logs). Until the ORT/DLL gate passes, Windows ships text + cloud STT/TTS + full OpenAI Realtime with **local voice marked unavailable** in `hina doctor`.
- **C2. Latency/quality benchmark numbers — DEFERRED to each runtime's phase.** The benchmark *harness* is built early (Phase 6) and is non-interactive on all Tier 1 hosts; the actual numbers (Supertonic cold/warm, Nemotron partial cadence/final latency, VAD false-start/interruption rates, name-substitution rate off vs on) come from running it. Targets in the main plan stand until measured.
- **C3. WebRTC media bridge measured latency — DEFERRED to Phase 3.** Architecture is decided (B5); the actual Opus-decode→resample→playback latency and packet-loss behavior get measured against the loopback in Phase 3.
- **C4. Automation `automation.v1` semantics — PARTIALLY DEFERRED to Phase 9.** Selector/template syntax, retry/error policy, idempotency, artifact-promotion rules, side-effect confirmation, and schema evolution are *design* tasks owned by Phase 9 before portable imports are promised. The JSON shape and the GitHub-review example already exist in the main plan.
- **C5. Secret-vault threat model — DECIDED-as-documented.** Unattended Automations require the server to decrypt granted secrets and mount agent-auth state at run time. Therefore secrets can be hidden from the database and the normal admin UI, **but not from a malicious host/root admin or a modified server binary.** Envelope encryption (per-secret data key wrapped by a local master key in OS keyring/DPAPI/ACL-guarded file). This boundary is a documentation/UX requirement, built in Phase 7 — nothing to research, but it must be stated plainly to users.
- **C6. `nlpodyssey/openai-agents-go` adoption — DECIDED in Phase 6: keep the minimal custom loop.** The shared `internal/agent.Loop` (streaming, `context` cancellation, the typed-event envelope, and a reserved tool-routing seam) is ~100 lines, fully tested, and run by both text and voice. The SDK stays YELLOW (only `v0.1.0` tagged, `main` quiet after 2026-03, two maintainers), and adopting it would mean threading Hina's event/session model through its abstractions for no current gain. Re-evaluate it for the tool/MCP/session machinery in Phase 7+ and pin a commit SHA if adopted. See [`phase-06-live-voice.md`](phase-06-live-voice.md).

---

## Part D — Corrections to apply to `hina-agent-plan.md`

These supersede statements in the main plan (left in place there as historical context; this section is authoritative):

1. **Drop `CODEX_API_KEY`** from the Codex notes/setup commands → use `OPENAI_API_KEY` or `codex login --with-api-key`; `CODEX_ACCESS_TOKEN` pairs with `--with-access-token`. (B7)
2. **Codex `--full-auto` deprecated** → `--sandbox workspace-write --ask-for-approval never` or `--yolo`. (B7)
3. **Cursor CLI has no custom-provider mode** → it stays an account-backed agent only; Pi is the sole local/account-free agent. (B7)
4. **`sbx` re-attach is `sbx run --name <id>`** and `host.docker.internal` needs an explicit `sbx policy allow network localhost:<port>` under non-Open policies; **pin the `sbx` version**. (B6)
5. **OpenAI Realtime: capture the `call_id` from the `Location` header** of `POST /v1/realtime/calls`; ephemeral secrets are minted at `POST /v1/realtime/client_secrets` (not `/sessions`). (B8)
6. **Local Opus encode is avoided entirely** — outbound assistant audio is **raw PCM over `RTCDataChannel` to an AudioWorklet**, not an Opus return track. (B5)
7. **Prefer the Nemotron `smcleod int8` export** (combined `decoder_joint.onnx`, full chunk range, clean OpenMDW-1.1) over the int4 community export for the primary path. (B3, B10)
8. **Nemotron log-mel front-end must be implemented in Go** (not in the ONNX graph): `n_mels=128, n_fft=512, win=400, hop=160, preemph=0.97`. (B3)
9. **OpenAI Go SDK is on `/v3`** (`github.com/openai/openai-go/v3`); Google SDK is `google.golang.org/genai`. (B9)
10. **Supertonic OpenRAIL-M:** V2 default is **preset voices, no cloning**, which clears the acute deepfake/impersonation concern; still ship the license + a short acceptable-use line in the ToS. Revisit only if a cloning path is added later. (B10)

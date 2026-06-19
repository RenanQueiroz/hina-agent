# Phase 2 Plan: V2 Realtime Voice Architecture

Date: 2026-06-13
Updated: 2026-06-19

> **Companion docs (added 2026-06-18).** This file remains the canonical vision /
> architecture document. The implementation is now broken into phases — see
> [`roadmap.md`](roadmap.md) (index + dependency graph) and
> [`phase-01-foundation.md`](phase-01-foundation.md) … [`phase-11-windows-hardening.md`](phase-11-windows-hardening.md).
> The open research/spike items and the "clarify before first code" decisions are
> resolved in [`research-findings.md`](research-findings.md), which is authoritative
> where it and this document differ (see its Part D for specific corrections).
> Naming note: "Phase 2" in this title means the **V2 product** (vs. the V1 Python
> app); it is unrelated to implementation **Phase 1–11** in `roadmap.md`.

## Goal

Design and build a v2 voice-agent architecture that feels like talking to a person:

- extremely low perceived latency,
- continuous listening,
- speak-to-interrupt / barge-in,
- robust echo handling so TTS is not mistaken for user speech,
- a local user Web UI usable by other devices on the same network,
- a separate admin Web UI for setup, backend control, logs, and user/session management,
- per-user tool/workspace isolation through Docker `sbx`-backed sandboxes,
- user-configurable Automations that can run scheduled unattended tasks while the server is up,
- clean support for local model stacks and cloud realtime stacks,
- first-class native host support for Windows, macOS, and Linux in V2.

V1 is considered complete after the ONNX ASR / Parakeet work. Freeze the current
Python/Textual app as the v1 reference implementation. The V1 checkout is
`/home/renan/voice-agent`. This plan currently lives in
`/home/renan/hina-agent`; treat that as the intended V2 implementation workspace
unless the user explicitly redirects. V2 should be a rewrite, not an incremental
patch to the current `VoicePipeline` loop. Do not patch V1 except to tag,
freeze, or document it.

## Source Notes

V1 reference implementation:

- Reference checkout: `/home/renan/voice-agent`.
- `README.md`: current feature matrix, setup flow, config files, supported
  local/cloud runtime combinations, and the fact that Windows support is WSL2
  only in V1.
- `voice-agent/app.py`: Textual app lifecycle, pipeline worker startup, model
  switching, interrupt/mute/reset controls, and shell-approval UI behavior.
- `voice-agent/pipeline.py`: current turn loop around the OpenAI Agents
  `VoicePipeline`; this is the clearest source for why V2 is a rewrite. V1
  records one segment, mutes capture while responding, cancels on manual
  interrupt, and saves partial assistant history.
- `voice-agent/audio.py`: `sounddevice` capture/playback, Silero VAD chunking,
  pre-roll/silence thresholds, and interruption-sensitive PortAudio behavior.
- `voice-agent/providers.py`: current OpenAI Agents SDK adapter, local/cloud
  provider glue, streaming TTS behavior, metrics, tool handling, and audio
  cleanup details.
- `voice-agent/runtimes.py` and `voice-agent/servers.py`: runtime registry,
  OS filtering, readiness checks, process startup/logging, llama.cpp idle
  unload, and orphan-process cleanup. Port the concepts, not the Python process
  plumbing.
- `config.toml`, `models.toml`, `preferences.toml.example`,
  `mcp_servers.toml.example`, and `llamacpp-models.ini.example`: config
  semantics to preserve or deliberately replace.

Use V1 as a behavior reference and migration corpus, not as V2 architecture.
The key break is that V1 is terminal-first, effectively single-user, and mutes
the recorder during assistant speech; V2 is server-first, web-first,
authenticated, sandboxed, and must keep listening while the assistant speaks.

NVIDIA NeMo voice-agent example:

- Uses a server/client architecture with a browser client.
- Uses Pipecat-style frame processors for VAD, STT, diarization, turn-taking, LLM, TTS, and output.
- Supports `allow_interruptions=True`.
- Uses Silero VAD parameters for start/stop detection.
- Adds a turn-taking service that can ignore backchannel phrases while the bot is speaking and interrupt when user speech is not a backchannel.
- Default config uses Parakeet Realtime EOU, Sortformer diarization, and local TTS/LLM services.

Local STT / TTS notes:

- V1's `onnx-asr` Parakeet integration proved CPU-friendly local STT is viable, but V2 should move away from `onnx-asr` so we can own streaming state, partial transcript events, target-language behavior, and UI transcript updates.
- Whisper.cpp should not be supported in V2. It was useful in V1, but low-latency local Whisper generally wants CUDA on Linux, which competes with the local LLM for VRAM. V2's local ASR should be CPU-only ONNX Nemotron so GPU/VRAM stays available for the LLM.
- NVIDIA `nvidia/nemotron-3.5-asr-streaming-0.6b` is a 600M-parameter cache-aware FastConformer-RNNT streaming ASR model. It supports 40 language-locales, punctuation/capitalization, optional automatic language detection, and runtime chunk sizes of 80ms, 160ms, 320ms, 560ms, and 1120ms.
- `onnx-community/nemotron-3.5-asr-streaming-0.6b-onnx-int4` is an ONNX INT4 export optimized for the 560ms chunk size and includes ONNX Runtime GenAI-style assets.
- `smcleod/nemotron-3.5-asr-streaming-0.6b-int8` is an ONNX INT8 layout aimed at `parakeet-rs`, with `encoder.onnx`, `decoder_joint.onnx`, `tokenizer.model`, `config.json`, 16 kHz mono float input, 560ms chunks, punctuation/capitalization, and no word-level timestamps.
- `parakeet-rs` supports Nemotron streaming, including multilingual mode and target-language selection. It is a useful reference implementation and possible fallback spike, but not the preferred V2 foundation if we can implement the runtime cleanly in Go.
- `yalue/onnxruntime_go` wraps ONNX Runtime for Go and is a plausible foundation for native Go inference, but a native Go Nemotron implementation still requires owning preprocessing, cache tensors, prompt/language inputs, RNNT decoding, tokenizer integration, and streaming state.
- Supertonic 3 ships multi-runtime ONNX examples including Go, is 99M parameters, supports 31 languages, runs on CPU, and outputs 44.1 kHz audio. V2 should call Supertonic directly from Go through ONNX Runtime rather than running a separate local TTS server.

OpenAI voice/realtime docs:

- The voice-agents guide distinguishes two architectures:
    - speech-to-speech live audio sessions for natural low-latency conversations, barge-in, natural turn taking, and realtime tool use;
    - chained voice pipelines when explicit STT -> agent -> TTS control is more important.
- The browser-oriented Realtime path uses a frontend `RealtimeSession`, usually over WebRTC with an ephemeral client secret from the app server.
- Python Agents SDK Realtime transport docs say server-side WebSocket is the default Python path; browser WebRTC is outside the Python SDK and should use the official Realtime WebRTC docs.
- Realtime WebRTC docs recommend WebRTC rather than WebSockets when connecting from browser/mobile clients because it gives more consistent performance.
- Realtime VAD supports `server_vad` settings including `threshold`, `prefix_padding_ms`, `silence_duration_ms`, `create_response`, and `interrupt_response`. It also documents `semantic_vad`, which classifies whether the user is done speaking.
- `semantic_vad` uses a semantic classifier over the user's utterance to decide whether the user is done, supports `eagerness = low | medium | high | auto`, and is less likely to cut off trailing/unfinished speech than silence-only VAD.
- Realtime conversation docs explicitly call out interruption mechanics such as `response.cancel`, truncating unplayed audio, and clearing output buffers depending on transport.
- The Realtime WebRTC docs support both a unified browser SDP exchange through the app server and ephemeral client secrets minted by the app server.
- The server-side controls docs describe sideband connections: browser/mobile can own the media WebRTC connection while the application server monitors the session, updates instructions, and responds to tool calls.
- The Agents SDK overview explicitly says to use the Responses API directly when the application wants to own the loop, tool dispatch, and state handling. That matches V2's need to own realtime state, interruption, sandboxing, and transport decisions.
- Relevant docs:
    - https://github.com/NVIDIA-NeMo/NeMo/tree/main/examples/voice_agent
    - https://openai.github.io/openai-agents-python/realtime/transport/
    - https://openai.github.io/openai-agents-python/
    - https://developers.openai.com/api/docs/guides/voice-agents
    - https://developers.openai.com/api/docs/guides/realtime-webrtc
    - https://developers.openai.com/api/docs/guides/realtime-websocket
    - https://developers.openai.com/api/docs/guides/realtime-vad
    - https://developers.openai.com/api/docs/guides/realtime-conversations
    - https://developers.openai.com/api/docs/guides/realtime-server-controls
    - https://openai.github.io/openai-agents-js/openai/agents/realtime/classes/openairealtimewebrtc/
    - https://openai.github.io/openai-agents-js/openai/agents/realtime/classes/openairealtimewebsocket/

Community agent SDK notes:

- `nlpodyssey/openai-agents-go` is a Go port of the OpenAI Agents Python SDK. It advertises Responses / Chat Completions support, MCP examples, hosted MCP, sessions, tool examples, and voice examples. Treat it as the best community SDK candidate to spike in V2, but not as a foundation assumption.
- `slb350/open-agent-sdk-rust` / `open-agent-sdk` is streaming-first Rust agent tooling aimed primarily at local OpenAI-compatible servers such as LM Studio, Ollama, llama.cpp, and vLLM. Because V2 is Go-only by default, treat this as background research, not an implementation target.
- The local v1 workspace did not have Go or Rust toolchains installed during this planning pass, so the language and SDK notes above are docs/ecosystem research, not build validation.
- Relevant links:
    - https://github.com/nlpodyssey/openai-agents-go
    - https://community.openai.com/t/create-a-open-ai-agent-with-go-lang/1145891
    - https://github.com/slb350/open-agent-sdk-rust
    - https://docs.rs/open-agent-sdk/latest/open_agent/
    - https://crates.io/crates/openai-agents-rust

Automation / callable agent notes:

- Codex can run as an MCP server with `codex mcp-server`. Its MCP surface exposes a `codex` tool for starting a Codex session and `codex-reply` for continuing one by thread id. This supports deterministic, reviewable multi-agent workflows where the orchestrator owns tool dispatch.
- Codex CLI also has `codex exec` for scripted/non-interactive runs. Useful flags for the adapter are `--json`, `--output-schema`, `--cd`, `--skip-git-repo-check`, `--sandbox`, `--ask-for-approval never`, and `--dangerously-bypass-approvals-and-sandbox` / `--yolo` when running inside an external sandbox. `--full-auto` is deprecated and should not be used.
- Codex supports subscription/ChatGPT authentication through `codex login`, API-key login through `codex login --with-api-key`, device-code auth through `codex login --device-auth`, and status checks through `codex login status`. For API-key based non-interactive runs, `CODEX_API_KEY` is supported by `codex exec`; for trusted ChatGPT automation, `CODEX_ACCESS_TOKEN` can be piped to `codex login --with-access-token`.
- Claude Code supports non-interactive `claude -p` / `--print`. It supports `--output-format json`, `--output-format stream-json --verbose --include-partial-messages`, `--json-schema`, `--allowedTools`, `--disallowedTools`, `--permission-mode`, `--max-turns`, and `--dangerously-skip-permissions`. Browser/subscription login uses `claude auth login`; status is `claude auth status`. API/key environment options include `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, and `CLAUDE_CODE_OAUTH_TOKEN`, with `ANTHROPIC_API_KEY` taking precedence over subscription login when present.
- Claude `--bare` is useful for deterministic API-key scripts, but it skips OAuth/keychain credentials and should not be used for subscription/browser-auth runs. For browser-auth-backed automations, prefer normal `claude -p` or `claude --safe-mode -p` if the installed version supports it.
- Cursor CLI supports browser login through `agent login`, status through `agent status`, and logout through `agent logout`. It also supports non-interactive `agent -p` / `--print`; `--force` or `--yolo` enables file modifications in print mode. It supports `--output-format json`, `--output-format stream-json`, and `--stream-partial-output`; API-key mode uses `CURSOR_API_KEY` or `--api-key`.
- Pi coding agent is a useful fourth callable agent for Automations because it can run against a custom OpenAI-compatible local provider. In V2, the Pi adapter should be local-only and should always use the host llama.cpp model, never a cloud provider, so users can run agent Automations without Codex, Claude, or Cursor accounts/subscriptions.
- Pi supports JSON/RPC-oriented operation suitable for a typed adapter. It also supports custom providers/models through `~/.pi/agent/models.json`, including OpenAI-compatible completion/response APIs, which lets the Go server generate a per-run Pi config pointed at the host llama.cpp endpoint with a dummy API key.
- Pi does not provide a built-in sandbox boundary. Run the whole Pi process inside the per-run Docker `sbx` sandbox, and disable or explicitly control local Pi extensions/skills/context loading unless the user/admin enables them, because Pi tools and extensions run with the Pi process permissions.
- Docker `sbx` is purpose-built for AI coding-agent sandboxes and should be the V2 sandbox runtime from the first implementation milestone. Its CLI surface includes `sbx create`, `sbx run`, `sbx exec`, `sbx kit`, `sbx policy`, `sbx secret`, `sbx cp`, and lifecycle commands. `sbx run` can launch known agents or `shell`, accept additional workspaces including read-only mounts, and set resources such as CPUs/memory through flags. `sbx policy` should drive network/host-service handling where practical. `sbx secret` should be used for supported service/registry secret injection when it preserves the product's per-user/sandbox scope, but the product-level per-user vault remains the source of truth for user secrets and grants.
- The `pi-sbx-llamacpp` guide is a relevant reference pattern: run the coding agent inside Docker `sbx`, keep llama.cpp on the host in router/server mode, and expose only the host local inference endpoint into the sandbox through a controlled bridge such as `host.docker.internal` plus allow-listed policy.
- For V2, prefer Go-owned typed adapters and optional MCP facade tools over asking the active LLM to write raw CLI invocations.
- Relevant links:
    - https://developers.openai.com/codex/guides/agents-sdk
    - https://developers.openai.com/codex/cli/features#scripting-codex
    - https://developers.openai.com/codex/cli/reference
    - https://developers.openai.com/codex/codex-manual.md
    - https://code.claude.com/docs/en/headless
    - https://code.claude.com/docs/en/cli-reference
    - https://code.claude.com/docs/en/authentication
    - https://cursor.com/docs/cli/headless
    - https://cursor.com/docs/cli/reference/authentication
    - https://cursor.com/docs/cli/reference/output-format
    - https://docs.docker.com/reference/cli/sbx/
    - https://docs.docker.com/reference/cli/sbx/run/
    - https://docs.docker.com/reference/cli/sbx/create/
    - https://docs.docker.com/reference/cli/sbx/policy/
    - https://docs.docker.com/reference/cli/sbx/secret/
    - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/index.md
    - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/usage.md
    - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md
    - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/models.md
    - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/providers.md
    - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/containerization.md
    - https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/security.md
    - https://raw.githubusercontent.com/cuolm/pi-sbx-llamacpp/refs/heads/main/README.md

Technology stack notes:

- shadcn/ui now has Base UI documentation and examples for its components alongside Radix. V2 should generate shadcn components against Base UI primitives by default, while treating the generated local components as the app's owned design system.
- Zustand is appropriate for frontend-only UI preferences and transient UI state, especially with persisted slices for theme, density, layout, selected devices, and composer/live-mode UI preferences. Do not use it as the source of truth for server data.
- TanStack Query should own frontend server-state fetching/caching/mutations. TanStack Router should own typed client-side routes and URL/search state. TanStack Table should own dense admin/user data grids.
- Automation editing should be an on-rails builder first, not a raw JSON editor. React Hook Form plus Zod is the default frontend form stack. The backend JSON Schema remains authoritative, and the frontend can use Ajv for immediate validation against the same schema.
- CodeMirror 6 is useful for read-only generated JSON previews, import repair, logs/artifacts, and advanced/admin diagnostics. It should not be the primary Automation editing surface.
- React Flow / xyflow is a plausible later tool for a visual DAG editor, but do not start V2's Automation UI with a freeform canvas. Start with structured forms for trigger, permissions, deterministic steps, agent steps, aggregation, and outputs.
- Gemini-native cloud adapters should use the Google Gen AI Go SDK. Gemini remains worth retaining because the API can offer useful free quota for some users. Keep Gemini as an optional cloud provider path, not a dependency of local mode.
- Relevant links:
    - https://ui.shadcn.com/docs/changelog/2026-01-base-ui
    - https://ui.shadcn.com/docs/forms/react-hook-form
    - https://zustand.docs.pmnd.rs/
    - https://zustand.docs.pmnd.rs/reference/integrations/persisting-store-data
    - https://tanstack.com/query/latest
    - https://tanstack.com/router/latest
    - https://react-hook-form.com/
    - https://zod.dev/
    - https://ajv.js.org/
    - https://codemirror.net/
    - https://reactflow.dev/
    - https://github.com/googleapis/go-genai

Native Windows support notes:

- V2 should treat native Windows as a first-class host, not as a WSL-only
  compatibility path. The practical first target is Windows 11 x64 because
  Docker `sbx` documents Windows support with Windows 11, x86_64, and Windows
  Hypervisor Platform enabled. Windows 10 and Windows on ARM can be considered
  later or supported only for feature subsets until the sandbox and local
  runtime story is validated there.
- Docker `sbx` currently documents Windows install through `winget install -h
Docker.sbx`, states Docker Desktop is not required, and uses
  `host.docker.internal` for sandbox access to host services. The V2 sandbox
  runner should use that host-service name across platforms and enforce the
  same allow-list semantics for `localhost:<port>` on the host gateway.
- Pion WebRTC v4 is a good fit for native Windows because it is pure Go, has no
  CGo dependency, and documents support for Windows, macOS, Linux, FreeBSD,
  iOS, Android, and WASM. Keep capture/playback in the browser; do not add a
  Go desktop microphone/speaker path for Windows.
- llama.cpp has native Windows distribution paths: upstream releases publish
  Windows x64 CPU, Windows arm64 CPU, Windows x64 CUDA, Vulkan, OpenVINO, SYCL,
  and HIP assets, and upstream install docs list `winget install llama.cpp`.
  Building from source on Windows is also documented through Visual Studio
  2022 / CMake, but V2 should prefer managed prebuilt downloads or winget over
  asking users to install a native C++ toolchain.
- ONNX Runtime itself is Windows-capable. Current docs say Windows 10 1809+
  and Windows 11 can run ONNX Runtime, with WinML recommended on Windows 11
  24H2+ for execution-provider selection. However, the Go integration still
  needs a real spike: `yalue/onnxruntime_go` works by manually loading the
  ONNX Runtime shared library / DLL and requires CGo plus a matching ORT C API
  version. That may be acceptable, but local ASR/TTS should not be considered
  Windows-ready until Nemotron and Supertonic both run through the chosen Go
  ORT binding on a native Windows host.
- Supertonic's Go example requires installing the ONNX Runtime C library. That
  means the V2 runtime manager must own ORT C library installation, version
  pinning, and DLL discovery on Windows rather than relying on a Homebrew-style
  system path.
- Avoid adding avoidable CGo dependencies to the server outside the local ONNX
  adapters. For SQLite, prefer a CGo-free driver such as `modernc.org/sqlite`
  unless a measured blocker appears; requiring GCC/MinGW just to build the
  control plane would make native Windows support fragile.
- Go's standard library is portable, but POSIX assumptions still leak easily:
  `os.UserConfigDir` maps to `%AppData%`, `os.UserCacheDir` maps to
  `%LocalAppData%`, `os.Chmod` only toggles Windows read-only behavior,
  `os.Chown` is unsupported on Windows, and `os.Interrupt` process signaling
  is not implemented the way POSIX code expects. V2 needs a small platform
  abstraction for paths, permissions, process-tree cleanup, and service
  installation instead of scattered OS checks.
- Windows secret storage should use DPAPI / Credential Manager or an ACL-guarded
  local master-key file. The Unix `0600` key-file rule in the secret-vault
  design must become an OS-specific permission check.
- Relevant docs:
    - https://docs.docker.com/ai/sandboxes/get-started/
    - https://github.com/docker/docs/blob/main/content/manuals/ai/sandboxes/usage.md
    - https://pkg.go.dev/github.com/pion/webrtc/v4
    - https://github.com/ggml-org/llama.cpp/releases
    - https://github.com/ggml-org/llama.cpp/blob/master/docs/install.md
    - https://github.com/ggml-org/llama.cpp/blob/master/docs/build.md
    - https://onnxruntime.ai/docs/get-started/with-windows.html
    - https://onnxruntime.ai/docs/install/
    - https://github.com/yalue/onnxruntime_go
    - https://github.com/supertone-inc/supertonic
    - https://pkg.go.dev/modernc.org/sqlite
    - https://pkg.go.dev/os
    - https://learn.microsoft.com/windows/win32/api/dpapi/nf-dpapi-cryptprotectdata

## Product Direction

V2 should be a local voice-agent product with a server-owned control plane and
browser-owned user interaction:

- User Web UI: default user client for phones, tablets, and browsers on localhost or LAN. It should feel like a chat app first, with a text composer and a prominent control to enter live voice mode.
- Admin Web UI: setup, backend selection, runtime health, logs, user/session management, sandbox policy, and restart controls.
- Headless mode: run only the server APIs and Realtime-compatible WebRTC endpoints for deployment or external UI experiments.

First-class host platforms:

- Windows 11 x64 native.
- macOS Apple Silicon.
- Linux x86_64, with Linux aarch64 where local runtime assets support it.

The first V2 implementation should keep those platforms visible in every
runtime, setup, path, and CI decision. Windows support is not a separate WSL
mode. WSL can remain a workaround for V1 or unsupported V2 host variants, but
native Windows is a product target for V2.

Windows 10 and Windows on ARM are not first-milestone full-support targets
because Docker `sbx` currently documents Windows 11 x64 prerequisites. They can
run a reduced V2 feature subset only if the installer and runtime health checks
clearly mark sandbox-dependent features unavailable.

Drop the Textual/TUI client for V2. V1 remains available for terminal-first
power-user workflows. V2 should be server-first and web-first.

Users should not choose STT / LLM / TTS backends directly. The admin portal owns
runtime selection and policy. User sessions inherit the active backend policy,
with later room for admin-defined user groups or per-user model policies.

Core user model: one persistent chat/session can be used through text or live
voice. Users can start in text, enter live mode with the existing chat context,
end the live call, and keep typing in the same session immediately or later.
They can also start in live mode, end the call, and continue the same
conversation through text. Text and voice are interaction modes over the same
conversation state, not separate products or separate histories.

## Architecture Recommendation

### Language And Runtime Recommendation

Use Go for the whole V2 application unless an early implementation spike shows
a concrete blocker.

Why Go fits the product:

- simple static deployment,
- efficient HTTP servers, WebRTC signaling, and event fanout,
- strong standard-library process management,
- good Docker client ecosystem,
- lower steady-state memory than a Python web app,
- straightforward concurrency for many session/log/event streams,
- cross-platform binaries for Windows, macOS, and Linux from one codebase,
- enough native/CGO reach to host ONNX Runtime-backed local STT/TTS adapters,
  while keeping CGo isolated to the local inference boundary instead of the
  control plane.

Do not split the V2 app into Go plus Rust by default. The project should have
one implementation language for server code, local adapters, sandbox control,
and benchmark tooling. A Rust component is only acceptable later if a benchmark
shows a concrete problem that cannot be solved cleanly in Go.

Avoid embedding Python in the Go orchestrator with PyO3, RustPython, or a
similar in-process bridge as the default architecture. If Python remains useful
for a provider or experiment, run it as an isolated sidecar with a framed local
protocol so it can be supervised, restarted, and measured independently.

### Agent Harness Direction

Do not assume V2 needs full OpenAI Agents SDK parity. The application needs
tight ownership of:

- realtime session state,
- interruption and playback truncation,
- tool dispatch and approval policy,
- Docker `sbx` sandbox routing,
- local/cloud transport differences,
- transcript persistence,
- text and live voice turns sharing one conversation history,
- admin observability.

Start with a small custom agent loop in Go:

1. maintain conversation state and compact/truncate it explicitly,
2. stream model deltas into the session event bus,
3. detect tool calls and route them through the approval/sandbox layer,
4. support cancellation at every blocking boundary,
5. emit deterministic events for UI, logs, and benchmark harnesses.

Use direct APIs where they are the better fit:

- OpenAI text/tool mode: Responses API directly, unless a Go SDK proves it saves substantial work without hiding cancellation/state details.
- Local LLM mode: OpenAI-compatible Chat Completions or Responses endpoint exposed by llama.cpp/vLLM/etc.
- Full-cloud OpenAI speech-to-speech: official OpenAI Realtime WebRTC from the browser, bypassing local STT/LLM/TTS and the chained local harness.

Spike `nlpodyssey/openai-agents-go` after the minimal harness exists. Adopt it
only if it cleanly supports streaming, cancellation, tool approvals, MCP, and
local OpenAI-compatible backends without fighting the V2 event model.

Do not use Pi, Codex, Claude Code, or Cursor as the realtime voice
orchestrator. They are callable Automation/chat-tool agents. The Go server's
OpenAI-compatible session layer remains responsible for STT/LLM/TTS
coordination, interruption, transcript persistence, and Realtime-like browser
transport. If the official Python OpenAI Agents SDK proves necessary for voice
coordination, treat that as a separate spike because it would reopen the
Go-only server decision.

### Conversation Modes

A `Session` is a durable conversation history with one or more turns. It is not
the same thing as a WebRTC call.

Supported interaction modes:

- **Text mode**: user sends typed messages through the chat composer. The server
  calls the configured LLM directly, streams text deltas to the UI, runs tools
  through the same approval/sandbox layer, and persists the turn.
- **Live mode**: user starts a WebRTC call attached to an existing session. The
  server loads the current conversation context into the local/mixed pipeline or
  full OpenAI Realtime session, streams voice turns, persists user transcripts
  and assistant text/audio metadata, and keeps the same session history.

Mode transitions:

- Text -> live: start a WebRTC call with the existing session context.
- Live -> text: end the call, commit any final transcript/assistant turn, and
  return to the same chat composer.
- Live -> later text: resuming a session should show the transcript/history and
  allow typed follow-up without rehydrating audio state.
- Text -> later live: resuming a session and entering live mode should provide
  the prior text conversation as model context.

The persistence layer should store canonical text for every turn. Voice turns
store STT transcript, assistant text, timing/audio metadata, and optional audio
artifact references; the model context should be reconstructed from canonical
text plus tool results, not from raw audio.

### Core Components

Build a central Go server process:

- server/control plane: owns auth, sessions, policy, model runtime supervision, tool execution, Docker `sbx` sandboxes, logs, and event fanout.
- user web app: text composer, browser microphone/playback, session list, shared conversation timeline, transcripts, live-mode controls, Automations, interrupt/mute/reset controls.
- admin web app: runtime selection, setup flows, backend logs, health, users, sessions, sandbox/automation policy, and restart controls.
- local adapters: Go packages for default STT/TTS plus process supervision for external model servers such as llama.cpp.

Potential new-repo shape:

```text
cmd/
  voice-agent-server/
internal/
  api/                  # HTTP routes, WebRTC signaling, middleware
  auth/                 # local users, admin bootstrap, sessions, tokens
  config/               # config files, env expansion, validation
  events/               # typed event schema + serialization
  platform/             # OS-specific paths, process groups/job objects,
                        #   permissions, key storage, install helpers
  sessions/             # session lifecycle, conversation state, persistence
  transports/
    realtime/           # local Realtime-like WebRTC + OpenAI token minting/sideband
  runtimes/             # backend registry + process supervision
  providers/
    stt/                # native Go Nemotron streaming ASR adapter
    llm/                # OpenAI / local OpenAI-compatible clients
    tts/                # native Go Supertonic ONNX adapter
  tools/                # MCP, shell, hosted tool bridge
  automations/          # schedules, workflow DAGs, run records, JSON import/export
  sandbox/              # per-user Docker sbx workspaces and command execution
  secrets/              # per-user secret vault and sandbox injection
  agents/               # Codex / Claude Code / Cursor / Pi adapters and auth broker
  logs/                 # process log capture, retention, streaming
  metrics/              # latency and quality measurement
web/
  user/
  admin/
bench/
  fixtures/
  harness/
```

Do not start with this exact tree unless it still matches the code when
implementation begins. The important change is ownership: the server owns
policy, sessions, backends, tools, sandboxes, logs, and event fanout; browser
clients own capture/playback for user sessions.

### Platform Support And Packaging

Native Windows support has to be designed from the first V2 commit. Do not
write POSIX-first code and plan to port it later.

Support tiers:

- **Tier 1 / full support:** Windows 11 x64, macOS Apple Silicon, Linux x86_64.
- **Tier 1 when assets exist:** Linux aarch64 for cloud/text/server features and
  local runtimes with published binaries or validated source builds.
- **Tier 2 / feature-subset support:** Windows 10 and Windows on ARM until
  Docker `sbx`, llama.cpp, ONNX Runtime, and CI coverage are validated for the
  same feature set.

The Windows 11 x64 full-support target includes:

- local web server, user UI, admin UI, auth, sessions, logs, and SQLite storage,
- text chat and full OpenAI Realtime browser WebRTC mode,
- local/mixed WebRTC mode once Nemotron and Supertonic pass the Windows ONNX
  spike,
- managed llama.cpp local LLM with Windows release assets or winget fallback,
- Docker `sbx` sandboxes for chat tools and Automations,
- per-user secret vault and agent-auth state storage,
- callable-agent Automations through `sbx`.

Packaging and setup:

- Replace V1-style Bash setup scripts with Go-owned setup jobs and CLI commands,
  e.g. `voice-agent setup`, `voice-agent runtime install llamacpp`,
  `voice-agent runtime doctor`, and `voice-agent bench`.
- Runtime installers should download, verify, extract, and register runtime
  assets into app-managed directories. They should not require users to run
  arbitrary shell scripts.
- Use platform-specific installers only when they make the user path simpler:
  `winget` for `sbx` and optionally llama.cpp on Windows, Homebrew where useful
  on macOS, apt/dnf/pacman/zypper only for Linux system packages.
- Prefer prebuilt llama.cpp release assets on Windows. Source build through
  Visual Studio 2022 / CMake is an admin-visible fallback, not the normal path.
- ONNX Runtime C libraries/DLLs for Nemotron and Supertonic should be pinned,
  downloaded, checksummed, and loaded from an app-managed runtime directory.
  The Admin UI should show the ORT version, execution provider, DLL path, and
  health status.
- Keep the main server buildable without MinGW/GCC whenever local ONNX adapters
  are disabled. If CGo is required for local ASR/TTS, isolate it behind build
  tags or a small internal package so cloud-only/headless builds stay simple.
- Use `os.UserConfigDir`, `os.UserCacheDir`, and equivalent platform helpers
  through `internal/platform`, not repo-relative state paths. Keep user data,
  cache/model downloads, logs, runtime binaries, secret vault material, and
  temporary run state in distinct directories.
- Use `filepath`, `fs.ValidPath` where relevant, and URL/path encoding helpers.
  Never assume `/`, `~`, executable bits, symlinks, case-sensitive paths, or
  POSIX ownership semantics.
- Process supervision must have platform-specific implementations: process
  groups/signals on Unix, Windows Job Objects or an equivalent process-tree
  cleanup path on Windows. `exec.CommandContext` alone is not enough for child
  processes spawned by model servers or agent setup flows.
- Shell execution should be argv-first and mediated by sandbox policy. Avoid
  `/bin/sh -c` or `cmd.exe /C` except for explicitly allowed shell-string tools.
- Logs and setup output should normalize Windows CRLF line endings and redact
  Windows-style paths/secrets as carefully as POSIX paths.
- The benchmark harness and admin diagnostics must run on every Tier 1 host.
  Any benchmark helper that needs terminal interaction should have a
  non-interactive mode so Windows CI and PowerShell users can run it.

### Backend And Sandbox Ownership

Do not run one copy of local STT/LLM/TTS per user by default. Shared model
backends are the only practical path on local hardware because model memory and
VRAM dominate resource use.

Use Docker `sbx` isolation for user tools and workspaces from the first V2
sandbox milestone:

- one persistent workspace/state area per user, optionally one per session or Automation,
- short-lived `sbx run` / named `sbx create` sandboxes for shell/code/tool invocations,
- generated `sbx` kits/templates for the allowed CLI tools, MCP tools, Pi config, agent config, and policy defaults,
- `sbx --clone` for repo-oriented agent work when possible, so the host checkout is read-only and changes flow back through sandbox artifacts/remotes instead of direct host writes,
- explicit `--cpus`, `--memory`, process, timeout, workspace, and read-only mount limits per run,
- no default access to model weights, server config, API keys, Docker socket, or other users' files,
- network access controlled by Automation/user policy and implemented through `sbx policy` / allow-listed proxy rules where available,
- log every sandbox invocation and retain stdout/stderr/exit status for the admin portal,
- background janitor for stale sandboxes, volumes/state, and temporary files.

Windows-specific sandbox requirements:

- Treat `sbx.exe` as the Windows sandbox runtime. The setup doctor should detect
  whether it is installed, whether the user is signed in, whether Windows
  Hypervisor Platform is enabled, and whether a trivial `sbx run shell` can
  start.
- Do not require Docker Desktop for Windows if `sbx` does not require it. If a
  future `sbx` version or enterprise policy changes that, surface it as a
  runtime health error rather than silently falling back to unsandboxed tools.
- Use app-managed Windows paths for persistent user workspaces, then pass them
  through the `sbx` workspace/mount mechanism. Validate path translation with
  spaces, long paths, Unicode names, and case-insensitive collisions.
- Test `sbx --clone`, `sbx cp`, generated kits/templates, policy files, and
  secret injection on Windows before marking Automations supported there.
- Host inference access should use the same `host.docker.internal:<port>` path
  on Windows, macOS, and Linux where `sbx` supports it. The server-owned gateway
  still authorizes the specific host service; the hostname alone is not the
  security boundary.
- Agent browser-auth setup containers must work from a Windows browser even
  though the auth command runs inside `sbx`. Device-code and paste-code flows
  are mandatory fallbacks because localhost callback URLs inside a sandbox may
  not resolve to the user's Windows browser.

This sandbox boundary applies to work initiated by the main session model too,
not only to Automations or external coding agents. In text mode and live mode,
the shared LLM/STT/TTS runtimes may be admin-owned infrastructure, but any
user-scoped side effect requested by the model must be mediated through typed
tools that execute inside that user's `sbx` context. Examples include shell
commands, file reads/writes, repo checkouts, local MCP tools, HTTP requests,
agent calls, and secret-backed CLI invocations. A model response by itself
should never receive direct host filesystem access, raw host environment
variables, another user's workspace, or another user's secrets.

Host inference access from sandboxes should be narrow and explicit. Pi and any
future local-only agent should reach llama.cpp through a server-owned host
inference gateway or allow-listed `host.docker.internal:<port>` rule, not broad
host networking. The gateway should expose only the local OpenAI-compatible LLM
endpoint needed by that run and should not expose arbitrary host services.

Sandbox storage should be persistent where the user expects durable work, but
ephemeral for container implementation details:

- Persist user-owned workspaces as app-managed `sbx` mounts/volumes, not as
  mutated container root filesystems. The root filesystem should be recreated
  from the admin-controlled kit/template so tooling stays reproducible and
  stale state does not become a hidden permission boundary.
- Provide a default durable workspace per user for files, checked-out repos,
  uploads, long-lived project state, and user-promoted Automation artifacts.
- Optionally provide durable per-session workspaces so a chat can keep working
  files across text/live turns and across server restarts without mixing with
  other sessions.
- Treat Automation run workspaces as ephemeral by default. Persist immutable run
  logs, selected artifacts, final outputs, and any files the workflow explicitly
  promotes into the user's durable workspace.
- Keep provider auth state and agent state in separate per-user encrypted state
  areas, not in the normal workspace. Mount those only into auth setup containers
  and agent/tool runs that were explicitly granted access.
- Inject secrets as temporary env vars or mounted files for one run/tool call.
  Secret material should not be written into durable workspaces unless the user
  explicitly chooses to create such a file, and the UI should warn before doing
  that.
- Enforce per-user quotas, retention controls, export/delete flows, and admin
  visibility into storage usage without exposing file contents or secrets.

Hosted model backends, local model servers, and setup scripts remain admin-owned
shared infrastructure. User-level isolation applies to tools, files, sessions,
and credentials. Admin/runtime setup tasks may run outside user sandboxes, but
they must not be reachable as user tools unless wrapped in explicit admin-only
operations.

### User Sandbox Environment

Each user should have a Sandbox Environment settings area that is independent
from any single chat session or Automation. This is where the user configures
the tools their sandboxes can use when they are actively chatting with the LLM
and when Automations run unattended.

User-configurable sandbox settings:

- available CLI tools and versions, subject to admin policy,
- MCP servers available to that user's sandboxes,
- default network policy for user-initiated chat tools and Automations,
- default writable mounts/workspace layout,
- secret grants and environment variable names,
- agent CLI authentication profiles for Codex, Claude Code, and Cursor CLI,
- local Pi agent availability when admin policy enables Pi and host llama.cpp is available.

Agent authentication must support both browser/subscription login and API-key
login for account-backed agent CLIs. Many users will prefer subscription-plan
auth because API-key billing can be much more expensive for heavy agent use. Pi
is the exception: it is local-only, uses the host llama.cpp model, and should not
require a user agent account or cloud API key.

Agent auth broker:

- The server exposes a user-only agent setup flow in the Sandbox Environment page.
- For each provider, the user can choose browser/subscription auth or API-key/secret auth.
- Browser auth starts a short-lived auth setup container with the user's persistent agent state mounted, network enabled, and only the selected CLI available.
- The server runs the provider login command in a PTY so interactive prompts, browser URLs, device codes, and "paste code" prompts can be detected.
- The frontend streams a sanitized view of the login output, highlights detected URLs/codes, and lets the user open the URL in a new tab. If the CLI asks for a code to be pasted back, the UI provides an input that writes to the PTY.
- On success, the server runs the provider's status command and records the auth profile as available for that user.
- The resulting CLI credential state is treated as secret material. It is never shown in admin UI, logs, Automation exports, or run artifacts.
- Logout/re-auth flows should run the provider logout command and delete that user's stored provider state after confirmation.

Provider setup commands:

- Codex browser/subscription auth: `codex login`, with `codex login --device-auth` available when the auth container cannot receive the localhost callback. Status: `codex login status`. Use a per-user `CODEX_HOME`; for containerized auth, prefer `cli_auth_credentials_store = "file"` so the auth state lives in the mounted encrypted state directory rather than an unavailable host keyring.
- Codex API-key auth: either pipe a vaulted key to `codex login --with-api-key` for persisted API-key login, or inject `CODEX_API_KEY` only for `codex exec` runs. Do not require `OPENAI_API_KEY` for Codex agent runs unless the adapter explicitly uses `codex login --with-api-key` from that secret.
- Claude browser/subscription auth: `claude auth login`; status: `claude auth status`. Use a per-user `CLAUDE_CONFIG_DIR` so container-side credentials land under that mounted state directory. Do not set `ANTHROPIC_API_KEY` in this profile because it takes precedence over subscription credentials.
- Claude API-key/token auth: support `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, and `CLAUDE_CODE_OAUTH_TOKEN` as vaulted secret-backed environment variables. `CLAUDE_CODE_OAUTH_TOKEN` comes from `claude setup-token` and is subscription-backed, but it is still a secret env profile, not browser state.
- Cursor browser auth: `agent login`; status: `agent status`; logout: `agent logout`. Treat Cursor's local credential store as opaque provider state mounted from the user's encrypted agent state.
- Cursor API-key auth: inject vaulted `CURSOR_API_KEY`, or use `--api-key` only if the adapter can avoid exposing the value in process lists/logs.
- Pi local auth: none. Generate Pi `models.json` / `settings.json` per run or per user-state snapshot so Pi points at the server-approved host llama.cpp endpoint. Do not mount a user's host `~/.pi/agent` into sandboxes by default.

Agent state storage:

- Maintain per-user, per-provider agent state separate from the normal workspace. Mount it read/write only into auth setup containers and agent run containers for that user.
- Pi's generated config/state is not credential material by default, but it is still user data. Keep it under the same per-user sandbox state boundary and generate provider config from server policy instead of trusting host `~/.pi/agent` files.
- Prefer encrypted state snapshots or encrypted per-user volumes using the same secret-vault boundary as user secrets. A database-only compromise should not reveal browser-auth tokens.
- Agent runs may refresh provider tokens. After a run exits, persist any updated state atomically.
- Admin UI can show coarse status such as "Codex authenticated" or "Claude not configured" but must not expose credential files, tokens, OAuth URLs after completion, or device codes.
- Agent setup logs should be short-lived and aggressively redacted. Login URLs and device codes are visible only to the requesting user during the active setup flow.

Automation agent eligibility:

- When creating or editing an Automation, the UI can only offer Codex, Claude Code, or Cursor if that user has either a valid browser/subscription auth profile or the required vaulted API-key/token profile.
- The UI can offer Pi when admin policy allows `agent.pi.run`, the active local LLM backend exposes a llama.cpp-compatible endpoint, and the Automation sandbox policy allows host inference access. Pi must never require or use a cloud provider in V2.
- Validation should fail if an Automation references an unavailable agent auth profile, a missing secret ref, or a callable agent that admin policy does not allow.
- Runs should record which auth profile type was used (`browser_state`, `api_key`, `oauth_token`, `local_llamacpp`, etc.) without storing credential values.

### Runtime Lifecycle And Idle Footprint

The Go server should be able to run indefinitely with a small idle footprint.
When no text chat, live voice session, or Automation run needs a model, local
model weights should not occupy CPU/GPU memory.

Runtime policy:

- The Go server, scheduler, auth/session APIs, admin UI, and lightweight health checks stay up.
- Llama.cpp should keep its V1-style idle unloading behavior (`--sleep-idle-seconds` or equivalent) so local LLM weights leave VRAM/RAM after inactivity.
- Nemotron ASR should lazy-load when a live voice session starts, stay warm while sessions are active, and unload after an idle TTL.
- Supertonic TTS should lazy-load when synthesis is first needed, stay warm while sessions are active, and unload after an idle TTL.
- Text-only chat and most Automations should not load STT/TTS.
- Full OpenAI Realtime mode should not load local STT/LLM/TTS unless another local/mixed session or Automation needs them.
- Server shutdown should stop all runtime processes, Automation runs, sandboxes, and local model workers. Nothing should linger in the background after the server exits.

Admin should be able to configure idle TTLs and see what is currently loaded.

### Automations

Automations are user-owned scheduled workflows that can run while the server is
up, wake the configured model/tool stack, execute work in the user's sandbox,
and produce artifacts or external side effects.

Use the term `Automations` in code and UI. Do not call them "scheduled jobs" in
user-facing surfaces.

Core behavior:

- Automations only run while the Go server is running.
- On server restart, the scheduler resumes enabled Automations from persisted definitions and computes the next due run.
- If the server was down during a scheduled time, the default missed-run policy is `skip`. Allow `run_once` as an explicit opt-in. Defer catch-up/backfill execution until a real workflow needs it because it can create surprising external side effects.
- Each run creates an immutable run record with input snapshot, step logs, model calls, tool calls, spawned agent runs, artifacts, final output, timings, status, and error details.
- Runs execute inside per-user/per-automation sandboxes and inherit that Automation's permission profile.
- Deterministic trigger/filter steps should be supported so cheap checks can happen before waking LLMs or callable agents.

Automations should be represented as JSON conforming to a versioned schema.
Generated database fields such as internal id, owner user id, created/updated
timestamps, last run, and next run should not be required in import/export JSON.
Support:

- manual UI creation/editing,
- import/export as JSON,
- LLM-assisted creation where the active server LLM produces Automation JSON from a natural-language request.

LLM-assisted creation flow:

1. Explain the Automation JSON schema to the model.
2. Ask the model to produce only JSON.
3. Validate against the schema.
4. If invalid, feed validation errors back and retry up to a limit.
5. Show the validated Automation to the user for review before enabling it.

Initial `automation.v1` shape:

```json
{
    "schema_version": "automation.v1",
    "name": "GitHub PR review requests",
    "description": "Check for requested PR reviews and draft a combined review.",
    "enabled": false,
    "timezone": "America/New_York",
    "trigger": {
        "type": "interval",
        "every": "5m"
    },
    "missed_run_policy": "skip",
    "concurrency": {
        "policy": "skip_if_running",
        "max_parallel": 1
    },
    "budget": {
        "timeout_seconds": 1800,
        "max_model_calls": 12,
        "max_agent_runs": 4,
        "max_log_bytes": 10485760,
        "max_artifact_bytes": 52428800
    },
    "sandbox": {
        "mode": "granular",
        "network": "enabled",
        "allowed_host_services": ["llamacpp"],
        "allowed_cli_tools": [
            "git",
            "gh",
            "codex",
            "claude",
            "cursor-agent",
            "pi"
        ],
        "allowed_tools": [
            "github.notifications",
            "github.pr_checkout",
            "agent.codex.exec",
            "agent.claude.run",
            "agent.cursor.run",
            "agent.pi.run"
        ],
        "writable_paths": ["workspace", "tmp", "artifacts"],
        "secret_refs": ["github_token"],
        "agent_auth_refs": [
            "codex_browser",
            "claude_browser",
            "cursor_browser"
        ],
        "resources": {
            "cpus": 4,
            "memory_mb": 8192,
            "pids": 512
        }
    },
    "steps": [
        {
            "id": "find_review_requests",
            "type": "tool",
            "tool": "github.notifications",
            "with": {
                "reasons": ["review_requested"],
                "include_participating": true
            }
        },
        {
            "id": "skip_if_empty",
            "type": "condition",
            "if": {
                "input": "find_review_requests.items",
                "op": "is_empty"
            },
            "then": ["finish_noop"],
            "else": ["review_each_pr"]
        },
        {
            "id": "finish_noop",
            "type": "finish",
            "status": "skipped",
            "message": "No matching review requests."
        },
        {
            "id": "review_each_pr",
            "type": "for_each",
            "items_from": "find_review_requests.items",
            "steps": [
                {
                    "id": "checkout_pr",
                    "type": "tool",
                    "tool": "github.pr_checkout",
                    "with": {
                        "notification": "${item}"
                    }
                },
                {
                    "id": "agent_reviews",
                    "type": "parallel",
                    "steps": [
                        {
                            "id": "codex_review",
                            "type": "agent_cli",
                            "adapter": "codex",
                            "workspace_from": "checkout_pr.workspace",
                            "prompt_template": "Review this PR for correctness, regressions, security issues, and missing tests.",
                            "output_schema_ref": "schemas/pr_review.v1.json"
                        },
                        {
                            "id": "claude_review",
                            "type": "agent_cli",
                            "adapter": "claude",
                            "workspace_from": "checkout_pr.workspace",
                            "prompt_template": "Review this PR for correctness, regressions, security issues, and missing tests.",
                            "output_schema_ref": "schemas/pr_review.v1.json"
                        },
                        {
                            "id": "cursor_review",
                            "type": "agent_cli",
                            "adapter": "cursor",
                            "workspace_from": "checkout_pr.workspace",
                            "prompt_template": "Review this PR for correctness, regressions, security issues, and missing tests.",
                            "output_schema_ref": "schemas/pr_review.v1.json"
                        },
                        {
                            "id": "pi_review",
                            "type": "agent_cli",
                            "adapter": "pi",
                            "workspace_from": "checkout_pr.workspace",
                            "prompt_template": "Review this PR for correctness, regressions, security issues, and missing tests.",
                            "output_schema_ref": "schemas/pr_review.v1.json"
                        }
                    ]
                },
                {
                    "id": "combine_reviews",
                    "type": "llm",
                    "inputs": ["agent_reviews"],
                    "prompt_template": "Merge the review reports, remove duplicates, verify claims, and produce a final PR review.",
                    "output_schema_ref": "schemas/final_pr_review.v1.json"
                },
                {
                    "id": "post_review",
                    "type": "tool",
                    "tool": "github.pr_comment",
                    "with": {
                        "pr": "${item.pr}",
                        "body_from": "combine_reviews.markdown"
                    }
                }
            ]
        }
    ],
    "outputs": [
        {
            "type": "artifact",
            "from_step": "combine_reviews",
            "name": "final-review.md"
        }
    ]
}
```

Workflow model:

- Trigger: cron, fixed interval, manual run, and later webhook/event triggers.
- Timezone: explicit per Automation.
- Inputs: shell command, HTTP request, MCP tool, file read, API query, or deterministic GitHub/CLI checks. Prefer argv-style commands over shell strings for deterministic execution and safer validation.
- Conditions: run/skip based on previous step output, JSONPath-like selectors, exit code, text match, or model classifier.
- Steps: shell command, HTTP request, typed tool, MCP tool, LLM call, spawned callable-agent adapter, parallel agent group, for-each loop, aggregation/verification step.
- Outputs: artifact, notification, file write, HTTP request, shell command, or final model-written summary.
- Concurrency: skip if running, queue one, allow parallel up to N, or cancel previous.
- Budgets: max wall time, max model calls/tokens if measurable, max spawned agents, max artifact size, max log size.
- Host services: explicit allow-list for server-owned local endpoints that a sandbox can reach, initially only `llamacpp` for Pi and other local-only agent paths.

Initial deterministic tools:

- `github.notifications`: list and filter notifications/review requests using `gh` or the GitHub API.
- `github.pr_checkout`: create a clean per-run checkout for a PR.
- `github.pr_comment`: post or draft a PR comment from an artifact.
- `http.request`: bounded HTTP call with method, URL, headers, body, timeout, and response capture policy.
- `shell.exec`: argv-only process execution by default, with shell-string execution gated by sandbox policy.
- `mcp.call`: call an allow-listed MCP tool with JSON arguments.
- `agent.codex.exec`, `agent.claude.run`, `agent.cursor.run`, `agent.pi.run`: normalized wrappers around the supported callable agents.

Permission profiles:

- **Unrestricted sandbox**: the Automation can use any available CLI tool in the sandbox and make arbitrary network requests permitted by the sandbox runtime. Host services such as llama.cpp should still be explicitly listed so unrestricted internet/network access does not imply unrestricted host access. It still cannot escape the container, access other users' workspaces, or access secrets not granted to that Automation.
- **Granular sandbox**: user selects allowed network access, allowed host services, allowed CLI tools, allowed MCP tools, mounted secrets, mounted agent auth profiles, writable paths, CPU/memory/process limits, and timeout.

Callable agent support:

- First-class adapters: Codex CLI, Claude Code, Cursor CLI, and Pi coding agent.
- Do not make the LLM construct raw CLI commands for these tools. The Automation runner should own typed adapters and expose them as internal tools and, where useful, as an MCP server/tool facade. The model can request `agent.codex.exec` or `agent.pi.run` with structured arguments; Go code builds the actual process invocation, environment, timeout, output schema, and artifact capture.
- Codex can also run as an MCP server (`codex mcp-server`), which is useful when the active LLM is MCP-capable and should call Codex as a tool. Keep a direct `codex exec` adapter as the primary Automation path because it is easier to budget, stream, cancel, and normalize.
- Run Codex, Claude Code, and Cursor in headless/autonomous modes inside the Automation sandbox. Docker `sbx` is the primary security boundary; CLI "yolo" or bypass-permission modes only remove the agent CLI's interactive prompts inside that already-isolated environment.
- Run Pi inside Docker `sbx` too. Pi is the local/account-free callable agent path and should always use the host llama.cpp model through the allow-listed host inference gateway. Do not allow Pi to select a cloud provider in V2.
- Initial Codex adapter: use `codex exec` with `--json`, `--cd <workspace>`, `--skip-git-repo-check`, and `--output-schema <schema>` when structured output is requested. Do not use deprecated `--full-auto`. For the unrestricted/highest-autonomy profile, use `--dangerously-bypass-approvals-and-sandbox` / `--yolo` only inside the per-run Docker `sbx` sandbox; otherwise prefer `--sandbox workspace-write --ask-for-approval never`.
- Initial Claude adapter: use `claude -p` with `--output-format stream-json --verbose --include-partial-messages` for streamed progress, `--output-format json --json-schema <schema>` for structured terminal output, `--max-turns` from the Automation budget, and `--permission-mode bypassPermissions` or `--dangerously-skip-permissions` only inside the per-run Docker `sbx` sandbox. For subscription/browser-auth runs, do not use `--bare`; prefer `--safe-mode` if available and tested because it keeps auth working while reducing local customization loading. Use `--allowedTools` / `--disallowedTools` for granular profiles when we want Claude's own policy layer to mirror the sandbox.
- Initial Cursor adapter: use `agent -p` / `--print`; add `--force` or `--yolo` for write-capable runs; use `--output-format json` for final results or `--output-format stream-json --stream-partial-output` for progress. Provide `CURSOR_API_KEY` through the per-user secret injection path, not global process env.
- Initial Pi adapter: generate a Pi config for the run with a local provider pointing at `http://host.docker.internal:<llamacpp-port>/v1` or the server-owned inference gateway, `api = "openai-completions"` unless a spike proves `openai-responses` is better for the active llama.cpp build, and a dummy local API key. Prefer `pi --mode rpc` for controlled runs so the adapter can stream events, steer/cancel, and collect state. Use one-shot JSON/print mode only for simple tasks. Disable Pi extensions, skills, prompt templates, and context-file loading by default unless the user/admin explicitly enables them inside the sandbox policy.
- Each adapter should emit a normalized `AgentRunResult` containing status, final text, structured output if present, changed files, tool calls if parseable, token/cost metadata if available, raw stdout/stderr artifact paths, and duration.
- Callable agents inherit the Automation's sandbox restrictions. If network, host inference, or a CLI is not allow-listed, the spawned agent cannot bypass that.
- Treat CLI flags, auth requirements, output formats, and autonomy modes as versioned adapter capabilities with health checks because these tools change over time.

Example GitHub code-review Automation:

1. Interval trigger every 5 minutes.
2. Deterministic `gh` query checks for relevant GitHub notifications or PR review requests.
3. If none match, finish without waking the LLM.
4. For each matching PR, create a sandbox run, fetch the repo/PR, and spawn Codex, Claude, Cursor, and/or Pi agents in parallel according to the user's configured agent availability and sandbox policy.
5. Save each agent report as an artifact.
6. Run an aggregation/verification step where agents or the configured LLM review each report for accuracy.
7. Produce a final combined review.
8. Post through `gh` or save as a draft artifact according to the Automation's sandbox permissions and configured output step.

### Default Local Backends

Default local STT should target Nemotron 3.5 ASR streaming, not `onnx-asr`.
It should be the only local ASR runtime in V2; do not carry forward
Whisper.cpp support.
It should lazy-load when live voice starts, stay warm while voice sessions are
active, and unload after an admin-configured idle TTL when no sessions/runs need
ASR.

Nemotron is intended to be available on every Tier 1 host, including native
Windows 11 x64, but only after a Windows ONNX Runtime spike proves the chosen Go
binding can load the model, manage streaming cache tensors, and run with stable
latency from an app-managed ORT DLL. Until that spike passes, Windows can still
support text mode and full OpenAI Realtime mode, but local/mixed voice mode must
show local ASR as unavailable.

Default local TTS should target Supertonic 3 directly in Go:

- lazy-load ONNX Runtime and Supertonic assets when synthesis is first needed,
- keep one shared model instance or bounded worker pool according to benchmark results,
- unload after an admin-configured idle TTL when no sessions/runs need TTS,
- synthesize through an internal Go API, not a local HTTP hop,
- emit 44.1 kHz audio frames/events directly to the session output path,
- avoid WAV container encode/decode on the hot path unless a browser/download endpoint explicitly asks for WAV,
- expose model load, warmup, synthesis latency, and failure events to the admin UI.

Supertonic is also a Tier 1 local runtime target for native Windows 11 x64, with
the same gate: the Go adapter must load pinned ONNX Runtime DLLs from the
runtime directory, synthesize through the native API, and pass cold/warm latency
benchmarks on Windows before local TTS is advertised as supported there.

Local llama.cpp remains the default local LLM server. The V2 runtime manager
should support Windows by installing either upstream Windows release assets or a
known-good winget package, then launching `llama-server.exe` with the same
router/model-preset/idle-unload policy used on macOS/Linux. CUDA, Vulkan,
OpenVINO, SYCL, and HIP variants should be treated as runtime capabilities
detected by the setup doctor, not assumed by OS alone.

Drop Kokoro-FastAPI and Qwen3-TTS support from V2. Keep cloud TTS paths through
the cloud provider layer.

### Event Model

Replace the current single `_run_vad()` loop with an event-driven frame pipeline.

Every event should include at least:

- `event_id`
- `session_id`
- `user_id` when authenticated
- `turn_id` when attached to a turn
- monotonic server timestamp
- sequence number within the session
- source (`client`, `server`, `runtime`, `sandbox`, `openai_realtime`, etc.)

Audio events should also include sample rate, channel count, sample format,
frame count, and playback/capture cursor metadata when available. Without this,
interruption and `ConversationTruncated` cannot be made reliable.

Core events:

- `SessionCreated`
- `SessionResumed`
- `ModeChanged`
- `UserTextSubmitted`
- `AudioInputFrame`
- `AudioOutputFrame`
- `VADStarted`
- `VADStopped`
- `ASRPartial`
- `ASRFinal`
- `TurnStarted`
- `TurnCommitted`
- `AgentTextDelta`
- `AgentReasoningDelta`
- `AgentTextCompleted`
- `ToolCallStarted`
- `ToolCallFinished`
- `ToolApprovalRequested`
- `ToolApprovalResolved`
- `TTSStarted`
- `PlaybackStarted`
- `PlaybackProgress`
- `PlaybackStopped`
- `UserInterrupted`
- `ConversationTruncated`
- `RuntimeStarting`
- `RuntimeReady`
- `RuntimeLog`
- `AutomationScheduled`
- `AutomationRunStarted`
- `AutomationRunStepStarted`
- `AutomationRunStepFinished`
- `AutomationAgentSpawned`
- `AutomationArtifactCreated`
- `AutomationRunFinished`
- `AgentAuthSetupStarted`
- `AgentAuthSetupOutput`
- `AgentAuthSetupUrlDetected`
- `AgentAuthSetupInputRequested`
- `AgentAuthSetupFinished`
- `AgentAuthStatusChanged`
- `SandboxStarted`
- `SandboxFinished`
- `ErrorEvent`

Each provider/runtime should consume and emit events through explicit
interfaces. Avoid drawing UI from providers. User and admin UIs subscribe to
session, runtime, log, and sandbox event streams.

### Transport Strategy

Use WebRTC from the first V2 milestone. The only way to talk to the agent in V2
is through the browser voice UI.

Why:

- OpenAI recommends WebRTC rather than WebSockets for browser/mobile Realtime clients.
- Browser/WebRTC gives us echo cancellation, noise suppression, automatic gain control, jitter buffering, device switching, and playback/capture APIs that are directly relevant to barge-in.
- Supporting WebSocket audio and WebRTC audio in parallel would split the latency/echo/debug surface before we know the product works.
- A browser-only voice path matches the product direction: V2 is web-first, and V1 remains the terminal-first fallback.

#### Local Realtime-Compatible Endpoint

The Go server should expose a local Realtime-like WebRTC endpoint for local and
mixed local/cloud pipelines. The target is practical frontend compatibility, not
a promise to clone every OpenAI Realtime API field.

Shape:

1. Browser authenticates to the local app server.
2. Browser creates a WebRTC peer connection and sends an SDP offer to the local server.
3. Go server answers the SDP offer using a Go WebRTC stack such as Pion.
4. Browser sends microphone media over WebRTC audio tracks.
5. Browser and server exchange JSON events over `RTCDataChannel`.
6. Server sends synthesized audio back over WebRTC audio tracks.

The browser session client should be able to switch base targets:

- local Realtime-like endpoint for local/mixed STT -> LLM -> TTS,
- official OpenAI Realtime endpoint for full-cloud speech-to-speech.

Where practical, mirror the official OpenAI WebRTC flow shape: an SDP
offer/answer endpoint, a call/session ID, audio tracks for media, and a
Realtime-style data channel for JSON events. The frontend should call one
high-level session adapter whether the target is local or official OpenAI
Realtime.

Keep the local event schema intentionally close to OpenAI Realtime concepts:

- session/config updates,
- turn-detection settings,
- partial/final transcript events,
- response text/audio deltas,
- tool call and tool result events,
- response cancel / interruption events,
- error and lifecycle events.

Do not block local implementation on full OpenAI API parity. Implement the
subset required by the V2 browser UI, admin observability, tools, interruption,
and transcript persistence.

#### Execution Modes

Admin can choose one of these voice execution modes:

- **Local/mixed Realtime-like mode**: browser connects to the Go server's local
  WebRTC endpoint. The admin can choose local Nemotron or cloud STT, local
  OpenAI-compatible LLM or cloud LLM, and local Supertonic or cloud TTS.
  Nemotron is the only local ASR runtime, and Supertonic is the only local TTS
  runtime.
- **Full OpenAI Realtime mode**: browser connects to the official OpenAI
  Realtime WebRTC endpoint using an ephemeral client secret or unified SDP
  exchange created by the app server. The app server keeps a sideband/control
  connection for tools, policy, transcript capture, and observability.

#### WebRTC Requirements

- Use browser `getUserMedia` with echo cancellation, noise suppression, and automatic gain control enabled where available.
- Use WebRTC audio tracks for media and `RTCDataChannel` for control/session events.
- Track playback progress from the browser so the server can truncate assistant state to the last actually played boundary.
- LAN microphone access requires HTTPS except for localhost. Support user-provided cert/key and document `mkcert` or reverse-proxy setups.
- Use one active talk session per user initially. Add broader concurrency only after admission control and backend resource accounting are measured.
- Keep the server-side WebRTC stack platform-neutral. Pion is the starting
  point because it is pure Go and supports Windows/macOS/Linux; avoid adding
  native desktop media-device dependencies to the Go server just to support
  Windows audio. Browser capture/playback is the device abstraction.

## Speak-To-Interrupt Design

The current app mutes the recorder while responding to avoid echo. V2 must keep listening during assistant speech.

### Interruption Policy

Use a layered policy:

1. Capture input continuously while output plays.
2. Client/browser enables echo cancellation and noise suppression whenever available.
3. Server tracks playback state and actual audio progress.
4. Turn detector ignores input that is likely TTS echo.
5. If user speech persists beyond a short threshold, begin provisional interruption handling.
6. Use partial transcript/backchannel detection to decide whether to fully interrupt.
7. On confirmed interruption:
    - stop playback immediately,
    - cancel in-flight LLM/TTS work,
    - truncate assistant conversation state to the last actually played audio boundary,
    - preserve the partial assistant text with an interrupted marker,
    - continue collecting the user's new utterance with pre-roll.

### Echo Handling

Use multiple layers; do not rely on one trick.

- Browser/WebRTC AEC where possible.
- Headphone-friendly path still works without AEC.
- Playback-aware suppression: while local TTS is playing, compare mic frames against recent output frames using energy/correlation or a lightweight acoustic echo heuristic.
- Output gate: ignore speech detections whose spectrum/energy strongly matches current TTS output.
- User override: if speech continues longer than a threshold or partial ASR produces non-backchannel words, interrupt even if echo confidence is nonzero.

### Backchannel Handling

Use NeMo's idea directly:

- Maintain a configurable backchannel phrase list.
- While the assistant is speaking, ignore short phrases such as "yeah", "okay", "uh-huh", "right", and "thanks" unless the user continues beyond the phrase.
- If partial ASR accumulates more than a small number of non-backchannel words, interrupt immediately.
- Expose a setting to disable backchannel filtering for users who prefer aggressive interruption.

### Local Turn Detection Options

Expose an OpenAI-inspired `turn_detection` shape in the local Realtime-like
session config:

- `type = "server_vad" | "semantic_vad"`
- `threshold`
- `prefix_padding_ms`
- `silence_duration_ms`
- `create_response`
- `interrupt_response`
- `eagerness = "low" | "medium" | "high" | "auto"` for `semantic_vad`

Implementation order:

1. **Server VAD equivalent**: WebRTC/browser AEC + server-side Silero or equivalent audio VAD + prefix padding + silence duration + Nemotron streaming partials.
2. **Semantic VAD v1**: local classifier over Nemotron partial transcript text, punctuation, trailing filler words, elapsed speech/silence, and confidence/progress signals. It should delay turn commit for incomplete utterances such as trailing "umm..." and commit quickly for semantically complete requests.
3. **Semantic VAD tuning**: support `eagerness` values compatible with the OpenAI concept. `low` waits longer, `high` commits sooner, `auto` maps to the default profile.
4. **Backchannel handling**: while the assistant is speaking, combine semantic VAD and backchannel phrase filtering so short acknowledgements do not interrupt unless the user continues.
5. **Parakeet Realtime EOU experiment**: optional benchmark target, not the default.
6. **Sortformer diarization**: only if multi-speaker behavior becomes a product goal. Do not make it mandatory for barge-in.

The local semantic VAD does not need to be architecturally identical to OpenAI's
classifier, but the behavior and configuration should be close enough that the
browser UI and admin settings feel consistent across local and OpenAI Realtime
modes.

## Local Streaming ASR

Phase 1's `onnx-asr` is a segment recognizer. V2 should replace it with a
streaming ASR runtime that produces partial transcript events while the user is
speaking.

Preferred target:

- Model: `nvidia/nemotron-3.5-asr-streaming-0.6b` family.
- First implementation target: native Go adapter around ONNX Runtime / ONNX Runtime GenAI assets.
- First quantization to test: `onnx-community/nemotron-3.5-asr-streaming-0.6b-onnx-int4`, because it is smaller and optimized for a 560ms operating point.
- Comparison target: `smcleod/nemotron-3.5-asr-streaming-0.6b-int8`, because it is laid out for `parakeet-rs` and can validate whether our Go adapter matches a known implementation.
- Input contract: 16 kHz mono PCM converted to float32 frames.
- Output contract: `ASRPartial` events during speech, `ASRFinal` on committed turn, optional detected language metadata, chunk timing, and model latency metrics.
- Streaming state: own encoder/decoder cache tensors per active audio stream; do not reload model or recreate sessions per turn.

Implementation options:

1. Native Go Nemotron adapter. Best long-term fit if the ONNX graph inputs,
   cache state, tokenizer, prompt/language handling, and RNNT decoding are
   straightforward enough to own. This keeps the project single-language and
   minimizes process/IPC overhead.
2. `parakeet-rs` reference spike. Use it to verify expected streaming behavior,
   chunking, target-language behavior, and transcript quality. Do not make it
   the default architecture unless the native Go adapter proves too risky.
3. Python / NeMo / `onnx-asr` path. V1 history only. Do not use as the V2 STT
   foundation because it does not give us the desired streaming control.

Key risk: native Go STT is not just "run an ONNX file." We have to implement or
port enough of the model adapter to manage preprocessing, chunking, cache
tensors, prompt/language inputs, RNNT decoding, decoder-side context biasing
(see "Agent Name Recognition" below), tokenizer output, and reset semantics
correctly. This should be one of the first serious V2 spikes after the WebRTC
audio loopback.

Windows acceptance for the ASR spike:

- run the same Go ASR adapter on native Windows 11 x64 without WSL,
- load the pinned ONNX Runtime DLL from the app-managed runtime directory,
- prove the chosen Go binding does not require a user-installed compiler at
  runtime,
- stream partials from a fixture at the target chunk size,
- reset per-stream cache state cleanly across turns,
- report CPU, memory, cold-load, warm-stream, and finalization latency,
- document whether WinML, DirectML, or CPU execution provider is used and why.

### Agent Name Recognition (Decoder-Side Context Biasing)

The agent has a spoken name (default `Hina`) that users say to address it. In a
chained STT -> text-LLM turn, that name must transcribe consistently or it
corrupts the prompt that non-audio LLMs receive. Short names also have
near-homophones (`Hina` vs `Nina`/`Tina`); the fix is to bias the decoder toward
the configured name.

NVIDIA's stack already solves this — NeMo GPU-PB / TurboBias context biasing,
Riva/NIM per-request word boosting (boost score ~0.5–2.0 for RNNT/TDT/Nemotron),
global word boosting baked at build time, and lexicon mapping for explicit
pronunciations. **But all of it lives in the NeMo/Riva/NIM Python+CUDA serving
path, which V2 deliberately does not use.** Because V2 runs Nemotron as a
CPU-only ONNX graph behind our own Go RNNT decoder, biasing is not inherited and
must be implemented in our decoder. It is a decoding-time algorithm, not a model
change, so it does not touch the ONNX graph or the CPU-only constraint.

Approach (shallow-fusion, token-level boosting):

- Tokenize each bias phrase (the agent name + a small alias list) with the
  model's SentencePiece tokenizer into subword IDs. This is why arbitrary
  out-of-vocabulary names need no pronunciation lexicon — the name is just a
  token sequence.
- Build a small prefix trie ("boosting tree") over those token sequences.
- During greedy/beam RNNT decoding, add a positive bonus to a hypothesis score
  for each token that advances a match in the trie, and remove the accumulated
  bonus if the hypothesis later diverges (so partial matches do not leak score).
  Completed matches keep the bonus.
- Reference starting params from NeMo for RNNT: `context_score ≈ 1.0`,
  `depth_scaling ≈ 2.0`. Treat these as starting points to tune on fixtures, and
  validate against NeMo/TurboBias on the same phrases if a discrepancy appears.
- Cost is negligible for a handful of phrases; it runs inline in the existing
  decode loop with no extra ONNX work, so CPU-only latency targets are unaffected.

User-customizable name (supported, low-complexity):

- The agent name is a config field (e.g. `[agent].name`, default `Hina`) with an
  optional `[agent].name_aliases` list for spelling/casing variants and known
  mis-hearings (e.g. `["Hina", "Heena"]`).
- On startup and whenever the name changes, the decoder rebuilds the boosting
  trie from the configured name + aliases. No retraining, no model rebuild, no
  ONNX change — only re-tokenizing a few strings and rebuilding a small trie.
  That is what makes runtime customization cheap enough to support.
- Keep the bias list intentionally small (the agent name, a few aliases, and any
  wake/address variants). This is name recognition, not a general user-dictionary
  feature; a broad per-user custom-vocabulary surface is out of scope for V2
  unless a later need justifies the added decoder/config complexity.
- Capitalization: Nemotron emits punctuation/capitalization, so seed bias phrases
  in their expected surface form (capitalized name).

Defense in depth (so one miss never poisons the LLM):

- Treat the wake/address token as a routing token: detect-and-strip it at the
  session layer before building the LLM prompt, and match it case-insensitively
  against the configured name + aliases. A single mis-transcription then degrades
  wake detection for that turn rather than corrupting the user's actual request.
  This pairs with the conversation-modes canonical context builder so text and
  live turns share one prompt path.

Benchmark hook:

- Add a name-recognition fixture to the ASR harness: record the address phrase
  ("<name>, ...") many times and measure the substitution rate with biasing off
  vs on, and across candidate names, so the name choice and the bias params are
  validated on real CPU-ONNX output rather than predicted.

## Web UI Scope

The first web UIs should be functional, not landing pages.

### User Web UI

- ChatGPT-like conversation layout with a persistent message list.
- Text composer for typed messages that bypass STT/TTS and call the LLM directly.
- Live mode button/control that starts a WebRTC voice call inside the current session.
- End-call control that returns the user to text mode without leaving the chat.
- Conversation transcript with streaming assistant text and user transcript updates.
- Live audio state: listening, speaking, thinking, interrupted, muted.
- Device selector for microphone/speaker where browser APIs expose it.
- Start new session.
- Continue old session.
- View prior transcripts.
- Sandbox Environment settings for CLI tools, MCP servers, default Docker `sbx` sandbox policy, host-service grants, secret/env grants, and agent auth/local-agent profiles.
- Agent setup flows for Codex, Claude Code, and Cursor CLI, including browser/subscription login, API-key/token auth, status checks, logout, and re-auth. Pi should appear as a local agent availability/status check backed by host llama.cpp, not an auth flow.
- Automations list with enabled/disabled state, next run, last run, and status.
- On-rails Automation editor for trigger, schedule, permission profile, secrets, tools, workflow steps, agent steps, aggregation, and output actions. This editor generates `automation.v1` JSON rather than asking users to write JSON directly.
- Automation JSON import/export flows for portability. Imports must validate against the backend schema and show repairable errors before enabling.
- Read-only generated JSON preview and advanced import-repair editor for users who intentionally want to inspect or modify raw JSON outside the normal builder.
- Natural-language Automation builder that asks the active server LLM to produce validated Automation JSON.
- Automation run history with logs, artifacts, spawned agent reports, and final output.
- Session reset, mute, and interrupt controls.
- User-visible tool approval cards only for tools the admin policy allows that user to approve.

Do not expose STT / LLM / TTS runtime selection to normal users.

Text mode and live mode should render into the same timeline. A voice turn
appears as a user transcript followed by the assistant response; a typed turn
appears as user text followed by the assistant response. The user should not
feel like they switched products when moving between modes.

### Admin Web UI

- Runtime catalog and active backend policy.
- Model/runtime setup and download/build status.
- Start/stop/restart controls for local runtimes.
- General server logs.
- Per-backend logs for llama.cpp, STT, TTS, and setup jobs.
- User management and session inspection.
- Docker `sbx` sandbox policy, host-service allow-lists, and per-user/per-session sandbox logs.
- Automation policy, scheduler health, active runs, and global concurrency limits.
- Tool approval policy.
- Resource views: CPU, memory, GPU/VRAM if available, active sessions, queue depth.
- LAN/HTTPS/auth configuration.

Both web UIs should be dense and operational, not marketing-oriented.

## Security And LAN Access

Minimum:

- Bind to `127.0.0.1` by default.
- Require explicit config/CLI flag for LAN binding, e.g. `--host 0.0.0.0`.
- Create an admin bootstrap credential on first run and require changing/confirming it before LAN mode.
- Require authenticated browser sessions for both user and admin UIs.
- Do not trust the local network after first pairing. LAN clients still authenticate.
- Use secure, httpOnly cookies or short-lived bearer tokens for local web sessions.
- Require admin role for backend selection, setup, logs, sandbox policy, and user management.
- Never expose shell/tool approvals without an authenticated user session and an admin-defined policy.

For WebRTC / browser mic on LAN:

- Plan for HTTPS. Options: self-signed local cert, `mkcert`, or documented reverse proxy.
- Avoid putting long-lived OpenAI API keys in browser code. Use ephemeral client secrets for OpenAI Realtime sessions.

For Docker `sbx` sandboxes:

- Treat every user tool invocation as hostile unless explicitly trusted by admin policy.
- Prefer `sbx` kits/templates generated by the server over ad hoc container images.
- Disable privilege escalation and never mount the host Docker socket into user sandboxes.
- Use read-only mounts except for the user's workspace, temp directories, and explicit artifacts/state paths. Prefer `sbx --clone` for repository work so source checkout access is controlled and changes are reviewed as artifacts/remotes.
- Default to no network for shell/code tools, with explicit admin/user allow-lists for networked tools and host services.
- Treat host inference as a separate permission from internet/network access. A Pi run may receive access to llama.cpp without receiving arbitrary host-network access.
- Apply resource limits and hard execution timeouts.
- Persist audit records for command, user, session, sandbox name/id, kit/template, exit code, timings, policy decisions, and output paths.

For user secrets:

- Passwords must be stored with a memory-hard password hash such as Argon2id, never reversible encryption.
- User secrets should be stored in a per-user encrypted vault and exposed only to sandboxes/runs that the user explicitly grants.
- Browser-auth CLI credential caches and agent state directories are secret material too. Treat Codex `CODEX_HOME`, Claude `CLAUDE_CONFIG_DIR`, Cursor auth state, and equivalent provider credential stores with the same boundary as vaulted secrets.
- Secrets and agent auth state should not be visible in the admin UI, logs, artifacts, run records, or exported Automation JSON.
- A database-only compromise should not reveal plaintext passwords or secrets.
- For unattended Automations, the server must be able to decrypt granted secrets and mount granted agent auth state at run time. This means secrets and browser-auth tokens can be protected from database compromise and normal admin UI access, but not from a malicious host/root admin or modified server binary. Document this boundary clearly.
- Use envelope encryption: each secret value is encrypted with a random per-secret data key; data keys are wrapped by a local server master key stored outside the database. Prefer OS keyring where practical; on Windows, prefer DPAPI / Credential Manager or an ACL-guarded key file tied to the service account. On Unix-like systems, a deployment-created key file must use `0600`-style permissions and fail closed if permissions are unsafe. Support a deployment-provided master key for managed installs.
- Store only secret metadata in the database: id, user id, name, created/updated timestamps, optional description, and encrypted payload. Never include secret values in Automation exports; export only `secret_refs`.
- Inject secrets into sandboxes only for the run that needs them, preferably as environment variables or mounted files with predictable names chosen by the user. Remove temp secret files when the run exits.
- Use Docker `sbx secret` as the preferred injection mechanism for supported service/registry secrets when it can preserve per-user/sandbox scoping and keep raw values out of the sandbox filesystem. Do not let it replace the product's per-user vault: the app still owns secret metadata, user grants, Automation export redaction, arbitrary secret support, and admin non-disclosure guarantees. For unsupported/custom secrets, inject from the app vault as temporary environment variables or mounted files for the specific run/tool call.
- Leave room for a future user-unlocked vault mode where secrets are wrapped by a user-derived key and only usable while the user is actively logged in. Do not make that the default because it conflicts with unattended Automations.
- Redact known secret values and environment variable names from logs where possible.

## Migration Strategy

Freeze V1 and build V2 deliberately. V1 lives at `/home/renan/voice-agent` and
should remain runnable or be clearly tagged/frozen before V2 work invalidates
any assumptions. V2 work should happen in `/home/renan/hina-agent` unless the
user explicitly redirects.

Before step 1, close the "Clarify before first code" items in the
Implementation Readiness Review so the first scaffold does not bake in the
wrong names, auth model, database shape, or event contracts.

Recommended V2 sequence:

1. Tag/freeze the current Python/Textual app as V1.
2. Create the Go server skeleton: config, auth, event schema, session store,
   logs, health endpoints, and the `internal/platform` abstraction for
   OS-specific paths, permissions, secrets, and process cleanup.
3. Set up CI/build targets for Windows 11 x64, macOS Apple Silicon, and Linux
   x86_64 from the start. Add a non-GPU smoke test path that runs without local
   ONNX models.
4. Build the cross-platform setup/doctor CLI: app state directories, runtime
   asset directories, `sbx` detection, llama.cpp detection, ORT DLL/library
   detection, HTTPS cert checks, and clear feature-availability reporting.
5. Create the TypeScript web shell with separate user/admin routes and a
   text-first chat UI.
6. Build the benchmark harness and audio fixture replay before tuning
   VAD/interruption. The harness must run non-interactively on all Tier 1 hosts.
7. Run the native Windows platform spike before local-model commitments:
   Pion loopback, SQLite, process supervision, `sbx run shell`, llama.cpp
   Windows install/launch, ONNX Runtime DLL load through the chosen Go binding,
   and a minimal Supertonic/Nemotron fixture load if assets are ready.
8. Implement local WebRTC audio loopback and Realtime-like data-channel events
   with no model dependency on Windows/macOS/Linux.
9. Implement and benchmark direct Go Supertonic synthesis on every Tier 1 host;
   keep Windows local TTS disabled until the ONNX/DLL path passes.
10. Implement and benchmark native Go Nemotron streaming ASR with partial
    transcript events on every Tier 1 host; keep Windows local ASR disabled
    until the ONNX/DLL path passes.
11. Port the runtime registry/reconciler ideas from V1 into Go, but only for
    supported V2 runtimes and with explicit per-platform capability checks.
12. Implement managed llama.cpp install/start/health/idle-unload for
    Windows/macOS/Linux.
13. Implement text-mode typed chat against the configured LLM with streaming
    text deltas and tool approval/sandbox flow.
14. Add transcript/session persistence with canonical text turns.
15. Implement live mode as a WebRTC call attached to an existing session,
    loading prior text context into the voice pipeline.
16. Implement the Docker `sbx` runner abstraction for tools, generated
    kits/templates, resource limits, host-service allow-lists, and admin audit
    logs on Windows/macOS/Linux.
17. Implement per-user secret vaults and explicit secret grants to `sbx`
    sandboxes, including Windows DPAPI/Credential Manager or ACL-checked
    master-key storage.
18. Implement Sandbox Environment settings for user tools, MCP servers,
    host-service grants, secret/env grants, account-backed agent auth profiles,
    and local Pi availability.
19. Implement the agent auth broker for Codex, Claude Code, and Cursor
    browser/subscription login plus API-key/token auth, with device-code and
    paste-code fallbacks tested from a Windows browser.
20. Implement `automation.v1` schema, import/export, manual editor, durable
    scheduler, and run records.
21. Implement deterministic Automation steps before model/agent wake-up,
    starting with the typed GitHub review-request flow.
22. Implement Codex / Claude Code / Cursor / Pi Automation adapters as Go-owned
    typed tools/MCP facade capabilities. Pi should use host llama.cpp through
    the controlled host inference gateway from the start.
23. Implement LLM-assisted Automation creation with JSON schema validation/retry.
24. Implement the minimal custom agent loop for local/mixed STT -> LLM -> TTS
    behind the local Realtime-like WebRTC endpoint.
25. Add server VAD, semantic VAD, interruption, backchannel filtering, and echo
    handling behind benchmark gates.
26. Add full OpenAI Realtime browser WebRTC mode with server-side sideband
    controls.
27. Spike `openai-agents-go` only after the event/session model is stable.
28. Use `parakeet-rs` only as a reference implementation / quality benchmark if
    the Go adapter needs validation.

## Latency Targets

Initial targets, to be validated on real hardware:

- Mic frame to VAD-start event: under 100 ms.
- User stop to committed turn: under 250 ms when VAD is confident.
- Semantic VAD should delay incomplete utterances without adding perceptible delay to complete requests.
- Confirmed interruption to playback stop: under 150 ms.
- STT partial update cadence: at least once per Nemotron chunk while speech is active.
- STT final for short local utterance: under 300 ms after turn commit with Nemotron on CPU, if hardware allows.
- LLM first token: under 500 ms for local small models or cloud low-reasoning settings.
- First audible TTS after first sentence: under 700 ms for fast local/cloud TTS.

Track percentile metrics, not just averages.

## Benchmark Harness

Build a repeatable harness before tuning:

- Audio fixture replay through the same input pipeline.
- Echo fixture: assistant TTS playing while user speaks over it.
- Backchannel fixture: user says "yeah", "okay", "uh-huh" during assistant speech.
- Interruption fixture: user starts a real new request during assistant speech.
- Noise fixture: keyboard/fan/background speech.

Metrics:

- false VAD starts,
- missed starts,
- end-of-turn delay,
- interruption delay,
- false interruption rate,
- backchannel suppression accuracy,
- semantic VAD false-commit / over-wait rates,
- STT latency,
- WER/subjective transcript quality,
- first assistant token,
- first audio,
- total turn time.

Run the benchmark harness against:

- no-model audio loopback,
- local WebRTC media loopback,
- native Windows WebRTC loopback with browser capture/playback and Pion server
  on localhost,
- local STT-only,
- local chained STT -> LLM -> TTS,
- OpenAI Realtime WebRTC,
- interruption while local TTS is playing,
- interruption while OpenAI Realtime audio is playing,
- server VAD vs semantic VAD fixtures,
- sandbox tool call during an active voice session,
- Automation deterministic no-op run that should not wake models,
- Automation run that spawns one account-backed agent CLI,
- Automation run that spawns Pi through host llama.cpp without user agent auth,
- Automation run that spawns multiple callable agents in parallel and aggregates reports,
- Docker `sbx` create/run startup overhead and artifact extraction latency,
- Windows native runtime setup: `sbx` smoke, llama.cpp install/launch, ORT DLL
  load, process cleanup, and secret-vault unlock.

## Implementation Readiness Review

> **Status (2026-06-18):** The "Clarify before first code" items below are RESOLVED
> in [`research-findings.md`](research-findings.md) Part A and built in
> [`phase-01-foundation.md`](phase-01-foundation.md). The "Research or spike before
> dependent feature work" items are resolved in `research-findings.md` Part B, except
> the hardware-gated ones (Part C, mostly Phase 11). Kept here for context;
> `research-findings.md` is authoritative.

The plan is ready to start a narrow bootstrap only after the first-code
clarifications below are closed. Do not wait for every local-voice,
Windows-runtime, or Automation spike before creating the initial server, config,
auth/session, migration, event, and web-app skeleton. Those deeper features
remain gated by their own spikes.

Clarify before first code:

- Repository/product identity: use `/home/renan/hina-agent` as the V2 workspace
  and choose the Go module path, binary name, config directory name, service
  name, and user-facing product name. Treat `voice-agent` references as V1-only
  unless intentionally describing migration.
- Bootstrap auth/session v0: define the first-run admin credential flow, password
  hashing, secure cookie or bearer-token session storage, admin/user roles,
  local-only defaults, and LAN enablement gates before building user/admin
  routes.
- Persistence schema v0: choose the SQLite driver/migration tool and sketch the
  initial table boundaries for users, sessions, turns, events, runtime state,
  Automation definitions/runs/artifacts, sandbox state, secret metadata, and
  agent-auth state. Avoid letting early UI routes invent ad hoc JSON blobs that
  later fight the event model.
- Event/API wire contracts v0: decide HTTP route shape, user/admin event stream
  transport, event replay/reconnect behavior, `RTCDataChannel` event envelope,
  versioning, and generated TypeScript types before the frontend and backend
  drift.
- Tier 1 validation hosts: confirm access to native Windows 11 x64, macOS Apple
  Silicon, and Linux x86_64. If one is unavailable, mark that platform as
  planned but unvalidated in docs and setup output.

Research or spike before dependent feature work:

- WebRTC media bridge: Pion can own the WebRTC transport, but local ASR/TTS need
  PCM frames while browser WebRTC normally carries Opus RTP. Decide and measure
  the Opus decode path for microphone input, the Opus encode path for assistant
  audio output, resampling between 48 kHz WebRTC audio and 16/44.1 kHz model
  contracts, packet-loss behavior, latency, and CGo/build implications.
- Docker `sbx` production fit: re-verify current install, authentication,
  kits/templates, policy, secrets, `--clone`, workspace mounts, `sbx cp`,
  `host.docker.internal`, and Windows behavior before building the sandbox
  runner around undocumented assumptions.
- Drift-prone CLI adapters: re-verify Codex, Claude Code, Cursor, and Pi flags,
  auth flows, stream formats, structured-output modes, cancellation behavior,
  and version reporting immediately before implementing each adapter. Treat
  them as versioned capabilities discovered by health checks.
- Model asset licensing and distribution: confirm the licenses, download terms,
  checksum sources, cache layout, and redistribution constraints for Nemotron,
  the ONNX community exports, Supertonic 3, Silero VAD, and llama.cpp release
  assets before shipping managed installers.
- ONNX Runtime Go binding: choose the binding, version pin, build tags, runtime
  library/DLL discovery, execution provider, and Windows packaging strategy
  before committing local ASR/TTS code to a specific adapter API.
- OpenAI Realtime integration: refresh official docs for the unified SDP flow,
  ephemeral secrets, sideband/server controls, tool-call handling, cancellation,
  and transcript capture before implementing full-cloud Realtime mode.
- Automation semantics: define selector/template syntax, retry/error policy,
  idempotency expectations, artifact promotion rules, notification/output
  side-effect confirmation, and schema evolution before promising portable
  `automation.v1` imports.
- Secret-vault threat model: explicitly document that unattended Automations
  require server-side decryptability, so secrets can be hidden from the database
  and normal admin UI but not from a malicious host/root admin or modified
  server binary.

## Major Risks

- Echo cancellation remains hard even with browser/WebRTC; AEC reduces the problem but does not remove the need for playback-aware turn detection.
- Browser microphone access from LAN devices has HTTPS/security constraints.
- A rewrite can lose working V1 runtime-management knowledge unless V1 behavior is used as a reference corpus.
- Official OpenAI Realtime mode and local Realtime-like mode have different internals; keep the frontend/session abstraction shared while allowing the server execution engines to differ.
- Text mode and live mode can drift if they use different context-building paths. Keep one canonical session-history builder for both.
- Tool approvals and shell execution are more sensitive in a multi-user browser product than in a local terminal.
- Per-user Docker `sbx` isolation can create operational complexity: kit/template management, stale sandboxes, storage growth, network policy, host-service allow-lists, platform requirements, and resource limits.
- Docker `sbx` is newer and more specialized than plain Docker containers. Validate host OS support, install/update flow, policy behavior, secret behavior, and failure modes before betting the whole Automation runner on it.
- Host inference access for Pi can accidentally become broad host-network access if the bridge is too permissive. Keep llama.cpp access behind an explicit gateway/allow-list and test that other host services are unreachable.
- Automations are powerful enough to create external side effects. The product must make sandbox permissions, granted secrets, network access, and enabled outputs visible before a user enables an Automation.
- LLM-generated Automation JSON can be wrong or unsafe even when schema-valid. Always show the validated configuration to the user before enabling.
- Unattended secret use cannot be made unreadable to a malicious host/root admin because the server must decrypt secrets to run jobs.
- Browser-auth CLI credentials are powerful bearer credentials. Treat per-user CLI state volumes as secrets, support logout/revocation, and avoid copying them into normal workspaces or artifacts.
- CLI browser auth inside containers can fail when a provider expects a localhost callback or a real desktop browser. The auth broker must support URL/code capture, PTY input, and provider-specific device-code or paste-code flows.
- Agent CLI tools can change headless flags, auth flows, output formats, and autonomy modes. Keep each as a versioned adapter with health checks.
- Pi lowers the account/subscription barrier for Automations, but local model quality, context length, tool-use reliability, and structured-output reliability will depend heavily on the active llama.cpp model. Benchmark Pi separately from cloud-backed agents and make quality limits visible in the UI.
- Go ecosystem agent SDKs may lag official OpenAI API features. Do not depend on them before a spike proves fit.
- Native Go Nemotron streaming ASR may be more work than expected because the adapter owns cache tensors, RNNT decoding, language prompts, and tokenizer behavior.
- Native Windows local ASR/TTS may be blocked or delayed by ONNX Runtime Go binding,
  CGo, DLL loading, execution-provider, or packaging issues. Keep text mode,
  cloud STT/TTS, and full OpenAI Realtime functional on Windows even if local
  ONNX voice is temporarily unavailable.
- Windows process cleanup is different from POSIX process groups. If model
  servers or setup/auth helpers spawn children, cancellation must use Job
  Objects or another validated process-tree strategy so restarts and shutdowns
  do not leave orphaned `llama-server.exe`, `sbx.exe`, or agent processes.
- Windows filesystem semantics can break naive sandbox and workspace code:
  case-insensitive names, long paths, drive letters, backslashes, symlink
  privileges, ACLs, and partial `chmod` behavior all need fixture tests.
- Docker `sbx` on Windows is promising but still a platform dependency with
  virtualization prerequisites and evolving behavior. Validate installation,
  login, policy, host-service access, mounts, `sbx cp`, secrets, and failure
  modes on real Windows 11 hosts before treating Windows Automations as GA.
- Windows secret storage cannot rely on Unix file modes. The DPAPI/Credential
  Manager or ACL-checked key-file path must be tested against service-account,
  single-user desktop, and backup/restore scenarios.
- Local WebRTC in Go adds ICE/DTLS/SRTP/RTP/Opus complexity earlier than a WebSocket prototype would. This is intentional because browser-only voice quality is a V2 product requirement.
- Local semantic VAD can become a product-quality sink if it is not measured against fixtures. Keep the first version small and benchmark-driven.
- Multi-user support complicates audio ownership and local backend admission control. Start with one active talk session per user and a simple global concurrency limit.
- Admin/user split adds auth, RBAC, and audit requirements from day one.

## Decisions Closed In This Revision

- V2 may use a JavaScript/TypeScript frontend build step.
- V2 application code should be Go-first and Go-only by default.
- V2 should support native Windows 11 x64 as a first-class host platform
  alongside macOS Apple Silicon and Linux x86_64.
- WSL remains acceptable for V1 and unsupported host variants, but it is not the
  Windows support story for V2.
- Windows 10 and Windows on ARM are not first-milestone full-support targets
  because the sandbox/runtime stack must be validated there separately.
- Do not embed Python in the main server as the default. Use non-Go workers only as explicitly justified exceptions.
- Drop Textual/TUI from V2. V1 remains the terminal-first app.
- `/home/renan/voice-agent` is the V1 reference corpus. `/home/renan/hina-agent`
  is the intended V2 workspace unless the user explicitly redirects.
- Serve both a user Web UI and an admin Web UI.
- Web UI is enabled on localhost by default. LAN binding is explicit.
- LAN access always requires authentication; do not trust the local network after first pairing.
- Backend/model/runtime selection belongs in the admin portal, not the user UI.
- Full OpenAI Realtime mode should bypass local STT/LLM/TTS through browser WebRTC plus server sideband controls.
- Local/mixed mode remains a chained pipeline behind the local Realtime-like WebRTC endpoint, with server-owned policy and browser-owned capture/playback.
- Multi-user support matters for sessions, auth, transcripts, and tool sandboxes. Multi-speaker diarization is not a V2 foundation requirement.
- Shared model backends are admin-owned infrastructure; Docker `sbx` sandboxing is for user tools/workspaces, not per-user model servers.
- Main-model tool calls in text and live sessions use the same per-user Docker `sbx` sandbox boundary as Automations. Shared model runtimes can reason over user context, but user-scoped side effects happen only through typed sandboxed tools with explicit grants.
- Supertonic 3 should be integrated directly through a native Go ONNX Runtime adapter, with no local HTTP server path.
- Default local STT should target Nemotron 3.5 ASR streaming, not `onnx-asr`.
- Whisper.cpp should not be supported in V2; local ASR is Nemotron-only.
- The agent has a configurable spoken name (default `Hina`, via `[agent].name` plus optional `[agent].name_aliases`). Reliable recognition is handled by decoder-side context biasing implemented in our Go RNNT decoder — a shallow-fusion token-level boosting trie — not by NeMo/Riva word boosting, which the CPU-ONNX path does not use. Name customization rebuilds the trie at runtime with no model retrain or ONNX change. Broad per-user custom vocabulary is out of scope for V2.
- Supertonic 3 should be the only local TTS runtime in V2; cloud TTS remains available through cloud modes.
- Browser/WebRTC is the only V2 voice transport. Do not build a separate browser WebSocket audio mode.
- The Go server should expose a local Realtime-like WebRTC endpoint for local/mixed pipelines.
- Text mode is a first-class interaction path. Users can message the LLM without STT/TTS.
- Text and live mode share the same durable session history and can be entered/exited in either order.
- `Automations` is the product/code name for scheduled unattended workflows.
- Automations run only while the Go server is running; server shutdown stops schedules, active runs, sandboxes, agents, and runtime workers.
- Automations support guided UI editing, JSON import/export, generated JSON preview, and LLM-assisted creation through schema-validated JSON with retry. Users should not need to write raw JSON in the normal editor.
- `automation.v1` starts with interval/cron/manual triggers, default missed-run policy `skip`, opt-in `run_once`, concurrency/budget controls, granular/unrestricted sandboxes, deterministic tools, typed agent adapters, and artifacts.
- Automations support deterministic steps before model/agent wake-up.
- Automations support unrestricted and granular sandbox permission profiles.
- Users have a Sandbox Environment settings area for CLI tools, MCP servers, default Docker `sbx` sandbox policy, host-service grants, secrets/env grants, account-backed agent authentication, and local Pi availability.
- Sandbox storage is persistent for user-owned workspaces, optional per-session workspaces, selected Automation artifacts, and per-user encrypted agent/auth state. Container root filesystems and ordinary Automation run workspaces are ephemeral by default.
- Agent auth supports both browser/subscription login and API-key/token profiles for Codex, Claude Code, and Cursor. Automation and chat tool UIs may only offer account-backed agents whose auth profile is configured and allowed by admin policy.
- Pi coding agent is supported as a callable Automation agent that never requires Codex/Cursor/Claude accounts or cloud API keys. In V2, Pi must always use the host llama.cpp model through an explicit host inference allow-list/gateway.
- Automation callable-agent adapters should support Codex, Claude Code, Cursor CLI, and Pi in headless/autonomous modes, inheriting the Automation sandbox restrictions. The LLM should call typed tools/MCP capabilities, not build raw CLI commands.
- Per-user secrets are isolated, encrypted at rest, hidden from admin UI/logs/exports, and explicitly granted to Automations. Docker `sbx secret` is an allowed injection backend for supported service/registry secrets, but the app vault remains the source of truth.
- Initial persistence should use SQLite with WAL and explicit migrations. Prefer
  a CGo-free Go SQLite driver if it satisfies performance and migration needs,
  so native Windows builds do not require a compiler for the control plane.
  Revisit Postgres only if multi-host deployment becomes a near-term goal.
- Use TypeScript + Vite + React for the first web implementation, with separate user/admin route trees sharing API/event clients.
- Use shadcn/ui generated components backed by Base UI primitives by default, plus Tailwind CSS and lucide-react icons. Do not start a new V2 UI on Radix primitives unless a Base UI component gap blocks implementation.
- Use TanStack Router for typed client-side routing, TanStack Query for server state, TanStack Table for dense data grids, React Hook Form plus Zod for guided forms, Ajv for frontend JSON Schema validation, and Zustand only for frontend-local UI preferences/transient state.
- Use CodeMirror 6 only for generated JSON preview, import repair, logs/artifacts, and advanced/admin diagnostics. Use structured Automation builder forms for normal Automation editing.
- Use the official OpenAI Go SDK for OpenAI REST/Responses paths and the Google Gen AI Go SDK for Gemini-native cloud provider adapters.
- Start Docker `sbx` sandboxing with one admin-controlled kit/template and per-user/per-session workspaces. Add per-tool or per-user kits/images later only if the admin workflow needs them.
- Replace V1's shell-script setup model with Go-owned setup/doctor/runtime
  commands that work on Windows/macOS/Linux.
- Prefer prebuilt llama.cpp assets or winget on Windows; Visual Studio/CMake
  source builds are a fallback, not the default user path.
- Local ONNX STT/TTS support on Windows is gated by a successful Go ONNX Runtime
  spike with pinned app-managed DLLs.
- Keep the main V2 control plane free of avoidable CGo dependencies; isolate
  CGo to local ONNX adapters or provide build tags/feature gates for cloud-only
  builds.
- Use OS-specific secure storage for the local secret-vault master key, including
  DPAPI/Credential Manager or ACL-checked files on Windows.
- For LAN HTTPS, support user-provided cert/key and document `mkcert` / reverse proxy setup. Do not spend v2 milestone time trying to auto-install trusted local CAs.

## Remaining Spikes

> **Status (2026-06-18):** The docs/library/licensing spikes here are CLOSED in
> [`research-findings.md`](research-findings.md) Part B with chosen libraries and
> versions. The remaining open spikes are code/hardware tasks owned by their phase in
> [`roadmap.md`](roadmap.md) — the native-Windows spikes are gathered into
> [`phase-11-windows-hardening.md`](phase-11-windows-hardening.md). See
> `research-findings.md` Part D for corrections that supersede earlier statements in
> this document (e.g. `CODEX_API_KEY` removed, `--full-auto` deprecated, local Opus
> encoding avoided via PCM-over-datachannel, OpenAI Realtime `call_id` from the
> `Location` header).

- Build the native Windows 11 x64 platform spike: install/run server from
  PowerShell, create app state/cache directories, run SQLite migrations, start
  and cancel child processes with process-tree cleanup, verify logs, and run the
  setup doctor without WSL.
- Validate Docker `sbx` on Windows 11 x64: `winget install -h Docker.sbx`,
  Docker login, Hypervisor Platform detection, `sbx run shell`, generated kit,
  workspace mount with spaces/Unicode, `sbx cp`, network policy,
  `host.docker.internal` host-service access, secret injection, and failure
  reporting.
- Build the Windows llama.cpp runtime spike: install via upstream release asset
  or winget, launch `llama-server.exe` with `--models-preset`, verify health,
  idle unload, restart, log streaming, cancellation, CUDA/Vulkan capability
  detection, and Pi access through the host inference gateway.
- Build the Windows ONNX Runtime Go spike: download/pin ORT CPU DLLs, load them
  from the app runtime directory through the chosen Go binding, run a tiny ONNX
  fixture, then run minimal Supertonic and Nemotron fixture passes. Record
  whether CGo is required at build time only or leaks into end-user setup.
- Build the Windows secret-vault spike: compare DPAPI/Credential Manager vs.
  ACL-guarded key file for desktop and service-account deployments, test
  backup/restore behavior, and verify redaction in logs/artifacts.
- Build Windows path/permission fixtures for workspace roots, long paths,
  drive-letter paths, case collisions, symlinks/reparse points, ACL failures,
  CRLF logs, and sandbox mount translation.
- Build a small Go WebRTC audio loopback prototype with browser
  capture/playback progress, data-channel events, Opus RTP decode to PCM input,
  PCM-to-Opus output, model-rate resampling, latency metrics, and
  CGo/build-tag implications.
- Build the typed chat path and shared session-context builder before live-mode context handoff.
- Scaffold the React/Vite frontend stack with shadcn/ui Base UI components, Tailwind CSS, TanStack Router/Query/Table, Zustand UI preference store, React Hook Form, Zod, Ajv, and Playwright/Vitest test wiring.
- Implement `automation.v1` as JSON Schema and build import/export validation around it.
- Define `automation.v1` selector/template syntax, retry/error policy,
  idempotency expectations, side-effect confirmation rules, artifact promotion,
  and schema evolution before relying on portable imports.
- Build the first guided Automation builder UI that emits `automation.v1` JSON, validates through frontend Ajv plus backend schema validation, and reserves CodeMirror for generated preview/import repair only.
- Build a durable SQLite-backed scheduler with server-up-only execution semantics.
- Build the per-user secret vault and sandbox secret injection path.
- Build the Docker `sbx` runner abstraction: generated kits/templates, named run lifecycle, resource limits, workspace mapping, host-service allow-lists, policy logs, artifact extraction, and cleanup.
- Build the Sandbox Environment settings UI/API and per-user persistent sandbox state layout.
- Build the agent auth broker: PTY-based login runner, URL/code detection, frontend handoff, status checks, logout, and encrypted per-user agent state persistence.
- Build the typed GitHub review-request Automation path: notifications query, PR checkout, no-op skip, optional PR comment output.
- Re-verify current Codex, Claude Code, Cursor, and Pi CLI/auth/output/cancel
  behavior before freezing adapter contracts.
- Wrap Codex, Claude Code, and Cursor CLI in versioned adapters with health checks, structured output parsing, cancellation, timeout, artifact capture, and normalized `AgentRunResult`.
- Build the Pi local-only adapter: generated Pi config pointing at host llama.cpp, `pi --mode rpc` event handling, cancellation, structured-output validation/retry, artifact capture, and normalized `AgentRunResult`.
- Validate Docker `sbx` policy and secret primitives against the product requirements, including per-user/sandbox-scoped service-secret injection through `sbx secret`, unsupported/custom secret fallback injection from the app vault, and log/artifact redaction.
- Build cloud provider adapter spikes using the official OpenAI Go SDK for OpenAI Responses/Realtime-related server paths and the Google Gen AI Go SDK for Gemini-native cloud model paths.
- Refresh OpenAI Realtime docs immediately before implementation and verify the
  unified SDP flow, ephemeral secrets, sideband/server controls, cancellation,
  tool-call handling, and transcript capture against the chosen Go/browser
  client path.
- Optionally expose the agent adapters through an MCP server facade so MCP-capable LLMs can call them as tools while Go still owns process invocation.
- Confirm model/runtime asset licenses, download terms, checksums, cache layout,
  and redistribution constraints for Nemotron, ONNX exports, Supertonic 3,
  Silero VAD, llama.cpp release assets, and any bundled tokenizer/config files.
- Build a native Go Supertonic 3 synthesis spike using ONNX Runtime and measure cold start, warm synthesis latency, CPU, and memory.
- Build a native Go Nemotron 3.5 streaming ASR spike using ONNX Runtime / ONNX Runtime GenAI assets and measure partial cadence, final latency, CPU, and memory.
- Add decoder-side context biasing (shallow-fusion token-level boosting trie over the SentencePiece tokenizer) to the Go Nemotron decoder so the configurable agent name (default `Hina`) is reliably transcribed. Build the trie at runtime from `[agent].name` + `name_aliases`, start from NeMo's RNNT params (`context_score ≈ 1.0`, `depth_scaling ≈ 2.0`), pair it with detect-and-strip wake-token routing, and fixture-test substitution rate with biasing off vs on across candidate names.
- Build a semantic VAD prototype over Nemotron partial transcripts and fixture-test it against incomplete utterances, backchannels, and definitive statements.
- Use `parakeet-rs` as a reference benchmark for Nemotron output quality and chunking if needed.
- Build a minimal Go custom agent loop against one local OpenAI-compatible LLM and one OpenAI Responses model.
- Evaluate `nlpodyssey/openai-agents-go` against cancellation, streaming tool calls, MCP, hosted tools, and local OpenAI-compatible backends.
- Install the Go toolchain and ONNX Runtime libraries in the v2 environment before native local-backend spikes.
- Decide the Go WebRTC stack and signaling details, likely starting with Pion unless a spike shows a better fit.

## Exit Criteria

- V1 remains runnable or is clearly tagged/frozen before V2 work breaks compatibility.
- V2 can be run as a local server and opened from a browser on the same machine
  on Windows 11 x64, macOS Apple Silicon, and Linux x86_64.
- The setup/doctor flow reports platform capabilities clearly on every Tier 1
  host, including missing `sbx`, missing Hypervisor Platform on Windows, missing
  llama.cpp, missing ORT DLLs, unavailable local ASR/TTS, and HTTPS/LAN issues.
- Optional LAN mode can be opened from another device with authentication and HTTPS guidance.
- Admin can configure active STT / LLM / TTS backends and view setup/backend logs.
- CI or release smoke tests cover Windows 11 x64, macOS, and Linux for server
  startup, migrations, web assets, config parsing, path handling, process
  supervision, and text-mode chat.
- The web UI is implemented with React + TypeScript + Vite, shadcn/ui Base UI components, Tailwind CSS, TanStack Router/Query/Table, Zustand for frontend-only UI preferences, and the agreed form/test stack.
- Local ASR is Nemotron-only and local TTS is Supertonic-only; Whisper.cpp, Kokoro-FastAPI, and Qwen3-TTS are not part of V2.
- Local ASR/TTS on Windows is enabled only after Nemotron and Supertonic pass the
  native Windows ONNX Runtime benchmark gates. Until then, Windows still passes
  V2 cloud/text/full-OpenAI-Realtime criteria with local voice marked
  unavailable.
- llama.cpp local LLM can be installed, started, health-checked, idle-unloaded,
  restarted, and stopped cleanly on Windows 11 x64, macOS, and Linux.
- Browser/WebRTC is the only voice transport, with a local Realtime-like endpoint and a full OpenAI Realtime mode.
- Users can log in, start sessions, resume sessions, view transcripts, and send typed messages without STT/TTS.
- Users can switch text -> live and live -> text within the same session without losing context.
- Resuming an old text or live session can continue in either mode using the prior conversation history.
- User can interrupt assistant speech by speaking.
- Local semantic VAD reduces premature turn commits on unfinished utterances without making complete requests feel sluggish.
- Backchannels do not usually interrupt the assistant.
- Assistant TTS output is not usually mistaken for user speech.
- User tool execution runs in per-user/per-session Docker `sbx` sandboxes with resource limits, host-service allow-lists, artifact capture, and audit logs.
- On Windows 11 x64, `sbx` sandboxes pass create/run/cp/policy/secret/mount
  smoke tests, and host inference access goes through the same explicit gateway
  and allow-list used on macOS/Linux.
- Main-model shell/file/MCP/HTTP/agent tool calls from text or live sessions run inside the calling user's `sbx` context and cannot see host files, host env, other users' workspaces, or ungranted secrets.
- Users have durable sandbox workspaces that survive server restarts and container teardown, while container root filesystems and unpromoted Automation run scratch space are cleaned up according to retention policy.
- Users can configure Sandbox Environment settings, including allowed CLI tools, MCP servers, host-service grants, secret/env grants, account-backed agent auth profiles, and local Pi availability.
- Codex, Claude Code, and Cursor can be authenticated through browser/subscription flows in the web UI, verified with status checks, logged out, and then used by chat tools or Automations without requiring API keys.
- API-key/token auth remains available through vaulted per-user secrets using provider-specific env vars, and the UI prevents Automations from selecting unavailable agent auth profiles.
- Secret vault master-key storage uses OS-appropriate protection: DPAPI /
  Credential Manager or ACL-checked files on Windows, and permission-checked
  key files or keyring on Unix-like systems.
- Pi can be invoked as a local-only Automation agent without Codex/Claude/Cursor accounts or API keys, using the host llama.cpp model through the controlled host inference gateway.
- Users can create, import, export, enable, disable, and manually run Automations. Normal creation/editing uses a guided builder that emits schema-validated `automation.v1` JSON; raw JSON is limited to import/export preview, import repair, and advanced/admin diagnostics.
- Automations can run on schedules while the server is up, resume schedules after restart, and stop cleanly on server shutdown.
- Automations support unrestricted and granular sandbox permission profiles, per-user secret grants, deterministic pre-model steps, run logs, artifacts, and final outputs.
- Codex, Claude Code, Cursor CLI, and Pi can be invoked through Automation adapters inside the user's sandbox and inherit that Automation's restrictions.
- Local mode and OpenAI Realtime mode have clearly documented transport choices and limitations.

# Phase 10 — Full OpenAI Realtime mode (browser WebRTC + server sideband)

Status: ready after Phases 3 + 6.
Depends on: Phase 3 (browser session client/abstraction), Phase 6 (session/event/barge-in model), Phase 7 (tools, for sideband tool calls).
Unblocks: full-cloud speech-to-speech as an admin-selectable execution mode.

## Goal

Add the second voice execution mode: the browser connects **directly to OpenAI's Realtime WebRTC endpoint** for full-cloud speech-to-speech, while the Go server keeps a **sideband control connection** for instructions, tool calls, transcript capture, and observability. The same browser session adapter from Phase 3 switches base targets (local vs OpenAI) so the UI feels identical. This bypasses local STT/LLM/TTS entirely.

Endpoints/flow verified in [`research-findings.md` B8](research-findings.md#b8-openai-realtime-integration--green) — **refresh the official docs immediately before implementing** (Realtime moves).

## Scope

### In
1. **Ephemeral secret minting** (app server): `POST /v1/realtime/client_secrets` with the real API key; set `OpenAI-Safety-Identifier` (hashed user id); never put the real key in browser code. Return the `ek_...` to the authenticated browser.
2. **SDP exchange**: browser (or a thin server proxy) `POST /v1/realtime/calls` with `Content-Type: application/sdp` + `Authorization: Bearer ek_...` → plain-text SDP answer. **Capture the `call_id` (`rtc_...`) from the `Location` header** — required for the sideband.
3. **Server sideband**: WebSocket to `wss://api.openai.com/v1/realtime?call_id=<rtc_...>` with the real API key. Browser owns media; server owns control — `session.update` (instructions/policy), monitor events, `response.create`, **handle tool calls through the Phase 7 sandbox layer**, capture transcripts into canonical turns (shared session history). Lifecycle via `POST /v1/realtime/calls/{id}/{accept,reject,refer,hangup}`.
4. **Turn detection + barge-in parity**: expose `server_vad`/`semantic_vad` config consistent with the local mode (Phase 6); on WebRTC, OpenAI auto-truncates unplayed audio (proactively `output_audio_buffer.clear`); reconcile its truncation with our canonical-text persistence so context stays aligned.
5. **Mode selection**: admin chooses voice execution mode (local/mixed vs full OpenAI Realtime); the browser session adapter targets the right base; idle-footprint policy keeps local STT/LLM/TTS unloaded in this mode unless another local session needs them.
6. **Models**: `gpt-realtime` family (configurable); legacy `gpt-4o-realtime-preview` superseded.

### Explicitly out (deferred)
- Mixed cloud-STT/local-LLM permutations beyond what the local Realtime-like endpoint (Phase 6) already covers.
- Anything requiring new browser-client architecture — reuse Phase 3's adapter.

## Windows posture
Pure cloud + browser; no local ONNX. **This mode works on Windows from the moment it ships** (it's part of why Windows can be a Tier-1 host before the local-voice ONNX gate). Validate in the Windows pass alongside text + cloud.

## Testable exit criteria
- [ ] A user holds a full-cloud speech-to-speech conversation; the real API key never reaches the browser (only `ek_...`).
- [ ] The server sideband attaches via the `call_id` from the `Location` header, updates instructions, and **handles a tool call through the user's `sbx` sandbox** during a live cloud call.
- [ ] Transcripts persist into the same canonical session history; switching that session back to text mode (Phase 2) shows the cloud-call turns.
- [ ] Barge-in works; truncation reconciles with persisted canonical text.
- [ ] Admin can switch a session between local/mixed and full OpenAI Realtime; idle local runtimes stay unloaded in cloud mode.
- [ ] Works on Windows (text/cloud Tier-1 surface).

## Risks & mitigations
- **Realtime API drift** → refresh docs right before implementing; the `call_id`-from-`Location` detail is the easy-to-miss one.
- **Two execution engines diverging** → keep the frontend/session abstraction shared (Phase 3), let only the server engines differ; reconcile cloud truncation with canonical persistence via the one context builder.
- **Key exposure** → ephemeral secrets only; sideband uses the real key server-side.

## References
- Realtime endpoints/flow/turn-detection/barge-in: [`research-findings.md`](research-findings.md) B8; `hina-agent-plan.md` (Transport Strategy, Execution Modes).

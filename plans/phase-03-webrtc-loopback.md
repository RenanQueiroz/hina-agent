# Phase 3 — WebRTC audio loopback + local Realtime-like endpoint (no models)

Status: ready after Phase 1 (can run parallel to Phase 2).
Depends on: Phase 1 (auth, event envelope).
Unblocks: Phase 4 (audio out), Phase 5 (audio in), Phase 6 (live pipeline), Phase 10 (cloud Realtime reuses the client).

## Goal

Prove the entire browser↔server audio transport **before any model exists**. A logged-in user opens a live session, the browser captures the mic and sends it over WebRTC to the Go/Pion server, the server sends audio back, and JSON control events flow over a datachannel. The first milestone is a literal **loopback** (server echoes mic audio back) plus a **tone/recorded-clip generator**, with end-to-end latency and the browser playback cursor measured. This is where all the WebRTC/Opus/resampling risk is retired.

The decided architecture is fixed in [`research-findings.md` B5](research-findings.md#b5-webrtc-media-bridge--green-architecture-yellow-on-one-sub-point). Summary: **inbound = Opus track decoded in pure Go; outbound = raw PCM over an `RTCDataChannel` to a browser AudioWorklet** (no Opus encoder needed), signaling mirrors OpenAI's `application/sdp` contract.

## Scope

### In
1. **Pion WebRTC v4** server peer: SDP offer/answer endpoint `POST /api/v1/realtime/calls` accepting `Content-Type: application/sdp`, returning the Pion answer as `application/sdp` plain text (mirrors OpenAI). Authenticated; one active talk session per user.
2. **Inbound audio**: `TrackRemote.ReadRTP()` → **`pion/opus`** decode (pure Go) → **`tphakala/go-audio-resampler`** 48 kHz→16 kHz (`QualityLow`) → 16 kHz mono float32 frames available to a (stubbed) consumer.
3. **Outbound audio**: server pushes **raw PCM frames (24 kHz s16) over an `RTCDataChannel`** (unordered, `maxRetransmits:0`); browser plays them through an **AudioWorklet ring buffer (~80–120 ms)**. Two sources to exercise it: (a) loopback of the decoded mic PCM, (b) a generated tone / bundled WAV clip.
4. **Control datachannel** (`events`): the **same typed event envelope from Phase 1**, now also flowing over the datachannel. Implement the audio/lifecycle subset: `ModeChanged`, `AudioInputFrame`(meta), `AudioOutputFrame`(meta), `PlaybackStarted/Progress/Stopped`, `UserInterrupted` (manual button for now), `ErrorEvent`.
5. **Playback cursor**: the AudioWorklet reports how much audio has actually been played back over the datachannel; the server tracks it. This is the foundation for truncation/barge-in in Phase 6 (Pion can't provide it — it must come from the browser).
6. **Browser live-mode UI (minimal)**: a "go live" control in the conversation (from Phase 2 if available, else a standalone test page), mic permission via `getUserMedia` with **echo cancellation + noise suppression + AGC** constraints enabled, device selector, listening/speaking state, end-call.
7. **Latency + loss instrumentation**: measure mic-capture→server, server→playback, round-trip loopback; expose Pion RTCP/loss stats (`RegisterDefaultInterceptors`, NACK, receiver reports). Admin-visible.
8. **HTTPS guidance**: LAN mic needs HTTPS (localhost exempt) — support user-provided cert/key and document `mkcert`/reverse-proxy (don't auto-install a CA). `hina doctor` reports cert status.

### Explicitly out (deferred)
- ASR/LLM/TTS (Phases 4–6) — the consumers of inbound PCM and the producers of outbound PCM are stubs/generators this phase.
- VAD, semantic turn detection, automatic interruption, echo *suppression* logic (Phase 6). This phase only proves transport + manual interrupt + cursor.
- The full OpenAI Realtime cloud mode (Phase 10) — but keep the client's session adapter abstract so it can later switch base targets (local vs OpenAI) per the main plan.

## Windows posture
Pion v4 is pure Go / no CGo, so this **builds and cross-compiles for Windows now** with no native toolchain — a deliberate reason it's early. The browser owns capture/playback, so there is no Go desktop audio device path to port. Hands-on Windows WebRTC loopback (browser capture/playback + Pion on localhost) is part of Phase 12's benchmark matrix, but the code is Windows-ready from this phase.

## Work breakdown
1. **Signaling endpoint** mirroring OpenAI's `/realtime/calls` `application/sdp` shape; per-user single active session; tie the peer's lifetime to a conversation id so live mode attaches to a session.
2. **Inbound pipeline**: track handler → `ReadRTP` → `pion/opus` decode → resampler → ring buffer of 16 kHz f32 frames + `AudioInputFrame` meta events (sample rate, channels, frame count, capture cursor).
3. **Outbound pipeline**: a PCM frame writer over the datachannel + the browser AudioWorklet player + ring buffer; pacing at ~20 ms; cursor reporting back.
4. **Loopback + generator** sources feeding the outbound pipeline.
5. **Datachannel event envelope**: reuse Phase 1 types; ensure the same event can be emitted to SSE (admin observability) and the datachannel (client).
6. **Live-mode UI**: getUserMedia constraints, AudioWorklet, datachannel JSON, manual interrupt button, state display, device selector.
7. **Metrics**: latency probes + Pion stats interceptor → admin view.
8. **HTTPS**: cert/key config + doctor check + docs.

## Testable exit criteria
- [ ] A user "goes live," speaks, and hears their own voice looped back with measured round-trip latency reported (target ballpark per the main plan's latency section; just *measure* here).
- [ ] A generated tone/clip plays cleanly through the AudioWorklet with no glitches at the ring-buffer size chosen.
- [ ] The server logs an accurate **playback cursor** that tracks what the browser has actually played (verified by stopping playback mid-clip and checking the reported cursor).
- [ ] Control events flow both directions over the datachannel using the Phase 1 envelope; a manual "interrupt" button stops outbound audio within the target budget and the cursor reflects the truncation point.
- [ ] Pion RTCP/loss stats are visible in the admin UI; inducing packet loss (throttle) degrades gracefully.
- [ ] Mic works over HTTPS on a second LAN device with a user-provided cert (documented), and over plain localhost.
- [ ] Builds + smoke-passes on the Windows CI runner (hands-on browser loopback on Windows deferred to Phase 12).

## Risks & mitigations
- **No pure-Go Opus encoder** → avoided entirely by PCM-over-datachannel (B5). Keep `jj11hh/opus` (WASM, CGo-free) noted as the fallback only if a true Opus return track is ever needed.
- **Datachannel head-of-line blocking under loss** → unordered + `maxRetransmits:0` + client ring buffer; measure under induced loss.
- **Knowing what was actually played** (barge-in foundation) → solved by the AudioWorklet cursor reported over the datachannel, not by server-side guessing.
- **Resampler latency** → `QualityLow` speech preset for the ASR-facing downsample; benchmark, don't over-spec quality.

## References
- Architecture + libraries + the OpenAI `application/sdp` contract to mirror: [`research-findings.md`](research-findings.md) B5, B8.

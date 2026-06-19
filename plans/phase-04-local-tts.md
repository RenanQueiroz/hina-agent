# Phase 4 — Local TTS (Supertonic) + ONNX runtime plumbing + idle-unload manager

Status: ready after Phases 2 + 3.
Depends on: Phase 2 (a turn to speak), Phase 3 (audio-out transport).
Unblocks: Phase 5 (shares the ORT runtime/manager), Phase 6 (TTS in the live loop).

## Goal

Give Hina a local voice. Stand up the **ONNX Runtime plumbing the whole local-inference stack shares** (`yalue/onnxruntime_go` + bundled ORT, lazy-load + idle-unload runtime manager), then implement **Supertonic 3** synthesis directly in Go and wire it to the Phase 3 audio-out path. The end-to-end demo: type a message (Phase 2) → assistant text → **spoken reply** streamed over WebRTC. TTS is the right first ONNX target — it's the strongest-GREEN component with an official Go example and **no phonemizer dependency**.

Library/version/model facts are fixed in [`research-findings.md` B1–B2](research-findings.md#b1-onnx-runtime-go-binding--green).

## Scope

### In
1. **ORT runtime foundation**: `yalue/onnxruntime_go` pinned `v1.31.0` + bundled ORT `1.31.x` shared libs, loaded via `SetSharedLibraryPath` from the **app-managed runtime dir** (never the system path). CGo isolated behind a build tag (`//go:build onnx`) so cloud-only/headless builds stay CGo-free. Asset download/pin/checksum/extract into the runtime dir; ORT version + provider + lib path surfaced in `hina doctor` and the admin UI.
2. **Runtime/idle-unload manager** (shared by TTS now, ASR in Phase 5): lazy-load a model on first need, keep one shared instance or a bounded worker pool (benchmark-driven), unload after an admin-configured idle TTL, expose load/warmup/synth-latency/failure events. Mirrors V1's idle-unload philosophy but in-process for ONNX (vs. V1's HTTP servers).
3. **Supertonic Go adapter**: port the official `supertone-inc/supertonic` Go example. Pure-Go text prep (NFKD via `golang.org/x/text`, codepoint→token-ID, no espeak-ng/misaki); the 4 ONNX graphs (`text_encoder`, `duration_predictor`, `vector_estimator` ~8 flow-matching steps, `vocoder`); per-voice style vectors; **44.1 kHz** mono output. Synthesize through an **internal Go API** (no local HTTP TTS hop), streaming sentence-by-sentence.
4. **Wire to audio-out**: emit synthesized 44.1 kHz frames into the Phase 3 PCM-over-datachannel path (resample 44.1→target only if needed; the AudioWorklet can also resample). Sentence-eager splitting (port V1's decimal-aware splitter so "3.14" isn't read as "three"). `TTSStarted`/`PlaybackStarted` events.
5. **Text-driven voice demo path**: a Phase-2 text turn can be spoken aloud into an active live session — the first real audio-out of model output, without needing ASR yet.
6. **Admin TTS health**: model load state, cold/warm synth latency, voice selection, idle-TTL config, failure surfacing.

### Explicitly out (deferred)
- ASR (Phase 5) — but the runtime manager built here is the one Phase 5 reuses.
- VAD/interruption/echo (Phase 6) — TTS here just plays to completion; mid-utterance truncation is Phase 6 (the playback cursor from Phase 3 is ready for it).
- **Windows local TTS stays disabled** until the Phase 11 ORT-DLL gate passes (Nemotron+Supertonic loading via the `yalue` binding from an app-managed DLL on a real Windows host). On Windows, `hina doctor` reports local TTS unavailable; cloud TTS still works.
- Cloud TTS adapters (OpenAI/Gemini) — keep them available through the cloud provider layer, but this phase is about the local path.

## Windows posture
Write the adapter and runtime manager cross-platform now; the ORT DLL loading path is coded (with the Windows `onnxruntime.dll` discovery via `SetSharedLibraryPath`) but its **hands-on validation is Phase 11**. The build-tagged CGo means the default Windows control-plane build still has no compiler dependency; the `onnx`-tagged build is what Phase 11 validates on Windows.

## Work breakdown
1. **ORT bring-up**: install Go toolchain + ORT 1.31.x libs in the dev/CI environment; `yalue` binding smoke (load a trivial ONNX, run it) on macOS/Linux; the build-tag split for CGo isolation.
2. **Asset manager**: pin Supertonic + ORT to specific commits/releases, download + checksum into the runtime dir, surface in doctor.
3. **Runtime manager**: lazy-load/idle-TTL/shared-instance/worker-pool + lifecycle events; design it model-agnostic so ASR slots in.
4. **Supertonic adapter**: text prep → 4-graph pipeline → 44.1 kHz frames; voice/style-vector selection; streaming by sentence.
5. **Audio-out wiring**: into Phase 3's datachannel PCM path + sentence splitter + TTS events.
6. **Demo + admin health + idle-TTL config.**
7. **Bench hooks**: cold-start, warm synth latency, CPU, memory (numbers gathered with the Phase 6 harness; hooks added here).
8. **OpenRAIL-M compliance**: V2 ships **preset voices only — no user voice cloning by default** (project decision), which clears the acute deepfake/impersonation concern. Still ship/point to the OpenRAIL-M license + a short acceptable-use line in the ToS. Do **not** add a CSM-style `ref_audio` cloning path without revisiting the license. Per [`research-findings.md` B10](research-findings.md#b10-model--runtime-asset-licensing--green-no-blockers-2-conditions).

## Testable exit criteria
- [ ] On macOS + Linux, `yalue`/ORT loads from the app-managed lib path and runs a trivial ONNX model.
- [ ] Supertonic synthesizes intelligible 44.1 kHz speech from text through the internal Go API (no HTTP hop), streaming by sentence.
- [ ] A Phase-2 text turn is **spoken aloud** into an active live session over the Phase 3 datachannel with no glitches.
- [ ] The runtime manager lazy-loads on first synth, stays warm during activity, and unloads after the idle TTL (verified by memory drop + `Runtime*` events).
- [ ] Admin UI shows ORT version/provider/lib path + TTS load state + cold/warm latency.
- [ ] Default control-plane build is still `CGO_ENABLED=0`; only the `onnx`-tagged build links ORT. CI builds both.
- [ ] Windows: control-plane builds; `hina doctor` correctly reports local TTS unavailable (gated to Phase 11).

## Risks & mitigations
- **`vector_estimator` (257 MB, ~8 steps) latency** → benchmark cold/warm; consider step-count/quality tradeoffs; bounded worker pool.
- **ORT packaging/cross-compile + DLL shipping** → the real engineering cost (not the text pipeline); pin ORT, app-managed lib dir, doctor visibility; Windows DLL validated in Phase 11.
- **CGo creeping into the control plane** → strict build-tag isolation + CI building the CGo-free default.

## References
- Binding/version/model details: [`research-findings.md`](research-findings.md) B1, B2, B10.
- V1 Supertonic handling (native 44.1 kHz, sentence splitter, expression tags): `/home/renan/voice-agent/AGENTS.md`.

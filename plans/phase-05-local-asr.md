# Phase 5 — Local streaming ASR (Nemotron) + agent-name biasing

Status: **implemented.** The full pipeline is a pure-Go port in `internal/asr` over the Phase 4 ORT runtime: a NeMo-faithful log-mel front-end (own radix-2 FFT + Slaney mel filterbank, **no** per-feature normalization), a SentencePiece unigram tokenizer (with Viterbi encoding for the bias trie), a cache-aware FastConformer encoder streaming loop, an RNNT greedy decoder, decode-time agent-name biasing, and a session-layer wake-word strip. Wired through `internal/onnx` (int32 tensors added for the decoder), `internal/assets` (Nemotron `smcleod` int8 export pinned + SHA256-verified), config/events/doctor/admin, and the rtc live path (`ListenStarted`/`ListenStopped` → `ASRPartial`/`ASRFinal`). Unit-tested with fakes + synthetic audio; the real graphs are exercised by the `onnx` CI job on Linux (`internal/asr` integration test). Real-speech WER and the name-substitution-rate measurement use recorded fixtures and run in the Phase 6 benchmark harness.
Depends on: Phase 3 (16 kHz mic frames), Phase 4 (ORT runtime + manager).
Unblocks: Phase 6 (the live STT→LLM→TTS loop).

> **Model + decode facts recorded during implementation** (re-verified against the live HF repo + `parakeet-rs` reference, per the AGENTS.md drift rule):
> - **Model repo:** `smcleod/nemotron-3.5-asr-streaming-0.6b-int8` (commit `f1f26d2`); files `encoder.onnx` (+ external `encoder.onnx.data`), `decoder_joint.onnx`, `tokenizer.model`. `config.json` is **not** shipped as an asset — the prompt dictionary + dims are embedded in `internal/asr`.
> - **Front-end:** `n_mels=128, n_fft=512, win=400, hop=160, preemph=0.97`, power spectrum, Slaney mel, `ln(x + 2^-24)`, and **`normalize="NA"`** — the encoder consumes raw log-mel (no per-feature mean/var), confirmed by config.json + the reference. Center zero-pad `n_fft/2`; symmetric (periodic=False) Hann.
> - **Streaming geometry:** 9 pre-encode-cache + 56 main = 65 mel frames per encoder call → 7 output frames; cache tensors `cache_last_channel[24,1,56,1024]`, `cache_last_time[24,1,1024,8]`, `cache_last_channel_len[1]` threaded each chunk; `prompt_index` default `auto`=101.
> - **Decoder:** combined `decoder_joint.onnx` with **int32** `targets`/`target_length`; vocab 13087 pieces + blank at 13087 (13088 joint logits); LSTM-640 ×2; `max_symbols_per_step=10`; `last_token` seeded with blank.
> - **External-data caveat:** the `yalue` binding has no in-memory external-data API, so `encoder.onnx` loads by its **verified path** (ORT resolves `encoder.onnx.data` next to it); the self-contained `decoder_joint.onnx` + `tokenizer.model` load from checksum-verified bytes. The asset root is owner-private (SecureRoot), so the residual is a same-user swap in the verify→open window.

## Goal

Give Hina ears. Implement **Nemotron 3.5 streaming ASR (0.6B)** as a native Go adapter over the Phase 4 ORT runtime, producing **partial transcript events while the user speaks** and a final on turn commit — plus **decoder-side context biasing** so the configurable agent name (`Hina`) transcribes reliably. This is the hardest local spike (we own the front-end + RNNT decode + cache state), which is why it follows the easier TTS win.

All model/graph/decode facts are pinned in [`research-findings.md` B3](research-findings.md#b3-nemotron-35-streaming-asr-06b-in-go--green-most-owned-code).

## Scope

### In
1. **Log-mel front-end in Go** (NOT in the ONNX graph): NeMo FilterbankFeatures spec — `n_mels=128, n_fft=512, win=400, hop=160, preemph=0.97`. Validate against a reference NeMo/`parakeet-rs` feature dump (small numeric mismatches silently hurt WER — this is the #1 risk).
2. **Encoder + streaming cache loop**: feed `processed_signal`, lengths, `cache_last_channel`[24,1,56,1024], `cache_last_time`[24,1,1024,8], `cache_last_channel_len`, `prompt_index` (101=auto-detect); thread the `*_next` caches back each chunk. Own per-stream cache tensors; never reload the model or recreate sessions per turn.
3. **RNNT greedy decode in Go**: loop joint per frame, emit non-blank, advance the 2-layer LSTM-640 prediction state, blank→next frame, cap `max_symbols_per_step`. `blank_id=13087`, vocab 13088. **`parakeet-rs` (MIT) `src/model_nemotron.rs` is the port reference.**
4. **SentencePiece tokenizer** (13,088 BPE) → text. Punctuation + capitalization come from the model (no separate punct model).
5. **Streaming events**: `ASRPartial` per chunk while speaking, `ASRFinal` on turn commit, detected-language metadata, chunk timing + model latency metrics. Chunk size configurable (80/160/320/560/1120 ms via `att_context_size=[56,R]`); start at 560 ms.
6. **Agent-name context biasing** (pure decode-time, graph-independent): a SentencePiece-token trie over `[agent].name` + `name_aliases`; per-hypothesis trie pointer; after the joint emits log-probs add `λ·boost` to tokens continuing an active path, advance/cancel on match/mismatch. Rebuild the trie at runtime when the name changes (no retrain, no ONNX change). Start from NeMo RNNT `context_score≈1.0`, `depth_scaling≈2.0`, tune on fixtures.
7. **Wake/address-token routing**: detect-and-strip the agent name at the session layer before building the LLM prompt, matched case-insensitively against name+aliases — so a single mis-transcription degrades wake detection for that turn instead of corrupting the user's request. Integrates with the Phase 2 shared context builder.
8. **Model choice**: primary = **`smcleod/...-int8`** (combined `decoder_joint.onnx`, full chunk range, clean OpenMDW-1.1); keep `onnx-community ...-int4` (560 ms-fixed) as a size/latency comparison only.

### Explicitly out (deferred)
- VAD-driven turn boundaries, semantic VAD, interruption (Phase 6) — this phase emits partials/finals given a speech segment; Phase 6 decides *when* a turn starts/ends.
- The full live loop (Phase 6).
- **Windows local ASR stays disabled** until the Phase 11 ORT-DLL gate; Windows reports local voice unavailable.

## Windows posture
Adapter written cross-platform over the `onnx`-tagged ORT build from Phase 4. Hands-on Windows ASR (DLL load + streaming cache + latency) is the centerpiece of the Phase 11 ORT spike. Until then, Windows = text + cloud + OpenAI Realtime only.

## Work breakdown
1. **Front-end** + a feature-parity fixture test vs a reference dump (gate before trusting WER).
2. **Encoder session + cache management** over the Phase 4 runtime manager (lazy-load/idle-TTL, like TTS).
3. **RNNT greedy decoder** + SentencePiece detokenize; chunked streaming with partials.
4. **Context-biasing trie** + runtime rebuild on name change; wake-token detect-and-strip at the session layer.
5. **ASR events** into the Phase 1 envelope; `ASRPartial`/`ASRFinal` rendered in the timeline (transcript updates).
6. **Name-recognition fixture**: record "<name>, …" many times; measure substitution rate biasing off vs on and across candidate names — validate the name choice + bias params on real CPU-ONNX output (Phase 6 harness runs it; fixture authored here).

## Testable exit criteria
- [x] Go log-mel front-end ported to the NeMo spec and unit-tested (FFT vs naïve DFT, Slaney filterbank shape/norm, preemphasis, silence-floor proving normalization is off, sine-band concentration). Bit-exact parity vs a reference NeMo dump is deferred to the Phase 6 harness (no NeMo runtime available here); the real encoder validates the front-end end-to-end in the `onnx` CI test.
- [x] Feeding mic frames produces streaming `ASRPartial` events at the chunk cadence and an `ASRFinal` on segment end, with per-stream cache + decoder state reset cleanly across turns (rtc listen-flow test + asr engine test; real graphs in the `onnx` CI integration test).
- [~] Punctuation + capitalization come from the model (no separate punct model); auto language detection via `prompt_index=101`. Multilingual-fixture WER is a Phase 6 harness measurement.
- [x] Biasing mechanism implemented + unit-tested (a confusable word-initial token flips to the biased one). The real "Hina" vs "Nina"/"Tina" substitution-rate drop is a Phase 6 fixture measurement.
- [x] Wake-token detect-and-strip removes a leading address before the body; a mis-transcription degrades only wake detection, not the request body (unit-tested).
- [x] Runtime manager lazy-loads/idle-unloads ASR like TTS (shared `onnx.Lifecycle`; idle-unload test).
- [x] Windows: control-plane builds; `hina doctor` reports local ASR unavailable (gated to Phase 11).

## Risks & mitigations
- **Front-end numeric fidelity** (#1 risk) → fixture-compare to NeMo/`parakeet-rs` before tuning anything else.
- **RNNT/cache correctness** → port carefully from `parakeet-rs`; per-stream cache reset tests across turns.
- **Native Go ASR being more work than expected** → it's the biggest spike; if it stalls, the `parakeet-rs` Rust reference is the fallback validation path (and, worst case, a sidecar), but the main plan's intent is native Go.

## References
- Full graph I/O, decode, biasing, model choice: [`research-findings.md`](research-findings.md) B3, B10.

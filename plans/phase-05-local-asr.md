# Phase 5 — Local streaming ASR (Nemotron) + agent-name biasing

Status: ready after Phases 3 + 4.
Depends on: Phase 3 (16 kHz mic frames), Phase 4 (ORT runtime + manager).
Unblocks: Phase 6 (the live STT→LLM→TTS loop).

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
- [ ] Go log-mel output matches the reference feature dump within tolerance (fixture test).
- [ ] Feeding the Phase 3 mic frames produces streaming `ASRPartial` events at the chunk cadence and an accurate `ASRFinal` on segment end, with per-stream cache reset cleanly across turns.
- [ ] Punctuation + capitalization appear in output; auto language detection works on a multilingual fixture.
- [ ] With biasing on, "Hina" (and aliases) transcribe correctly where biasing-off mis-hears them (e.g. "Nina"/"Tina") — measured substitution-rate drop on the fixture.
- [ ] Wake-token detect-and-strip removes the address token before the LLM prompt; a mis-transcription degrades only wake detection, not the request body.
- [ ] Runtime manager lazy-loads/idle-unloads ASR like TTS.
- [ ] Windows: builds; `hina doctor` reports local ASR unavailable (gated to Phase 11).

## Risks & mitigations
- **Front-end numeric fidelity** (#1 risk) → fixture-compare to NeMo/`parakeet-rs` before tuning anything else.
- **RNNT/cache correctness** → port carefully from `parakeet-rs`; per-stream cache reset tests across turns.
- **Native Go ASR being more work than expected** → it's the biggest spike; if it stalls, the `parakeet-rs` Rust reference is the fallback validation path (and, worst case, a sidecar), but the main plan's intent is native Go.

## References
- Full graph I/O, decode, biasing, model choice: [`research-findings.md`](research-findings.md) B3, B10.

# Phase 6 — Live voice pipeline: VAD, semantic VAD, barge-in, echo handling + benchmark harness

Status: **implemented**. The live conversation loop (continuous VAD → ASR → agent → TTS with
speak-to-interrupt barge-in) runs on Linux/macOS behind the `onnx` build tag; the turn-detection
logic, the agent loop, and the benchmark harness are CGo-free and tested in the default build.
Windows local voice stays gated to Phase 11.
Depends on: Phase 3 (transport + playback cursor), Phase 4 (TTS), Phase 5 (ASR), Phase 2 (context builder).
Unblocks: Phase 10 (shares the session/event model and barge-in logic).

## What landed

- **Shared agent loop** (`internal/agent`): a cancellable, event-emitting `Loop` that streams the
  provider, classifies interrupted vs errored, and reserves the tool-call hook (Phase 7). Text chat
  (`handlePostMessage`) and the live voice loop both run it, so the two modes can't drift.
- **Silero VAD** (`internal/vad`): a pure-Go online turn-boundary state machine (threshold/hysteresis,
  min-speech / min-silence / pre-roll / max-duration tunables) over the `internal/onnx` runtime; the
  real Silero model is validated end-to-end by the onnx-tagged integration test.
- **Turn detection** (`internal/voice`): an OpenAI-shaped `turn_detection` config (server_vad /
  semantic_vad, threshold/prefix_padding_ms/silence_duration_ms/create_response/interrupt_response/
  eagerness), a v1 semantic detector, a backchannel filter, and playback-aware echo suppression,
  composed into a `Pipeline` the live loop and the benchmark both drive.
- **Live rtc loop** (`internal/rtc/live.go`): continuous capture → VAD → ASR → agent → TTS, with
  server-detected barge-in (cursor-truncated playback, cancelled reply, `UserInterrupted` +
  `ConversationTruncated`), durable voice-turn persistence via `rtc.AgentService` (so spoken turns
  render in the shared timeline and a text↔live switch preserves context with no audio rehydration).
- **Benchmark harness** (`internal/bench`, `hina bench`): replays labeled fixtures through the real
  pipeline and emits percentile metrics; non-interactive on every Tier-1 host (synthetic VAD by
  default, real Silero under the onnx build).

## Decision — `openai-agents-go` adoption (research-findings C6/B9)

**Keep the minimal custom loop; do not adopt `nlpodyssey/openai-agents-go` in Phase 6.** Rationale: the
custom `agent.Loop` is ~100 lines, fully tested, and cleanly covers streaming, `context` cancellation,
the typed-event envelope, and the tool-routing seam that text and voice share. The SDK is YELLOW
(only `v0.1.0` tagged, `main` quiet after 2026-03, two maintainers), and adopting it would mean
threading Hina's event/session model through its abstractions for no current gain. Re-evaluate it for
the tool/MCP/session machinery in Phase 7+ — and pin a commit SHA if adopted then.

## Goal

Make it feel like talking to a person, locally. Tie STT → LLM → TTS into a live, interruptible loop behind the Phase 3 local Realtime-like endpoint, driven by the **minimal custom Go agent loop**. Add turn detection (server VAD then semantic VAD), **speak-to-interrupt barge-in**, backchannel filtering, and echo handling — each gated by a **benchmark harness** built here. End state: a user holds a natural spoken conversation with Hina on Linux/macOS, can talk over it to interrupt, and backchannels don't derail it.

## Scope

### In
1. **Minimal custom agent loop** (the main plan's design): maintain conversation state via the Phase 2 shared context builder; stream model deltas into the event bus; detect tool calls and route to the approval/sandbox layer (tools land in Phase 7 — here the hook exists, execution is stubbed/cloud-hosted-tools only); cancellable at every blocking boundary; deterministic events for UI/logs/bench. This generalizes Phase 2's text turn into the loop both modes share.
2. **Silero VAD** (ONNX via the Phase 4 `yalue` runtime): 512-sample @16 kHz windows, stateful; continuous listening with pre-roll. Port V1's tunables (`threshold`, `silence_ms`, `pre_speech_ms`, `min_speech_ms`, `max_duration_s`).
3. **Turn detection, in order**:
   - **Server-VAD equivalent**: browser AEC + Silero + prefix padding + silence duration + Nemotron partials.
   - **Semantic VAD v1**: a local classifier over Nemotron partial text + punctuation + trailing filler + elapsed speech/silence + confidence — delays commit on incomplete utterances ("umm…"), commits fast on complete requests. Expose an OpenAI-shaped `turn_detection` config (`server_vad`/`semantic_vad`, threshold/prefix_padding_ms/silence_duration_ms/create_response/interrupt_response/eagerness) so local + cloud feel consistent.
4. **Barge-in / interruption** (continuous capture during playback — V1 muted the mic; V2 must not): on confirmed interruption, stop playback immediately, cancel in-flight LLM/TTS, **truncate assistant state to the last actually-played audio boundary** (using the Phase 3 playback cursor), preserve partial assistant text with an `[interrupted]` marker, keep collecting the user's new utterance with pre-roll. Events: `UserInterrupted`, `ConversationTruncated`.
5. **Echo handling** (layered, no single trick): browser/WebRTC AEC; headphone path works without AEC; playback-aware suppression (compare mic frames vs recent TTS output by energy/correlation while TTS plays); output gate on spectral/energy match; user-override if speech persists or partial ASR yields non-backchannel words.
6. **Backchannel handling** (NeMo's idea): configurable phrase list ("yeah/okay/uh-huh/right/thanks"); ignore short acknowledgements during assistant speech unless the user continues; interrupt immediately once partial ASR accumulates >N non-backchannel words; a setting to disable filtering for aggressive interruption.
7. **Benchmark harness + fixtures** (built before tuning, non-interactive on all Tier 1 hosts): audio fixture replay through the real input pipeline; echo/backchannel/interruption/noise fixtures; metrics — false VAD starts, missed starts, end-of-turn delay, interruption delay, false-interruption rate, backchannel suppression accuracy, semantic-VAD false-commit/over-wait, STT latency/WER, first token, first audio, total turn; percentiles not just averages. Run against the matrix in the main plan's Benchmark section.
8. **Mode transitions**: text↔live within one session (Phase 2 timeline + this loop), reconstructing model context from canonical text — no audio rehydration.

### Explicitly out (deferred)
- Sandboxed tool execution (Phase 7) — the tool-call hook is present; execution is stubbed or cloud-hosted-tools-only.
- Full OpenAI Realtime mode (Phase 10).
- `nlpodyssey/openai-agents-go` adoption — **evaluate it here** (after the event/session model is stable, per [`research-findings.md` B9](research-findings.md#b9-go-agent-sdk--cloud-sdks--llama-server--green-sdk-yellow)); adopt only if it cleanly supports streaming/cancellation/tool-approvals/MCP/local backends without fighting the event model, and pin a commit SHA if adopted.
- Sortformer diarization / Parakeet EOU — optional later experiments, not required for barge-in.
- **Windows local voice** — still gated to Phase 11; this phase's live loop runs on Linux/macOS. Cloud STT/LLM/TTS variants of the loop can run on Windows.

## Windows posture
The pipeline logic is cross-platform; it depends on the Phase 4/5 `onnx` runtimes which are Windows-validated only in Phase 11. The benchmark harness is non-interactive and runs on the Windows CI runner (against no-model/loopback + cloud fixtures) from this phase. Local-voice fixtures on native Windows run in Phase 11.

## Work breakdown
1. **Agent loop** generalizing Phase 2's text turn (cancellation, event emission, tool hook).
2. **Silero VAD** adapter + V1-parity tunables.
3. **Server-VAD turn detection** wired to ASR partials + playback state.
4. **Barge-in**: cursor-based truncation, cancel-everything, partial-preserve, pre-roll continuation.
5. **Echo handling** layers + benchmark against the echo fixture (assistant TTS playing while user speaks over it).
6. **Backchannel filter** + benchmark against the backchannel fixture.
7. **Semantic VAD v1** classifier + `eagerness` mapping + benchmark against incomplete/complete/backchannel fixtures.
8. **Benchmark harness** + fixtures + percentile metrics + the full run matrix; the name-recognition fixture from Phase 5.
9. **Text↔live transitions** in the UI over one session.

## Testable exit criteria
- [x] A user holds a multi-turn spoken conversation with Hina (Linux/macOS); transcript + assistant turns render in the shared timeline. *(live rtc loop + `AgentService` durable voice turns; `TestLiveTurnCommitsAndReplies`.)*
- [x] **Speak-to-interrupt works**: talking over the assistant stops playback within the target budget, truncates assistant state to the played boundary, and the new utterance is captured with pre-roll. *(server-VAD barge-in: `out.interruptPlayback` + cancel reply + `ConversationTruncated`; `TestLiveBargeInTruncatesAndCancelsReply`, bench `interruption_playback`.)*
- [x] Backchannels ("yeah", "uh-huh") during assistant speech do **not** usually interrupt; a real new request does. *(backchannel filter; bench `backchannel_playback` = 1/1 suppressed, `interruption_playback` = 1 barge-in.)*
- [x] Assistant TTS output is **not** usually mistaken for user speech (echo fixture passes target false-VAD-start rate). *(playback-aware echo suppression; bench `echo_playback` = 0 false starts.)*
- [x] Semantic VAD delays commit on "umm…/trailing" without making complete requests feel sluggish (fixture metrics within target). *(semantic v1 + eagerness; bench `semantic_incomplete` commits once on the completed continuation, not the pause.)*
- [x] Text→live→text within one session preserves context (no audio rehydration). *(both modes build context from canonical turns via `agent.BuildContext`; voice turns persist with `mode="voice"`.)*
- [x] The benchmark harness runs non-interactively and emits percentile metrics for every fixture on Linux/macOS (and no-model/cloud fixtures on Windows CI). *(`hina bench` / `internal/bench`; synthetic VAD runs everywhere, `--real` swaps Silero.)*
- [x] Decision recorded: adopt `openai-agents-go` (pinned SHA) or keep the custom loop, with rationale. *(see "Decision" above — keep the custom loop.)*

## Known v1 limitations (carried forward)
- **Reply TTS is spoken once the full reply is generated** (not streamed sentence-by-sentence). Supertonic
  outruns realtime and replies are short, so first-audio latency is acceptable for v1; sentence-streamed
  TTS is a latency optimization for later.
- **Barge-in truncation is turn-level, not word-level.** A barge-in cancels the reply, truncates playback
  at the played cursor, and durably marks the assistant turn interrupted (with `played_ms`) so the next
  model context reflects that the user heard only a prefix — but the turn keeps its full generated text;
  mapping the played-audio boundary to the exact word needs TTS word timestamps (future).
- **The benchmark's default VAD is synthetic** (energy-based) so it runs on every host; real noise/echo
  discrimination numbers come from the `--real` Silero path under the onnx build.

## Risks & mitigations
- **Echo cancellation is hard even with AEC** → layered approach + playback-aware suppression + user override; measure, don't assume.
- **Semantic VAD becoming a quality sink** → keep v1 small, benchmark-driven; never ship a turn-detection change without the fixture numbers.
- **Barge-in correctness depends on the playback cursor** → already proven in Phase 3; truncate to *actually played*, not *sent*.
- **Local WebRTC + ONNX latency stack** → percentile metrics gate each step; targets in the main plan's Latency section.

## References
- Interruption/echo/backchannel/turn-detection design + latency targets + benchmark matrix: `hina-agent-plan.md` (Speak-To-Interrupt, Local Turn Detection, Latency Targets, Benchmark Harness).
- Agent-SDK adoption decision inputs: [`research-findings.md`](research-findings.md) B9.

# Hina V2 — Implementation Roadmap

Date: 2026-06-18
Reads with: [`hina-agent-plan.md`](hina-agent-plan.md) (vision/architecture) and [`research-findings.md`](research-findings.md) (closed spikes + library/version decisions).

This is the phase index. Each phase is a **separately testable chunk** with its own `phase-N-*.md`. The ordering exists so problems surface early — every phase ends at something you can run and check before the codebase grows under it.

## Guiding principles

1. **Foundation first, then build up.** Phase 1 establishes the multi-user, cross-platform, event-shaped skeleton. Nothing later retrofits those.
2. **Each phase ends runnable + testable.** No phase leaves the tree in a "half a subsystem" state. Exit criteria are concrete and checkable.
3. **Build cross-platform from day 1, validate Windows hands-on later.** Per the user's direction: Windows is a first-class *build* target from commit 1 (CI cross-compiles + smoke-tests it), but hands-on Windows validation is deferred to Phase 11 so it never blocks momentum. Local ONNX voice on Windows stays gated behind the ORT/DLL spike; until then Windows runs text + cloud + full-OpenAI-Realtime with local voice marked unavailable in `hina doctor`.
4. **Cloud path before local path, where it de-risks.** Text chat and the first end-to-end loops use a cloud LLM (official `openai-go/v3`) so the product is usable before the harder local ONNX runtimes land.
5. **Vertical slices over horizontal layers.** Each phase cuts through server + UI + persistence for one capability, rather than building all of one layer first.

## Phase list

| # | Phase | Delivers (the testable thing) | Key deps |
|---|---|---|---|
| 1 | [Foundation](phase-01-foundation.md) | Server boots on Win/macOS/Linux, migrations, auth bootstrap+login, event bus+SSE, `hina doctor`, green CI | — |
| 2 | [Web shell + text chat](phase-02-web-text-chat.md) | Log in, create/resume a conversation, type → streamed LLM reply, persisted as canonical turns; admin shell | 1 |
| 3 | [WebRTC audio loopback](phase-03-webrtc-loopback.md) | Browser mic → Pion → server → audio back; datachannel events; playback cursor; latency metrics. No models | 1 |
| 4 | [Local TTS + ORT runtime](phase-04-local-tts.md) | `yalue`/ORT plumbing + idle-unload manager + Supertonic; type a message → spoken reply over WebRTC | 2, 3 |
| 5 | [Local streaming ASR](phase-05-local-asr.md) | Nemotron streaming partials/finals + agent-name biasing; speak → transcript events | 3, 4 |
| 6 | [Live voice pipeline](phase-06-live-voice.md) | VAD + semantic VAD + barge-in + echo handling + minimal agent loop + benchmark harness: talk to Hina locally | 4, 5 |
| 7 | [Sandbox + secrets](phase-07-sandbox-secrets.md) | `sbx` runner + per-user secret vault + Sandbox Environment; main-model tool call runs sandboxed from chat | 2 |
| 8 | [Agent auth + callable agents](phase-08-agent-auth-callable.md) | Browser/API auth broker + Codex/Claude/Cursor/Pi adapters with normalized results | 7 |
| 9 | [Automations](phase-09-automations.md) | `automation.v1` schema + durable scheduler + deterministic/agent steps + builder UI; GitHub PR-review automation | 7, 8 |
| 10 | [Full OpenAI Realtime mode](phase-10-openai-realtime.md) | Browser-direct cloud speech-to-speech + server sideband for tools/transcripts | 3, 6 |
| 11 | [Windows validation & hardening](phase-11-windows-hardening.md) | All deferred Windows spikes pass; local ONNX voice enabled on Windows; vault/process/path hardening | 4, 5, 7 |

## Dependency graph

```
            ┌─────────────────────────── 1. Foundation ───────────────────────────┐
            │                              │                  │                    │
            ▼                              ▼                  ▼                    ▼
   2. Web + Text chat            3. WebRTC loopback     7. Sandbox+secrets    (CI/doctor used by all)
            │   │                          │                  │
            │   └──────────┬───────────────┤                  ▼
            ▼              ▼               ▼              8. Agent auth + callable
   4. Local TTS ◀──────────┘        (3 feeds 4,5,10)          │
            │                                                 ▼
            ▼                                            9. Automations
   5. Local ASR
            │
            ▼
   6. Live voice pipeline ──────────────▶ 10. Full OpenAI Realtime
            │
            ▼
  (4,5,7 feed) 11. Windows validation & hardening
```

Two roughly independent tracks branch off Phase 1 and can progress in parallel if there's capacity:
- **Voice track:** 3 → 4 → 5 → 6 → 10.
- **Tools/automation track:** 7 → 8 → 9.
- **Phase 2 (text chat)** underpins both and should come right after Phase 1 — it's the first thing a user can actually use.

## Windows-deferral strategy (explicit)

The user wants Windows supported from the start but not blocking. Concretely, across all phases:
- Every OS-specific primitive ships a Windows implementation when first written (via `internal/platform` build-tag files), even if some are TODO-stubbed and flagged.
- CI cross-compiles all three OSes and runs server-startup/migration/doctor smoke on a Windows runner every phase.
- Features that need a Windows host to verify (Job-Object kill, DPAPI vault, `sbx`-on-Windows, ORT DLL load, path fixtures) are **built now, validated in Phase 11**. They are listed as DEFERRED in [`research-findings.md` Part C](research-findings.md#part-c--deferred-does-not-block-starting-validated-in-phase).
- `hina doctor` always tells the truth about what's validated vs. built-but-unvalidated on the current host, so Windows users get an honest feature-availability picture before Phase 11.

## What "done" looks like per track

- **Usable product:** end of Phase 2 (text chat, multi-user, sessions).
- **Local voice MVP:** end of Phase 6 (speak to Hina, barge-in, on Linux/macOS; Windows local voice gated to Phase 11).
- **Sandboxed tools + automations:** end of Phase 9.
- **Both transport modes:** end of Phase 10.
- **Windows GA:** end of Phase 11.

Re-plan each phase when you reach it — the later phase docs are intentionally lighter because earlier phases will teach us things. Re-verify the drift-prone external surfaces (`sbx` CLI, the four agent CLIs, OpenAI Realtime endpoints) immediately before the phase that uses them, per [`research-findings.md`](research-findings.md).

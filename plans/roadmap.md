# Hina V2 — Implementation Roadmap

Date: 2026-06-18
Reads with: [`hina-agent-plan.md`](hina-agent-plan.md) (vision/architecture) and [`research-findings.md`](research-findings.md) (closed spikes + library/version decisions).

This is the phase index. Each phase is a **separately testable chunk** with its own `phase-N-*.md`. The ordering exists so problems surface early — every phase ends at something you can run and check before the codebase grows under it.

## Guiding principles

1. **Foundation first, then build up.** Phase 1 establishes the multi-user, cross-platform, event-shaped skeleton. Nothing later retrofits those.
2. **Each phase ends runnable + testable.** No phase leaves the tree in a "half a subsystem" state. Exit criteria are concrete and checkable.
3. **Build cross-platform from day 1, validate Windows hands-on later.** Per the user's direction: Windows is a first-class *build* target from commit 1 (CI cross-compiles + smoke-tests it), but hands-on Windows validation is deferred to Phase 12 so it never blocks momentum. Local ONNX voice on Windows stays gated behind the ORT/DLL spike; until then Windows runs text + cloud + full-OpenAI-Realtime with local voice marked unavailable in `hina doctor`.
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
| 8 | [Agent auth + callable agents](phase-08-agent-auth-callable.md) | Browser/API auth broker + Codex/Claude/Cursor/Pi adapters with normalized results | 7, 11 |
| 9 | [Automations](phase-09-automations.md) | `automation.v1` schema + durable scheduler + deterministic/agent steps + builder UI; GitHub PR-review automation | 7, 8 |
| 10 | [Full OpenAI Realtime mode](phase-10-openai-realtime.md) | Browser-direct cloud speech-to-speech + server sideband for tools/transcripts | 3, 6 |
| 11 | [Managed local llama.cpp LLM](phase-11-managed-local-llm.md) | Install/spawn/supervise/idle-unload a local `llama-server` + web-admin runtime/model management (structured preset editor → restart); local text + voice with no cloud account | 1, 2 |
| 12 | [Windows validation & hardening](phase-12-windows-hardening.md) | All deferred Windows spikes pass; local ONNX voice + llama.cpp enabled on Windows; vault/process/path hardening | 4, 5, 7, 11 |

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

   11. Managed llama.cpp LLM backend  (deps 1, 2) — the account-free local model:
       the default backend for 6's agent loop and the Pi endpoint consumed in 8.
   12. Windows validation & hardening — validates 4, 5, 7, 11 on Windows (GA).
```

Phase 11 (managed local llama.cpp) is a cross-cutting **backend** the LLM provider abstraction plugs into. Its **core** (install/supervise the backend + drive the main app's local text and voice) depends only on 1 + 2, so it can land any time after text chat; only its **sandbox-exposure hook** waits on Phase 7's host-inference gateway. Its high number reflects when the gap was identified, not its priority — it's what makes Hina's local text **and** voice work with no cloud account, and it's a prerequisite for the local-only Pi path (Phase 8).

Three roughly independent tracks branch off Phase 1 and can progress in parallel if there's capacity:
- **Voice track:** 3 → 4 → 5 → 6 → 10.
- **Tools/automation track:** 7 → 8 → 9.
- **Local-backend track:** 11 (managed llama.cpp) — feeds the local default for 6's agent loop and the Phase 8 Pi adapter.
- **Phase 2 (text chat)** underpins all three and should come right after Phase 1 — it's the first thing a user can actually use.

## Windows-deferral strategy (explicit)

The user wants Windows supported from the start but not blocking. Concretely, across all phases:
- Every OS-specific primitive ships a Windows implementation when first written (via `internal/platform` build-tag files), even if some are TODO-stubbed and flagged.
- CI cross-compiles all three OSes and runs server-startup/migration/doctor smoke on a Windows runner every phase.
- Features that need a Windows host to verify (Job-Object kill, DPAPI vault, `sbx`-on-Windows, ORT DLL load, `llama-server.exe` supervision, path fixtures) are **built now, validated in Phase 12**. They are listed as DEFERRED in [`research-findings.md` Part C](research-findings.md#part-c--deferred-does-not-block-starting-validated-in-phase).
- `hina doctor` always tells the truth about what's validated vs. built-but-unvalidated on the current host, so Windows users get an honest feature-availability picture before Phase 12.

## What "done" looks like per track

- **Usable product:** end of Phase 2 (text chat, multi-user, sessions).
- **Local voice MVP:** end of Phase 6 (speak to Hina, barge-in, on Linux/macOS; Windows local voice gated to Phase 12).
- **Sandboxed tools in chat + secret vault:** end of Phase 7 (a model tool call runs in the user's `sbx` sandbox with policy + approval + audit; per-user encrypted secrets).
- **Callable coding agents:** end of Phase 8 (authenticate Codex/Claude/Cursor via the web UI — browser login or API key, credentials kept as encrypted agent-state — and call them as typed, sandboxed `agent.<provider>.run` tools; Pi waits on Phase 11). **Full Automations:** end of Phase 9.
- **Both transport modes:** end of Phase 10.
- **Account-free local LLM:** end of Phase 11 (managed `llama-server` drives local text + voice with no cloud account; Windows validated in Phase 12).
- **Windows GA:** end of Phase 12.

Re-plan each phase when you reach it — the later phase docs are intentionally lighter because earlier phases will teach us things. Re-verify the drift-prone external surfaces (`sbx` CLI, the four agent CLIs, OpenAI Realtime endpoints) immediately before the phase that uses them, per [`research-findings.md`](research-findings.md).

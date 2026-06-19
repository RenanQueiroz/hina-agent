# Hina V2 — Planning docs

Planning and design for the V2 rewrite of the voice agent (**Hina**), a server-first, web-first, multi-user voice/text agent with local + cloud STT-LLM-TTS, Docker `sbx` sandboxing, per-user secrets, callable-agent Automations, and first-class native Windows support. V1 (the Python/Textual app) lives at `/home/renan/voice-agent` and is the reference corpus, not the V2 architecture.

## Start here

| Doc | What it is |
|---|---|
| [`roadmap.md`](roadmap.md) | **The index.** Phase list, dependency graph, and the Windows-deferral strategy. Read this first. |
| [`hina-agent-plan.md`](hina-agent-plan.md) | The canonical **vision / architecture** document (product direction, components, security, event model). |
| [`research-findings.md`](research-findings.md) | **Closed research/spikes + decisions** — chosen libraries/versions with verdicts, the "clarify before first code" answers, and what's deferred. Authoritative where it and the vision doc differ. |

## Implementation phases

Each is a separately testable chunk (details in its file; overview in [`roadmap.md`](roadmap.md)):

1. [Foundation](phase-01-foundation.md) — server skeleton, platform abstraction, persistence, auth v0, events, CI, `hina doctor`
2. [Web shell + text chat](phase-02-web-text-chat.md) — first usable product; shared session-context builder
3. [WebRTC audio loopback](phase-03-webrtc-loopback.md) — transport proven, no models
4. [Local TTS + ORT runtime](phase-04-local-tts.md) · 5. [Local streaming ASR](phase-05-local-asr.md) · 6. [Live voice pipeline](phase-06-live-voice.md)
7. [Sandbox + secrets](phase-07-sandbox-secrets.md) · 8. [Agent auth + callable agents](phase-08-agent-auth-callable.md) · 9. [Automations](phase-09-automations.md)
10. [Full OpenAI Realtime mode](phase-10-openai-realtime.md)
11. [Windows validation & hardening](phase-11-windows-hardening.md)

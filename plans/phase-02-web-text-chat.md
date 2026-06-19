# Phase 2 — Web shell + text chat + sessions

Status: ready after Phase 1.
Depends on: Phase 1 (auth, events, persistence, wire contracts).
Unblocks: Phase 4 (TTS needs a turn to speak), Phase 7 (tools attach to text turns).

## Goal

The first thing a real user can use. A ChatGPT-like web client where a logged-in user creates or resumes a conversation, types a message, and gets a **streamed** assistant reply that is persisted as a canonical text turn. Plus the admin shell skeleton. This establishes the **shared session-context builder** that text and (later) voice both go through — the single most important anti-drift decision in the product.

## Scope

### In
1. **Frontend stack scaffold** (the decided stack): TypeScript + Vite + React; **shadcn/ui on Base UI primitives** + Tailwind + lucide-react; **TanStack Router** (typed routes, separate `user/` and `admin/` route trees), **TanStack Query** (server state), **TanStack Table** (admin grids); **Zustand** for frontend-only UI prefs (theme/density/composer); React Hook Form + Zod + Ajv reserved for the Automation builder later; Vitest + Playwright wiring.
2. **Auth UI**: login, change-password (bootstrap flow), session handling against Phase 1 auth.
3. **Conversation UI**: session list, new session, resume session, shared message timeline (renders text turns now; voice turns later use the *same* timeline), streaming assistant text, copy.
4. **Text-mode backend**: `UserTextSubmitted` → server calls the configured LLM directly, streams `AgentTextDelta` → `AgentTextCompleted`, persists the turn. Cloud OpenAI via **`github.com/openai/openai-go/v3`** (Responses API, streaming) is the first adapter; local `llama-server` via the same OpenAI-compatible client (custom base URL) is an optional second adapter to prove the local path early. Gemini via `google.golang.org/genai` is an optional third.
5. **Shared session-context builder**: one canonical function that turns persisted turns (+ later tool results) into model context. Text mode uses it now; voice mode will reuse it verbatim in Phase 6. This is where the "text and live share one history" guarantee lives.
6. **Admin shell skeleton**: admin route tree, a runtime/backend status page reading `runtime_state`, server + per-backend log views (wired to the Phase 1 log stream), user list. Most admin controls are stubs that fill in over later phases.
7. **LLM provider config (admin-owned)**: admin selects the active text LLM backend/policy; users do **not** choose STT/LLM/TTS.

### Explicitly out (deferred)
- STT/TTS/voice/WebRTC (Phases 3–6).
- Tool execution / sandbox / approvals — emit the `ToolCall*`/`ToolApproval*` event *types* in the timeline plumbing, but actual tool execution is Phase 7. Until then, tools are disabled.
- Automations UI (Phase 9). Sandbox Environment settings (Phase 7).
- Any user-facing model picker.

## Windows posture
Pure web + Go HTTP; nothing OS-specific here. CI keeps building/serving on the Windows runner. No deferred Windows work introduced.

## Work breakdown

1. **Scaffold `web/`** with the stack above; one Vite build, two route trees (`user`, `admin`) sharing an API/event client generated from the Phase 1 wire types. Establish the design-system convention (generated shadcn/Base UI components are owned local components).
2. **API/event client**: typed fetch layer (TanStack Query) + the SSE event-stream subscriber (reconnect with last `seq`, from Phase 1). One `useConversationEvents(id)` hook drives the timeline.
3. **Auth screens** against Phase 1 endpoints; route guards; bootstrap-password-change gate.
4. **Conversation endpoints + UI**: create/list/resume conversations; the timeline component subscribes to events and renders user/assistant turns; streaming assistant text via `AgentTextDelta`.
5. **LLM adapter interface** in Go: `Stream(ctx, context, opts) -> (deltas, done, err)` with cancellation at every boundary (per the main plan's agent-loop principles). Implementations: OpenAI Responses (`openai-go/v3`), local OpenAI-compatible (`llama-server` base URL), Gemini (`go-genai`) optional. Reasoning deltas (local models) go to a UI-only region, never into TTS/canonical text — carry V1's hard rule forward.
6. **Shared context builder**: `BuildModelContext(conversation) -> messages`, from canonical text turns (+ tool results later). Persist canonical text for every turn. Unit-test it directly — it's the contract both modes depend on.
7. **Minimal text agent turn**: submit → build context → stream → persist `TurnCommitted`. No tool loop yet (or a no-op tool stage gated off). This is the seed of the Phase 6 custom agent loop.
8. **Admin shell**: route tree + runtime status + log views + user list (mostly read-only this phase).

## Stack notes (as built)
- **LLM adapters:** `mock` (default, credential-free) + `openai` (official `openai-go/v3` Responses API, cloud) + `openai-compat` (thin `/chat/completions` client for local llama.cpp). Gemini (`go-genai`) remains optional/later.
- **TanStack Table** backs the admin users grid. **Base UI primitives** are introduced when a component needs them (Dialog/Select/Menu); the current owned Tailwind components (`ui.tsx`) already follow the shadcn "owned components" philosophy. **React Hook Form + Zod + Ajv** are deliberately deferred to the Automation builder (Phase 9), per the main plan's "reserved for later."
- **Generated TS** covers the wire DTOs + event envelope today; the full *editable-config* TS type lands with the admin config-editing UI in a later phase. `[paths]` overrides exist in `config.Config` now.
- **CSRF:** cookie flows are protected by a same-origin (Origin/Referer) check on unsafe methods, in addition to `SameSite=Lax` + httpOnly.
- **Event envelope `turn_id`** is populated on the envelope (not just payloads), so the `events.turn_id` column and the wire contract are honored.

## Persistence touchpoints
Populate `conversations`, `turns` (canonical_text, mode=`text`), `events`. No schema changes beyond Phase 1's boundaries.

## Testable exit criteria
- [ ] A user logs in, creates a conversation, types a message, and sees the assistant reply **stream in token-by-token**, then both turns persist and survive reload.
- [ ] Resuming the conversation later shows full history and accepts a follow-up that has the prior context (proves the shared context builder).
- [ ] Switching the admin-selected LLM backend between cloud OpenAI and local `llama-server` works without code changes (config only).
- [ ] Cancelling a streaming turn (navigate away / stop) cancels the upstream LLM call (no leaked request).
- [ ] Admin shell shows live server + backend logs and the user list; non-admins can't reach admin routes.
- [ ] CI green on all three OSes; TS types still match Go (Phase 1 check).

## Risks & mitigations
- **Text/voice context drift** → one `BuildModelContext` used by both, unit-tested now, reused unchanged in Phase 6.
- **UI inventing ad-hoc server-state shapes** → all server data via TanStack Query against the typed client; Zustand only for local UI prefs (never server data) — carry the main plan's rule.
- **Provider env-var footguns** (V1 hit Gemini "multiple credentials" 400s, stray `OPENAI_BASE_URL`) → port V1's defensive client config (explicit base URL, omit org/project headers, `trust_env=false`) into the Go adapters.

## References
- Frontend + provider decisions: [`research-findings.md`](research-findings.md) B9 + the main plan's "Technology stack notes."
- V1 provider pitfalls worth porting: `/home/renan/voice-agent/AGENTS.md` ("Providers", "Gemini integration").

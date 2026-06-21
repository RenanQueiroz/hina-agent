# Phase 8 — Agent auth broker + callable agent adapters (Codex / Claude / Cursor / Pi)

Status: **implemented** (Linux/macOS scope; Pi gated on Phase 11, Windows browser-auth on Phase 12).
Depends on: Phase 7 (sandboxes + secret/agent-state storage); Phase 11 for the Pi adapter (the managed host llama.cpp endpoint it targets).
Unblocks: Phase 9 (Automations spawn callable agents).

## Implementation notes (what landed)

The full vertical slice is implemented and tested the way Phase 7 was — without the
external CLIs present (they live inside the `sbx` container image, like `sbx` itself):

- **`internal/agentcli`** — pure, versioned adapters (codex/claude/cursor/pi): the
  B7-verified `exec`/`login`/`status` argv + tolerant output parsers → a normalized
  `AgentRunResult`. Fixture-tested; **re-verify each CLI's flags on a host that has it
  before trusting in production** (the parsers + `VersionArgs` are the drift guard).
- **`internal/agentauth`** — the auth broker: interactive login in a short-lived `sbx`
  auth container (`sbx run -it`, host attached by pipes; **no host PTY needed** — the
  container TTY + the mandatory device/paste-code fallback cover it), sanitized output
  streaming with URL/device-code/paste-prompt detection, status-command confirmation,
  API-key/token profiles, logout. Pure detection/sanitization + the session state
  machine are fully tested with a fake session factory.
- **`internal/vault` + `internal/store`** — per-provider encrypted **agent-state** (a
  tar of the credential store, or the key) under the secret envelope, plus a
  metadata-only `agent_profiles` row (auth-profile type, never a credential).
- **`internal/sandbox` `AgentRouter`** — built from the tool Router (shared lock /
  approval / redaction / quota / audit); resolves the profile, mounts the decrypted
  store or injects the key, runs the agent inside `sbx`, re-encrypts a refreshed store,
  parses + redacts the result. **Fails closed unless `[sandbox] network_isolated`** (an
  agent run carries credentials and needs egress Hina can't gate per-container yet).
  A **tar-slip-hardened** archiver moves agent-state in/out.
- **`internal/httpapi` + `web/`** — `/agents` catalog + eligibility, key/login broker
  endpoints (start / SSE / paste-input / cancel) + logout, admin coarse status; a
  **Coding agents** card on the Sandbox page (streamed login dialog + API-key form).
- **`[agents]` config**, `hina doctor` callable-agents check, and the agent loop routing
  `agent.*` tool calls to the AgentRouter.

**Deferred:** the **MCP facade** (scope item 5 — optional); the **Pi** adapter is built
but gated unavailable until Phase 11 wires the managed llama.cpp host-inference proxy;
real-CLI/container validation is for an `sbx`-equipped host; Windows browser-auth →
Phase 12.

## Goal

Let users authenticate account-backed coding-agent CLIs through the web UI and call them as **typed, sandboxed tools**. The Go server owns process invocation, environment, timeouts, output schemas, and artifact capture — the model requests `agent.codex.exec` / `agent.pi.run` with structured args; it never builds raw CLI strings. Pi is the local/account-free path (host llama.cpp, no cloud).

Verified CLI flags + the **5 corrections** are in [`research-findings.md` B7](research-findings.md#b7-callable-agent-clis--confirmed-with-5-corrections). **Re-verify each CLI's flags immediately before implementing its adapter** — they drift.

## Scope

### In
1. **Agent auth broker** (per-user, Sandbox Environment page): for Codex/Claude/Cursor, choose browser/subscription auth or API-key/token auth.
   - Browser auth runs the provider login in a **PTY** inside a short-lived auth container (user's persistent agent state mounted, network on, only the selected CLI present). Server detects login URLs / device codes / paste-code prompts; frontend streams a sanitized view, highlights URLs/codes, opens URLs in a new tab, and writes pasted codes back to the PTY. On success, run the provider status command and record the profile. Device-code / paste-code are **mandatory fallbacks** (localhost callbacks inside a sandbox may not reach the user's browser).
   - Per-provider specifics (from B7): Codex `codex login` / `--device-auth` / `login status`, per-user `CODEX_HOME`, prefer `cli_auth_credentials_store="file"`; **no `CODEX_API_KEY`** (use `OPENAI_API_KEY` or `codex login --with-api-key`). Claude `claude auth login`/`status`, per-user `CLAUDE_CONFIG_DIR`, **keep `ANTHROPIC_API_KEY` unset in the browser profile** (it overrides subscription in `-p`); API-key profile supports `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`/`CLAUDE_CODE_OAUTH_TOKEN`. Cursor `agent login`/`status`/`logout` (`NO_OPEN_BROWSER=1`), `CURSOR_API_KEY`.
2. **Encrypted per-user agent state**: provider credential stores (`CODEX_HOME`, `CLAUDE_CONFIG_DIR`, Cursor state) treated as secret material in the same vault boundary as Phase 7; mounted RW only into that user's auth/run containers; updated atomically after runs (tokens may refresh); admin UI shows only coarse status ("Codex authenticated") — never tokens/URLs/codes.
3. **Typed adapters** (Codex/Claude/Cursor/Pi), each a versioned capability with a health check (`--version`/status), structured-output parsing, streaming, cancellation, timeout, artifact capture, normalized **`AgentRunResult`** (status, final text, structured output, changed files, tool calls if parseable, token/cost if available, raw stdout/stderr paths, duration). All run **inside the Phase 7 `sbx` sandbox** in headless/autonomous mode (CLI "yolo"/bypass only removes the CLI's own prompts inside the already-isolated container).
   - Codex: `codex exec --json [--output-schema] --cd <ws> --skip-git-repo-check`; autonomy `--sandbox workspace-write --ask-for-approval never` or `--yolo` inside `sbx`. **Not `--full-auto`** (deprecated).
   - Claude: `claude -p --output-format stream-json --verbose --include-partial-messages` (progress) or `--output-format json --json-schema <s>` (structured); `--max-turns` from budget; `--dangerously-skip-permissions` only inside `sbx`; `--safe-mode` for subscription runs; **never `--bare` for subscription** (it ignores OAuth).
   - Cursor: `agent -p --output-format json|stream-json [--stream-partial-output]`, `--force`/`--yolo`; `CURSOR_API_KEY` via per-user injection. Verify headless cancellation empirically.
   - **Pi (local-only)**: generate per-run `models.json` pointing at the **Phase 11 managed backend through the Hina-owned host-inference proxy** (`http://host.docker.internal:<proxy-port>/v1`, the path-filtered `/v1` gateway — never the raw `llama-server` port), `api="openai-completions"`, dummy key; prefer `pi --mode rpc` (stream/steer/abort); disable extensions/skills/context (`--no-extensions/--no-skills/--no-context-files`); `PI_OFFLINE=1`; **never a cloud provider**; reach llama.cpp only through that Phase 7 host-inference proxy/allow-list.
4. **Eligibility validation**: chat/Automation UIs offer Codex/Claude/Cursor only with a valid configured profile; offer Pi only when admin policy allows `agent.pi.run`, the local LLM exposes a llama.cpp endpoint, and sandbox policy allows host inference. Runs record the auth-profile *type* (`browser_state`/`api_key`/`oauth_token`/`local_llamacpp`) without storing credential values.
5. **Optional MCP facade**: expose the adapters as MCP tools so MCP-capable LLMs can call them while Go still owns invocation. (Codex itself can also run as `codex mcp-server`; keep the direct `codex exec` adapter primary — easier to budget/stream/cancel.)

### Explicitly out (deferred)
- Automation scheduler/steps that *orchestrate* these agents (Phase 9) — Phase 8 makes one agent callable as a tool; Phase 9 wires them into workflows.
- Windows browser-auth-from-sandbox validation (device/paste-code from a Windows browser) → Phase 12.

## Windows posture
Adapters are cross-platform process invocations inside `sbx`. The Windows-specific concern — browser/device/paste-code auth working from a Windows browser while the CLI runs inside `sbx` — is validated in Phase 12.

## Testable exit criteria (Linux/macOS this phase)
- [ ] A user browser-authenticates Codex (or Claude/Cursor) through the web UI via PTY streaming with URL/code capture; status check confirms; logout deletes stored state.
- [ ] The credential store is encrypted at rest, mounted only into that user's containers, and never visible in admin UI/logs.
- [ ] Each adapter runs headlessly inside `sbx`, returns a normalized `AgentRunResult` with captured artifacts, respects timeout, and cancels cleanly.
- [ ] Pi runs against host llama.cpp with no account/cloud key, reachable only via the host-inference allow-list.
- [ ] The UI prevents selecting an unavailable/unauthorized agent; runs record auth-profile type without credential values.

## Risks & mitigations
- **CLI drift** (flags/auth/output/autonomy change) → versioned adapters + health checks; re-verify before implementing each (B7).
- **Container browser-auth failures** (localhost callback unavailable) → mandatory device-code/paste-code + PTY input.
- **Browser-auth creds are powerful bearer tokens** → vault boundary, logout/revocation, never copied into workspaces/artifacts.
- **Local model quality for Pi** → benchmark Pi separately; surface quality limits in the UI.

## References
- Verified flags + corrections: [`research-findings.md`](research-findings.md) B7.
- Auth broker + agent-state design: `hina-agent-plan.md` (User Sandbox Environment, Agent auth broker, Provider setup commands, Callable agent support).

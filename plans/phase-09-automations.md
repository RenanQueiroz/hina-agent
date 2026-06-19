# Phase 9 — Automations

Status: ready after Phases 7 + 8.
Depends on: Phase 7 (sandboxes + secrets), Phase 8 (callable agents), Phase 2 (LLM + builder UI stack).
Unblocks: the product's headline differentiator (scheduled unattended workflows).

## Goal

User-owned scheduled workflows that run while the server is up, wake the model/tool stack, execute in the user's sandbox, and produce artifacts/side effects. Ship the `automation.v1` schema, a **durable scheduler** (server-up-only), deterministic pre-model steps, agent steps, run records, and an **on-rails builder UI** (not a raw JSON editor) plus LLM-assisted creation. The proof is the typed **GitHub PR-review Automation** from the main plan.

Automation semantics still to nail down before promising portable imports are in [`research-findings.md` C4](research-findings.md#part-c--deferred-does-not-block-starting-validated-in-phase) — settle them at the start of this phase.

## Scope

### In
1. **`automation.v1` JSON Schema** (the shape in the main plan): trigger (interval/cron/manual), timezone, `missed_run_policy` default `skip` (opt-in `run_once`), concurrency, budget, sandbox profile (granular/unrestricted), steps (tool/condition/for_each/parallel/llm/agent_cli/finish), outputs. Generated DB fields (id, owner, timestamps, last/next run) are **not** required in import/export JSON. Validate with backend JSON Schema; frontend Ajv against the same schema.
2. **Semantics (settle first, per C4)**: selector/template syntax (`${item}`, `step.field` references, JSONPath-like), retry/error policy, idempotency expectations, artifact-promotion rules, side-effect confirmation, schema evolution/versioning.
3. **Durable scheduler**: persists definitions (`automations`); on restart resumes enabled Automations and computes next run; missed runs while down default to `skip`. Concurrency policies (skip-if-running / queue-one / parallel-N / cancel-previous). Budgets (wall time, model calls, agent runs, artifact/log bytes). Stops cleanly on server shutdown (schedules, runs, sandboxes, agents, workers — nothing lingers).
4. **Run records** (`automation_runs`, `automation_artifacts`): immutable per run — input snapshot, step logs, model calls, tool calls, spawned agent runs, artifacts, final output, timings, status, errors. Run inside per-user/per-automation `sbx` sandboxes (Phase 7) inheriting the Automation's permission profile.
5. **Deterministic tools** (run before waking LLMs): `github.notifications`, `github.pr_checkout`, `github.pr_comment`, `http.request` (bounded), `shell.exec` (argv-first; shell-string gated by policy), `mcp.call`. Argv-style over shell strings for safe validation.
6. **Agent steps**: `agent.codex.exec` / `agent.claude.run` / `agent.cursor.run` / `agent.pi.run` via the Phase 8 adapters; `parallel` agent groups; aggregation/verification `llm` step. Host services allow-list (initially only `llamacpp` for Pi).
7. **Builder UI** (React Hook Form + Zod + Ajv; CodeMirror only for generated-JSON preview / import repair / advanced): structured forms for trigger, schedule, permission profile, secrets, tools, workflow steps, agent steps, aggregation, outputs → emits `automation.v1`. Import/export with repairable validation errors. Run history with logs/artifacts/agent reports/final output.
8. **LLM-assisted creation**: explain the schema to the active server LLM → ask for JSON only → validate → feed errors back and retry to a limit → show the validated Automation for review before enabling. Always require human review before enable (schema-valid ≠ safe).
9. **Eligibility/visibility**: surface sandbox permissions, granted secrets, network access, enabled outputs before a user enables an Automation; validation fails on unavailable agent auth profiles, missing secret refs, or disallowed agents.

### Explicitly out (deferred)
- Webhook/event triggers (later) — start with interval/cron/manual.
- Catch-up/backfill of missed runs (deferred until a real workflow needs it — surprising side effects).
- Visual DAG canvas (React Flow) — start with structured forms.
- Windows Automation validation (`sbx`-on-Windows, agent auth from Windows browser) → Phase 11.

## Windows posture
Scheduler + schema + runner are cross-platform; the sandbox/agent execution underneath is Windows-validated in Phase 11. Automations marked supported on Windows only after that.

## Testable exit criteria (Linux/macOS this phase)
- [ ] Create the GitHub PR-review Automation in the builder; it emits valid `automation.v1`; export/import round-trips with redacted secrets (only `secret_refs`).
- [ ] Interval trigger fires while the server is up; a deterministic `gh`/notifications check that finds nothing **finishes without waking any LLM**.
- [ ] When a PR matches: per-PR sandbox checkout → parallel agent reviews (per the user's configured/available agents) → aggregation `llm` step → final combined review posted/drafted; each agent report saved as an artifact; immutable run record captures everything.
- [ ] Server restart resumes enabled schedules and recomputes next run; a run in flight at shutdown stops cleanly with nothing lingering.
- [ ] LLM-assisted creation produces schema-valid JSON (with retry on validation errors) and requires review before enabling.
- [ ] Budgets/concurrency/missed-run `skip` all enforced; an Automation referencing an unavailable agent/secret fails validation.

## Risks & mitigations
- **External side effects from powerful Automations** → visible permissions/secrets/network/outputs before enable; argv-first tools; human review of LLM-generated JSON.
- **Portable-import promises before semantics are fixed** → settle C4 selector/retry/idempotency/promotion/evolution before advertising import portability.
- **Unattended secret use** → documented threat model (C5); server must decrypt to run — hidden from DB/admin-UI, not from malicious host/root.

## References
- `automation.v1` shape + workflow model + example: `hina-agent-plan.md` (Automations).
- Open semantics + threat model: [`research-findings.md`](research-findings.md) C4, C5.

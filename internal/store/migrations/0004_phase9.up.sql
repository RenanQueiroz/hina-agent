-- 0004_phase9.sql — Phase 9: Automations. The automations / automation_runs /
-- automation_artifacts tables were created empty in 0001 as placeholders; this
-- migration extends them with the columns the durable scheduler and the immutable
-- run records need. Forward-only; never edit an applied migration.
--
-- The full automation.v1 document lives in automations.definition (JSON); the full
-- per-run record (step logs, accounting, final output) lives in automation_runs.record
-- (JSON). The added scalar columns are the queryable projection the scheduler and
-- the list/history views use (name, next/last run, owner, trigger, status, error).

ALTER TABLE automations ADD COLUMN name TEXT NOT NULL DEFAULT '';
ALTER TABLE automations ADD COLUMN next_run_at TEXT;
ALTER TABLE automations ADD COLUMN last_run_at TEXT;
-- pending_fire durably records that a queue_one/cancel_previous replacement was queued
-- after the scheduler already advanced next_run_at. It holds a per-queue TOKEN (''=none),
-- so a drain consumes ONLY the exact occurrence it claimed (compare-and-clear) and can't
-- erase a newer occurrence queued while it ran. Without it, a crash/restart between
-- queueing and running would silently lose that claimed fire; reconcile drains it.
ALTER TABLE automations ADD COLUMN pending_fire TEXT NOT NULL DEFAULT '';
-- trigger is a scalar projection of definition.trigger.type so the LIST view can render a
-- summary WITHOUT scanning the (potentially large) definition JSON for every row.
ALTER TABLE automations ADD COLUMN trigger TEXT NOT NULL DEFAULT '';
-- gen is a monotonic generation counter bumped on EVERY user-visible transition (create /
-- update / enable / disable / delete). It is the concurrency generation the scheduler uses
-- to detect that an automation changed between a fire being CLAIMED and RUN — unlike
-- updated_at (a wall-clock string that two same-instant edits can collide on), an integer
-- that only ever increments is a reliable compare-and-set token. The scheduler's own
-- bookkeeping (next_run/last_run/pending_fire) must NOT bump it. DEFAULT 1 (a POSITIVE
-- generation) so every row — including any that pre-dates this migration — has a real
-- generation the scheduler can compare exactly; there is no "generation 0" that would
-- bypass the stale-fire guard.
ALTER TABLE automations ADD COLUMN gen INTEGER NOT NULL DEFAULT 1;
-- Defensive backfill: any row somehow left at 0 is lifted to a positive generation.
UPDATE automations SET gen=1 WHERE gen<1;
-- deleted soft-deletes an automation: a delete marks this 1 (and disables it) instead of
-- hard-deleting, so its immutable run/artifact records (the only durable audit of the
-- automation's side effects) survive — the row just disappears from the owner's views.
ALTER TABLE automations ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0;

ALTER TABLE automation_runs ADD COLUMN owner_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE automation_runs ADD COLUMN trigger TEXT NOT NULL DEFAULT '';
ALTER TABLE automation_runs ADD COLUMN error TEXT NOT NULL DEFAULT '';

ALTER TABLE automation_artifacts ADD COLUMN step_id TEXT NOT NULL DEFAULT '';
ALTER TABLE automation_artifacts ADD COLUMN size INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_automations_owner ON automations(owner_user_id);
CREATE INDEX idx_automations_enabled ON automations(enabled);
CREATE INDEX idx_automation_runs_automation ON automation_runs(automation_id, started_at);
CREATE INDEX idx_automation_runs_owner ON automation_runs(owner_user_id);
CREATE INDEX idx_automation_artifacts_run ON automation_artifacts(run_id);

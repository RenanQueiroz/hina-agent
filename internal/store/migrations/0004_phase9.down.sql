-- Revert 0004_phase9: drop the added indexes, then the added columns, returning the
-- automations / automation_runs / automation_artifacts tables to their 0001
-- placeholder shape.

DROP INDEX IF EXISTS idx_automation_artifacts_run;
DROP INDEX IF EXISTS idx_automation_runs_owner;
DROP INDEX IF EXISTS idx_automation_runs_automation;
DROP INDEX IF EXISTS idx_automations_enabled;
DROP INDEX IF EXISTS idx_automations_owner;

ALTER TABLE automation_artifacts DROP COLUMN size;
ALTER TABLE automation_artifacts DROP COLUMN step_id;

ALTER TABLE automation_runs DROP COLUMN error;
ALTER TABLE automation_runs DROP COLUMN trigger;
ALTER TABLE automation_runs DROP COLUMN owner_user_id;

ALTER TABLE automations DROP COLUMN deleted;
ALTER TABLE automations DROP COLUMN gen;
ALTER TABLE automations DROP COLUMN trigger;
ALTER TABLE automations DROP COLUMN pending_fire;
ALTER TABLE automations DROP COLUMN last_run_at;
ALTER TABLE automations DROP COLUMN next_run_at;
ALTER TABLE automations DROP COLUMN name;

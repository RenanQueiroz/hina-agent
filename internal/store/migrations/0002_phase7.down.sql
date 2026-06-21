-- Revert 0002_phase7.

DROP INDEX idx_sandbox_runs_created;
DROP INDEX idx_sandbox_runs_user;
DROP TABLE sandbox_runs;
DROP INDEX idx_sandbox_state_user_kind;
DROP INDEX idx_secrets_meta_user_name;

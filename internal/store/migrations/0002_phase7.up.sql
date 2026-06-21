-- 0002_phase7.sql — Phase 7: Docker `sbx` runner + per-user secret vault +
-- Sandbox Environment. Fleshes out the v0 placeholder tables (secrets_meta,
-- sandbox_state) with the constraints the runtime relies on, and adds the
-- sandbox-run audit log. Forward-only; never edit an applied migration.

-- A user's secret names are unique: the name doubles as the env-var key the
-- vault injects, so two secrets named the same would be ambiguous at injection.
CREATE UNIQUE INDEX idx_secrets_meta_user_name ON secrets_meta(user_id, name);

-- Each user has at most one Sandbox Environment row per kind (kind='environment'
-- holds the per-user policy: allowed tools, MCP servers, network policy, mounts,
-- secret grants). The runtime upserts on (user_id, kind).
CREATE UNIQUE INDEX idx_sandbox_state_user_kind ON sandbox_state(user_id, kind);

-- Audit log of every sandbox tool invocation (admin visibility + forensics).
-- Holds NO secret values and no raw command output — only metadata plus paths to
-- the captured (already secret-redacted) output files on disk. `command` is a
-- redacted summary of the argv, never the raw secret-bearing command line.
CREATE TABLE sandbox_runs (
  id              TEXT PRIMARY KEY,
  user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  conversation_id TEXT,
  tool            TEXT NOT NULL,
  sandbox_id      TEXT NOT NULL DEFAULT '',
  command         TEXT NOT NULL DEFAULT '',
  decision        TEXT NOT NULL DEFAULT '',
  exit_code       INTEGER NOT NULL DEFAULT 0,
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  error           TEXT NOT NULL DEFAULT '',
  stdout_path     TEXT NOT NULL DEFAULT '',
  stderr_path     TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL
);
CREATE INDEX idx_sandbox_runs_user ON sandbox_runs(user_id);
CREATE INDEX idx_sandbox_runs_created ON sandbox_runs(created_at);

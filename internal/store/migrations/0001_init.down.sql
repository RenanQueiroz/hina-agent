-- 0001_init.down.sql — reverse of 0001_init.up.sql.
-- Drops every table the up migration created, child-before-parent so foreign
-- keys never block a drop. schema_migrations is owned by the migration runner,
-- not by this migration, so it is intentionally left intact.
DROP TABLE IF EXISTS agent_auth_state;
DROP TABLE IF EXISTS secrets_meta;
DROP TABLE IF EXISTS sandbox_state;
DROP TABLE IF EXISTS automation_artifacts;
DROP TABLE IF EXISTS automation_runs;
DROP TABLE IF EXISTS automations;
DROP TABLE IF EXISTS runtime_state;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS turns;
DROP TABLE IF EXISTS conversations;
DROP TABLE IF EXISTS auth_sessions;
DROP TABLE IF EXISTS users;

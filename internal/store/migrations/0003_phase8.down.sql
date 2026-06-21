-- Down: drop the Phase 8 agent-profile metadata.
DROP INDEX IF EXISTS idx_agent_profiles_user_provider;
DROP TABLE IF EXISTS agent_profiles;

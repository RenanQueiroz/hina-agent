-- 0003_phase8.sql — Phase 8: agent auth broker + callable agent adapters.
-- Records, per user, which callable coding-agent CLIs (codex/claude/cursor/pi) are
-- configured and HOW they authenticate — metadata only. The credential material
-- itself (a browser/subscription credential store, or an API key/OAuth token) is
-- envelope-encrypted agent-state on disk (internal/vault), NEVER in this database.
-- Forward-only; never edit an applied migration.

CREATE TABLE agent_profiles (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider    TEXT NOT NULL,             -- codex | claude | cursor | pi
  auth_type   TEXT NOT NULL,             -- browser_state | api_key | oauth_token | local_llamacpp
  status      TEXT NOT NULL DEFAULT '',  -- authenticated | pending | error
  -- Coarse, non-sensitive label for the admin/user UI (e.g. "configured"); MUST
  -- NEVER hold a token, URL, device code, or any credential value.
  label       TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

-- A user has at most one profile per provider (the upsert key).
CREATE UNIQUE INDEX idx_agent_profiles_user_provider ON agent_profiles(user_id, provider);

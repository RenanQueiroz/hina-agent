-- 0001_init.sql — Hina v0 schema.
-- Table boundaries are drawn up front (even where empty) so early routes don't
-- invent ad-hoc shapes that later fight the event model. Columns are fleshed
-- out per phase. Timestamps are RFC3339 UTC text.

CREATE TABLE users (
  id                   TEXT PRIMARY KEY,
  username             TEXT NOT NULL UNIQUE,
  role                 TEXT NOT NULL CHECK (role IN ('admin','user')),
  password_hash        TEXT NOT NULL,
  status               TEXT NOT NULL DEFAULT 'active',
  must_change_password INTEGER NOT NULL DEFAULT 0,
  created_at           TEXT NOT NULL,
  updated_at           TEXT NOT NULL
);

-- Browser/login sessions — distinct from conversation sessions.
CREATE TABLE auth_sessions (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE INDEX idx_auth_sessions_user ON auth_sessions(user_id);

-- The durable "Session" in the product sense: a conversation history.
CREATE TABLE conversations (
  id            TEXT PRIMARY KEY,
  owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title         TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE INDEX idx_conversations_owner ON conversations(owner_user_id);

CREATE TABLE turns (
  id              TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  role            TEXT NOT NULL CHECK (role IN ('user','assistant','system','tool')),
  mode            TEXT NOT NULL DEFAULT 'text' CHECK (mode IN ('text','voice')),
  canonical_text  TEXT NOT NULL DEFAULT '',
  metadata        TEXT NOT NULL DEFAULT '{}',
  created_at      TEXT NOT NULL
);
CREATE INDEX idx_turns_conversation ON turns(conversation_id);

-- Append-only event log: source of truth behind replay/reconnect.
CREATE TABLE events (
  event_id        TEXT PRIMARY KEY,
  conversation_id TEXT REFERENCES conversations(id) ON DELETE CASCADE,
  user_id         TEXT REFERENCES users(id) ON DELETE SET NULL,
  turn_id         TEXT,
  seq             INTEGER NOT NULL,
  source          TEXT NOT NULL,
  type            TEXT NOT NULL,
  payload         TEXT NOT NULL DEFAULT '{}',
  server_ts       TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_events_conv_seq ON events(conversation_id, seq);

CREATE TABLE runtime_state (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- Placeholders populated in later phases.
CREATE TABLE automations (
  id            TEXT PRIMARY KEY,
  owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  definition    TEXT NOT NULL,
  enabled       INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE automation_runs (
  id            TEXT PRIMARY KEY,
  automation_id TEXT NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
  status        TEXT NOT NULL,
  started_at    TEXT,
  finished_at   TEXT,
  record        TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE automation_artifacts (
  id         TEXT PRIMARY KEY,
  run_id     TEXT NOT NULL REFERENCES automation_runs(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  path       TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE sandbox_state (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL,
  data       TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL
);

CREATE TABLE secrets_meta (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE agent_auth_state (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider     TEXT NOT NULL,
  profile_type TEXT NOT NULL,
  status       TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

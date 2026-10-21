CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  name          TEXT NOT NULL DEFAULT '',
  username      TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL DEFAULT '',
  role          TEXT NOT NULL DEFAULT 'admin',
  feed_token    TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at    TEXT
);
CREATE UNIQUE INDEX users_username ON users(username) WHERE username <> '';

CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL DEFAULT '',
  token_hash   TEXT NOT NULL UNIQUE,
  prefix       TEXT NOT NULL DEFAULT '',
  last_used_at TEXT,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

-- Box-wide settings (title, setup_complete). Everything a person chooses
-- is in user_settings under their id.
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT ''
);
CREATE TABLE user_settings (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key     TEXT NOT NULL,
  value   TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, key)
);

-- A monitored machine. kind = local (this box, read directly), agent
-- (pocket-monitoring agent elsewhere) or glances (a Glances 4 web server).
-- secret = the sealed agent token or Glances password.
CREATE TABLE servers (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL,
  url         TEXT NOT NULL DEFAULT '',
  username    TEXT NOT NULL DEFAULT '',
  secret      TEXT NOT NULL DEFAULT '',
  color       TEXT NOT NULL DEFAULT '',
  enabled     INTEGER NOT NULL DEFAULT 1,
  interval_s  INTEGER NOT NULL DEFAULT 10,
  sort_order  REAL NOT NULL DEFAULT 0,
  last_seen   TEXT NOT NULL DEFAULT '',
  last_error  TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX servers_user ON servers(user_id);

-- Read-only access for someone else (or everyone on the box).
CREATE TABLE server_shares (
  id         INTEGER PRIMARY KEY,
  server_id  INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  user_id    INTEGER REFERENCES users(id) ON DELETE CASCADE,
  everyone   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE UNIQUE INDEX server_shares_user ON server_shares(server_id, user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX server_shares_everyone ON server_shares(server_id) WHERE everyone = 1;
-- A viewer who removed a server shared with everyone.
CREATE TABLE server_hidden (
  server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (server_id, user_id)
);

-- One minute of a server, averaged (cpu…temp) from the readings in it.
-- extra = JSON {"temps": {sensor key: °C}}.
CREATE TABLE samples (
  server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  ts        INTEGER NOT NULL,           -- unix seconds, start of the minute
  cpu REAL, iowait REAL, mem REAL, swap REAL, load1 REAL, load5 REAL, load15 REAL,
  rx REAL, tx REAL, rd REAL, wr REAL, temp REAL,
  extra TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (server_id, ts)
) WITHOUT ROWID;

-- Fifteen minutes, rolled up from samples; kept much longer.
CREATE TABLE samples_15m (
  server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  ts        INTEGER NOT NULL,
  cpu REAL, iowait REAL, mem REAL, swap REAL, load1 REAL, load5 REAL, load15 REAL,
  rx REAL, tx REAL, rd REAL, wr REAL, temp REAL,
  extra TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (server_id, ts)
) WITHOUT ROWID;

-- Sensor names seen per server (the history legend).
CREATE TABLE sensors (
  server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  key       TEXT NOT NULL,
  label     TEXT NOT NULL,
  PRIMARY KEY (server_id, key)
) WITHOUT ROWID;

CREATE TABLE rules (
  id         INTEGER PRIMARY KEY,
  server_id  INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  metric     TEXT NOT NULL,
  target     TEXT NOT NULL DEFAULT '',
  threshold  REAL NOT NULL DEFAULT 0,
  for_s      INTEGER NOT NULL DEFAULT 0,
  severity   TEXT NOT NULL DEFAULT 'warning',
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX rules_server ON rules(server_id);

-- An alert opens when a rule's condition has held for for_s and resolves
-- when it stops; instance = the mount / sensor / container it fired for.
CREATE TABLE alerts (
  id          INTEGER PRIMARY KEY,
  server_id   INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  rule_id     INTEGER REFERENCES rules(id) ON DELETE SET NULL,
  metric      TEXT NOT NULL,
  target      TEXT NOT NULL DEFAULT '',
  severity    TEXT NOT NULL DEFAULT 'warning',
  message     TEXT NOT NULL DEFAULT '',
  value       REAL NOT NULL DEFAULT 0,
  threshold   REAL NOT NULL DEFAULT 0,
  started_at  TEXT NOT NULL,
  resolved_at TEXT
);
CREATE INDEX alerts_server ON alerts(server_id, started_at);
CREATE INDEX alerts_open ON alerts(resolved_at) WHERE resolved_at IS NULL;

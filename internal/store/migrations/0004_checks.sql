-- Uptime checks: HTTP(S) with an optional keyword, TCP ports, DNS lookups and
-- push heartbeats that scripts send. They run from the machine the hub runs on
-- and belong to its "local" server, so sharing and alert rules come with it.
-- Status codes follow Uptime Kuma: 0 down, 1 up, 2 pending (failing, still
-- retrying), 3 maintenance.
CREATE TABLE checks (
  id               INTEGER PRIMARY KEY,
  server_id        INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  name             TEXT NOT NULL,
  kind             TEXT NOT NULL,                 -- http | tcp | dns | push
  target           TEXT NOT NULL DEFAULT '',      -- URL, host name or DNS name
  port             INTEGER NOT NULL DEFAULT 0,
  method           TEXT NOT NULL DEFAULT 'GET',
  keyword          TEXT NOT NULL DEFAULT '',      -- http: the page must contain it
  invert_keyword   INTEGER NOT NULL DEFAULT 0,    -- … or must not
  status_codes     TEXT NOT NULL DEFAULT '200-299',
  ignore_tls       INTEGER NOT NULL DEFAULT 0,
  follow_redirects INTEGER NOT NULL DEFAULT 1,
  dns_server       TEXT NOT NULL DEFAULT '',      -- '' = the system resolver
  dns_type         TEXT NOT NULL DEFAULT 'A',
  push_token       TEXT NOT NULL DEFAULT '',
  interval_s       INTEGER NOT NULL DEFAULT 60,
  retry_s          INTEGER NOT NULL DEFAULT 60,   -- how soon to try again while failing
  timeout_s        INTEGER NOT NULL DEFAULT 30,
  retries          INTEGER NOT NULL DEFAULT 1,    -- failed tries before it counts as down
  enabled          INTEGER NOT NULL DEFAULT 1,
  notify           INTEGER NOT NULL DEFAULT 1,
  public           INTEGER NOT NULL DEFAULT 0,    -- shown on the status page
  group_name       TEXT NOT NULL DEFAULT '',
  notes            TEXT NOT NULL DEFAULT '',
  source_ref       TEXT NOT NULL DEFAULT '',      -- kuma:<id> for imported checks
  created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  -- where it stands (kept here so a restart carries on)
  status           INTEGER NOT NULL DEFAULT 2,
  fails            INTEGER NOT NULL DEFAULT 0,
  last_at          INTEGER NOT NULL DEFAULT 0,    -- unix s of the newest result
  last_ms          REAL,
  last_msg         TEXT NOT NULL DEFAULT '',
  changed_at       INTEGER NOT NULL DEFAULT 0,    -- when status last flipped up/down
  cert_expires     INTEGER NOT NULL DEFAULT 0     -- unix s, https only
);
CREATE INDEX checks_server ON checks(server_id);
CREATE UNIQUE INDEX checks_push ON checks(push_token) WHERE push_token <> '';
CREATE UNIQUE INDEX checks_ref ON checks(server_id, source_ref) WHERE source_ref <> '';

-- Every result for a week (response time charts, the beat bar).
CREATE TABLE check_beats (
  check_id INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
  ts       INTEGER NOT NULL,
  status   INTEGER NOT NULL,
  ms       REAL,
  msg      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (check_id, ts)
) WITHOUT ROWID;

-- Per local day, kept for a long time: uptime over months.
CREATE TABLE check_days (
  check_id INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
  day      TEXT NOT NULL,
  up       INTEGER NOT NULL DEFAULT 0,
  down     INTEGER NOT NULL DEFAULT 0,
  ms_sum   REAL NOT NULL DEFAULT 0,
  ms_n     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (check_id, day)
) WITHOUT ROWID;

-- Every time a check went down or came back.
CREATE TABLE check_events (
  check_id INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
  ts       INTEGER NOT NULL,
  status   INTEGER NOT NULL,
  msg      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (check_id, ts)
) WITHOUT ROWID;

-- A public, read-only page of the checks marked public (Kuma's JSON shape).
CREATE TABLE status_pages (
  server_id   INTEGER PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
  slug        TEXT NOT NULL UNIQUE,
  title       TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  enabled     INTEGER NOT NULL DEFAULT 1
);

INSERT INTO rules (server_id, metric, target, threshold, for_s, severity, enabled)
  SELECT id, 'check', '', 0, 0, 'critical', 1 FROM servers WHERE kind = 'local';
INSERT INTO rules (server_id, metric, target, threshold, for_s, severity, enabled)
  SELECT id, 'cert', '', 14, 0, 'warning', 1 FROM servers WHERE kind = 'local';

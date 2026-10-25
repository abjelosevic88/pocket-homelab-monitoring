-- Scheduled jobs (systemd timers) read through the agent or the jobs helper.
-- What a person chose for a job: a group, a friendlier name, muted alerts.
CREATE TABLE job_settings (
  server_id  INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  unit       TEXT NOT NULL,
  group_name TEXT NOT NULL DEFAULT '',
  label      TEXT NOT NULL DEFAULT '',
  notes      TEXT NOT NULL DEFAULT '',
  muted      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, unit)
) WITHOUT ROWID;

-- Runs worth keeping one by one: every failure and every run that wrote
-- something. Quiet successes (a watchdog that found nothing) only count in
-- job_days, or a job running every 10 s would fill the table.
CREATE TABLE job_runs (
  server_id   INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  unit        TEXT NOT NULL,
  invocation  TEXT NOT NULL,
  started_ms  INTEGER NOT NULL DEFAULT 0,
  ended_ms    INTEGER NOT NULL,
  ok          INTEGER NOT NULL DEFAULT 1,
  result      TEXT NOT NULL DEFAULT '',
  exit_status INTEGER NOT NULL DEFAULT 0,
  summary     TEXT NOT NULL DEFAULT '',
  lines       INTEGER NOT NULL DEFAULT 0,
  warnings    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, invocation)
) WITHOUT ROWID;
CREATE INDEX job_runs_unit ON job_runs(server_id, unit, ended_ms);
CREATE INDEX job_runs_ended ON job_runs(server_id, ended_ms);

-- Every run, counted per job and local day.
CREATE TABLE job_days (
  server_id  INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  unit       TEXT NOT NULL,
  day        TEXT NOT NULL,
  runs       INTEGER NOT NULL DEFAULT 0,
  failed     INTEGER NOT NULL DEFAULT 0,
  busy       INTEGER NOT NULL DEFAULT 0, -- runs that wrote something
  dur_ms     INTEGER NOT NULL DEFAULT 0, -- total of the runs with a known start
  timed      INTEGER NOT NULL DEFAULT 0, -- how many runs dur_ms covers
  max_ms     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (server_id, unit, day)
) WITHOUT ROWID;

-- Every server that can report jobs gets the jobs alert rule.
INSERT INTO rules (server_id, metric, threshold, for_s, severity, enabled)
  SELECT id, 'job', 0, 0, 'warning', 1 FROM servers WHERE kind IN ('local', 'agent')
  AND NOT EXISTS (SELECT 1 FROM rules r WHERE r.server_id = servers.id AND r.metric = 'job');

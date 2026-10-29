-- Internet speed tests (the Ookla CLI on the machine the hub runs on).
-- Speeds in megabits per second, times in milliseconds; NULL = not measured.
CREATE TABLE speedtests (
  id              INTEGER PRIMARY KEY,
  server_id       INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  ts              INTEGER NOT NULL,            -- unix seconds the test ran
  ok              INTEGER NOT NULL DEFAULT 1,
  error           TEXT NOT NULL DEFAULT '',
  trigger         TEXT NOT NULL DEFAULT 'schedule', -- schedule | manual | import
  down_mbps       REAL,
  up_mbps         REAL,
  ping_ms         REAL,
  jitter_ms       REAL,
  loss_pct        REAL,
  down_latency_ms REAL,                         -- latency while downloading (IQM)
  up_latency_ms   REAL,                         -- latency while uploading (IQM)
  down_bytes      INTEGER NOT NULL DEFAULT 0,
  up_bytes        INTEGER NOT NULL DEFAULT 0,
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  isp             TEXT NOT NULL DEFAULT '',
  external_ip     TEXT NOT NULL DEFAULT '',
  vpn             INTEGER NOT NULL DEFAULT 0,
  test_server_id  INTEGER NOT NULL DEFAULT 0,
  test_server     TEXT NOT NULL DEFAULT '',
  test_location   TEXT NOT NULL DEFAULT '',
  result_url      TEXT NOT NULL DEFAULT '',
  busy_mbps       REAL,                         -- other traffic on the box right before
  note            TEXT NOT NULL DEFAULT '',
  source_ref      TEXT NOT NULL DEFAULT ''      -- where an imported test came from
);
CREATE INDEX speedtests_server_ts ON speedtests(server_id, ts);
CREATE UNIQUE INDEX speedtests_ref ON speedtests(server_id, source_ref) WHERE source_ref <> '';

-- How and when a server tests its connection, and what its plan promises.
CREATE TABLE speedtest_settings (
  server_id    INTEGER PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
  enabled      INTEGER NOT NULL DEFAULT 0,
  every_min    INTEGER NOT NULL DEFAULT 180,
  offset_min   INTEGER NOT NULL DEFAULT 7,      -- minutes past midnight of the first slot
  test_server_id INTEGER NOT NULL DEFAULT 0,    -- 0 = nearest
  test_server  TEXT NOT NULL DEFAULT '',
  plan_down    REAL NOT NULL DEFAULT 0,
  plan_up      REAL NOT NULL DEFAULT 0,
  min_down     REAL NOT NULL DEFAULT 0,         -- what counts as slow (0 = no line)
  min_up       REAL NOT NULL DEFAULT 0,
  max_ping     REAL NOT NULL DEFAULT 0,
  busy_mbps    REAL NOT NULL DEFAULT 0,         -- put a test off while traffic is above this (0 = never)
  keep_days    INTEGER NOT NULL DEFAULT 0       -- 0 = keep every test
);

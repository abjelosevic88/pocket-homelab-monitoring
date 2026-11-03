-- Drive health (SMART) of every disk a server reports. One row per drive with
-- its latest reading, kept after the drive disappears so a dead disk shows as
-- missing instead of silently dropping off the list.
CREATE TABLE drives (
  server_id  INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  key        TEXT NOT NULL,                 -- the serial number, cleaned up
  name       TEXT NOT NULL DEFAULT '',      -- /dev/sda
  model      TEXT NOT NULL DEFAULT '',
  serial     TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL DEFAULT '',      -- passed | warning | failed
  data       TEXT NOT NULL DEFAULT '',      -- the latest reading (JSON)
  first_seen INTEGER NOT NULL DEFAULT 0,    -- unix s
  last_seen  INTEGER NOT NULL DEFAULT 0,    -- unix s of the newest reading
  PRIMARY KEY (server_id, key)
) WITHOUT ROWID;
CREATE INDEX drives_serial ON drives(serial);

-- One row per drive and hour (the newest reading of the hour); older than
-- 90 days thinned to one a day. attrs holds every attribute's raw value.
CREATE TABLE drive_samples (
  server_id INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  key       TEXT NOT NULL,
  ts        INTEGER NOT NULL,              -- unix s, start of the hour
  status    TEXT NOT NULL DEFAULT '',
  temp      REAL,
  hours     INTEGER,
  wear      REAL,
  spare     REAL,
  errors    INTEGER,
  written   INTEGER,
  read_b    INTEGER,
  attrs     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (server_id, key, ts)
) WITHOUT ROWID;

-- Every server gets the drive rules (they stay quiet where there's no SMART).
INSERT INTO rules (server_id, metric, target, threshold, for_s, severity, enabled)
  SELECT id, 'drive', '', 0, 0, 'critical', 1 FROM servers;
INSERT INTO rules (server_id, metric, target, threshold, for_s, severity, enabled)
  SELECT id, 'drive_errors', '', 0, 0, 'warning', 1 FROM servers;
INSERT INTO rules (server_id, metric, target, threshold, for_s, severity, enabled)
  SELECT id, 'drive_wear', '', 80, 0, 'warning', 1 FROM servers;
INSERT INTO rules (server_id, metric, target, threshold, for_s, severity, enabled)
  SELECT id, 'drive_temp', '', 0, 600, 'warning', 1 FROM servers;

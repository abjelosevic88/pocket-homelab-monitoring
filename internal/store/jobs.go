package store

import (
	"context"
	"database/sql"
	"time"
)

// JobSettings is what a person chose for one job.
type JobSettings struct {
	Unit  string `json:"unit"`
	Group string `json:"group"`
	Label string `json:"label"`
	Notes string `json:"notes"`
	Muted bool   `json:"muted"`
}

// JobRun is one stored run.
type JobRun struct {
	Unit       string `json:"unit"`
	Invocation string `json:"invocation"`
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	OK         bool   `json:"ok"`
	Result     string `json:"result"`
	ExitStatus int    `json:"exit_status"`
	Summary    string `json:"summary"`
	Lines      int    `json:"lines"`
	Warnings   int    `json:"warnings"`
}

// JobStats sums a job's runs over some days.
type JobStats struct {
	Runs   int   `json:"runs"`
	Failed int   `json:"failed"`
	Busy   int   `json:"busy"`
	AvgMs  int64 `json:"avg_ms"`
	MaxMs  int64 `json:"max_ms"`
}

// JobDay is one day of a job (or of all jobs).
type JobDay struct {
	Day    string `json:"day"`
	Runs   int    `json:"runs"`
	Failed int    `json:"failed"`
	Busy   int    `json:"busy"`
}

// JobSettingsFor returns every job's settings on a server, by unit.
func (s *Store) JobSettingsFor(ctx context.Context, serverID int64) map[string]JobSettings {
	out := map[string]JobSettings{}
	rows, err := s.DB.QueryContext(ctx, `SELECT unit, group_name, label, notes, muted FROM job_settings WHERE server_id = ?`, serverID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var j JobSettings
		var muted int
		if rows.Scan(&j.Unit, &j.Group, &j.Label, &j.Notes, &muted) == nil {
			j.Muted = muted == 1
			out[j.Unit] = j
		}
	}
	return out
}

// SaveJobSettings stores one job's settings.
func (s *Store) SaveJobSettings(ctx context.Context, serverID int64, j JobSettings) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO job_settings (server_id, unit, group_name, label, notes, muted) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(server_id, unit) DO UPDATE SET group_name = excluded.group_name, label = excluded.label, notes = excluded.notes, muted = excluded.muted`,
		serverID, j.Unit, j.Group, j.Label, j.Notes, boolInt(j.Muted))
	return err
}

// RecordJobRuns counts runs per day and keeps the ones worth keeping;
// a run already recorded (same invocation) is skipped. Returns the new ones.
func (s *Store) RecordJobRuns(ctx context.Context, serverID int64, runs []JobRun, loc *time.Location) ([]JobRun, error) {
	if len(runs) == 0 {
		return nil, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	var fresh []JobRun
	for _, r := range runs {
		// The invocation id is the key, so a run read twice counts once; quiet
		// successes stay only a day and a half for that (PruneJobRuns).
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO job_runs (server_id, unit, invocation, started_ms, ended_ms, ok, result, exit_status, summary, lines, warnings)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, serverID, r.Unit, r.Invocation, r.Start, r.End, boolInt(r.OK), r.Result, r.ExitStatus, r.Summary, r.Lines, r.Warnings)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		fresh = append(fresh, r)
		dur := int64(0)
		timed := 0
		if r.Start > 0 && r.End >= r.Start {
			dur, timed = r.End-r.Start, 1
		}
		day := time.UnixMilli(r.End).In(loc).Format("2006-01-02")
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_days (server_id, unit, day, runs, failed, busy, dur_ms, timed, max_ms) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?)
			ON CONFLICT(server_id, unit, day) DO UPDATE SET runs = runs + 1, failed = failed + excluded.failed, busy = busy + excluded.busy,
			dur_ms = dur_ms + excluded.dur_ms, timed = timed + excluded.timed, max_ms = MAX(max_ms, excluded.max_ms)`,
			serverID, r.Unit, day, boolInt(!r.OK), boolInt(r.Lines > 0), dur, timed, dur); err != nil {
			return nil, err
		}
	}
	return fresh, tx.Commit()
}

// PruneJobRuns drops quiet successes after a day, other runs after keepDays, day counts after a year.
func (s *Store) PruneJobRuns(ctx context.Context, keepDays int) {
	now := time.Now()
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM job_runs WHERE ok = 1 AND lines = 0 AND ended_ms < ?`, now.Add(-36*time.Hour).UnixMilli())
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM job_runs WHERE ended_ms < ?`, now.AddDate(0, 0, -keepDays).UnixMilli())
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM job_days WHERE day < ?`, now.AddDate(-1, 0, 0).Format("2006-01-02"))
}

// JobRuns lists a server's kept runs, newest first: one job ("" = all),
// only failures, only runs that wrote something, before a moment (paging).
func (s *Store) JobRuns(ctx context.Context, serverID int64, unit string, failedOnly, busyOnly bool, before int64, limit int) ([]JobRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT unit, invocation, started_ms, ended_ms, ok, result, exit_status, summary, lines, warnings FROM job_runs WHERE server_id = ?`
	args := []any{serverID}
	if unit != "" {
		q += ` AND unit = ?`
		args = append(args, unit)
	}
	if failedOnly {
		q += ` AND ok = 0`
	}
	if busyOnly {
		q += ` AND (ok = 0 OR lines > 0)`
	} else {
		q += ` AND (ok = 0 OR lines > 0 OR unit = ?)` // quiet runs only when looking at one job
		args = append(args, unit)
	}
	if before > 0 {
		q += ` AND ended_ms < ?`
		args = append(args, before)
	}
	q += ` ORDER BY ended_ms DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JobRun{}
	for rows.Next() {
		var r JobRun
		var ok int
		if err := rows.Scan(&r.Unit, &r.Invocation, &r.Start, &r.End, &ok, &r.Result, &r.ExitStatus, &r.Summary, &r.Lines, &r.Warnings); err != nil {
			return nil, err
		}
		r.OK = ok == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// JobStatsSince sums every job's days from a date on, by unit.
func (s *Store) JobStatsSince(ctx context.Context, serverID int64, day string) map[string]JobStats {
	out := map[string]JobStats{}
	rows, err := s.DB.QueryContext(ctx, `SELECT unit, SUM(runs), SUM(failed), SUM(busy), SUM(dur_ms), SUM(timed), MAX(max_ms) FROM job_days WHERE server_id = ? AND day >= ? GROUP BY unit`, serverID, day)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var unit string
		var st JobStats
		var dur, timed int64
		if rows.Scan(&unit, &st.Runs, &st.Failed, &st.Busy, &dur, &timed, &st.MaxMs) == nil {
			if timed > 0 {
				st.AvgMs = dur / timed
			}
			out[unit] = st
		}
	}
	return out
}

// JobDays returns per-day counts from a date on, for one job or all ("").
func (s *Store) JobDays(ctx context.Context, serverID int64, unit, day string) []JobDay {
	q := `SELECT day, SUM(runs), SUM(failed), SUM(busy) FROM job_days WHERE server_id = ? AND day >= ?`
	args := []any{serverID, day}
	if unit != "" {
		q += ` AND unit = ?`
		args = append(args, unit)
	}
	q += ` GROUP BY day ORDER BY day`
	out := []JobDay{}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var d JobDay
		if rows.Scan(&d.Day, &d.Runs, &d.Failed, &d.Busy) == nil {
			out = append(out, d)
		}
	}
	return out
}

// LastJobRunEnd is the newest run end stored for a server (unix ms; 0 = none).
func (s *Store) LastJobRunEnd(ctx context.Context, serverID int64) int64 {
	var v sql.NullInt64
	_ = s.DB.QueryRowContext(ctx, `SELECT MAX(ended_ms) FROM job_runs WHERE server_id = ?`, serverID).Scan(&v)
	return v.Int64
}

// EnsureJobRule adds the jobs alert rule to a server that has none.
func (s *Store) EnsureJobRule(ctx context.Context, serverID int64) bool {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO rules (server_id, metric, threshold, for_s, severity, enabled)
		SELECT ?, 'job', 0, 0, 'warning', 1 WHERE NOT EXISTS (SELECT 1 FROM rules WHERE server_id = ? AND metric = 'job')`, serverID, serverID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

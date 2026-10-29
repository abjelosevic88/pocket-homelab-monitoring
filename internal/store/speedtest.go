package store

import (
	"context"
	"database/sql"
	"time"
)

// SpeedTest is one internet speed test. Pointers are nil when not measured.
type SpeedTest struct {
	ID            int64    `json:"id"`
	ServerID      int64    `json:"server_id"`
	T             int64    `json:"t"`
	OK            bool     `json:"ok"`
	Error         string   `json:"error"`
	Trigger       string   `json:"trigger"`
	DownMbps      *float64 `json:"down_mbps"`
	UpMbps        *float64 `json:"up_mbps"`
	PingMs        *float64 `json:"ping_ms"`
	JitterMs      *float64 `json:"jitter_ms"`
	LossPct       *float64 `json:"loss_pct"`
	DownLatencyMs *float64 `json:"down_latency_ms"`
	UpLatencyMs   *float64 `json:"up_latency_ms"`
	DownBytes     int64    `json:"down_bytes"`
	UpBytes       int64    `json:"up_bytes"`
	DurationMs    int64    `json:"duration_ms"`
	ISP           string   `json:"isp"`
	ExternalIP    string   `json:"external_ip"`
	VPN           bool     `json:"vpn"`
	TestServerID  int      `json:"test_server_id"`
	TestServer    string   `json:"test_server"`
	TestLocation  string   `json:"test_location"`
	ResultURL     string   `json:"result_url"`
	BusyMbps      *float64 `json:"busy_mbps"`
	Note          string   `json:"note"`
	SourceRef     string   `json:"-"`
}

// SpeedSettings is how a server tests its connection.
type SpeedSettings struct {
	Enabled      bool    `json:"enabled"`
	EveryMin     int     `json:"every_min"`
	OffsetMin    int     `json:"offset_min"`
	TestServerID int     `json:"test_server_id"`
	TestServer   string  `json:"test_server"`
	PlanDown     float64 `json:"plan_down"`
	PlanUp       float64 `json:"plan_up"`
	MinDown      float64 `json:"min_down"`
	MinUp        float64 `json:"min_up"`
	MaxPing      float64 `json:"max_ping"`
	BusyMbps     float64 `json:"busy_mbps"`
	KeepDays     int     `json:"keep_days"`
}

// DefaultSpeedSettings: every 3 hours, 7 past (off the hour, when other cron jobs run).
func DefaultSpeedSettings() SpeedSettings {
	return SpeedSettings{EveryMin: 180, OffsetMin: 7}
}

const speedCols = `id, server_id, ts, ok, error, trigger, down_mbps, up_mbps, ping_ms, jitter_ms, loss_pct, down_latency_ms, up_latency_ms,
	down_bytes, up_bytes, duration_ms, isp, external_ip, vpn, test_server_id, test_server, test_location, result_url, busy_mbps, note, source_ref`

func scanSpeed(row interface{ Scan(...any) error }) (*SpeedTest, error) {
	var t SpeedTest
	var ok, vpn int
	var down, up, ping, jit, loss, dl, ul, busy sql.NullFloat64
	if err := row.Scan(&t.ID, &t.ServerID, &t.T, &ok, &t.Error, &t.Trigger, &down, &up, &ping, &jit, &loss, &dl, &ul,
		&t.DownBytes, &t.UpBytes, &t.DurationMs, &t.ISP, &t.ExternalIP, &vpn, &t.TestServerID, &t.TestServer, &t.TestLocation, &t.ResultURL, &busy, &t.Note, &t.SourceRef); err != nil {
		return nil, err
	}
	t.OK, t.VPN = ok == 1, vpn == 1
	f := func(n sql.NullFloat64) *float64 {
		if !n.Valid {
			return nil
		}
		v := n.Float64
		return &v
	}
	t.DownMbps, t.UpMbps, t.PingMs, t.JitterMs, t.LossPct, t.DownLatencyMs, t.UpLatencyMs, t.BusyMbps = f(down), f(up), f(ping), f(jit), f(loss), f(dl), f(ul), f(busy)
	return &t, nil
}

func nullF(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// AddSpeedTest stores a test; an imported one already there (same source_ref) is skipped (false).
func (s *Store) AddSpeedTest(ctx context.Context, t *SpeedTest) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO speedtests (server_id, ts, ok, error, trigger, down_mbps, up_mbps, ping_ms, jitter_ms, loss_pct, down_latency_ms, up_latency_ms,
		down_bytes, up_bytes, duration_ms, isp, external_ip, vpn, test_server_id, test_server, test_location, result_url, busy_mbps, note, source_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ServerID, t.T, boolInt(t.OK), t.Error, t.Trigger, nullF(t.DownMbps), nullF(t.UpMbps), nullF(t.PingMs), nullF(t.JitterMs), nullF(t.LossPct), nullF(t.DownLatencyMs), nullF(t.UpLatencyMs),
		t.DownBytes, t.UpBytes, t.DurationMs, t.ISP, t.ExternalIP, boolInt(t.VPN), t.TestServerID, t.TestServer, t.TestLocation, t.ResultURL, nullF(t.BusyMbps), t.Note, t.SourceRef)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	t.ID, _ = res.LastInsertId()
	return true, nil
}

// SpeedTests lists a server's tests from a moment on (0 = all), oldest first;
// limit keeps the newest that many (0 = all).
func (s *Store) SpeedTests(ctx context.Context, serverID, from int64, limit int) ([]SpeedTest, error) {
	q := `SELECT ` + speedCols + ` FROM speedtests WHERE server_id = ? AND ts >= ? ORDER BY ts DESC`
	args := []any{serverID, from}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SpeedTest{}
	for rows.Next() {
		t, err := scanSpeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// LastSpeedTests returns a server's newest n tests, newest first.
func (s *Store) LastSpeedTests(ctx context.Context, serverID int64, n int) []SpeedTest {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+speedCols+` FROM speedtests WHERE server_id = ? ORDER BY ts DESC LIMIT ?`, serverID, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []SpeedTest
	for rows.Next() {
		if t, err := scanSpeed(rows); err == nil {
			out = append(out, *t)
		}
	}
	return out
}

// GetSpeedTest loads one test of a server.
func (s *Store) GetSpeedTest(ctx context.Context, serverID, id int64) (*SpeedTest, error) {
	t, err := scanSpeed(s.DB.QueryRowContext(ctx, `SELECT `+speedCols+` FROM speedtests WHERE server_id = ? AND id = ?`, serverID, id))
	return t, notFound(err)
}

// SetSpeedNote changes a test's note.
func (s *Store) SetSpeedNote(ctx context.Context, serverID, id int64, note string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE speedtests SET note = ? WHERE server_id = ? AND id = ?`, note, serverID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSpeedTest removes one test.
func (s *Store) DeleteSpeedTest(ctx context.Context, serverID, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM speedtests WHERE server_id = ? AND id = ?`, serverID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountSpeedTests is how many tests a server has.
func (s *Store) CountSpeedTests(ctx context.Context, serverID int64) int {
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM speedtests WHERE server_id = ?`, serverID).Scan(&n)
	return n
}

// GetSpeedSettings returns a server's speed test settings (defaults when never saved).
func (s *Store) GetSpeedSettings(ctx context.Context, serverID int64) SpeedSettings {
	st := DefaultSpeedSettings()
	var en int
	err := s.DB.QueryRowContext(ctx, `SELECT enabled, every_min, offset_min, test_server_id, test_server, plan_down, plan_up, min_down, min_up, max_ping, busy_mbps, keep_days
		FROM speedtest_settings WHERE server_id = ?`, serverID).Scan(&en, &st.EveryMin, &st.OffsetMin, &st.TestServerID, &st.TestServer, &st.PlanDown, &st.PlanUp, &st.MinDown, &st.MinUp, &st.MaxPing, &st.BusyMbps, &st.KeepDays)
	if err != nil {
		return DefaultSpeedSettings()
	}
	st.Enabled = en == 1
	return st
}

// SaveSpeedSettings stores a server's speed test settings.
func (s *Store) SaveSpeedSettings(ctx context.Context, serverID int64, st SpeedSettings) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO speedtest_settings (server_id, enabled, every_min, offset_min, test_server_id, test_server, plan_down, plan_up, min_down, min_up, max_ping, busy_mbps, keep_days)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(server_id) DO UPDATE SET enabled = excluded.enabled, every_min = excluded.every_min, offset_min = excluded.offset_min,
		test_server_id = excluded.test_server_id, test_server = excluded.test_server, plan_down = excluded.plan_down, plan_up = excluded.plan_up,
		min_down = excluded.min_down, min_up = excluded.min_up, max_ping = excluded.max_ping, busy_mbps = excluded.busy_mbps, keep_days = excluded.keep_days`,
		serverID, boolInt(st.Enabled), st.EveryMin, st.OffsetMin, st.TestServerID, st.TestServer, st.PlanDown, st.PlanUp, st.MinDown, st.MinUp, st.MaxPing, st.BusyMbps, st.KeepDays)
	return err
}

// SpeedTestServers lists the servers with scheduled speed tests on.
func (s *Store) SpeedTestServers(ctx context.Context) []int64 {
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id FROM speedtest_settings WHERE enabled = 1`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// PruneSpeedTests drops tests older than each server's keep_days.
func (s *Store) PruneSpeedTests(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id, keep_days FROM speedtest_settings WHERE keep_days > 0`)
	if err != nil {
		return
	}
	type keep struct {
		id   int64
		days int
	}
	var list []keep
	for rows.Next() {
		var k keep
		if rows.Scan(&k.id, &k.days) == nil {
			list = append(list, k)
		}
	}
	rows.Close()
	for _, k := range list {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM speedtests WHERE server_id = ? AND ts < ?`, k.id, time.Now().AddDate(0, 0, -k.days).Unix())
	}
}

// EnsureSpeedRules adds the speed test alert rules to a server that has none.
func (s *Store) EnsureSpeedRules(ctx context.Context, serverID int64, st SpeedSettings) bool {
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM rules WHERE server_id = ? AND metric IN ('speedtest', 'speed_down', 'speed_up', 'speed_ping')`, serverID).Scan(&n)
	if n > 0 {
		return false
	}
	pick := func(v, def float64) (float64, bool) {
		if v > 0 {
			return v, true
		}
		return def, false
	}
	down, dOn := pick(st.MinDown, 50)
	up, uOn := pick(st.MinUp, 10)
	ping, pOn := pick(st.MaxPing, 50)
	for _, r := range []Rule{
		{ServerID: serverID, Metric: "speedtest", ForS: 2, Severity: "critical", Enabled: true},
		{ServerID: serverID, Metric: "speed_down", Threshold: down, ForS: 2, Severity: "warning", Enabled: dOn},
		{ServerID: serverID, Metric: "speed_up", Threshold: up, ForS: 2, Severity: "warning", Enabled: uOn},
		{ServerID: serverID, Metric: "speed_ping", Threshold: ping, ForS: 2, Severity: "warning", Enabled: pOn},
	} {
		r := r
		_ = s.SaveRule(ctx, &r)
	}
	return true
}

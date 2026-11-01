package store

import (
	"context"
	"database/sql"
	"time"
)

// Check statuses (the same numbers as Uptime Kuma).
const (
	CheckDown    = 0
	CheckUp      = 1
	CheckPending = 2
)

// Check is one uptime check and where it stands.
type Check struct {
	ID              int64    `json:"id"`
	ServerID        int64    `json:"server_id"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	Target          string   `json:"target"`
	Port            int      `json:"port"`
	Method          string   `json:"method"`
	Keyword         string   `json:"keyword"`
	InvertKeyword   bool     `json:"invert_keyword"`
	StatusCodes     string   `json:"status_codes"`
	IgnoreTLS       bool     `json:"ignore_tls"`
	FollowRedirects bool     `json:"follow_redirects"`
	DNSServer       string   `json:"dns_server"`
	DNSType         string   `json:"dns_type"`
	PushToken       string   `json:"push_token"`
	IntervalS       int      `json:"interval_s"`
	RetryS          int      `json:"retry_s"`
	TimeoutS        int      `json:"timeout_s"`
	Retries         int      `json:"retries"`
	Enabled         bool     `json:"enabled"`
	Notify          bool     `json:"notify"`
	Public          bool     `json:"public"`
	Group           string   `json:"group"`
	Notes           string   `json:"notes"`
	SourceRef       string   `json:"source_ref,omitempty"`
	CreatedAt       string   `json:"created_at"`
	Status          int      `json:"status"`
	Fails           int      `json:"fails"`
	LastAt          int64    `json:"last_at"`
	LastMs          *float64 `json:"last_ms"`
	LastMsg         string   `json:"last_msg"`
	ChangedAt       int64    `json:"changed_at"`
	CertExpires     int64    `json:"cert_expires"`
}

// Beat is one result.
type Beat struct {
	T      int64    `json:"t"`
	Status int      `json:"s"`
	Ms     *float64 `json:"ms"`
	Msg    string   `json:"msg,omitempty"`
}

// CheckDay is one local day of results.
type CheckDay struct {
	Day   string  `json:"day"`
	Up    int     `json:"up"`
	Down  int     `json:"down"`
	MsSum float64 `json:"-"`
	MsN   int     `json:"-"`
	AvgMs float64 `json:"avg_ms"`
}

// CheckEvent is a check going down or coming back.
type CheckEvent struct {
	CheckID int64  `json:"check_id"`
	T       int64  `json:"t"`
	Status  int    `json:"s"`
	Msg     string `json:"msg"`
}

// StatusPage is a server's public page.
type StatusPage struct {
	ServerID    int64  `json:"server_id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

const checkCols = `id, server_id, name, kind, target, port, method, keyword, invert_keyword, status_codes, ignore_tls, follow_redirects,
	dns_server, dns_type, push_token, interval_s, retry_s, timeout_s, retries, enabled, notify, public, group_name, notes, source_ref, created_at,
	status, fails, last_at, last_ms, last_msg, changed_at, cert_expires`

func scanCheck(row interface{ Scan(...any) error }) (*Check, error) {
	var c Check
	var inv, tls, follow, en, notify, pub int
	var ms sql.NullFloat64
	err := row.Scan(&c.ID, &c.ServerID, &c.Name, &c.Kind, &c.Target, &c.Port, &c.Method, &c.Keyword, &inv, &c.StatusCodes, &tls, &follow,
		&c.DNSServer, &c.DNSType, &c.PushToken, &c.IntervalS, &c.RetryS, &c.TimeoutS, &c.Retries, &en, &notify, &pub, &c.Group, &c.Notes, &c.SourceRef, &c.CreatedAt,
		&c.Status, &c.Fails, &c.LastAt, &ms, &c.LastMsg, &c.ChangedAt, &c.CertExpires)
	if err != nil {
		return nil, err
	}
	c.InvertKeyword, c.IgnoreTLS, c.FollowRedirects, c.Enabled, c.Notify, c.Public = inv == 1, tls == 1, follow == 1, en == 1, notify == 1, pub == 1
	if ms.Valid {
		v := ms.Float64
		c.LastMs = &v
	}
	return &c, nil
}

func (s *Store) queryChecks(ctx context.Context, q string, args ...any) ([]Check, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+checkCols+` FROM checks `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Check{}
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Checks lists a server's checks, grouped and by name.
func (s *Store) Checks(ctx context.Context, serverID int64) ([]Check, error) {
	return s.queryChecks(ctx, `WHERE server_id = ? ORDER BY group_name = '', group_name COLLATE NOCASE, name COLLATE NOCASE`, serverID)
}

// AllChecks lists every check on the box (the runner).
func (s *Store) AllChecks(ctx context.Context) ([]Check, error) {
	return s.queryChecks(ctx, `ORDER BY id`)
}

// GetCheck loads one check of a server.
func (s *Store) GetCheck(ctx context.Context, serverID, id int64) (*Check, error) {
	return notFoundCheck(scanCheck(s.DB.QueryRowContext(ctx, `SELECT `+checkCols+` FROM checks WHERE server_id = ? AND id = ?`, serverID, id)))
}

// CheckByToken finds the push check a token belongs to.
func (s *Store) CheckByToken(ctx context.Context, token string) (*Check, error) {
	return notFoundCheck(scanCheck(s.DB.QueryRowContext(ctx, `SELECT `+checkCols+` FROM checks WHERE push_token = ? AND push_token <> ''`, token)))
}

func notFoundCheck(c *Check, err error) (*Check, error) {
	if err != nil {
		return nil, notFound(err)
	}
	return c, nil
}

// SaveCheck inserts or updates a check's settings (not its state).
func (s *Store) SaveCheck(ctx context.Context, c *Check) error {
	args := []any{c.Name, c.Kind, c.Target, c.Port, c.Method, c.Keyword, boolInt(c.InvertKeyword), c.StatusCodes, boolInt(c.IgnoreTLS), boolInt(c.FollowRedirects),
		c.DNSServer, c.DNSType, c.PushToken, c.IntervalS, c.RetryS, c.TimeoutS, c.Retries, boolInt(c.Enabled), boolInt(c.Notify), boolInt(c.Public), c.Group, c.Notes}
	if c.ID == 0 {
		if c.CreatedAt == "" {
			c.CreatedAt = Now()
		}
		res, err := s.DB.ExecContext(ctx, `INSERT INTO checks (name, kind, target, port, method, keyword, invert_keyword, status_codes, ignore_tls, follow_redirects,
			dns_server, dns_type, push_token, interval_s, retry_s, timeout_s, retries, enabled, notify, public, group_name, notes, server_id, source_ref, created_at, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append(args, c.ServerID, c.SourceRef, c.CreatedAt, CheckPending)...)
		if err != nil {
			return conflict(err)
		}
		c.ID, _ = res.LastInsertId()
		c.Status = CheckPending
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE checks SET name = ?, kind = ?, target = ?, port = ?, method = ?, keyword = ?, invert_keyword = ?, status_codes = ?, ignore_tls = ?, follow_redirects = ?,
		dns_server = ?, dns_type = ?, push_token = ?, interval_s = ?, retry_s = ?, timeout_s = ?, retries = ?, enabled = ?, notify = ?, public = ?, group_name = ?, notes = ?
		WHERE id = ?`, append(args, c.ID)...)
	return conflict(err)
}

// SaveCheckState stores where a check stands after a result.
func (s *Store) SaveCheckState(ctx context.Context, c *Check) error {
	var ms any
	if c.LastMs != nil {
		ms = *c.LastMs
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE checks SET status = ?, fails = ?, last_at = ?, last_ms = ?, last_msg = ?, changed_at = ?, cert_expires = ? WHERE id = ?`,
		c.Status, c.Fails, c.LastAt, ms, c.LastMsg, c.ChangedAt, c.CertExpires, c.ID)
	return err
}

// DeleteCheck removes a check and its history.
func (s *Store) DeleteCheck(ctx context.Context, serverID, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM checks WHERE server_id = ? AND id = ?`, serverID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddBeat records a result: the beat itself, the day's tally and, when the
// status flipped between up and down, an event.
func (s *Store) AddBeat(ctx context.Context, checkID int64, b Beat, event bool, loc *time.Location) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var ms any
	if b.Ms != nil {
		ms = *b.Ms
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO check_beats (check_id, ts, status, ms, msg) VALUES (?, ?, ?, ?, ?)`, checkID, b.T, b.Status, ms, b.Msg); err != nil {
		return err
	}
	up, down, msN, msSum := 0, 0, 0, 0.0
	if b.Status == CheckDown {
		down = 1
	} else {
		up = 1
	}
	if b.Ms != nil && b.Status != CheckDown {
		msN, msSum = 1, *b.Ms
	}
	day := time.Unix(b.T, 0).In(loc).Format("2006-01-02")
	if _, err := tx.ExecContext(ctx, `INSERT INTO check_days (check_id, day, up, down, ms_sum, ms_n) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(check_id, day) DO UPDATE SET up = up + excluded.up, down = down + excluded.down, ms_sum = ms_sum + excluded.ms_sum, ms_n = ms_n + excluded.ms_n`,
		checkID, day, up, down, msSum, msN); err != nil {
		return err
	}
	if event {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO check_events (check_id, ts, status, msg) VALUES (?, ?, ?, ?)`, checkID, b.T, b.Status, b.Msg); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Beats returns a check's results since from (oldest first).
func (s *Store) Beats(ctx context.Context, checkID, from int64) ([]Beat, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT ts, status, ms, msg FROM check_beats WHERE check_id = ? AND ts >= ? ORDER BY ts`, checkID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Beat{}
	for rows.Next() {
		var b Beat
		var ms sql.NullFloat64
		if err := rows.Scan(&b.T, &b.Status, &ms, &b.Msg); err != nil {
			return nil, err
		}
		if ms.Valid {
			v := ms.Float64
			b.Ms = &v
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RecentBeats returns the newest n results of every check of a server, oldest first.
func (s *Store) RecentBeats(ctx context.Context, serverID int64, n int) (map[int64][]Beat, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT check_id, ts, status, ms FROM (
		SELECT b.check_id, b.ts, b.status, b.ms, ROW_NUMBER() OVER (PARTITION BY b.check_id ORDER BY b.ts DESC) AS rn
		FROM check_beats b JOIN checks c ON c.id = b.check_id WHERE c.server_id = ? AND b.ts >= ?) WHERE rn <= ? ORDER BY check_id, ts`,
		serverID, time.Now().Add(-48*time.Hour).Unix(), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]Beat{}
	for rows.Next() {
		var id int64
		var b Beat
		var ms sql.NullFloat64
		if err := rows.Scan(&id, &b.T, &b.Status, &ms); err != nil {
			return nil, err
		}
		if ms.Valid {
			v := ms.Float64
			b.Ms = &v
		}
		out[id] = append(out[id], b)
	}
	return out, rows.Err()
}

// Uptime is the share of results that weren't down since from (-1 = none),
// read from the beats when they reach back that far, else from the days.
func (s *Store) Uptime(ctx context.Context, serverID int64, from time.Time, loc *time.Location) map[int64]float64 {
	out := map[int64]float64{}
	q := `SELECT b.check_id, SUM(b.status <> 0), COUNT(1) FROM check_beats b JOIN checks c ON c.id = b.check_id WHERE c.server_id = ? AND b.ts >= ? GROUP BY b.check_id`
	args := []any{serverID, from.Unix()}
	if time.Since(from) > 7*24*time.Hour {
		q = `SELECT d.check_id, SUM(d.up), SUM(d.up + d.down) FROM check_days d JOIN checks c ON c.id = d.check_id WHERE c.server_id = ? AND d.day >= ? GROUP BY d.check_id`
		args = []any{serverID, from.In(loc).Format("2006-01-02")}
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var up, n float64
		if rows.Scan(&id, &up, &n) == nil && n > 0 {
			out[id] = up / n
		}
	}
	return out
}

// CheckDays returns a check's days since from (oldest first).
func (s *Store) CheckDays(ctx context.Context, checkID int64, from string) ([]CheckDay, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT day, up, down, ms_sum, ms_n FROM check_days WHERE check_id = ? AND day >= ? ORDER BY day`, checkID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckDay{}
	for rows.Next() {
		var d CheckDay
		if err := rows.Scan(&d.Day, &d.Up, &d.Down, &d.MsSum, &d.MsN); err != nil {
			return nil, err
		}
		if d.MsN > 0 {
			d.AvgMs = d.MsSum / float64(d.MsN)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CheckEvents returns a server's (or one check's) newest ups and downs.
func (s *Store) CheckEvents(ctx context.Context, serverID, checkID int64, limit int) ([]CheckEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT e.check_id, e.ts, e.status, e.msg FROM check_events e JOIN checks c ON c.id = e.check_id WHERE c.server_id = ?`
	args := []any{serverID}
	if checkID > 0 {
		q += ` AND e.check_id = ?`
		args = append(args, checkID)
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY e.ts DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckEvent{}
	for rows.Next() {
		var e CheckEvent
		if err := rows.Scan(&e.CheckID, &e.T, &e.Status, &e.Msg); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ImportCheckDay adds an imported day (from Uptime Kuma) unless it is already there.
func (s *Store) ImportCheckDay(ctx context.Context, checkID int64, d CheckDay) {
	_, _ = s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO check_days (check_id, day, up, down, ms_sum, ms_n) VALUES (?, ?, ?, ?, ?, ?)`,
		checkID, d.Day, d.Up, d.Down, d.AvgMs*float64(d.Up), d.Up)
}

// ImportCheckEvent adds an imported up/down change.
func (s *Store) ImportCheckEvent(ctx context.Context, e CheckEvent) {
	_, _ = s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO check_events (check_id, ts, status, msg) VALUES (?, ?, ?, ?)`, e.CheckID, e.T, e.Status, e.Msg)
}

// ImportBeat adds an imported result without touching the days (they come separately).
func (s *Store) ImportBeat(ctx context.Context, checkID int64, b Beat) {
	var ms any
	if b.Ms != nil {
		ms = *b.Ms
	}
	_, _ = s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO check_beats (check_id, ts, status, ms, msg) VALUES (?, ?, ?, ?, ?)`, checkID, b.T, b.Status, ms, b.Msg)
}

// PruneChecks drops results older than a week and days/events older than ~13 months.
func (s *Store) PruneChecks(ctx context.Context) {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM check_beats WHERE ts < ?`, time.Now().Add(-8*24*time.Hour).Unix())
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM check_days WHERE day < ?`, time.Now().AddDate(0, 0, -400).Format("2006-01-02"))
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM check_events WHERE ts < ?`, time.Now().AddDate(0, 0, -400).Unix())
}

// EnsureCheckRules gives a server the check and certificate rules once.
func (s *Store) EnsureCheckRules(ctx context.Context, serverID int64) bool {
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM rules WHERE server_id = ? AND metric IN ('check', 'cert')`, serverID).Scan(&n)
	if n > 0 {
		return false
	}
	for _, r := range []Rule{
		{ServerID: serverID, Metric: "check", Severity: "critical", Enabled: true},
		{ServerID: serverID, Metric: "cert", Threshold: 14, Severity: "warning", Enabled: true},
	} {
		r := r
		_ = s.SaveRule(ctx, &r)
	}
	return true
}

// GetStatusPage returns a server's status page (nil when it has none).
func (s *Store) GetStatusPage(ctx context.Context, serverID int64) *StatusPage {
	var p StatusPage
	var en int
	if err := s.DB.QueryRowContext(ctx, `SELECT server_id, slug, title, description, enabled FROM status_pages WHERE server_id = ?`, serverID).Scan(&p.ServerID, &p.Slug, &p.Title, &p.Description, &en); err != nil {
		return nil
	}
	p.Enabled = en == 1
	return &p
}

// StatusPageBySlug finds an enabled status page.
func (s *Store) StatusPageBySlug(ctx context.Context, slug string) *StatusPage {
	var p StatusPage
	if err := s.DB.QueryRowContext(ctx, `SELECT server_id, slug, title, description FROM status_pages WHERE slug = ? AND enabled = 1`, slug).Scan(&p.ServerID, &p.Slug, &p.Title, &p.Description); err != nil {
		return nil
	}
	p.Enabled = true
	return &p
}

// SaveStatusPage stores a server's status page.
func (s *Store) SaveStatusPage(ctx context.Context, p StatusPage) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO status_pages (server_id, slug, title, description, enabled) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(server_id) DO UPDATE SET slug = excluded.slug, title = excluded.title, description = excluded.description, enabled = excluded.enabled`,
		p.ServerID, p.Slug, p.Title, p.Description, boolInt(p.Enabled))
	return conflict(err)
}

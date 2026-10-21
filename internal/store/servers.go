package store

import (
	"context"
	"database/sql"
	"strings"
)

const serverCols = `id, user_id, name, kind, url, username, secret, color, enabled, interval_s, sort_order, last_seen, last_error, created_at, updated_at`

func scanServer(row interface{ Scan(...any) error }) (*Server, error) {
	var s Server
	var en int
	if err := row.Scan(&s.ID, &s.UserID, &s.Name, &s.Kind, &s.URL, &s.Username, &s.Secret, &s.Color, &en, &s.IntervalS, &s.SortOrder, &s.LastSeen, &s.LastError, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, notFound(err)
	}
	s.Enabled = en == 1
	s.HasSecret = s.Secret != ""
	return &s, nil
}

func (s *Store) queryServers(ctx context.Context, q string, args ...any) ([]Server, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Server{}
	for rows.Next() {
		sv, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sv)
	}
	return out, rows.Err()
}

// AllServers lists every server on the box (for the pollers, never for a person).
func (s *Store) AllServers(ctx context.Context) ([]Server, error) {
	return s.queryServers(ctx, `SELECT `+serverCols+` FROM servers ORDER BY id`)
}

// GetServer loads one server regardless of who asks (internal use).
func (s *Store) GetServer(ctx context.Context, id int64) (*Server, error) {
	return scanServer(s.DB.QueryRowContext(ctx, `SELECT `+serverCols+` FROM servers WHERE id = ?`, id))
}

// VisibleServers lists the servers a person owns plus those shared with them.
func (s *Store) VisibleServers(ctx context.Context, uid int64) ([]Server, error) {
	return s.queryServers(ctx, `SELECT `+serverCols+` FROM servers sv WHERE sv.user_id = ?
		OR (EXISTS (SELECT 1 FROM server_shares sh WHERE sh.server_id = sv.id AND (sh.user_id = ? OR sh.everyone = 1))
		    AND NOT EXISTS (SELECT 1 FROM server_hidden h WHERE h.server_id = sv.id AND h.user_id = ?))
		ORDER BY CASE WHEN sv.user_id = ? THEN 0 ELSE 1 END, sv.sort_order, sv.id`, uid, uid, uid, uid)
}

// ServerFor returns a server if uid may see it, with their role ("owner" or
// "viewer"); anything else is ErrNotFound.
func (s *Store) ServerFor(ctx context.Context, uid, id int64) (*Server, string, error) {
	sv, err := s.GetServer(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if sv.UserID == uid {
		return sv, "owner", nil
	}
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM server_shares sh WHERE sh.server_id = ? AND (sh.user_id = ? OR sh.everyone = 1)
		AND NOT EXISTS (SELECT 1 FROM server_hidden h WHERE h.server_id = sh.server_id AND h.user_id = ?)`, id, uid, uid).Scan(&n)
	if n == 0 {
		return nil, "", ErrNotFound
	}
	return sv, "viewer", nil
}

// Watchers are the people who see a server: the owner and its sharees.
func (s *Store) Watchers(ctx context.Context, id int64) []int64 {
	rows, err := s.DB.QueryContext(ctx, `SELECT user_id FROM servers WHERE id = ?
		UNION SELECT u.id FROM server_shares sh JOIN users u ON (sh.user_id = u.id OR sh.everyone = 1)
		WHERE sh.server_id = ? AND NOT EXISTS (SELECT 1 FROM server_hidden h WHERE h.server_id = sh.server_id AND h.user_id = u.id)`, id, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var x int64
		if rows.Scan(&x) == nil {
			out = append(out, x)
		}
	}
	return out
}

// CreateServer inserts a server at the end of the owner's list.
func (s *Store) CreateServer(ctx context.Context, sv *Server) error {
	if sv.IntervalS <= 0 {
		sv.IntervalS = 10
	}
	_ = s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order), 0) + 1 FROM servers WHERE user_id = ?`, sv.UserID).Scan(&sv.SortOrder)
	res, err := s.DB.ExecContext(ctx, `INSERT INTO servers (user_id, name, kind, url, username, secret, color, enabled, interval_s, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sv.UserID, sv.Name, sv.Kind, sv.URL, sv.Username, sv.Secret, sv.Color, boolInt(sv.Enabled), sv.IntervalS, sv.SortOrder)
	if err != nil {
		return conflict(err)
	}
	sv.ID, _ = res.LastInsertId()
	sv.CreatedAt, sv.UpdatedAt = Now(), Now()
	sv.HasSecret = sv.Secret != ""
	return nil
}

// UpdateServer writes the editable fields.
func (s *Store) UpdateServer(ctx context.Context, sv *Server) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE servers SET name = ?, url = ?, username = ?, secret = ?, color = ?, enabled = ?, interval_s = ?, sort_order = ?, updated_at = ? WHERE id = ?`,
		sv.Name, sv.URL, sv.Username, sv.Secret, sv.Color, boolInt(sv.Enabled), sv.IntervalS, sv.SortOrder, Now(), sv.ID)
	sv.HasSecret = sv.Secret != ""
	return err
}

// MarkSeen records a successful (err "") or failed reading.
func (s *Store) MarkSeen(ctx context.Context, id int64, seen, errMsg string) {
	if seen != "" {
		_, _ = s.DB.ExecContext(ctx, `UPDATE servers SET last_seen = ?, last_error = '' WHERE id = ?`, seen, id)
		return
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE servers SET last_error = ? WHERE id = ?`, errMsg, id)
}

// DeleteServer removes a server with its history, rules and alerts.
func (s *Store) DeleteServer(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReorderServers sets sort_order of the owner's servers to the given order.
func (s *Store) ReorderServers(ctx context.Context, uid int64, ids []int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE servers SET sort_order = ? WHERE id = ? AND user_id = ?`, i+1, id, uid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HasLocalServer reports whether some server reads this box directly.
func (s *Store) HasLocalServer(ctx context.Context) bool {
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM servers WHERE kind = 'local'`).Scan(&n)
	return n > 0
}

// ---- shares ----

// ListShares lists who a server is shared with.
func (s *Store) ListShares(ctx context.Context, serverID int64) ([]Share, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT sh.id, sh.server_id, sh.user_id, sh.everyone, sh.created_at, COALESCE(u.name, ''), COALESCE(u.email, '')
		FROM server_shares sh LEFT JOIN users u ON u.id = sh.user_id WHERE sh.server_id = ? ORDER BY sh.everyone DESC, sh.id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Share{}
	for rows.Next() {
		var sh Share
		var uid sql.NullInt64
		var ev int
		var name, email string
		if err := rows.Scan(&sh.ID, &sh.ServerID, &uid, &ev, &sh.CreatedAt, &name, &email); err != nil {
			return nil, err
		}
		sh.Everyone = ev == 1
		if uid.Valid {
			id := uid.Int64
			sh.UserID = &id
			sh.Person = &Person{ID: id, Name: name, Email: email}
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// CreateShare shares a server with a person (uid > 0) or everyone.
func (s *Store) CreateShare(ctx context.Context, serverID, uid int64, everyone bool) (*Share, error) {
	var res sql.Result
	var err error
	if everyone {
		res, err = s.DB.ExecContext(ctx, `INSERT INTO server_shares (server_id, everyone) VALUES (?, 1)`, serverID)
	} else {
		res, err = s.DB.ExecContext(ctx, `INSERT INTO server_shares (server_id, user_id) VALUES (?, ?)`, serverID, uid)
		if err == nil {
			_, _ = s.DB.ExecContext(ctx, `DELETE FROM server_hidden WHERE server_id = ? AND user_id = ?`, serverID, uid)
		}
	}
	if err != nil {
		return nil, conflict(err)
	}
	id, _ := res.LastInsertId()
	shares, err := s.ListShares(ctx, serverID)
	if err != nil {
		return nil, err
	}
	for _, sh := range shares {
		if sh.ID == id {
			return &sh, nil
		}
	}
	return nil, ErrNotFound
}

// GetShare loads a share with its server's owner.
func (s *Store) GetShare(ctx context.Context, id int64) (*Share, int64, error) {
	var sh Share
	var uid sql.NullInt64
	var ev int
	var owner int64
	err := s.DB.QueryRowContext(ctx, `SELECT sh.id, sh.server_id, sh.user_id, sh.everyone, sh.created_at, sv.user_id FROM server_shares sh JOIN servers sv ON sv.id = sh.server_id WHERE sh.id = ?`, id).
		Scan(&sh.ID, &sh.ServerID, &uid, &ev, &sh.CreatedAt, &owner)
	if err != nil {
		return nil, 0, notFound(err)
	}
	sh.Everyone = ev == 1
	if uid.Valid {
		v := uid.Int64
		sh.UserID = &v
	}
	return &sh, owner, nil
}

// DeleteShare removes a share.
func (s *Store) DeleteShare(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM server_shares WHERE id = ?`, id)
	return err
}

// LeaveServer takes a shared server off a viewer's list.
func (s *Store) LeaveServer(ctx context.Context, serverID, uid int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM server_shares WHERE server_id = ? AND user_id = ?`, serverID, uid)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO server_hidden (server_id, user_id) VALUES (?, ?)`, serverID, uid)
	return err
}

// People lists everyone on the box (name and email only).
func (s *Store) People(ctx context.Context) ([]Person, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, email FROM users ORDER BY name, email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name, &p.Email); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- rules ----

const ruleCols = `id, server_id, metric, target, threshold, for_s, severity, enabled, created_at`

func scanRule(row interface{ Scan(...any) error }) (*Rule, error) {
	var r Rule
	var en int
	if err := row.Scan(&r.ID, &r.ServerID, &r.Metric, &r.Target, &r.Threshold, &r.ForS, &r.Severity, &en, &r.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	r.Enabled = en == 1
	return &r, nil
}

// ListRules lists the rules of the given servers (all servers when ids is nil).
func (s *Store) ListRules(ctx context.Context, serverIDs []int64) ([]Rule, error) {
	q := `SELECT ` + ruleCols + ` FROM rules`
	var args []any
	if serverIDs != nil {
		if len(serverIDs) == 0 {
			return []Rule{}, nil
		}
		q += ` WHERE server_id IN (?` + strings.Repeat(",?", len(serverIDs)-1) + `)`
		for _, id := range serverIDs {
			args = append(args, id)
		}
	}
	rows, err := s.DB.QueryContext(ctx, q+` ORDER BY server_id, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetRule loads one rule.
func (s *Store) GetRule(ctx context.Context, id int64) (*Rule, error) {
	return scanRule(s.DB.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM rules WHERE id = ?`, id))
}

// SaveRule inserts (ID 0) or updates a rule.
func (s *Store) SaveRule(ctx context.Context, r *Rule) error {
	if r.ID == 0 {
		res, err := s.DB.ExecContext(ctx, `INSERT INTO rules (server_id, metric, target, threshold, for_s, severity, enabled) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.ServerID, r.Metric, r.Target, r.Threshold, r.ForS, r.Severity, boolInt(r.Enabled))
		if err != nil {
			return err
		}
		r.ID, _ = res.LastInsertId()
		r.CreatedAt = Now()
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE rules SET metric = ?, target = ?, threshold = ?, for_s = ?, severity = ?, enabled = ? WHERE id = ?`,
		r.Metric, r.Target, r.Threshold, r.ForS, r.Severity, boolInt(r.Enabled), r.ID)
	return err
}

// DeleteRule removes a rule (its alerts stay in the history).
func (s *Store) DeleteRule(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, id)
	return err
}

// ---- alerts ----

const alertCols = `a.id, a.server_id, COALESCE(sv.name, ''), a.rule_id, a.metric, a.target, a.severity, a.message, a.value, a.threshold, a.started_at, a.resolved_at`

func scanAlert(row interface{ Scan(...any) error }) (*Alert, error) {
	var a Alert
	var rid sql.NullInt64
	var res sql.NullString
	if err := row.Scan(&a.ID, &a.ServerID, &a.ServerName, &rid, &a.Metric, &a.Target, &a.Severity, &a.Message, &a.Value, &a.Threshold, &a.StartedAt, &res); err != nil {
		return nil, notFound(err)
	}
	if rid.Valid {
		v := rid.Int64
		a.RuleID = &v
	}
	if res.Valid {
		v := res.String
		a.ResolvedAt = &v
	}
	return &a, nil
}

// ListAlerts returns alerts of the given servers, newest first.
func (s *Store) ListAlerts(ctx context.Context, serverIDs []int64, activeOnly bool, limit int) ([]Alert, error) {
	if len(serverIDs) == 0 {
		return []Alert{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT ` + alertCols + ` FROM alerts a LEFT JOIN servers sv ON sv.id = a.server_id WHERE a.server_id IN (?` + strings.Repeat(",?", len(serverIDs)-1) + `)`
	args := []any{}
	for _, id := range serverIDs {
		args = append(args, id)
	}
	if activeOnly {
		q += ` AND a.resolved_at IS NULL`
	}
	q += ` ORDER BY a.started_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// OpenAlerts returns every unresolved alert (the engine's state on start).
func (s *Store) OpenAlerts(ctx context.Context) ([]Alert, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+alertCols+` FROM alerts a LEFT JOIN servers sv ON sv.id = a.server_id WHERE a.resolved_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// OpenAlertCounts counts unresolved alerts per server.
func (s *Store) OpenAlertCounts(ctx context.Context) map[int64]int {
	out := map[int64]int{}
	rows, err := s.DB.QueryContext(ctx, `SELECT server_id, COUNT(1) FROM alerts WHERE resolved_at IS NULL GROUP BY server_id`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if rows.Scan(&id, &n) == nil {
			out[id] = n
		}
	}
	return out
}

// CreateAlert opens an alert.
func (s *Store) CreateAlert(ctx context.Context, a *Alert) error {
	if a.StartedAt == "" {
		a.StartedAt = Now()
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO alerts (server_id, rule_id, metric, target, severity, message, value, threshold, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ServerID, a.RuleID, a.Metric, a.Target, a.Severity, a.Message, a.Value, a.Threshold, a.StartedAt)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// ResolveAlert closes an alert.
func (s *Store) ResolveAlert(ctx context.Context, id int64) string {
	at := Now()
	_, _ = s.DB.ExecContext(ctx, `UPDATE alerts SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL`, at, id)
	return at
}

// PruneAlerts forgets resolved alerts older than days.
func (s *Store) PruneAlerts(ctx context.Context, days int) {
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM alerts WHERE resolved_at IS NOT NULL AND resolved_at < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', ?)`, "-"+itoa(days)+" days")
}

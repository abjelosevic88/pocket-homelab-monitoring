package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// DriveRow is one drive of a server: its summary columns and the latest
// reading as JSON (a collect.Drive, kept opaque here).
type DriveRow struct {
	ServerID  int64           `json:"server_id"`
	Key       string          `json:"key"`
	Name      string          `json:"name"`
	Model     string          `json:"model"`
	Serial    string          `json:"serial"`
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	FirstSeen int64           `json:"first_seen"`
	LastSeen  int64           `json:"last_seen"`
}

// DriveSample is one point of a drive's history.
type DriveSample struct {
	T       int64            `json:"t"`
	Status  string           `json:"status"`
	Temp    *float64         `json:"temp"`
	Hours   *int64           `json:"hours"`
	Wear    *float64         `json:"wear"`
	Spare   *float64         `json:"spare"`
	Errors  *int64           `json:"errors"`
	Written *int64           `json:"written"`
	Read    *int64           `json:"read"`
	Attrs   map[string]int64 `json:"attrs,omitempty"`
}

// DriveHourly keeps every hour of drive history this long, then one a day.
const DriveHourlyDays = 90

// SaveDrive records a drive's latest reading and the hour's history point
// (p nil: nothing new to add to the history).
func (s *Store) SaveDrive(ctx context.Context, serverID int64, d DriveRow, p *DriveSample) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO drives (server_id, key, name, model, serial, status, data, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (server_id, key) DO UPDATE SET name = excluded.name, model = excluded.model, serial = excluded.serial,
		  status = excluded.status, data = excluded.data, last_seen = excluded.last_seen`,
		serverID, d.Key, d.Name, d.Model, d.Serial, d.Status, string(d.Data), d.LastSeen, d.LastSeen)
	if err != nil || p == nil {
		return err
	}
	return s.insertDriveSample(ctx, "INSERT OR REPLACE", serverID, d.Key, *p)
}

func (s *Store) insertDriveSample(ctx context.Context, verb string, serverID int64, key string, p DriveSample) error {
	attrs := ""
	if len(p.Attrs) > 0 {
		b, _ := json.Marshal(p.Attrs)
		attrs = string(b)
	}
	_, err := s.DB.ExecContext(ctx, verb+` INTO drive_samples (server_id, key, ts, status, temp, hours, wear, spare, errors, written, read_b, attrs)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, serverID, key, p.T/3600*3600, p.Status, p.Temp, p.Hours, p.Wear, p.Spare, p.Errors, p.Written, p.Read, attrs)
	return err
}

// ImportDriveSample adds an older point (a Scrutiny import) without
// replacing one that is already there.
func (s *Store) ImportDriveSample(ctx context.Context, serverID int64, key string, p DriveSample) (bool, error) {
	var n int
	_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(1) FROM drive_samples WHERE server_id = ? AND key = ? AND ts = ?`, serverID, key, p.T/3600*3600).Scan(&n)
	if n > 0 {
		return false, nil
	}
	return true, s.insertDriveSample(ctx, "INSERT OR IGNORE", serverID, key, p)
}

const driveCols = `server_id, key, name, model, serial, status, data, first_seen, last_seen`

func scanDrive(row interface{ Scan(...any) error }) (*DriveRow, error) {
	var d DriveRow
	var data string
	if err := row.Scan(&d.ServerID, &d.Key, &d.Name, &d.Model, &d.Serial, &d.Status, &data, &d.FirstSeen, &d.LastSeen); err != nil {
		return nil, notFound(err)
	}
	if data == "" {
		data = "null"
	}
	d.Data = json.RawMessage(data)
	return &d, nil
}

// Drives lists the drives of the given servers.
func (s *Store) Drives(ctx context.Context, serverIDs []int64) ([]DriveRow, error) {
	out := []DriveRow{}
	if len(serverIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(serverIDs))
	for i, id := range serverIDs {
		args[i] = id
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+driveCols+` FROM drives WHERE server_id IN (?`+strings.Repeat(",?", len(serverIDs)-1)+`) ORDER BY server_id, name, key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		d, err := scanDrive(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// GetDrive returns one drive of a server.
func (s *Store) GetDrive(ctx context.Context, serverID int64, key string) (*DriveRow, error) {
	return scanDrive(s.DB.QueryRowContext(ctx, `SELECT `+driveCols+` FROM drives WHERE server_id = ? AND key = ?`, serverID, key))
}

// DriveBySerial finds a drive among the given servers by its serial number.
func (s *Store) DriveBySerial(ctx context.Context, serverIDs []int64, serial string) *DriveRow {
	list, err := s.Drives(ctx, serverIDs)
	if err != nil {
		return nil
	}
	for i := range list {
		if strings.EqualFold(strings.TrimSpace(list[i].Serial), strings.TrimSpace(serial)) {
			return &list[i]
		}
	}
	return nil
}

// DriveSeen is what the alert engine knows about a drive between readings.
type DriveSeen struct {
	Label    string
	LastSeen int64
}

// DrivesSeen maps a server's drives to their label and newest reading.
func (s *Store) DrivesSeen(ctx context.Context, serverID int64) map[string]DriveSeen {
	out := map[string]DriveSeen{}
	rows, err := s.DB.QueryContext(ctx, `SELECT key, model, name, last_seen FROM drives WHERE server_id = ?`, serverID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var key, model, name string
		var seen int64
		if rows.Scan(&key, &model, &name, &seen) == nil {
			out[key] = DriveSeen{Label: DriveLabel(model, name), LastSeen: seen}
		}
	}
	return out
}

// DriveLabel names a drive in alerts: "ST12000VN0008 (sda)".
func DriveLabel(model, name string) string {
	name = strings.TrimPrefix(name, "/dev/")
	switch {
	case model == "":
		return name
	case name == "":
		return model
	}
	return model + " (" + name + ")"
}

// DeleteDrive forgets a drive and its history.
func (s *Store) DeleteDrive(ctx context.Context, serverID int64, key string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM drives WHERE server_id = ? AND key = ?`, serverID, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound(sql.ErrNoRows)
	}
	_, err = s.DB.ExecContext(ctx, `DELETE FROM drive_samples WHERE server_id = ? AND key = ?`, serverID, key)
	return err
}

// DriveHistory returns a drive's points since from (unix s), oldest first.
func (s *Store) DriveHistory(ctx context.Context, serverID int64, key string, from int64) ([]DriveSample, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT ts, status, temp, hours, wear, spare, errors, written, read_b, attrs FROM drive_samples
		WHERE server_id = ? AND key = ? AND ts >= ? ORDER BY ts`, serverID, key, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DriveSample{}
	for rows.Next() {
		var p DriveSample
		var attrs string
		if err := rows.Scan(&p.T, &p.Status, &p.Temp, &p.Hours, &p.Wear, &p.Spare, &p.Errors, &p.Written, &p.Read, &attrs); err != nil {
			return nil, err
		}
		if attrs != "" {
			_ = json.Unmarshal([]byte(attrs), &p.Attrs)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PruneDriveSamples thins history older than DriveHourlyDays to the last
// point of each day.
func (s *Store) PruneDriveSamples(ctx context.Context) {
	cut := time.Now().AddDate(0, 0, -DriveHourlyDays).Unix()
	_, err := s.DB.ExecContext(ctx, `DELETE FROM drive_samples WHERE ts < ? AND ts NOT IN (
		SELECT MAX(d.ts) FROM drive_samples d WHERE d.server_id = drive_samples.server_id AND d.key = drive_samples.key AND d.ts < ? GROUP BY d.ts / 86400)`, cut, cut)
	if err != nil {
		s.log.Warn("prune drive history", "err", err)
	}
}

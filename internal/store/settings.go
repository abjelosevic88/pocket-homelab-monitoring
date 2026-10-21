package store

import (
	"context"
	"database/sql"
	"strconv"
)

// DefaultTitle is the app title until someone changes it.
const DefaultTitle = "Pocket Monitoring"

// Settings are the box settings plus one person's preferences.
type Settings struct {
	Title         string `json:"title"`
	SetupComplete bool   `json:"setup_complete"`

	TimeFormat   string `json:"time_format"`   // 24h | 12h
	DefaultRange string `json:"default_range"` // live | 1h | 6h | 24h | 7d | 30d | 90d
	Density      string `json:"density"`       // comfortable | compact
	TempUnit     string `json:"temp_unit"`     // C | F

	NotifyKind     string `json:"notify_kind"` // apprise | ntfy | webhook
	NotifyURL      string `json:"notify_url"`
	NotifyToken    string `json:"-"`
	NotifyAlerts   bool   `json:"notify_alerts"`
	NotifyResolved bool   `json:"notify_resolved"`
}

func defaults() Settings {
	return Settings{
		Title: DefaultTitle, TimeFormat: "24h", DefaultRange: "1h", Density: "comfortable", TempUnit: "C",
		NotifyKind: "apprise", NotifyAlerts: true, NotifyResolved: true,
	}
}

// GetSettings loads the box settings plus the preferences of userID, with
// defaults applied. userID 0 gives the box settings with default preferences.
func (s *Store) GetSettings(ctx context.Context, userID int64) (Settings, error) {
	st := defaults()
	rows, err := s.DB.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN ('title', 'setup_complete')
		UNION ALL SELECT key, value FROM user_settings WHERE user_id = ?`, userID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return st, err
		}
		switch k {
		case "title":
			if v != "" {
				st.Title = v
			}
		case "setup_complete":
			st.SetupComplete = v == "1"
		case "time_format":
			st.TimeFormat = v
		case "default_range":
			st.DefaultRange = v
		case "density":
			st.Density = v
		case "temp_unit":
			st.TempUnit = v
		case "notify_kind":
			st.NotifyKind = v
		case "notify_url":
			st.NotifyURL = v
		case "notify_token":
			st.NotifyToken = v
		case "notify_alerts":
			st.NotifyAlerts = v != "0"
		case "notify_resolved":
			st.NotifyResolved = v != "0"
		}
	}
	return st, rows.Err()
}

func b2s(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func itoa(n int) string { return strconv.Itoa(n) }

// SaveSettings stores a person's preferences (not the notify token).
func (s *Store) SaveSettings(ctx context.Context, userID int64, st Settings) error {
	return s.putUserSettings(ctx, userID, map[string]string{
		"time_format": st.TimeFormat, "default_range": st.DefaultRange, "density": st.Density, "temp_unit": st.TempUnit,
		"notify_kind": st.NotifyKind, "notify_url": st.NotifyURL, "notify_alerts": b2s(st.NotifyAlerts), "notify_resolved": b2s(st.NotifyResolved),
	})
}

// UserSetting reads one raw key of a person's settings ("" when missing).
func (s *Store) UserSetting(ctx context.Context, userID int64, key string) string {
	var v string
	_ = s.DB.QueryRowContext(ctx, `SELECT value FROM user_settings WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	return v
}

// SetUserSetting stores one key for a person.
func (s *Store) SetUserSetting(ctx context.Context, userID int64, key, value string) error {
	return s.putUserSettings(ctx, userID, map[string]string{key: value})
}

// SaveBoxSettings stores the settings of the box itself: title and setup state.
func (s *Store) SaveBoxSettings(ctx context.Context, st Settings) error {
	kv := map[string]string{"title": st.Title, "setup_complete": b2s(st.SetupComplete)}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) putUserSettings(ctx context.Context, userID int64, kv map[string]string) error {
	if userID <= 0 {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?)
			ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`, userID, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OwnerID is the account the box was set up with (the first user): keys
// given to the whole box through the environment are theirs.
func (s *Store) OwnerID(ctx context.Context) int64 {
	var id sql.NullInt64
	_ = s.DB.QueryRowContext(ctx, `SELECT MIN(id) FROM users`).Scan(&id)
	return id.Int64
}

// UserIDs lists every user id.
func (s *Store) UserIDs(ctx context.Context) []int64 {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM users ORDER BY id`)
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

// BoxSetting reads one raw key of the box settings ("" when missing).
func (s *Store) BoxSetting(ctx context.Context, key string) string {
	var v string
	_ = s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

// SetBoxSetting stores one key of the box settings.
func (s *Store) SetBoxSetting(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

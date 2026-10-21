package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/notify"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	st, err := s.store.GetSettings(ctx, u.ID)
	if err != nil {
		s.fail(w, err, "settings")
		return
	}
	writeJSON(w, http.StatusOK, s.publicSettings(ctx, u, st))
}

func (s *Server) publicSettings(ctx context.Context, u *store.User, st store.Settings) map[string]any {
	return map[string]any{
		"settings":         st,
		"notify_has_token": st.NotifyToken != "",
		"notify_box_route": s.cfg.NotifyURL != "" && s.isOwner(ctx, u.ID),
		"local_collector":  s.cfg.LocalCollector,
		"core":             s.coreStatus(),
	}
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	st, err := s.store.GetSettings(ctx, u.ID)
	if err != nil {
		s.fail(w, err, "settings")
		return
	}
	var in struct {
		Title          *string `json:"title"`
		TimeFormat     *string `json:"time_format"`
		DefaultRange   *string `json:"default_range"`
		Density        *string `json:"density"`
		TempUnit       *string `json:"temp_unit"`
		NotifyKind     *string `json:"notify_kind"`
		NotifyURL      *string `json:"notify_url"`
		NotifyToken    *string `json:"notify_token"`
		NotifyAlerts   *bool   `json:"notify_alerts"`
		NotifyResolved *bool   `json:"notify_resolved"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Title != nil && u.Role == "admin" && strings.TrimSpace(*in.Title) != "" {
		box, _ := s.store.GetSettings(ctx, 0)
		box.Title = strings.TrimSpace(*in.Title)
		_ = s.store.SaveBoxSettings(ctx, box)
		st.Title = box.Title
	}
	if in.TimeFormat != nil && (*in.TimeFormat == "24h" || *in.TimeFormat == "12h") {
		st.TimeFormat = *in.TimeFormat
	}
	if in.DefaultRange != nil {
		switch *in.DefaultRange {
		case "live", "1h", "6h", "24h", "7d", "30d", "90d":
			st.DefaultRange = *in.DefaultRange
		}
	}
	if in.Density != nil && (*in.Density == "comfortable" || *in.Density == "compact") {
		st.Density = *in.Density
	}
	if in.TempUnit != nil && (*in.TempUnit == "C" || *in.TempUnit == "F") {
		st.TempUnit = *in.TempUnit
	}
	if in.NotifyKind != nil {
		switch *in.NotifyKind {
		case "apprise", "ntfy", "webhook":
			st.NotifyKind = *in.NotifyKind
		}
	}
	if in.NotifyURL != nil {
		st.NotifyURL = strings.TrimSpace(*in.NotifyURL)
	}
	if in.NotifyAlerts != nil {
		st.NotifyAlerts = *in.NotifyAlerts
	}
	if in.NotifyResolved != nil {
		st.NotifyResolved = *in.NotifyResolved
	}
	if err := s.store.SaveSettings(ctx, u.ID, st); err != nil {
		s.fail(w, err, "save settings")
		return
	}
	if in.NotifyToken != nil {
		_ = s.store.SetUserSetting(ctx, u.ID, "notify_token", strings.TrimSpace(*in.NotifyToken))
	}
	s.touch()
	st, _ = s.store.GetSettings(ctx, u.ID)
	writeJSON(w, http.StatusOK, s.publicSettings(ctx, u, st))
}

// notifyRoute is where a person's notifications go: their own route, else
// (for the owner) the box route from the environment.
func (s *Server) notifyRoute(ctx context.Context, uid int64, st store.Settings) (kind, url, token string) {
	if st.NotifyURL != "" {
		return st.NotifyKind, st.NotifyURL, st.NotifyToken
	}
	if s.cfg.NotifyURL != "" && s.isOwner(ctx, uid) {
		return s.cfg.NotifyKind, s.cfg.NotifyURL, ""
	}
	return "", "", ""
}

// notifyUser sends one message to a person when they want that kind
// ("alerts" or "resolved").
func (s *Server) notifyUser(ctx context.Context, uid int64, what, title, body, link string) {
	st, err := s.store.GetSettings(ctx, uid)
	if err != nil {
		return
	}
	if (what == "alerts" && !st.NotifyAlerts) || (what == "resolved" && !st.NotifyResolved) {
		return
	}
	kind, url, token := s.notifyRoute(ctx, uid, st)
	if url == "" {
		return
	}
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := notify.Send(c, kind, url, token, title, body, link); err != nil {
			s.log.Warn("notification failed", "user", uid, "what", what, "err", err)
		}
	}()
}

func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	st, _ := s.store.GetSettings(ctx, u.ID)
	kind, url, token := s.notifyRoute(ctx, u.ID, st)
	if url == "" {
		writeErr(w, http.StatusBadRequest, "set a notification address first")
		return
	}
	if err := notify.Send(ctx, kind, url, token, "Pocket Monitoring", "Notifications work. Alerts will look like this.", s.cfg.BaseURL); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	info := map[string]any{
		"version": Version, "go": runtime.Version(), "uptime": time.Since(s.started).Round(time.Second).String(),
		"counts": s.store.UserCounts(ctx, u.ID), "core": s.coreStatus(), "auth_mode": s.cfg.AuthMode,
		"base_url": s.cfg.BaseURL, "timezone": s.cfg.Timezone, "local_collector": s.cfg.LocalCollector,
		"retention": map[string]int{"minute_days": store.MinuteDays, "rollup_days": store.RollupDays},
	}
	if u.Role == "admin" {
		if fi, err := os.Stat(s.cfg.DBPath); err == nil {
			info["db_size"] = fi.Size()
		}
		info["users"], _ = s.store.CountUsers(ctx)
		if s.mon.local != nil {
			info["host_root"] = s.mon.local.P.Root
			info["docker"] = !s.mon.local.Docker.Missing()
		}
	}
	writeJSON(w, http.StatusOK, info)
}

// handleBackup streams a consistent copy of the whole database; only an
// admin, and only while they are the only person on the box.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := userFrom(ctx)
	if u.Role != "admin" {
		writeErr(w, http.StatusForbidden, "only an admin can download the database")
		return
	}
	if n, _ := s.store.CountUsers(ctx); n > 1 {
		writeErr(w, http.StatusForbidden, "the database holds other people's servers; back it up on the server (pocket-monitoring backup)")
		return
	}
	tmp := fmt.Sprintf("%s/backups/download-%d.db", s.cfg.DataDir, time.Now().UnixNano())
	if err := s.store.Backup(ctx, tmp); err != nil {
		s.fail(w, err, "backup")
		return
	}
	defer os.Remove(tmp)
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="pocket-monitoring-`+time.Now().Format("20060102")+`.db"`)
	http.ServeFile(w, r, tmp)
}

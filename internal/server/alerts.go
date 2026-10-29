package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

func (s *Server) alertRoutes(r chi.Router) {
	r.Get("/rules", s.handleListRules)
	r.Post("/rules", s.handleCreateRule)
	r.Put("/rules/{id}", s.handleUpdateRule)
	r.Delete("/rules/{id}", s.handleDeleteRule)
	r.Get("/alerts", s.handleListAlerts)
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

var metrics = map[string]bool{"cpu": true, "mem": true, "swap": true, "load": true, "disk": true, "temp": true, "offline": true, "container": true, "raid": true, "job": true, "speedtest": true, "speed_down": true, "speed_up": true, "speed_ping": true}

func (s *Server) ownedServerIDs(ctx context.Context, uid int64) []int64 {
	list, _ := s.store.VisibleServers(ctx, uid)
	ids := []int64{}
	for _, sv := range list {
		if sv.UserID == uid {
			ids = append(ids, sv.ID)
		}
	}
	return ids
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ids := s.ownedServerIDs(ctx, me(r))
	if id := queryInt64(r, "server_id"); id > 0 {
		keep := []int64{}
		for _, x := range ids {
			if x == id {
				keep = append(keep, x)
			}
		}
		ids = keep
	}
	out, err := s.store.ListRules(ctx, ids)
	if err != nil {
		s.fail(w, err, "list rules")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type ruleInput struct {
	ServerID  int64    `json:"server_id"`
	Metric    *string  `json:"metric"`
	Target    *string  `json:"target"`
	Threshold *float64 `json:"threshold"`
	ForS      *int     `json:"for_s"`
	Severity  *string  `json:"severity"`
	Enabled   *bool    `json:"enabled"`
}

func applyRule(r *store.Rule, in ruleInput) error {
	if in.Metric != nil {
		if !metrics[*in.Metric] {
			return badRequest("unknown metric")
		}
		r.Metric = *in.Metric
	}
	if in.Target != nil {
		r.Target = strings.TrimSpace(*in.Target)
	}
	if in.Threshold != nil {
		if math.IsNaN(*in.Threshold) || *in.Threshold < 0 || *in.Threshold > 10000 {
			return badRequest("the limit is out of range")
		}
		r.Threshold = *in.Threshold
	}
	if in.ForS != nil {
		if *in.ForS < 0 || *in.ForS > 7*86400 {
			return badRequest("“for” is between 0 seconds and a week")
		}
		r.ForS = *in.ForS
	}
	if in.Severity != nil {
		if *in.Severity != "warning" && *in.Severity != "critical" {
			return badRequest("severity is warning or critical")
		}
		r.Severity = *in.Severity
	}
	if in.Enabled != nil {
		r.Enabled = *in.Enabled
	}
	switch r.Metric {
	case "cpu", "mem", "swap", "disk":
		if r.Threshold <= 0 || r.Threshold > 100 {
			return badRequest("the limit is a percentage between 1 and 100")
		}
	case "temp":
		if r.Threshold <= 0 || r.Threshold > 150 {
			return badRequest("the limit is a temperature in °C")
		}
	case "load":
		if r.Threshold <= 0 {
			return badRequest("the limit is a load per thread, like 1.5")
		}
	case "speed_down", "speed_up":
		if r.Threshold <= 0 {
			return badRequest("the limit is a speed in Mbps")
		}
	case "speed_ping":
		if r.Threshold <= 0 {
			return badRequest("the limit is a ping in milliseconds")
		}
	}
	// speed rules count tests in a row, not seconds
	if isSpeedMetric(r.Metric) && (r.ForS < 1 || r.ForS > 10) {
		r.ForS = min(max(r.ForS, 1), 10)
	}
	if r.Metric == "" {
		return badRequest("pick a metric")
	}
	return nil
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in ruleInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	sv, role, err := s.store.ServerFor(ctx, me(r), in.ServerID)
	if err != nil || role != "owner" {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	rule := &store.Rule{ServerID: sv.ID, Severity: "warning", Enabled: true}
	if err := applyRule(rule, in); err != nil {
		s.fail(w, err, "add rule")
		return
	}
	if err := s.store.SaveRule(ctx, rule); err != nil {
		s.fail(w, err, "add rule")
		return
	}
	s.dropRules()
	writeJSON(w, http.StatusCreated, rule)
}

// ownRule loads a rule of a server the caller owns.
func (s *Server) ownRule(w http.ResponseWriter, r *http.Request) *store.Rule {
	rule, err := s.store.GetRule(r.Context(), idParam(r, "id"))
	if err == nil {
		if _, role, e := s.store.ServerFor(r.Context(), me(r), rule.ServerID); e != nil || role != "owner" {
			err = store.ErrNotFound
		}
	}
	if err != nil {
		s.fail(w, err, "get rule")
		return nil
	}
	return rule
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	rule := s.ownRule(w, r)
	if rule == nil {
		return
	}
	var in ruleInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := applyRule(rule, in); err != nil {
		s.fail(w, err, "update rule")
		return
	}
	if err := s.store.SaveRule(r.Context(), rule); err != nil {
		s.fail(w, err, "update rule")
		return
	}
	s.dropRules()
	writeJSON(w, http.StatusOK, rule)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	rule := s.ownRule(w, r)
	if rule == nil {
		return
	}
	if sv, err := s.store.GetServer(r.Context(), rule.ServerID); err == nil {
		s.resolveRule(r.Context(), *sv, *rule, nil, time.Now())
	}
	if err := s.store.DeleteRule(r.Context(), rule.ID); err != nil {
		s.fail(w, err, "delete rule")
		return
	}
	s.dropRules()
	s.touch()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := s.store.VisibleServers(ctx, me(r))
	if err != nil {
		s.fail(w, err, "alerts")
		return
	}
	ids := []int64{}
	want := queryInt64(r, "server_id")
	for _, sv := range list {
		if want == 0 || sv.ID == want {
			ids = append(ids, sv.ID)
		}
	}
	out, err := s.store.ListAlerts(ctx, ids, r.URL.Query().Get("state") != "all", queryInt(r, "limit"))
	if err != nil {
		s.fail(w, err, "alerts")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// forgetAlerts drops the engine's state for a deleted server.
func (s *Server) forgetAlerts(serverID int64) {
	e := s.alerts
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, a := range e.open {
		if a.ServerID == serverID {
			delete(e.open, k)
		}
	}
	for k := range e.pending {
		delete(e.pending, k) // cheap to rebuild
	}
}

// ---- delivery ----

func (s *Server) alertFired(ctx context.Context, sv store.Server, a *store.Alert) {
	s.log.Info("alert", "server", sv.Name, "metric", a.Metric, "target", a.Target, "message", a.Message)
	s.touch()
	icon := "⚠️"
	if a.Severity == "critical" {
		icon = "🔴"
	}
	title := fmt.Sprintf("%s %s: %s", icon, sv.Name, metricName(a.Metric))
	s.notifyWatchers(ctx, sv, "alerts", title, a.Message, s.alertLink(sv.ID, a.Metric))
	s.publish(ctx, "monitoring.alert", &sv, map[string]any{"alert_id": a.ID, "metric": a.Metric, "target": a.Target, "severity": a.Severity, "message": a.Message, "value": a.Value})
}

func (s *Server) alertResolved(ctx context.Context, sv store.Server, a *store.Alert, value float64) {
	s.touch()
	took := ""
	if t := store.ParseTime(a.StartedAt); !t.IsZero() {
		took = " after " + fmtDur(time.Since(t))
	}
	body := ""
	switch a.Metric {
	case "offline":
		body = fmt.Sprintf("%s answers again%s.", sv.Name, took)
	case "container":
		body = fmt.Sprintf("Container %s is fine again%s.", a.Target, took)
	case "raid":
		body = fmt.Sprintf("RAID array %s is healthy again%s.", a.Target, took)
	case "speedtest":
		body = fmt.Sprintf("Speed tests on %s pass again%s.", sv.Name, took)
	case "job":
		if a.Target == "" {
			body = fmt.Sprintf("Scheduled jobs on %s can be read again%s.", sv.Name, took)
		} else {
			body = fmt.Sprintf("Job %s is fine again%s.", strings.TrimSuffix(a.Target, ".timer"), took)
		}
	default:
		what := metricName(a.Metric)
		switch a.Metric {
		case "disk":
			what = "Disk " + a.Target
		case "temp":
			if i := strings.Index(a.Message, " at "); i > 0 {
				what = a.Message[:i] // the sensor's label
			}
		}
		if math.IsNaN(value) {
			body = fmt.Sprintf("%s alert cleared%s.", what, took)
		} else {
			body = fmt.Sprintf("%s back to %s%s.", what, fmtValue(a.Metric, value), took)
		}
	}
	s.log.Info("alert resolved", "server", sv.Name, "metric", a.Metric, "target", a.Target)
	s.notifyWatchers(ctx, sv, "resolved", "✅ "+sv.Name+": "+metricName(a.Metric)+" OK", body, s.alertLink(sv.ID, a.Metric))
	s.publish(ctx, "monitoring.resolved", &sv, map[string]any{"alert_id": a.ID, "metric": a.Metric, "target": a.Target, "message": body})
}

// alertLink opens the server on the tab the alert is about.
func (s *Server) alertLink(id int64, metric string) string {
	l := s.serverLink(id)
	switch {
	case l == "":
	case isSpeedMetric(metric):
		l += "?tab=internet"
	case metric == "job":
		l += "?tab=jobs"
	}
	return l
}

func (s *Server) serverLink(id int64) string {
	if s.cfg.BaseURL == "" {
		return ""
	}
	return s.cfg.BaseURL + "/server/" + itoa64(id)
}

// notifyWatchers tells the owner and everyone the server is shared with.
func (s *Server) notifyWatchers(ctx context.Context, sv store.Server, what, title, body, link string) {
	for _, uid := range s.store.Watchers(ctx, sv.ID) {
		s.notifyUser(ctx, uid, what, title, body, link)
	}
}

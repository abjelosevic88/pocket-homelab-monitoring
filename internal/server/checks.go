package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/probe"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// Uptime checks: HTTP(S) (status code, optional keyword, certificate
// expiry), TCP ports, DNS lookups, and push heartbeats that scripts send to
// /api/push/<token>. They run from the machine the hub runs on and belong to
// its "local" server, like the speed tests. A failing check is tried again
// after retry_s and only counts as down after `retries` failures in a row
// (Uptime Kuma's "pending"). The alert rules "check" (any check down) and
// "cert" (a certificate expiring within N days) cover them.
//
// For a drop-in Uptime Kuma replacement the hub also answers Kuma's push URL,
// its status page JSON (Homepage's uptimekuma widget) and its monitor_status
// lines on /metrics.

func (s *Server) checkRoutes(r chi.Router) {
	r.Get("/servers/{id}/checks", s.handleChecks)
	r.Post("/servers/{id}/checks", s.handleCreateCheck)
	r.Get("/servers/{id}/checks/events", s.handleCheckEvents)
	r.Post("/servers/{id}/checks/import", s.handleImportKuma)
	r.Put("/servers/{id}/status-page", s.handleSaveStatusPage)
	r.Get("/servers/{id}/checks/{cid}/history", s.handleCheckHistory)
	r.Put("/servers/{id}/checks/{cid}", s.handleUpdateCheck)
	r.Delete("/servers/{id}/checks/{cid}", s.handleDeleteCheck)
	r.Post("/servers/{id}/checks/{cid}/run", s.handleRunCheck)
	r.Post("/servers/{id}/checks/{cid}/token", s.handleNewPushToken)
}

// kumaRoutes are public, like Uptime Kuma's own.
func (s *Server) kumaRoutes(r chi.Router) {
	r.Get("/api/push/{token}", s.handlePush)
	r.Post("/api/push/{token}", s.handlePush)
	r.Get("/api/status-page/{slug}", s.handleStatusPageJSON)
	r.Get("/api/status-page/heartbeat/{slug}", s.handleStatusHeartbeats)
}

const checkConcurrency = 16

type checkHub struct {
	mu      sync.Mutex
	checks  map[int64]*store.Check
	next    map[int64]time.Time
	running map[int64]bool
	dirty   bool
	loaded  time.Time
	started time.Time
	sem     chan struct{}
}

func newCheckHub() *checkHub {
	return &checkHub{checks: map[int64]*store.Check{}, next: map[int64]time.Time{}, running: map[int64]bool{}, dirty: true, started: time.Now(), sem: make(chan struct{}, checkConcurrency)}
}

// reloadChecks makes the runner read the checks again on its next tick.
func (s *Server) reloadChecks() {
	s.checks.mu.Lock()
	s.checks.dirty = true
	s.checks.mu.Unlock()
}

// checksReason says why a server can't have uptime checks ("" = it can).
func checksReason(sv store.Server) string {
	if sv.Kind != "local" {
		return "Uptime checks run from the machine Pocket Monitoring itself runs on, so they live on that server's Uptime tab."
	}
	return ""
}

func (s *Server) userAgent() string {
	return "PocketMonitoring/" + Version + " (uptime check)"
}

// ---------- the runner ----------

func (s *Server) checksLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checksTick(ctx)
		}
	}
}

func (s *Server) loadChecks(ctx context.Context) {
	h := s.checks
	list, err := s.store.AllChecks(ctx)
	if err != nil {
		s.log.Warn("load checks", "err", err)
		return
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	fresh := make(map[int64]*store.Check, len(list))
	for i := range list {
		c := list[i]
		if old := h.checks[c.ID]; old != nil {
			// the runner's copy is newer than the row it saved a moment ago
			c.Status, c.Fails, c.LastAt, c.LastMs, c.LastMsg, c.ChangedAt, c.CertExpires = old.Status, old.Fails, old.LastAt, old.LastMs, old.LastMsg, old.ChangedAt, old.CertExpires
			if old.IntervalS != c.IntervalS || old.Kind != c.Kind || !old.Enabled {
				delete(h.next, c.ID)
			}
		}
		fresh[c.ID] = &c
		if _, ok := h.next[c.ID]; !ok {
			h.next[c.ID] = firstDue(c, now, h.started)
		}
	}
	for id := range h.next {
		if fresh[id] == nil {
			delete(h.next, id)
		}
	}
	h.checks, h.dirty, h.loaded = fresh, false, now
}

// firstDue spreads checks over their first interval and gives push checks
// their full window (and a restarted hub a minute) before a miss counts.
func firstDue(c store.Check, now, started time.Time) time.Time {
	iv := time.Duration(max(c.IntervalS, 20)) * time.Second
	if c.Kind == "push" {
		due := time.Unix(c.LastAt, 0).Add(iv)
		if c.LastAt == 0 {
			due = now.Add(iv)
		}
		if grace := started.Add(time.Minute); due.Before(grace) {
			due = grace
		}
		if due.Before(now.Add(5 * time.Second)) {
			due = now.Add(5 * time.Second)
		}
		return due
	}
	spread := time.Duration(c.ID%int64(min(c.IntervalS, 30))) * time.Second
	if c.LastAt > 0 {
		if due := time.Unix(c.LastAt, 0).Add(iv); due.After(now) {
			return due
		}
	}
	return now.Add(2*time.Second + spread)
}

func (s *Server) checksTick(ctx context.Context) {
	h := s.checks
	h.mu.Lock()
	reload := h.dirty || time.Since(h.loaded) > 5*time.Minute
	h.mu.Unlock()
	if reload {
		s.loadChecks(ctx)
	}
	now := time.Now()
	h.mu.Lock()
	var due []store.Check
	for id, c := range h.checks {
		if !c.Enabled || h.running[id] || now.Before(h.next[id]) {
			continue
		}
		h.running[id] = true
		due = append(due, *c)
	}
	h.mu.Unlock()
	for _, c := range due {
		go func(c store.Check) {
			defer func() {
				h.mu.Lock()
				delete(h.running, c.ID)
				h.mu.Unlock()
			}()
			if c.Kind == "push" {
				s.recordCheck(ctx, c.ID, false, nil, fmt.Sprintf("No push received in %s", fmtDur(time.Duration(c.IntervalS)*time.Second)), time.Time{}, false)
				return
			}
			select {
			case h.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			res := probe.Run(ctx, s.probeSpec(c))
			<-h.sem
			if ctx.Err() != nil {
				return
			}
			s.recordCheck(ctx, c.ID, res.OK, &res.Ms, res.Msg, res.CertExpires, false)
		}(c)
	}
}

func (s *Server) probeSpec(c store.Check) probe.Spec {
	to := time.Duration(c.TimeoutS) * time.Second
	if to <= 0 {
		to = 30 * time.Second
	}
	return probe.Spec{Kind: c.Kind, Target: c.Target, Port: c.Port, Method: c.Method, Keyword: c.Keyword, InvertKeyword: c.InvertKeyword,
		StatusCodes: c.StatusCodes, IgnoreTLS: c.IgnoreTLS, FollowRedirects: c.FollowRedirects, DNSServer: c.DNSServer, DNSType: c.DNSType,
		Timeout: to, UserAgent: s.userAgent()}
}

// nextStatus is where a check stands after a result.
func nextStatus(prev, fails, retries int, ok bool) (status, newFails int) {
	if ok {
		return store.CheckUp, 0
	}
	fails++
	if fails > retries || prev == store.CheckDown {
		return store.CheckDown, fails
	}
	return store.CheckPending, fails
}

// recordCheck applies one result: state, the beat, an event when it flipped,
// the next run and the alert rules.
func (s *Server) recordCheck(ctx context.Context, id int64, ok bool, ms *float64, msg string, cert time.Time, push bool) *store.Check {
	h := s.checks
	now := time.Now()
	h.mu.Lock()
	c := h.checks[id]
	if c == nil {
		h.mu.Unlock()
		return nil
	}
	prev := c.Status
	c.Status, c.Fails = nextStatus(prev, c.Fails, c.Retries, ok)
	flipped := (c.Status == store.CheckDown && prev != store.CheckDown) || (c.Status == store.CheckUp && prev == store.CheckDown)
	if flipped || c.ChangedAt == 0 && c.Status != store.CheckPending {
		c.ChangedAt = now.Unix()
	}
	c.LastAt, c.LastMsg = now.Unix(), msg
	if ms != nil {
		v := math.Round(*ms*10) / 10
		c.LastMs = &v
	} else if !push {
		c.LastMs = nil
	}
	if !cert.IsZero() {
		c.CertExpires = cert.Unix()
	}
	wait := c.IntervalS
	if c.Status == store.CheckPending && c.RetryS > 0 {
		wait = c.RetryS
	}
	h.next[id] = now.Add(time.Duration(max(wait, 1)) * time.Second)
	snap := *c
	h.mu.Unlock()

	b := store.Beat{T: now.Unix(), Status: snap.Status, Msg: msg}
	if ms != nil {
		b.Ms = snap.LastMs
	}
	if err := s.store.AddBeat(ctx, id, b, flipped, s.loc()); err != nil {
		s.log.Warn("store check result", "check", snap.Name, "err", err)
	}
	if err := s.store.SaveCheckState(ctx, &snap); err != nil {
		s.log.Warn("store check state", "check", snap.Name, "err", err)
	}
	if flipped {
		s.log.Info("check", "name", snap.Name, "status", statusWord(snap.Status), "msg", msg)
		s.touch()
	}
	if sv, err := s.store.GetServer(ctx, snap.ServerID); err == nil {
		s.evaluateChecks(ctx, *sv)
	}
	return &snap
}

func statusWord(st int) string {
	switch st {
	case store.CheckDown:
		return "down"
	case store.CheckUp:
		return "up"
	case store.CheckPending:
		return "pending"
	}
	return "maintenance"
}

// ---------- alerts ----------

func isCheckMetric(m string) bool { return m == "check" || m == "cert" }

func (s *Server) checksOf(serverID int64) []store.Check {
	h := s.checks
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []store.Check{}
	for _, c := range h.checks {
		if c.ServerID == serverID {
			out = append(out, *c)
		}
	}
	return out
}

func (s *Server) checkName(id string) string {
	n, _ := strconv.ParseInt(id, 10, 64)
	h := s.checks
	h.mu.Lock()
	defer h.mu.Unlock()
	if c := h.checks[n]; c != nil {
		return c.Name
	}
	return "Check " + id
}

func certHost(c store.Check) string {
	u, err := url.Parse(c.Target)
	if err != nil {
		return c.Target
	}
	return u.Hostname()
}

func checkConditions(r store.Rule, checks []store.Check, now time.Time) []condition {
	out := []condition{}
	for _, c := range checks {
		inst := strconv.FormatInt(c.ID, 10)
		if !c.Enabled || !c.Notify || (r.Target != "" && r.Target != inst) {
			continue
		}
		switch r.Metric {
		case "check":
			if c.LastAt == 0 {
				continue
			}
			label := c.Name + " is down"
			if c.LastMsg != "" {
				label += ": " + c.LastMsg
			}
			out = append(out, condition{inst: inst, label: label, fire: c.Status == store.CheckDown, clear: c.Status == store.CheckUp})
		case "cert":
			if c.Kind != "http" || c.IgnoreTLS || c.CertExpires == 0 || !strings.HasPrefix(strings.ToLower(c.Target), "https:") {
				continue
			}
			left := time.Unix(c.CertExpires, 0).Sub(now)
			days := math.Floor(left.Hours() / 24)
			label := fmt.Sprintf("The certificate of %s expires in %s (%s)", certHost(c), plural(int(days), "day"), time.Unix(c.CertExpires, 0).Format("Jan 2"))
			if left <= 0 {
				label = fmt.Sprintf("The certificate of %s has expired", certHost(c))
			}
			out = append(out, condition{inst: inst, label: label, value: days, fire: days < r.Threshold, clear: days >= r.Threshold})
		}
	}
	return out
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + one + "s"
}

// evaluateChecks runs a server's check rules against where its checks stand.
func (s *Server) evaluateChecks(ctx context.Context, sv store.Server) {
	checks := s.checksOf(sv.ID)
	now := time.Now()
	for _, r := range s.rulesFor(ctx, sv.ID) {
		if !isCheckMetric(r.Metric) {
			continue
		}
		if !r.Enabled {
			s.resolveRule(ctx, sv, r, nil, now)
			continue
		}
		s.apply(ctx, sv, r, checkConditions(r, checks, now), now, nil)
	}
}

// ---------- push ----------

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.store.CheckByToken(ctx, chi.URLParam(r, "token"))
	if err != nil || !c.Enabled || c.Kind != "push" {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "Monitor not found or not active."})
		return
	}
	q := r.URL.Query()
	ok := !strings.EqualFold(q.Get("status"), "down")
	msg := strings.TrimSpace(q.Get("msg"))
	if msg == "" {
		msg = "OK"
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	var ms *float64
	if p, err := strconv.ParseFloat(q.Get("ping"), 64); err == nil && p >= 0 && !math.IsInf(p, 0) {
		ms = &p
	}
	s.checks.mu.Lock()
	known := s.checks.checks[c.ID] != nil
	s.checks.mu.Unlock()
	if !known {
		s.loadChecks(context.WithoutCancel(ctx))
	}
	s.recordCheck(context.WithoutCancel(ctx), c.ID, ok, ms, msg, time.Time{}, true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func newPushToken() string {
	const abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 24)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(abc))))
		b[i] = abc[n.Int64()]
	}
	return string(b)
}

// ---------- API ----------

type checkView struct {
	store.Check
	Uptime24h *float64     `json:"uptime_24h"`
	Uptime30d *float64     `json:"uptime_30d"`
	Beats     []store.Beat `json:"beats"`
	NextAt    *time.Time   `json:"next_at,omitempty"`
}

func (s *Server) checkServer(w http.ResponseWriter, r *http.Request, ownerOnly bool) (*store.Server, string, bool) {
	sv, role, ok := s.speedServer(w, r, ownerOnly)
	if !ok {
		return nil, "", false
	}
	if ownerOnly {
		if why := checksReason(*sv); why != "" {
			writeErr(w, http.StatusBadRequest, why)
			return nil, "", false
		}
	}
	return sv, role, true
}

func ptrIf(m map[int64]float64, id int64) *float64 {
	if v, ok := m[id]; ok {
		v = math.Round(v*1e5) / 1e5
		return &v
	}
	return nil
}

func (s *Server) handleChecks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sv, role, ok := s.checkServer(w, r, false)
	if !ok {
		return
	}
	list, err := s.store.Checks(ctx, sv.ID)
	if err != nil {
		s.fail(w, err, "checks")
		return
	}
	now := time.Now()
	beats, _ := s.store.RecentBeats(ctx, sv.ID, 60)
	day := s.store.Uptime(ctx, sv.ID, now.Add(-24*time.Hour), s.loc())
	month := s.store.Uptime(ctx, sv.ID, now.AddDate(0, 0, -30), s.loc())
	s.checks.mu.Lock()
	out := make([]checkView, 0, len(list))
	for _, c := range list {
		if live := s.checks.checks[c.ID]; live != nil {
			c.Status, c.Fails, c.LastAt, c.LastMs, c.LastMsg, c.ChangedAt, c.CertExpires = live.Status, live.Fails, live.LastAt, live.LastMs, live.LastMsg, live.ChangedAt, live.CertExpires
		}
		if role != "owner" {
			c.PushToken = ""
		}
		v := checkView{Check: c, Uptime24h: ptrIf(day, c.ID), Uptime30d: ptrIf(month, c.ID), Beats: beats[c.ID]}
		if v.Beats == nil {
			v.Beats = []store.Beat{}
		}
		if n, ok := s.checks.next[c.ID]; ok && c.Enabled {
			n := n
			v.NextAt = &n
		}
		out = append(out, v)
	}
	s.checks.mu.Unlock()
	pushBase := strings.TrimRight(s.cfg.BaseURL, "/") + "/api/push/"
	writeJSON(w, http.StatusOK, map[string]any{
		"available": checksReason(*sv) == "", "reason": checksReason(*sv), "owner": role == "owner",
		"checks": out, "status_page": s.store.GetStatusPage(ctx, sv.ID), "push_base": pushBase, "base_url": s.cfg.BaseURL,
	})
}

type checkInput struct {
	Name            *string `json:"name"`
	Kind            *string `json:"kind"`
	Target          *string `json:"target"`
	Port            *int    `json:"port"`
	Method          *string `json:"method"`
	Keyword         *string `json:"keyword"`
	InvertKeyword   *bool   `json:"invert_keyword"`
	StatusCodes     *string `json:"status_codes"`
	IgnoreTLS       *bool   `json:"ignore_tls"`
	FollowRedirects *bool   `json:"follow_redirects"`
	DNSServer       *string `json:"dns_server"`
	DNSType         *string `json:"dns_type"`
	IntervalS       *int    `json:"interval_s"`
	RetryS          *int    `json:"retry_s"`
	TimeoutS        *int    `json:"timeout_s"`
	Retries         *int    `json:"retries"`
	Enabled         *bool   `json:"enabled"`
	Notify          *bool   `json:"notify"`
	Public          *bool   `json:"public"`
	Group           *string `json:"group"`
	Notes           *string `json:"notes"`
}

func applyCheck(c *store.Check, in checkInput) error {
	str := func(p *string, dst *string) {
		if p != nil {
			*dst = strings.TrimSpace(*p)
		}
	}
	str(in.Name, &c.Name)
	str(in.Kind, &c.Kind)
	str(in.Target, &c.Target)
	str(in.Method, &c.Method)
	if in.Keyword != nil {
		c.Keyword = *in.Keyword
	}
	str(in.StatusCodes, &c.StatusCodes)
	str(in.DNSServer, &c.DNSServer)
	str(in.DNSType, &c.DNSType)
	str(in.Group, &c.Group)
	str(in.Notes, &c.Notes)
	for _, p := range []struct {
		in  *int
		dst *int
	}{{in.Port, &c.Port}, {in.IntervalS, &c.IntervalS}, {in.RetryS, &c.RetryS}, {in.TimeoutS, &c.TimeoutS}, {in.Retries, &c.Retries}} {
		if p.in != nil {
			*p.dst = *p.in
		}
	}
	for _, p := range []struct {
		in  *bool
		dst *bool
	}{{in.InvertKeyword, &c.InvertKeyword}, {in.IgnoreTLS, &c.IgnoreTLS}, {in.FollowRedirects, &c.FollowRedirects}, {in.Enabled, &c.Enabled}, {in.Notify, &c.Notify}, {in.Public, &c.Public}} {
		if p.in != nil {
			*p.dst = *p.in
		}
	}
	c.Method = strings.ToUpper(c.Method)
	c.DNSType = strings.ToUpper(c.DNSType)
	switch {
	case c.Name == "":
		return badRequest("give the check a name")
	case len(c.Name) > 120:
		return badRequest("the name is too long")
	}
	switch c.Kind {
	case "http":
		u, err := url.Parse(c.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return badRequest("the URL starts with http:// or https://")
		}
		if c.Method == "" {
			c.Method = "GET"
		}
		if c.Method != "GET" && c.Method != "HEAD" && c.Method != "POST" && c.Method != "OPTIONS" {
			return badRequest("the method is GET, HEAD, POST or OPTIONS")
		}
		if c.StatusCodes == "" {
			c.StatusCodes = "200-299"
		}
		if !probe.ValidCodes(c.StatusCodes) {
			return badRequest("status codes look like 200-299, 301 or 4xx")
		}
	case "tcp":
		if c.Target == "" || strings.Contains(c.Target, "/") {
			return badRequest("the host is a name or an IP address")
		}
		if c.Port < 1 || c.Port > 65535 {
			return badRequest("the port is between 1 and 65535")
		}
	case "dns":
		if c.Target == "" {
			return badRequest("say which name to look up")
		}
		if c.DNSType == "" {
			c.DNSType = "A"
		}
		okType := false
		for _, t := range probe.DNSTypes {
			okType = okType || t == c.DNSType
		}
		if !okType {
			return badRequest("the record type is one of " + strings.Join(probe.DNSTypes, ", "))
		}
		if c.Port == 0 {
			c.Port = 53
		}
	case "push":
		c.Target = ""
	default:
		return badRequest("the type is http, tcp, dns or push")
	}
	minIv := 20
	if c.Kind == "push" {
		minIv = 60
	}
	if c.IntervalS < minIv || c.IntervalS > 7*86400 {
		return badRequest(fmt.Sprintf("check every %d seconds to a week", minIv))
	}
	if c.RetryS < 20 || c.RetryS > 86400 {
		c.RetryS = min(max(c.RetryS, 20), 86400)
	}
	if c.TimeoutS < 1 || c.TimeoutS > 120 {
		c.TimeoutS = min(max(c.TimeoutS, 1), 120)
	}
	if c.TimeoutS >= c.IntervalS && c.Kind != "push" {
		c.TimeoutS = max(1, c.IntervalS*4/5)
	}
	if c.Retries < 0 || c.Retries > 10 {
		return badRequest("retries are between 0 and 10")
	}
	return nil
}

func (s *Server) handleCreateCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sv, _, ok := s.checkServer(w, r, true)
	if !ok {
		return
	}
	var in checkInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	c := &store.Check{ServerID: sv.ID, Method: "GET", StatusCodes: "200-299", FollowRedirects: true, DNSType: "A", IntervalS: 60, RetryS: 60, TimeoutS: 30, Retries: 1, Enabled: true, Notify: true}
	if err := applyCheck(c, in); err != nil {
		s.fail(w, err, "add check")
		return
	}
	if c.Kind == "push" {
		c.PushToken = newPushToken()
	}
	if err := s.store.SaveCheck(ctx, c); err != nil {
		s.fail(w, err, "add check")
		return
	}
	if s.store.EnsureCheckRules(ctx, sv.ID) {
		s.dropRules()
	}
	s.reloadChecks()
	s.touch()
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) ownCheck(w http.ResponseWriter, r *http.Request) (*store.Server, *store.Check) {
	sv, _, ok := s.checkServer(w, r, true)
	if !ok {
		return nil, nil
	}
	c, err := s.store.GetCheck(r.Context(), sv.ID, idParam(r, "cid"))
	if err != nil {
		s.fail(w, err, "get check")
		return nil, nil
	}
	return sv, c
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sv, c := s.ownCheck(w, r)
	if c == nil {
		return
	}
	var in checkInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	wasPush := c.Kind == "push"
	if err := applyCheck(c, in); err != nil {
		s.fail(w, err, "update check")
		return
	}
	if c.Kind == "push" && (!wasPush || c.PushToken == "") {
		c.PushToken = newPushToken()
	}
	if c.Kind != "push" {
		c.PushToken = ""
	}
	if err := s.store.SaveCheck(ctx, c); err != nil {
		s.fail(w, err, "update check")
		return
	}
	s.loadChecks(ctx)
	s.evaluateChecks(ctx, *sv)
	s.touch()
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleDeleteCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sv, c := s.ownCheck(w, r)
	if c == nil {
		return
	}
	if err := s.store.DeleteCheck(ctx, sv.ID, c.ID); err != nil {
		s.fail(w, err, "delete check")
		return
	}
	s.loadChecks(ctx)
	s.evaluateChecks(ctx, *sv)
	s.touch()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, c := s.ownCheck(w, r)
	if c == nil {
		return
	}
	if c.Kind == "push" {
		writeErr(w, http.StatusBadRequest, "a push check waits for its script; it can't be run from here")
		return
	}
	s.checks.mu.Lock()
	if s.checks.checks[c.ID] == nil {
		s.checks.mu.Unlock()
		s.loadChecks(ctx)
	} else {
		s.checks.mu.Unlock()
	}
	res := probe.Run(ctx, s.probeSpec(*c))
	out := s.recordCheck(context.WithoutCancel(ctx), c.ID, res.OK, &res.Ms, res.Msg, res.CertExpires, false)
	if out == nil {
		writeErr(w, http.StatusConflict, "the check is paused")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleNewPushToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, c := s.ownCheck(w, r)
	if c == nil {
		return
	}
	if c.Kind != "push" {
		writeErr(w, http.StatusBadRequest, "only push checks have a token")
		return
	}
	c.PushToken = newPushToken()
	if err := s.store.SaveCheck(ctx, c); err != nil {
		s.fail(w, err, "new token")
		return
	}
	s.loadChecks(ctx)
	writeJSON(w, http.StatusOK, c)
}

type checkPoint struct {
	T    int64    `json:"t"`
	Ms   *float64 `json:"ms"`
	Up   int      `json:"up"`
	Down int      `json:"down"`
}

func (s *Server) handleCheckHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sv, _, ok := s.checkServer(w, r, false)
	if !ok {
		return
	}
	c, err := s.store.GetCheck(ctx, sv.ID, idParam(r, "cid"))
	if err != nil {
		s.fail(w, err, "check")
		return
	}
	now := time.Now()
	loc := s.loc()
	rng := r.URL.Query().Get("range")
	var points []checkPoint
	step := 0
	up, all, msSum, msN := 0, 0, 0.0, 0
	switch rng {
	case "", "24h", "7d":
		span := 24 * time.Hour
		step = 300
		if rng == "7d" {
			span, step = 7*24*time.Hour, 1800
		} else {
			rng = "24h"
		}
		if c.IntervalS > step/2 {
			step = max(step, c.IntervalS)
		}
		beats, err := s.store.Beats(ctx, c.ID, now.Add(-span).Unix())
		if err != nil {
			s.fail(w, err, "history")
			return
		}
		type acc struct {
			sum      float64
			n, u, dn int
		}
		buckets := map[int64]*acc{}
		for _, b := range beats {
			k := b.T / int64(step) * int64(step)
			a := buckets[k]
			if a == nil {
				a = &acc{}
				buckets[k] = a
			}
			all++
			if b.Status == store.CheckDown {
				a.dn++
			} else {
				a.u++
				up++
				if b.Ms != nil {
					a.sum += *b.Ms
					a.n++
					msSum += *b.Ms
					msN++
				}
			}
		}
		keys := make([]int64, 0, len(buckets))
		for k := range buckets {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, k := range keys {
			a := buckets[k]
			p := checkPoint{T: k, Up: a.u, Down: a.dn}
			if a.n > 0 {
				v := math.Round(a.sum/float64(a.n)*10) / 10
				p.Ms = &v
			}
			points = append(points, p)
		}
	default:
		days := map[string]int{"30d": 30, "90d": 90, "1y": 365}[rng]
		if days == 0 {
			writeErr(w, http.StatusBadRequest, "range is 24h, 7d, 30d, 90d or 1y")
			return
		}
		step = 86400
		list, err := s.store.CheckDays(ctx, c.ID, now.In(loc).AddDate(0, 0, -days+1).Format("2006-01-02"))
		if err != nil {
			s.fail(w, err, "history")
			return
		}
		for _, d := range list {
			t, _ := time.ParseInLocation("2006-01-02", d.Day, loc)
			p := checkPoint{T: t.Unix(), Up: d.Up, Down: d.Down}
			if d.MsN > 0 {
				v := math.Round(d.AvgMs*10) / 10
				p.Ms = &v
			}
			points = append(points, p)
			up += d.Up
			all += d.Up + d.Down
			msSum += d.MsSum
			msN += d.MsN
		}
	}
	if points == nil {
		points = []checkPoint{}
	}
	out := map[string]any{"range": rng, "step_s": step, "points": points, "uptime": nil, "avg_ms": nil}
	if all > 0 {
		out["uptime"] = math.Round(float64(up)/float64(all)*1e5) / 1e5
	}
	if msN > 0 {
		out["avg_ms"] = math.Round(msSum/float64(msN)*10) / 10
	}
	events, _ := s.store.CheckEvents(ctx, sv.ID, c.ID, 50)
	out["events"] = events
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCheckEvents(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.checkServer(w, r, false)
	if !ok {
		return
	}
	list, err := s.store.CheckEvents(r.Context(), sv.ID, 0, queryInt(r, "limit"))
	if err != nil {
		s.fail(w, err, "events")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func validSlug(v string) bool {
	if len(v) < 1 || len(v) > 60 {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func (s *Server) handleSaveStatusPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sv, _, ok := s.checkServer(w, r, true)
	if !ok {
		return
	}
	in := store.StatusPage{Enabled: true}
	if cur := s.store.GetStatusPage(ctx, sv.ID); cur != nil {
		in = *cur
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.ServerID = sv.ID
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.Title = strings.TrimSpace(in.Title)
	if !validSlug(in.Slug) {
		writeErr(w, http.StatusBadRequest, "the address uses lowercase letters, digits and dashes")
		return
	}
	if in.Title == "" {
		in.Title = sv.Name
	}
	if err := s.store.SaveStatusPage(ctx, in); err != nil {
		s.fail(w, err, "status page")
		return
	}
	writeJSON(w, http.StatusOK, s.store.GetStatusPage(ctx, sv.ID))
}

// ---------- Uptime Kuma's status page JSON ----------

func kumaTime(t int64) string { return time.Unix(t, 0).UTC().Format("2006-01-02 15:04:05.000") }

func (s *Server) publicChecks(ctx context.Context, slug string) (*store.StatusPage, []store.Check) {
	p := s.store.StatusPageBySlug(ctx, slug)
	if p == nil {
		return nil, nil
	}
	all, _ := s.store.Checks(ctx, p.ServerID)
	var out []store.Check
	for _, c := range all {
		if c.Public {
			out = append(out, c)
		}
	}
	return p, out
}

func (s *Server) handleStatusPageJSON(w http.ResponseWriter, r *http.Request) {
	p, checks := s.publicChecks(r.Context(), chi.URLParam(r, "slug"))
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "Status page not found."})
		return
	}
	type mon struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		SendURL int    `json:"sendUrl"`
		Type    string `json:"type"`
	}
	type group struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		Weight      int    `json:"weight"`
		MonitorList []mon  `json:"monitorList"`
	}
	groups := []*group{}
	byName := map[string]*group{}
	for _, c := range checks {
		name := c.Group
		if name == "" {
			name = "Services"
		}
		g := byName[name]
		if g == nil {
			g = &group{ID: len(groups) + 1, Name: name, Weight: len(groups) + 1, MonitorList: []mon{}}
			byName[name] = g
			groups = append(groups, g)
		}
		g.MonitorList = append(g.MonitorList, mon{ID: c.ID, Name: c.Name, Type: c.Kind})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config": map[string]any{"slug": p.Slug, "title": p.Title, "description": p.Description, "icon": "/favicon.svg", "theme": "auto", "published": true,
			"showTags": false, "domainNameList": []string{}, "customCSS": "", "footerText": nil, "showPoweredBy": false, "googleAnalyticsId": nil, "showCertificateExpiry": false},
		"incident": nil, "publicGroupList": groups, "maintenanceList": []any{},
	})
}

func (s *Server) handleStatusHeartbeats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, checks := s.publicChecks(ctx, chi.URLParam(r, "slug"))
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "msg": "Status page not found."})
		return
	}
	type beat struct {
		Status int      `json:"status"`
		Time   string   `json:"time"`
		Msg    string   `json:"msg"`
		Ping   *float64 `json:"ping"`
	}
	beats, _ := s.store.RecentBeats(ctx, p.ServerID, 100)
	day := s.store.Uptime(ctx, p.ServerID, time.Now().Add(-24*time.Hour), s.loc())
	hb := map[string][]beat{}
	up := map[string]float64{}
	for _, c := range checks {
		id := strconv.FormatInt(c.ID, 10)
		list := []beat{}
		for _, b := range beats[c.ID] {
			list = append(list, beat{Status: b.Status, Time: kumaTime(b.T), Msg: b.Msg, Ping: b.Ms})
		}
		hb[id] = list
		if v, ok := day[c.ID]; ok {
			up[id+"_24"] = v
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"heartbeatList": hb, "uptimeList": up})
}

// checkMetrics writes Uptime Kuma's monitor_* lines for /metrics.
func (s *Server) checkMetrics(w io.Writer) {
	h := s.checks
	h.mu.Lock()
	list := make([]store.Check, 0, len(h.checks))
	for _, c := range h.checks {
		list = append(list, *c)
	}
	h.mu.Unlock()
	if len(list) == 0 {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	labels := func(c store.Check) string {
		u, host, port := "https://", "null", "null"
		switch c.Kind {
		case "http":
			u = c.Target
		case "tcp", "dns":
			host, port = c.Target, strconv.Itoa(c.Port)
		}
		return fmt.Sprintf("monitor_name=%q,monitor_type=%q,monitor_url=%q,monitor_hostname=%q,monitor_port=%q", c.Name, c.Kind, u, host, port)
	}
	fmt.Fprint(w, "# HELP monitor_status Monitor Status (1 = UP, 0= DOWN, 2= PENDING, 3= MAINTENANCE)\n# TYPE monitor_status gauge\n")
	for _, c := range list {
		if c.Enabled {
			fmt.Fprintf(w, "monitor_status{%s} %d\n", labels(c), c.Status)
		}
	}
	fmt.Fprint(w, "# HELP monitor_response_time Monitor Response Time (ms)\n# TYPE monitor_response_time gauge\n")
	for _, c := range list {
		if c.Enabled && c.LastMs != nil {
			fmt.Fprintf(w, "monitor_response_time{%s} %g\n", labels(c), *c.LastMs)
		}
	}
	fmt.Fprint(w, "# HELP monitor_cert_days_remaining The number of days remaining until the certificate expires\n# TYPE monitor_cert_days_remaining gauge\n")
	for _, c := range list {
		if c.Enabled && c.CertExpires > 0 {
			fmt.Fprintf(w, "monitor_cert_days_remaining{%s} %d\n", labels(c), int(time.Until(time.Unix(c.CertExpires, 0)).Hours()/24))
		}
	}
}

// ---------- import from Uptime Kuma ----------

// kumaExport is Uptime Kuma's JSON backup (Settings → Backup → Export) plus
// an optional "pocket" block with history (see scripts/kuma-export.py).
type kumaExport struct {
	MonitorList []struct {
		ID                  int64           `json:"id"`
		Name                string          `json:"name"`
		Type                string          `json:"type"`
		URL                 string          `json:"url"`
		Method              string          `json:"method"`
		Hostname            string          `json:"hostname"`
		Port                int             `json:"port"`
		Interval            int             `json:"interval"`
		RetryInterval       int             `json:"retryInterval"`
		MaxRetries          int             `json:"maxretries"`
		Keyword             string          `json:"keyword"`
		InvertKeyword       bool            `json:"invertKeyword"`
		IgnoreTLS           bool            `json:"ignoreTls"`
		MaxRedirects        *int            `json:"maxredirects"`
		AcceptedStatusCodes []string        `json:"accepted_statuscodes"`
		DNSResolveServer    string          `json:"dns_resolve_server"`
		DNSResolveType      string          `json:"dns_resolve_type"`
		PushToken           string          `json:"pushToken"`
		Active              json.RawMessage `json:"active"`
		Timeout             float64         `json:"timeout"`
		Description         string          `json:"description"`
	} `json:"monitorList"`
	Pocket struct {
		Public     []int64                       `json:"public"`
		Groups     map[string]string             `json:"groups"`
		StatusPage *store.StatusPage             `json:"status_page"`
		Days       map[string][]store.CheckDay   `json:"days"`
		Events     map[string][]store.CheckEvent `json:"events"`
		Beats      map[string][]store.Beat       `json:"beats"`
		State      map[string]struct {
			Status    int      `json:"status"`
			LastAt    int64    `json:"last_at"`
			LastMs    *float64 `json:"last_ms"`
			LastMsg   string   `json:"last_msg"`
			ChangedAt int64    `json:"changed_at"`
		} `json:"state"`
	} `json:"pocket"`
}

func kumaActive(raw json.RawMessage) bool {
	v := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return v == "" || v == "1" || v == "true"
}

func (s *Server) handleImportKuma(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sv, _, ok := s.checkServer(w, r, true)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "attach Uptime Kuma's JSON backup as “file”")
		return
	}
	defer f.Close()
	var ex kumaExport
	if err := json.NewDecoder(f).Decode(&ex); err != nil {
		writeErr(w, http.StatusBadRequest, "that isn't an Uptime Kuma backup: "+err.Error())
		return
	}
	if len(ex.MonitorList) == 0 {
		writeErr(w, http.StatusBadRequest, "the backup has no monitors")
		return
	}
	existing, _ := s.store.Checks(ctx, sv.ID)
	byRef := map[string]*store.Check{}
	for i := range existing {
		if existing[i].SourceRef != "" {
			byRef[existing[i].SourceRef] = &existing[i]
		}
	}
	public := map[int64]bool{}
	for _, id := range ex.Pocket.Public {
		public[id] = true
	}
	added, updated, skipped := 0, 0, []string{}
	for _, m := range ex.MonitorList {
		ref := "kuma:" + strconv.FormatInt(m.ID, 10)
		c := byRef[ref]
		isNew := c == nil
		if isNew {
			c = &store.Check{ServerID: sv.ID, SourceRef: ref, Notify: true}
		}
		c.Name, c.Enabled, c.Public = m.Name, kumaActive(m.Active), public[m.ID]
		c.Group = ex.Pocket.Groups[strconv.FormatInt(m.ID, 10)]
		c.IntervalS, c.RetryS, c.Retries = max(m.Interval, 20), max(m.RetryInterval, 20), min(max(m.MaxRetries, 0), 10)
		c.TimeoutS = int(m.Timeout)
		if c.TimeoutS <= 0 {
			c.TimeoutS = max(1, c.IntervalS*4/5)
		}
		c.TimeoutS = min(c.TimeoutS, 120)
		c.Notes = m.Description
		switch m.Type {
		case "http", "keyword":
			c.Kind, c.Target, c.Method = "http", m.URL, strings.ToUpper(firstNonEmpty(m.Method, "GET"))
			c.IgnoreTLS, c.FollowRedirects = m.IgnoreTLS, m.MaxRedirects == nil || *m.MaxRedirects > 0
			c.StatusCodes = strings.Join(m.AcceptedStatusCodes, ",")
			if c.StatusCodes == "" {
				c.StatusCodes = "200-299"
			}
			c.Keyword, c.InvertKeyword = "", false
			if m.Type == "keyword" {
				c.Keyword, c.InvertKeyword = m.Keyword, m.InvertKeyword
			}
		case "port":
			c.Kind, c.Target, c.Port = "tcp", m.Hostname, m.Port
		case "dns":
			c.Kind, c.Target, c.Port, c.DNSServer, c.DNSType = "dns", m.Hostname, m.Port, m.DNSResolveServer, firstNonEmpty(m.DNSResolveType, "A")
			if c.Port == 0 {
				c.Port = 53
			}
		case "push":
			c.Kind, c.PushToken = "push", m.PushToken
			if c.PushToken == "" {
				c.PushToken = newPushToken()
			}
		default:
			skipped = append(skipped, fmt.Sprintf("%s (%s checks aren't supported)", m.Name, m.Type))
			continue
		}
		if err := applyCheck(c, checkInput{}); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", m.Name, err.Error()))
			continue
		}
		if err := s.store.SaveCheck(ctx, c); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", m.Name, err.Error()))
			continue
		}
		key := strconv.FormatInt(m.ID, 10)
		for _, d := range ex.Pocket.Days[key] {
			s.store.ImportCheckDay(ctx, c.ID, d)
		}
		for _, e := range ex.Pocket.Events[key] {
			e.CheckID = c.ID
			s.store.ImportCheckEvent(ctx, e)
		}
		for _, b := range ex.Pocket.Beats[key] {
			s.store.ImportBeat(ctx, c.ID, b)
		}
		if st, ok := ex.Pocket.State[key]; ok && (isNew || c.LastAt < st.LastAt) {
			c.Status, c.LastAt, c.LastMs, c.LastMsg, c.ChangedAt = st.Status, st.LastAt, st.LastMs, st.LastMsg, st.ChangedAt
			if c.Status == store.CheckDown {
				c.Fails = c.Retries + 1
			}
			_ = s.store.SaveCheckState(ctx, c)
		}
		if isNew {
			added++
		} else {
			updated++
		}
	}
	if p := ex.Pocket.StatusPage; p != nil && validSlug(p.Slug) && s.store.GetStatusPage(ctx, sv.ID) == nil {
		p.ServerID, p.Enabled = sv.ID, true
		if err := s.store.SaveStatusPage(ctx, *p); err != nil {
			skipped = append(skipped, "status page “"+p.Slug+"” ("+err.Error()+")")
		}
	}
	if s.store.EnsureCheckRules(ctx, sv.ID) {
		s.dropRules()
	}
	// imported state replaces whatever the runner held
	s.checks.mu.Lock()
	s.checks.checks = map[int64]*store.Check{}
	s.checks.next = map[int64]time.Time{}
	s.checks.mu.Unlock()
	s.loadChecks(ctx)
	s.touch()
	s.log.Info("imported uptime kuma monitors", "added", added, "updated", updated, "skipped", len(skipped))
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "updated": updated, "skipped": skipped})
}

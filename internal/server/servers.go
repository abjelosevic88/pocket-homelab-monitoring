package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

func (s *Server) serverRoutes(r chi.Router) {
	r.Get("/servers", s.handleListServers)
	r.Post("/servers", s.handleCreateServer)
	r.Post("/servers/test", s.handleTestServer)
	r.Post("/servers/reorder", s.handleReorderServers)
	r.Get("/servers/{id}", s.handleGetServer)
	r.Put("/servers/{id}", s.handleUpdateServer)
	r.Delete("/servers/{id}", s.handleDeleteServer)
	r.Post("/servers/{id}/leave", s.handleLeaveServer)
	r.Get("/servers/{id}/snapshot", s.handleSnapshot)
	r.Get("/servers/{id}/live", s.handleLive)
	r.Get("/servers/{id}/history", s.handleHistory)
	r.Get("/servers/{id}/shares", s.handleListShares)
	r.Post("/servers/{id}/shares", s.handleCreateShare)
	r.Delete("/shares/{sid}", s.handleDeleteShare)
	r.Get("/people", s.handlePeople)
}

// summary is the headline of a snapshot (the overview cards).
type summary struct {
	Time                string  `json:"time"`
	Hostname            string  `json:"hostname"`
	Platform            string  `json:"platform"`
	CPU                 float64 `json:"cpu"`
	MemPct              float64 `json:"mem_pct"`
	MemUsed             uint64  `json:"mem_used"`
	MemTotal            uint64  `json:"mem_total"`
	SwapPct             float64 `json:"swap_pct"`
	Load1               float64 `json:"load1"`
	Cores               int     `json:"cores"`
	Threads             int     `json:"threads"`
	Temp                float64 `json:"temp"`
	TempLabel           string  `json:"temp_label"`
	FSPct               float64 `json:"fs_pct"`
	FSMount             string  `json:"fs_mount"`
	RxBps               float64 `json:"rx_bps"`
	TxBps               float64 `json:"tx_bps"`
	ReadBps             float64 `json:"read_bps"`
	WriteBps            float64 `json:"write_bps"`
	UptimeS             int64   `json:"uptime_s"`
	Procs               int     `json:"procs"`
	ContainersRunning   int     `json:"containers_running"`
	ContainersTotal     int     `json:"containers_total"`
	ContainersUnhealthy int     `json:"containers_unhealthy"`
}

func summarize(snap *collect.Snapshot) *summary {
	if snap == nil {
		return nil
	}
	x := &summary{Time: store.FormatTime(snap.Time), Hostname: snap.Host.Hostname, Platform: snap.Host.Platform, CPU: snap.CPU.Total,
		MemPct: snap.Mem.Percent, MemUsed: snap.Mem.Used, MemTotal: snap.Mem.Total, SwapPct: snap.Swap.Percent, Load1: snap.Load.One,
		Cores: snap.Host.Cores, Threads: snap.Host.Threads, UptimeS: snap.Host.UptimeSec, Procs: snap.Procs.Total, ContainersTotal: len(snap.Containers)}
	x.Temp, x.TempLabel = snap.Hottest()
	x.FSPct, x.FSMount = snap.FullestFS()
	x.RxBps, x.TxBps = snap.NetTotals()
	x.ReadBps, x.WriteBps = snap.DiskTotals()
	for _, c := range snap.Containers {
		if c.State == "running" {
			x.ContainersRunning++
		}
		if c.Health == "unhealthy" {
			x.ContainersUnhealthy++
		}
	}
	return x
}

type serverView struct {
	store.Server
	Role         string        `json:"role"`
	Owner        *store.Person `json:"owner,omitempty"`
	Shares       []store.Share `json:"shares,omitempty"`
	Status       string        `json:"status"`
	Summary      *summary      `json:"summary"`
	Spark        []float64     `json:"spark"`
	AlertsActive int           `json:"alerts_active"`
}

func (s *Server) view(ctx context.Context, sv store.Server, role string, openCounts map[int64]int) serverView {
	v := serverView{Server: sv, Role: role, Spark: s.store.Spark(ctx, sv.ID, 60), AlertsActive: openCounts[sv.ID]}
	st := s.mon.state(sv.ID)
	now := time.Now()
	if st != nil {
		st.mu.Lock()
		v.Status = s.statusOf(st, sv, now)
		v.Summary = summarize(st.snap)
		if !st.seen.IsZero() {
			v.LastSeen = store.FormatTime(st.seen)
		}
		v.LastError = st.err
		st.mu.Unlock()
	} else {
		v.Status = s.statusOf(nil, sv, now)
	}
	if role == "owner" {
		v.Shares, _ = s.store.ListShares(ctx, sv.ID)
	} else if u, err := s.store.GetUser(ctx, sv.UserID); err == nil {
		v.Owner = &store.Person{ID: u.ID, Name: u.Name, Email: u.Email}
	}
	if sv.Kind == "agent" && role != "owner" {
		v.URL = ""
	}
	return v
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := me(r)
	list, err := s.store.VisibleServers(ctx, uid)
	if err != nil {
		s.fail(w, err, "list servers")
		return
	}
	counts := s.store.OpenAlertCounts(ctx)
	out := make([]serverView, 0, len(list))
	for _, sv := range list {
		role := "viewer"
		if sv.UserID == uid {
			role = "owner"
		}
		out = append(out, s.view(ctx, sv, role, counts))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	sv, role, err := s.store.ServerFor(r.Context(), me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "get server")
		return
	}
	writeJSON(w, http.StatusOK, s.view(r.Context(), *sv, role, s.store.OpenAlertCounts(r.Context())))
}

// ownServer loads a server the caller owns (404 otherwise, also for viewers:
// they may look but not change).
func (s *Server) ownServer(w http.ResponseWriter, r *http.Request) *store.Server {
	sv, role, err := s.store.ServerFor(r.Context(), me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "get server")
		return nil
	}
	if role != "owner" {
		writeErr(w, http.StatusForbidden, "only the owner can change this server")
		return nil
	}
	return sv
}

type serverInput struct {
	Name      *string `json:"name"`
	Kind      string  `json:"kind"`
	URL       *string `json:"url"`
	Token     *string `json:"token"`
	Username  *string `json:"username"`
	Password  *string `json:"password"`
	Color     *string `json:"color"`
	IntervalS *int    `json:"interval_s"`
	Enabled   *bool   `json:"enabled"`
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validColor(c string) bool { return c == "" || colorRe.MatchString(c) }

func cleanURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", badRequest("the address is required, like http://192.168.1.20:8193")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", badRequest("that address doesn't look right; use http://host:port")
	}
	return raw, nil
}

// apply copies input onto sv; secret changes are sealed.
func (s *Server) applyServer(sv *store.Server, in serverInput) error {
	if in.Name != nil {
		sv.Name = strings.TrimSpace(*in.Name)
	}
	if in.URL != nil && sv.Kind != "local" {
		u, err := cleanURL(*in.URL)
		if err != nil {
			return err
		}
		sv.URL = u
	}
	if in.Username != nil && sv.Kind == "glances" {
		sv.Username = strings.TrimSpace(*in.Username)
	}
	secret := in.Token
	if sv.Kind == "glances" {
		secret = in.Password
	}
	if secret != nil && sv.Kind != "local" {
		if *secret == "" {
			sv.Secret = ""
		} else {
			sealed, err := s.seal(strings.TrimSpace(*secret))
			if err != nil {
				return err
			}
			sv.Secret = sealed
		}
	}
	if in.Color != nil {
		if !validColor(*in.Color) {
			return badRequest("colour looks like #2a78d6")
		}
		sv.Color = *in.Color
	}
	if in.IntervalS != nil {
		v := *in.IntervalS
		if v < 2 || v > 3600 {
			return badRequest("the interval is between 2 seconds and an hour")
		}
		sv.IntervalS = v
	}
	if in.Enabled != nil {
		sv.Enabled = *in.Enabled
	}
	if sv.Name == "" {
		return badRequest("give the server a name")
	}
	if len(sv.Name) > 80 {
		return badRequest("that name is too long")
	}
	return nil
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in serverInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Kind != "agent" && in.Kind != "glances" {
		writeErr(w, http.StatusBadRequest, "kind is agent or glances")
		return
	}
	sv := &store.Server{UserID: me(r), Kind: in.Kind, Enabled: true, IntervalS: 10}
	if in.URL == nil {
		in.URL = new(string)
	}
	if err := s.applyServer(sv, in); err != nil {
		s.fail(w, err, "add server")
		return
	}
	if err := s.store.CreateServer(ctx, sv); err != nil {
		s.fail(w, err, "add server")
		return
	}
	for _, rule := range defaultRules(sv.ID) {
		_ = s.store.SaveRule(ctx, &rule)
	}
	s.dropRules()
	s.reconcile()
	s.touch()
	s.publish(ctx, "monitoring.server_added", sv, map[string]any{"kind": sv.Kind})
	writeJSON(w, http.StatusCreated, s.view(ctx, *sv, "owner", nil))
}

func (s *Server) handleTestServer(w http.ResponseWriter, r *http.Request) {
	var in serverInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	raw := ""
	if in.URL != nil {
		raw = *in.URL
	}
	u, err := cleanURL(raw)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	secret := ""
	if in.Kind == "glances" && in.Password != nil {
		secret = *in.Password
	} else if in.Token != nil {
		secret = *in.Token
	}
	user := ""
	if in.Username != nil {
		user = *in.Username
	}
	// Editing a server without retyping its secret: test with the stored one.
	if id := queryInt64(r, "id"); id > 0 && secret == "" {
		if sv, role, err := s.store.ServerFor(r.Context(), me(r), id); err == nil && role == "owner" {
			secret, _ = s.open(sv.Secret)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var snap *collect.Snapshot
	switch in.Kind {
	case "agent":
		snap, err = collect.Agent(ctx, u, strings.TrimSpace(secret))
	case "glances":
		snap, err = collect.Glances(ctx, u, strings.TrimSpace(user), secret)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "kind is agent or glances"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hostname": snap.Host.Hostname, "platform": snap.Host.Platform, "version": snap.Host.AgentVersion})
}

func (s *Server) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	sv := s.ownServer(w, r)
	if sv == nil {
		return
	}
	var in serverInput
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.applyServer(sv, in); err != nil {
		s.fail(w, err, "update server")
		return
	}
	if err := s.store.UpdateServer(r.Context(), sv); err != nil {
		s.fail(w, err, "update server")
		return
	}
	s.reconcile()
	s.touch()
	writeJSON(w, http.StatusOK, s.view(r.Context(), *sv, "owner", s.store.OpenAlertCounts(r.Context())))
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	sv := s.ownServer(w, r)
	if sv == nil {
		return
	}
	if err := s.store.DeleteServer(r.Context(), sv.ID); err != nil {
		s.fail(w, err, "delete server")
		return
	}
	if sv.Kind == "local" {
		_ = s.store.SetBoxSetting(r.Context(), "local_removed", "1")
	}
	s.forgetAlerts(sv.ID)
	s.dropRules()
	s.reconcile()
	s.touch()
	s.publish(r.Context(), "monitoring.server_removed", sv, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReorderServers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.store.ReorderServers(r.Context(), me(r), in.IDs); err != nil {
		s.fail(w, err, "reorder")
		return
	}
	s.touch()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLeaveServer(w http.ResponseWriter, r *http.Request) {
	sv, role, err := s.store.ServerFor(r.Context(), me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "leave")
		return
	}
	if role == "owner" {
		writeErr(w, http.StatusBadRequest, "it's your server; delete it instead")
		return
	}
	if err := s.store.LeaveServer(r.Context(), sv.ID, me(r)); err != nil {
		s.fail(w, err, "leave")
		return
	}
	s.touch()
	w.WriteHeader(http.StatusNoContent)
}

// visible loads a server the caller may see (owner or viewer).
func (s *Server) visible(w http.ResponseWriter, r *http.Request) (*store.Server, *serverState) {
	sv, _, err := s.store.ServerFor(r.Context(), me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "get server")
		return nil, nil
	}
	return sv, s.mon.state(sv.ID)
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	sv, st := s.visible(w, r)
	if sv == nil {
		return
	}
	if st == nil {
		writeErr(w, http.StatusNotFound, "no data yet")
		return
	}
	st.mu.Lock()
	snap := st.snap
	st.mu.Unlock()
	if snap == nil {
		writeErr(w, http.StatusNotFound, "no data yet")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	sv, st := s.visible(w, r)
	if sv == nil {
		return
	}
	pts := []store.Sample{}
	if st != nil {
		st.mu.Lock()
		for _, p := range st.ring {
			p.Temps = nil
			pts = append(pts, p)
		}
		st.mu.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"step_s": int(s.interval(*sv).Seconds()), "points": pts})
}

var ranges = map[string]struct {
	span time.Duration
	step int64
}{
	"1h":  {time.Hour, 60},
	"6h":  {6 * time.Hour, 120},
	"24h": {24 * time.Hour, 300},
	"7d":  {7 * 24 * time.Hour, 1800},
	"30d": {30 * 24 * time.Hour, 3 * 3600},
	"90d": {90 * 24 * time.Hour, 6 * 3600},
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	sv, _ := s.visible(w, r)
	if sv == nil {
		return
	}
	name := r.URL.Query().Get("range")
	rg, ok := ranges[name]
	if !ok {
		name, rg = "1h", ranges["1h"]
	}
	to := time.Now().Unix()
	from := to - int64(rg.span.Seconds())
	pts, err := s.store.History(r.Context(), sv.ID, from, to+60, rg.step)
	if err != nil {
		s.fail(w, err, "history")
		return
	}
	if pts == nil {
		pts = []store.Sample{}
	}
	labels := s.store.SensorNames(r.Context(), sv.ID)
	// One line per sensor name: a key that changed (another driver, a
	// moved drive) continues the old line instead of starting a twin.
	canon := map[string]string{}
	byLabel := map[string]string{}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		l := labels[k]
		if first, ok := byLabel[l]; ok {
			canon[k] = first
		} else {
			byLabel[l] = k
		}
	}
	seen := map[string]bool{}
	for i := range pts {
		for k, v := range pts[i].Temps {
			if c, ok := canon[k]; ok {
				if _, dup := pts[i].Temps[c]; !dup {
					pts[i].Temps[c] = v
				}
				delete(pts[i].Temps, k)
				k = c
			}
			seen[k] = true
		}
	}
	sensors := []store.SensorName{}
	for k := range seen {
		sensors = append(sensors, store.SensorName{Key: k, Label: firstNonEmpty(labels[k], k)})
	}
	sortSensors(sensors)
	writeJSON(w, http.StatusOK, map[string]any{"range": name, "step_s": rg.step, "from": from, "to": to, "points": pts, "sensors": sensors})
}

// sortSensors puts the CPU first, then drives, then the rest by name.
func sortSensors(list []store.SensorName) {
	rank := func(l string) int {
		switch {
		case l == "CPU package":
			return 0
		case strings.HasPrefix(l, "CPU"):
			return 1
		case strings.HasPrefix(l, "NVMe"), strings.HasPrefix(l, "Disk"):
			if strings.Contains(l, "Sensor") {
				return 4
			}
			return 2
		case l == "Chipset":
			return 3
		}
		return 5
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if rank(a.Label) > rank(b.Label) || rank(a.Label) == rank(b.Label) && a.Label > b.Label {
				list[j-1], list[j] = b, a
			}
		}
	}
}

// ---- shares ----

func (s *Server) handleListShares(w http.ResponseWriter, r *http.Request) {
	sv := s.ownServer(w, r)
	if sv == nil {
		return
	}
	out, err := s.store.ListShares(r.Context(), sv.ID)
	if err != nil {
		s.fail(w, err, "list shares")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateShare(w http.ResponseWriter, r *http.Request) {
	sv := s.ownServer(w, r)
	if sv == nil {
		return
	}
	var in struct {
		UserID   int64 `json:"user_id"`
		Everyone bool  `json:"everyone"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !in.Everyone {
		if in.UserID == me(r) {
			writeErr(w, http.StatusBadRequest, "it's already yours")
			return
		}
		if _, err := s.store.GetUser(r.Context(), in.UserID); err != nil {
			writeErr(w, http.StatusBadRequest, "no such person")
			return
		}
	}
	sh, err := s.store.CreateShare(r.Context(), sv.ID, in.UserID, in.Everyone)
	if err != nil {
		s.fail(w, err, "share")
		return
	}
	s.touch()
	writeJSON(w, http.StatusCreated, sh)
}

func (s *Server) handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	sh, owner, err := s.store.GetShare(r.Context(), idParam(r, "sid"))
	if err != nil || owner != me(r) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.store.DeleteShare(r.Context(), sh.ID); err != nil {
		s.fail(w, err, "unshare")
		return
	}
	s.touch()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.People(r.Context())
	if err != nil {
		s.fail(w, err, "people")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// publish queues a Core event for the server's owner.
func (s *Server) publish(ctx context.Context, topic string, sv *store.Server, extra map[string]any) {
	if !s.core.Configured() {
		return
	}
	p := map[string]any{"server_id": sv.ID, "server": sv.Name, "user_email": s.userEmail(ctx, sv.UserID)}
	for k, v := range extra {
		p[k] = v
	}
	if err := core.QueueEvent(ctx, s.store.DB, topic, "server:"+itoa64(sv.ID), p); err != nil {
		s.log.Debug("queue event", "topic", topic, "err", err)
	}
}

package server

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/speedtest"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// Internet speed tests: the Ookla CLI on the machine the web app runs on,
// tied to that machine's "local" server. Tests run on a schedule (slots
// every N minutes from an offset past midnight), can be put off while the
// box is busy moving data, and a failed or slow scheduled test is checked
// again 10 minutes later so one bad reading doesn't decide anything. The
// speed rules (speedtest, speed_down, speed_up, speed_ping) count tests in a
// row, not seconds: their for_s is that count.

func (s *Server) speedRoutes(r chi.Router) {
	r.Get("/servers/{id}/speedtest", s.handleSpeed)
	r.Get("/servers/{id}/speedtest/results", s.handleSpeedResults)
	r.Get("/servers/{id}/speedtest/live", s.handleSpeedLive)
	r.Post("/servers/{id}/speedtest/run", s.handleSpeedRun)
	r.Post("/servers/{id}/speedtest/cancel", s.handleSpeedCancel)
	r.Put("/servers/{id}/speedtest/settings", s.handleSpeedSettings)
	r.Get("/servers/{id}/speedtest/test-servers", s.handleSpeedTestServers)
	r.Put("/servers/{id}/speedtest/results/{rid}", s.handleSpeedNote)
	r.Delete("/servers/{id}/speedtest/results/{rid}", s.handleSpeedDelete)
	r.Get("/servers/{id}/speedtest/export.csv", s.handleSpeedExport)
	r.Post("/servers/{id}/speedtest/import", s.handleSpeedImport)
}

const (
	speedTimeout  = 3 * time.Minute
	speedLate     = 35 * time.Minute // a slot missed by more than this is skipped
	speedBusyWait = 30 * time.Minute // how long a busy box can put a test off
	speedRecheck  = 10 * time.Minute
)

type speedPoint struct {
	Ms    int64   `json:"ms"`
	Phase string  `json:"phase"`
	Mbps  float64 `json:"mbps"`
}

// speedLive is the test running right now.
type speedLive struct {
	ServerID int64             `json:"server_id"`
	Trigger  string            `json:"trigger"`
	Started  time.Time         `json:"started"`
	Phase    string            `json:"phase"`
	Progress float64           `json:"progress"`
	Mbps     float64           `json:"mbps"`
	PingMs   float64           `json:"ping_ms"`
	DownMbps float64           `json:"down_mbps"` // the download's last reading, once it's done
	Server   *speedtest.Server `json:"server,omitempty"`
	ISP      string            `json:"isp,omitempty"`
	Curve    []speedPoint      `json:"curve"`
}

type speedHub struct {
	runner speedtest.Runner

	mu        sync.Mutex
	live      *speedLive
	cancel    context.CancelFunc
	done      map[int64]int64     // newest slot handled per server (unix s)
	putOff    map[int64]bool      // the current slot is waiting for a quiet network
	recheck   map[int64]time.Time // a second look after a bad scheduled test
	servers   []speedtest.Server  // nearby test servers (cached)
	serversAt time.Time
	lastErr   map[int64]string // why the last scheduled test couldn't start
}

func newSpeedHub(r speedtest.Runner) *speedHub {
	return &speedHub{runner: r, done: map[int64]int64{}, putOff: map[int64]bool{}, recheck: map[int64]time.Time{}, lastErr: map[int64]string{}}
}

var errSpeedBusy = errors.New("a speed test is already running")

// speedReason says why a server can't run speed tests ("" = it can).
func (s *Server) speedReason(sv store.Server) string {
	if sv.Kind != "local" {
		return "Speed tests run on the machine Pocket Monitoring itself runs on, so they live on that server's Internet tab."
	}
	if !s.speed.runner.Available() {
		return "The Ookla Speedtest CLI isn't installed here. The Docker image ships it; on a plain install, put `speedtest` on PATH or point SPEEDTEST_BIN at it."
	}
	return ""
}

// ---------- schedule ----------

// speedSlot is the newest scheduled moment at or before t.
func speedSlot(t time.Time, st store.SpeedSettings, loc *time.Location) time.Time {
	every := time.Duration(st.EveryMin) * time.Minute
	lt := t.In(loc)
	mid := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	first := mid.Add(time.Duration(st.OffsetMin) * time.Minute)
	k := math.Floor(float64(t.Sub(first)) / float64(every))
	return first.Add(time.Duration(k) * every)
}

func speedNext(t time.Time, st store.SpeedSettings, loc *time.Location) time.Time {
	return speedSlot(t, st, loc).Add(time.Duration(st.EveryMin) * time.Minute)
}

// busyMbps is the traffic on the box's physical interfaces right now (-1 = unknown).
func (s *Server) busyMbps(serverID int64) float64 {
	for _, st := range s.mon.snapshotStates() {
		st.mu.Lock()
		id, snap, seen := st.srv.ID, st.snap, st.seen
		st.mu.Unlock()
		if id == serverID && snap != nil && time.Since(seen) < time.Minute {
			rx, tx := snap.NetTotals()
			return math.Round((rx+tx)*8/1e6*10) / 10
		}
	}
	return -1
}

func (s *Server) speedLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.speedTick(ctx)
		}
	}
}

func (s *Server) speedTick(ctx context.Context) {
	now := time.Now()
	loc := s.loc()
	h := s.speed
	for _, id := range s.store.SpeedTestServers(ctx) {
		sv, err := s.store.GetServer(ctx, id)
		if err != nil || !sv.Enabled || s.speedReason(*sv) != "" {
			continue
		}
		st := s.store.GetSpeedSettings(ctx, id)
		h.mu.Lock()
		running := h.live != nil
		re, hasRe := h.recheck[id]
		done := h.done[id]
		h.mu.Unlock()
		if running {
			return
		}
		if hasRe && !now.Before(re) {
			h.mu.Lock()
			delete(h.recheck, id)
			h.mu.Unlock()
			if err := s.startSpeed(*sv, "recheck", st.TestServerID); err == nil {
				return
			}
		}
		slot := speedSlot(now, st, loc)
		if done >= slot.Unix() {
			continue
		}
		if last := s.store.LastSpeedTests(ctx, id, 1); len(last) > 0 && last[0].T >= slot.Unix() {
			h.mu.Lock()
			h.done[id] = slot.Unix()
			h.mu.Unlock()
			continue
		}
		if now.Sub(slot) > speedLate {
			h.mu.Lock()
			h.done[id] = slot.Unix() // missed (the app was down): wait for the next one
			h.mu.Unlock()
			continue
		}
		if st.BusyMbps > 0 && now.Sub(slot) < speedBusyWait {
			if b := s.busyMbps(id); b > st.BusyMbps {
				h.mu.Lock()
				if !h.putOff[id] {
					s.log.Info("speed test put off: the network is busy", "server", sv.Name, "traffic_mbps", b, "limit", st.BusyMbps)
				}
				h.putOff[id] = true
				h.mu.Unlock()
				continue
			}
		}
		if err := s.startSpeed(*sv, "schedule", st.TestServerID); err != nil {
			if !errors.Is(err, errSpeedBusy) {
				s.log.Warn("speed test", "server", sv.Name, "err", err)
			}
			continue
		}
		h.mu.Lock()
		h.done[id] = slot.Unix()
		delete(h.putOff, id)
		h.mu.Unlock()
		return // one at a time
	}
}

// ---------- running one ----------

// startSpeed starts a test in the background (one at a time on the box).
func (s *Server) startSpeed(sv store.Server, trigger string, testServer int) error {
	h := s.speed
	h.mu.Lock()
	if h.live != nil {
		h.mu.Unlock()
		return errSpeedBusy
	}
	base := s.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, speedTimeout)
	live := &speedLive{ServerID: sv.ID, Trigger: trigger, Started: time.Now(), Phase: "starting", Curve: []speedPoint{}}
	h.live, h.cancel = live, cancel
	h.mu.Unlock()
	busy := s.busyMbps(sv.ID)
	s.touch()
	s.log.Info("speed test started", "server", sv.Name, "trigger", trigger, "test_server", testServer)

	go func() {
		defer cancel()
		res, err := h.runner.Run(ctx, testServer, func(p speedtest.Progress) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if live.Phase == "download" && p.Phase == "upload" {
				live.DownMbps = live.Mbps
			}
			live.Phase, live.Progress = p.Phase, p.Progress
			switch p.Phase {
			case "connecting":
				live.Server, live.ISP = p.Server, p.ISP
			case "ping":
				live.PingMs = p.PingMs
			case "download", "upload":
				live.Mbps = p.Mbps
				live.Curve = append(live.Curve, speedPoint{Ms: time.Since(live.Started).Milliseconds(), Phase: p.Phase, Mbps: p.Mbps})
			}
		})
		canceled := errors.Is(err, context.Canceled)
		h.mu.Lock()
		h.live, h.cancel = nil, nil
		h.mu.Unlock()
		if canceled {
			s.log.Info("speed test stopped", "server", sv.Name)
			s.touch()
			return
		}
		ctx := context.Background()
		t := &store.SpeedTest{ServerID: sv.ID, T: live.Started.Unix(), Trigger: trigger}
		if busy >= 0 {
			t.BusyMbps = &busy
		}
		if err != nil {
			t.OK = false
			t.Error = err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				t.Error = "the test took longer than " + fmtDur(speedTimeout)
			}
		} else {
			fillSpeed(t, res)
		}
		if _, err := s.store.AddSpeedTest(ctx, t); err != nil {
			s.log.Warn("store speed test", "err", err)
		}
		s.log.Info("speed test done", "server", sv.Name, "ok", t.OK, "down", ptrVal(t.DownMbps), "up", ptrVal(t.UpMbps), "ping", ptrVal(t.PingMs), "err", t.Error)
		s.afterSpeed(ctx, sv, t)
	}()
	return nil
}

func ptrVal(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func fp(v float64) *float64 { return &v }

// fillSpeed copies a CLI result into a stored test.
func fillSpeed(t *store.SpeedTest, r *speedtest.Result) {
	t.OK = true
	t.DownMbps, t.UpMbps, t.PingMs, t.JitterMs = fp(r.DownMbps), fp(r.UpMbps), fp(r.PingMs), fp(r.JitterMs)
	if r.LossPct >= 0 {
		t.LossPct = fp(r.LossPct)
	}
	if r.DownLatencyMs > 0 {
		t.DownLatencyMs = fp(r.DownLatencyMs)
	}
	if r.UpLatencyMs > 0 {
		t.UpLatencyMs = fp(r.UpLatencyMs)
	}
	t.DownBytes, t.UpBytes, t.DurationMs = r.DownBytes, r.UpBytes, r.DurationMs
	t.ISP, t.ExternalIP, t.VPN, t.ResultURL = r.ISP, r.ExternalIP, r.VPN, r.ResultURL
	t.TestServerID, t.TestServer = r.Server.ID, r.Server.Name
	t.TestLocation = strings.Trim(strings.Join([]string{r.Server.Location, r.Server.Country}, ", "), ", ")
}

// speedBad says whether a test misses the minimums (failed counts as bad).
func speedBad(t store.SpeedTest, st store.SpeedSettings) bool {
	if !t.OK {
		return true
	}
	return (st.MinDown > 0 && ptrVal(t.DownMbps) < st.MinDown) || (st.MinUp > 0 && ptrVal(t.UpMbps) < st.MinUp) || (st.MaxPing > 0 && ptrVal(t.PingMs) > st.MaxPing)
}

// afterSpeed: alerts, a second look after a bad scheduled test, the Core event.
func (s *Server) afterSpeed(ctx context.Context, sv store.Server, t *store.SpeedTest) {
	st := s.store.GetSpeedSettings(ctx, sv.ID)
	if t.Trigger == "schedule" && speedBad(*t, st) {
		s.speed.mu.Lock()
		s.speed.recheck[sv.ID] = time.Now().Add(speedRecheck)
		s.speed.mu.Unlock()
	}
	s.evaluateSpeed(ctx, sv)
	s.touch()
	ev := map[string]any{"test_id": t.ID, "ok": t.OK, "trigger": t.Trigger, "at": time.Unix(t.T, 0).UTC().Format(time.RFC3339)}
	if t.OK {
		ev["download_mbps"], ev["upload_mbps"], ev["ping_ms"], ev["jitter_ms"] = ptrVal(t.DownMbps), ptrVal(t.UpMbps), ptrVal(t.PingMs), ptrVal(t.JitterMs)
		ev["isp"], ev["external_ip"], ev["test_server"] = t.ISP, t.ExternalIP, t.TestServer
		ev["slow"] = speedBad(*t, st)
	} else {
		ev["error"] = t.Error
	}
	s.publish(ctx, "monitoring.speedtest", &sv, ev)
}

// ---------- alerts ----------

func isSpeedMetric(m string) bool {
	return m == "speedtest" || m == "speed_down" || m == "speed_up" || m == "speed_ping"
}

func speedValue(metric string, t store.SpeedTest) float64 {
	switch metric {
	case "speed_down":
		return ptrVal(t.DownMbps)
	case "speed_up":
		return ptrVal(t.UpMbps)
	case "speed_ping":
		return ptrVal(t.PingMs)
	}
	return math.NaN()
}

// speedConditions: the rule fires when its last n tests all missed (n = for_s).
func speedConditions(r store.Rule, recent []store.SpeedTest) []condition {
	n := r.ForS
	if n < 1 {
		n = 1
	}
	if r.Metric == "speedtest" {
		if len(recent) == 0 {
			return nil
		}
		fails := 0
		for _, t := range recent {
			if t.OK {
				break
			}
			fails++
		}
		label := fmt.Sprintf("Speed test failed %s", inARow(fails))
		if recent[0].Error != "" {
			label += ": " + recent[0].Error
		}
		return []condition{{label: label, value: float64(fails), fire: fails >= n, clear: recent[0].OK}}
	}
	var oks []store.SpeedTest
	for _, t := range recent {
		if t.OK && !math.IsNaN(speedValue(r.Metric, t)) {
			oks = append(oks, t)
		}
	}
	if len(oks) == 0 {
		return nil
	}
	bad := func(v float64) bool {
		if r.Metric == "speed_ping" {
			return v > r.Threshold
		}
		return v < r.Threshold
	}
	run := 0
	for _, t := range oks {
		if !bad(speedValue(r.Metric, t)) {
			break
		}
		run++
	}
	v := speedValue(r.Metric, oks[0])
	clear := v >= r.Threshold*1.05
	if r.Metric == "speed_ping" {
		clear = v <= r.Threshold*0.9
	}
	word := "under"
	if r.Metric == "speed_ping" {
		word = "over"
	}
	label := fmt.Sprintf("%s %s %s (limit %s)", metricName(r.Metric), fmtValue(r.Metric, v), inARow(run), fmtValue(r.Metric, r.Threshold))
	if run <= 1 {
		label = fmt.Sprintf("%s %s, %s the limit of %s", metricName(r.Metric), fmtValue(r.Metric, v), word, fmtValue(r.Metric, r.Threshold))
	}
	return []condition{{label: label, value: v, fire: run >= n, clear: clear}}
}

func inARow(n int) string {
	if n <= 1 {
		return "in the last test"
	}
	return fmt.Sprintf("in %d tests in a row", n)
}

// evaluateSpeed runs a server's speed rules against its newest tests.
func (s *Server) evaluateSpeed(ctx context.Context, sv store.Server) {
	recent := s.store.LastSpeedTests(ctx, sv.ID, 12)
	now := time.Now()
	for _, r := range s.rulesFor(ctx, sv.ID) {
		if !isSpeedMetric(r.Metric) {
			continue
		}
		if !r.Enabled {
			s.resolveRule(ctx, sv, r, nil, now)
			continue
		}
		conds := speedConditions(r, recent)
		if conds == nil {
			continue
		}
		rc := r
		rc.ForS = 0 // counted in tests above
		s.apply(ctx, sv, rc, conds, now, nil)
	}
}

// speedMetrics writes each server's newest successful test for /metrics.
func (s *Server) speedMetrics(ctx context.Context, w io.Writer) {
	ids := s.store.SpeedTestServers(ctx)
	if len(ids) == 0 {
		return
	}
	type gauge struct{ name, help string }
	gs := []gauge{{"download_mbps", "Download speed of the newest test (Mbps)."}, {"upload_mbps", "Upload speed of the newest test (Mbps)."},
		{"ping_ms", "Ping of the newest test (ms)."}, {"jitter_ms", "Jitter of the newest test (ms)."}, {"timestamp_seconds", "When the newest successful test ran."}}
	vals := map[int64][]float64{}
	names := map[int64]string{}
	for _, id := range ids {
		sv, err := s.store.GetServer(ctx, id)
		if err != nil {
			continue
		}
		for _, t := range s.store.LastSpeedTests(ctx, id, 12) {
			if t.OK {
				vals[id] = []float64{ptrVal(t.DownMbps), ptrVal(t.UpMbps), ptrVal(t.PingMs), ptrVal(t.JitterMs), float64(t.T)}
				names[id] = sv.Name
				break
			}
		}
	}
	for i, g := range gs {
		fmt.Fprintf(w, "# HELP pocket_monitoring_speedtest_%s %s\n# TYPE pocket_monitoring_speedtest_%s gauge\n", g.name, g.help, g.name)
		for id, v := range vals {
			if !math.IsNaN(v[i]) {
				fmt.Fprintf(w, "pocket_monitoring_speedtest_%s{server=%q} %g\n", g.name, names[id], v[i])
			}
		}
	}
}

// ---------- API ----------

func (s *Server) speedServer(w http.ResponseWriter, r *http.Request, ownerOnly bool) (*store.Server, string, bool) {
	sv, role, err := s.store.ServerFor(r.Context(), me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "get server")
		return nil, "", false
	}
	if ownerOnly && role != "owner" {
		writeErr(w, http.StatusForbidden, "only the server's owner can do that")
		return nil, "", false
	}
	return sv, role, true
}

func (s *Server) liveFor(id int64) *speedLive {
	h := s.speed
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.live == nil || h.live.ServerID != id {
		return nil
	}
	c := *h.live
	c.Curve = append(make([]speedPoint, 0, len(h.live.Curve)), h.live.Curve...)
	return &c
}

func (s *Server) handleSpeed(w http.ResponseWriter, r *http.Request) {
	sv, role, ok := s.speedServer(w, r, false)
	if !ok {
		return
	}
	ctx := r.Context()
	st := s.store.GetSpeedSettings(ctx, sv.ID)
	out := map[string]any{"available": false, "reason": s.speedReason(*sv), "owner": role == "owner", "settings": st, "count": s.store.CountSpeedTests(ctx, sv.ID), "live": s.liveFor(sv.ID)}
	out["available"] = out["reason"] == ""
	if last := s.store.LastSpeedTests(ctx, sv.ID, 1); len(last) > 0 {
		out["latest"] = last[0]
	}
	if st.Enabled {
		next := speedNext(time.Now(), st, s.loc())
		s.speed.mu.Lock()
		if re, ok := s.speed.recheck[sv.ID]; ok && re.Before(next) {
			out["recheck_at"] = store.FormatTime(re)
		}
		out["put_off"] = s.speed.putOff[sv.ID]
		s.speed.mu.Unlock()
		out["next_at"] = store.FormatTime(next)
	}
	out["busy_mbps"] = s.busyMbps(sv.ID)
	writeJSON(w, http.StatusOK, out)
}

var speedRanges = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour, "1y": 365 * 24 * time.Hour}

func (s *Server) handleSpeedResults(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, false)
	if !ok {
		return
	}
	var from int64
	if d, ok := speedRanges[r.URL.Query().Get("range")]; ok {
		from = time.Now().Add(-d).Unix()
	}
	if f := queryInt64(r, "from"); f > 0 {
		from = f
	}
	list, err := s.store.SpeedTests(r.Context(), sv.ID, from, queryInt(r, "limit"))
	if err != nil {
		s.fail(w, err, "speed tests")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSpeedLive(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"live": s.liveFor(sv.ID)})
}

func (s *Server) handleSpeedRun(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, true)
	if !ok {
		return
	}
	if why := s.speedReason(*sv); why != "" {
		writeErr(w, http.StatusNotImplemented, why)
		return
	}
	var in struct {
		TestServerID *int `json:"test_server_id"`
	}
	_ = decode(r, &in)
	ts := s.store.GetSpeedSettings(r.Context(), sv.ID).TestServerID
	if in.TestServerID != nil {
		ts = *in.TestServerID
	}
	if err := s.startSpeed(*sv, "manual", ts); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"live": s.liveFor(sv.ID)})
}

func (s *Server) handleSpeedCancel(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, true)
	if !ok {
		return
	}
	h := s.speed
	h.mu.Lock()
	if h.live != nil && h.live.ServerID == sv.ID && h.cancel != nil {
		h.cancel()
	}
	h.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

var speedEvery = map[int]bool{15: true, 30: true, 60: true, 120: true, 180: true, 240: true, 360: true, 480: true, 720: true, 1440: true}

func (s *Server) handleSpeedSettings(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, true)
	if !ok {
		return
	}
	ctx := r.Context()
	st := s.store.GetSpeedSettings(ctx, sv.ID)
	was := st.Enabled
	var in struct {
		Enabled      *bool    `json:"enabled"`
		EveryMin     *int     `json:"every_min"`
		OffsetMin    *int     `json:"offset_min"`
		TestServerID *int     `json:"test_server_id"`
		TestServer   *string  `json:"test_server"`
		PlanDown     *float64 `json:"plan_down"`
		PlanUp       *float64 `json:"plan_up"`
		MinDown      *float64 `json:"min_down"`
		MinUp        *float64 `json:"min_up"`
		MaxPing      *float64 `json:"max_ping"`
		BusyMbps     *float64 `json:"busy_mbps"`
		KeepDays     *int     `json:"keep_days"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	num := func(p *float64, dst *float64, what string) error {
		if p == nil {
			return nil
		}
		if math.IsNaN(*p) || *p < 0 || *p > 100000 {
			return badRequest(what + " is out of range")
		}
		*dst = math.Round(*p*100) / 100
		return nil
	}
	if in.Enabled != nil {
		st.Enabled = *in.Enabled
	}
	if in.EveryMin != nil {
		if !speedEvery[*in.EveryMin] {
			writeErr(w, http.StatusBadRequest, "pick how often from the list")
			return
		}
		st.EveryMin = *in.EveryMin
	}
	if in.OffsetMin != nil {
		st.OffsetMin = *in.OffsetMin
	}
	if st.OffsetMin < 0 || st.OffsetMin >= st.EveryMin {
		st.OffsetMin = ((st.OffsetMin % st.EveryMin) + st.EveryMin) % st.EveryMin
	}
	if in.TestServerID != nil {
		st.TestServerID = max(0, *in.TestServerID)
		st.TestServer = ""
	}
	if in.TestServer != nil {
		st.TestServer = clipStr(strings.TrimSpace(*in.TestServer), 120)
	}
	for _, x := range []struct {
		p    *float64
		dst  *float64
		what string
	}{{in.PlanDown, &st.PlanDown, "The plan's download"}, {in.PlanUp, &st.PlanUp, "The plan's upload"}, {in.MinDown, &st.MinDown, "The slowest download"},
		{in.MinUp, &st.MinUp, "The slowest upload"}, {in.MaxPing, &st.MaxPing, "The highest ping"}, {in.BusyMbps, &st.BusyMbps, "The busy limit"}} {
		if err := num(x.p, x.dst, x.what); err != nil {
			s.fail(w, err, "speed settings")
			return
		}
	}
	if in.KeepDays != nil {
		if *in.KeepDays < 0 || *in.KeepDays > 3650 {
			writeErr(w, http.StatusBadRequest, "keep tests between 0 (always) and 3650 days")
			return
		}
		st.KeepDays = *in.KeepDays
	}
	if st.Enabled && !was {
		if why := s.speedReason(*sv); why != "" {
			writeErr(w, http.StatusNotImplemented, why)
			return
		}
	}
	if err := s.store.SaveSpeedSettings(ctx, sv.ID, st); err != nil {
		s.fail(w, err, "speed settings")
		return
	}
	if st.Enabled && !was {
		if s.store.EnsureSpeedRules(ctx, sv.ID, st) {
			s.dropRules()
		}
		// the slot that just passed doesn't count: start from the next one
		s.speed.mu.Lock()
		s.speed.done[sv.ID] = speedSlot(time.Now(), st, s.loc()).Unix()
		s.speed.mu.Unlock()
	}
	s.touch()
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSpeedTestServers(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, false)
	if !ok {
		return
	}
	if why := s.speedReason(*sv); why != "" {
		writeErr(w, http.StatusNotImplemented, why)
		return
	}
	h := s.speed
	h.mu.Lock()
	list, at := h.servers, h.serversAt
	h.mu.Unlock()
	if list == nil || time.Since(at) > time.Hour || queryBool(r, "fresh") {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		fresh, err := h.runner.Servers(ctx)
		if err != nil {
			if list == nil {
				writeErr(w, http.StatusBadGateway, "couldn't list the test servers: "+err.Error())
				return
			}
		} else {
			list = fresh
			h.mu.Lock()
			h.servers, h.serversAt = fresh, time.Now()
			h.mu.Unlock()
		}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSpeedNote(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, true)
	if !ok {
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := s.store.SetSpeedNote(r.Context(), sv.ID, idParam(r, "rid"), clipStr(strings.TrimSpace(in.Note), 500)); err != nil {
		s.fail(w, err, "speed note")
		return
	}
	s.touch()
	t, _ := s.store.GetSpeedTest(r.Context(), sv.ID, idParam(r, "rid"))
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleSpeedDelete(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, true)
	if !ok {
		return
	}
	if err := s.store.DeleteSpeedTest(r.Context(), sv.ID, idParam(r, "rid")); err != nil {
		s.fail(w, err, "delete speed test")
		return
	}
	s.evaluateSpeed(r.Context(), *sv)
	s.touch()
	w.WriteHeader(http.StatusNoContent)
}

func fmtOpt(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func (s *Server) handleSpeedExport(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, false)
	if !ok {
		return
	}
	list, err := s.store.SpeedTests(r.Context(), sv.ID, 0, 0)
	if err != nil {
		s.fail(w, err, "export")
		return
	}
	name := strings.Map(func(r rune) rune {
		if r == ' ' || r == '/' || r == '"' {
			return '-'
		}
		return r
	}, strings.ToLower(sv.Name))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="speedtests-%s-%s.csv"`, name, time.Now().Format("2006-01-02")))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time", "ok", "download_mbps", "upload_mbps", "ping_ms", "jitter_ms", "packet_loss_pct", "download_latency_ms", "upload_latency_ms",
		"download_bytes", "upload_bytes", "isp", "external_ip", "test_server_id", "test_server", "test_location", "trigger", "other_traffic_mbps", "result_url", "error", "note"})
	loc := s.loc()
	for _, t := range list {
		_ = cw.Write([]string{time.Unix(t.T, 0).In(loc).Format(time.RFC3339), strconv.FormatBool(t.OK), fmtOpt(t.DownMbps), fmtOpt(t.UpMbps), fmtOpt(t.PingMs), fmtOpt(t.JitterMs),
			fmtOpt(t.LossPct), fmtOpt(t.DownLatencyMs), fmtOpt(t.UpLatencyMs), strconv.FormatInt(t.DownBytes, 10), strconv.FormatInt(t.UpBytes, 10), t.ISP, t.ExternalIP,
			strconv.Itoa(t.TestServerID), t.TestServer, t.TestLocation, t.Trigger, fmtOpt(t.BusyMbps), t.ResultURL, t.Error, t.Note})
	}
	cw.Flush()
}

// handleSpeedImport reads Speedtest Tracker's database.sqlite (its results table).
func (s *Server) handleSpeedImport(w http.ResponseWriter, r *http.Request) {
	sv, _, ok := s.speedServer(w, r, true)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "attach Speedtest Tracker's database.sqlite as “file”")
		return
	}
	defer f.Close()
	tmp, err := os.CreateTemp(s.cfg.DataDir, "import-*.sqlite")
	if err != nil {
		s.fail(w, err, "import")
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, f); err != nil {
		tmp.Close()
		s.fail(w, err, "import")
		return
	}
	tmp.Close()
	added, skipped, failed, err := s.importSpeedTracker(r.Context(), sv.ID, tmp.Name())
	if err != nil {
		s.fail(w, badRequest(err.Error()), "import")
		return
	}
	s.log.Info("speed tests imported", "server", sv.Name, "added", added, "already_there", skipped, "failed_tests", failed)
	s.touch()
	writeJSON(w, http.StatusOK, map[string]int{"added": added, "skipped": skipped, "failed": failed})
}

func (s *Server) importSpeedTracker(ctx context.Context, serverID int64, path string) (added, skipped, failed int, err error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return 0, 0, 0, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(created_at, ''), COALESCE(status, ''), COALESCE(data, ''), ping, download, upload, COALESCE(comments, ''), COALESCE(scheduled, 0) FROM results ORDER BY id`)
	if err != nil {
		return 0, 0, 0, errors.New("that isn't a Speedtest Tracker database (no results table)")
	}
	defer rows.Close()
	loc := s.loc()
	for rows.Next() {
		var id int64
		var created, status, data, comments string
		var ping, down, up sql.NullFloat64
		var scheduled int
		if err := rows.Scan(&id, &created, &status, &data, &ping, &down, &up, &comments, &scheduled); err != nil {
			return added, skipped, failed, err
		}
		t := &store.SpeedTest{ServerID: serverID, Trigger: "import", Note: strings.TrimSpace(comments), SourceRef: "speedtest-tracker:" + strconv.FormatInt(id, 10)}
		if tm, err := time.ParseInLocation("2006-01-02 15:04:05", created, loc); err == nil {
			t.T = tm.Unix()
		}
		switch status {
		case "completed":
			if res, err := speedtest.ParseResult([]byte(data)); err == nil {
				fillSpeed(t, res)
				if !res.Time.IsZero() {
					t.T = res.Time.Unix()
				}
			} else if down.Valid && up.Valid && ping.Valid {
				t.OK = true
				t.DownMbps, t.UpMbps, t.PingMs = fp(math.Round(down.Float64*8/1e4)/100), fp(math.Round(up.Float64*8/1e4)/100), fp(ping.Float64)
			} else {
				continue
			}
		case "failed":
			failed++
			t.OK = false
			t.Error = "failed"
			var l struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(data), &l) == nil && l.Message != "" {
				t.Error = l.Message
			}
		default:
			continue // still running or skipped
		}
		if t.T == 0 {
			continue
		}
		ok, err := s.store.AddSpeedTest(ctx, t)
		if err != nil {
			return added, skipped, failed, err
		}
		if ok {
			added++
		} else {
			skipped++
		}
	}
	return added, skipped, failed, rows.Err()
}

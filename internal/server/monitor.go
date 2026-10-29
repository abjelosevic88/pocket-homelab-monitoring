package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// Every enabled server has a poller: it takes a reading every interval,
// keeps the last 15 minutes in memory (the Live view), writes one averaged
// row per minute to the database and runs the alert rules.

const liveWindow = 15 * time.Minute

type serverState struct {
	mu      sync.Mutex
	srv     store.Server
	snap    *collect.Snapshot
	seen    time.Time // last good reading
	started time.Time // when the poller started (offline grace)
	err     string
	ring    []store.Sample
	minute  int64
	pts     []store.Sample
	sensors map[string]string
	dbSeen  time.Time
	running map[string]bool // containers seen running since start
	cancel  context.CancelFunc
}

type monitor struct {
	mu     sync.Mutex
	states map[int64]*serverState
	kick   chan struct{}
	local  *collect.Local
}

func newMonitor(local *collect.Local) *monitor {
	return &monitor{states: map[int64]*serverState{}, kick: make(chan struct{}, 1), local: local}
}

func (m *monitor) state(id int64) *serverState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.states[id]
}

// reconcile asks the monitor loop to pick up added, changed or removed servers now.
func (s *Server) reconcile() {
	select {
	case s.mon.kick <- struct{}{}:
	default:
	}
}

// monitorLoop keeps one poller per enabled server.
func (s *Server) monitorLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	offline := time.NewTicker(10 * time.Second)
	defer offline.Stop()
	for {
		s.syncPollers(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.mon.kick:
		case <-offline.C:
			s.checkOffline(ctx)
		}
	}
}

func (s *Server) syncPollers(ctx context.Context) {
	all, err := s.store.AllServers(ctx)
	if err != nil {
		s.log.Warn("list servers", "err", err)
		return
	}
	want := map[int64]store.Server{}
	for _, sv := range all {
		if sv.Enabled && (sv.Kind != "local" || s.mon.local != nil) {
			want[sv.ID] = sv
		}
	}
	m := s.mon
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, st := range m.states {
		sv, ok := want[id]
		if !ok {
			st.cancel()
			delete(m.states, id)
			continue
		}
		st.mu.Lock()
		changed := sv.URL != st.srv.URL || sv.Secret != st.srv.Secret || sv.Username != st.srv.Username || sv.IntervalS != st.srv.IntervalS
		if !changed {
			st.srv = sv // name, colour: no restart needed
		}
		st.mu.Unlock()
		if changed {
			st.cancel()
			delete(m.states, id) // started again below with the new settings
		}
	}
	for id, sv := range want {
		if _, ok := m.states[id]; ok {
			continue
		}
		pctx, cancel := context.WithCancel(ctx)
		st := &serverState{srv: sv, started: time.Now(), cancel: cancel, sensors: s.store.SensorNames(ctx, id), running: map[string]bool{}}
		if t := store.ParseTime(sv.LastSeen); !t.IsZero() {
			st.seen = t
		}
		m.states[id] = st
		go s.poll(pctx, st)
	}
}

func (s *Server) interval(sv store.Server) time.Duration {
	if sv.Kind == "local" {
		return s.cfg.LocalInterval
	}
	d := time.Duration(sv.IntervalS) * time.Second
	if d < 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// read takes one reading of a server.
func (s *Server) read(ctx context.Context, sv store.Server) (*collect.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	switch sv.Kind {
	case "local":
		if s.mon.local == nil {
			return nil, errors.New("the local collector is off (LOCAL_COLLECTOR=false)")
		}
		return s.mon.local.Collect(ctx)
	case "agent":
		tok, err := s.open(sv.Secret)
		if err != nil {
			return nil, err
		}
		return collect.Agent(ctx, sv.URL, tok)
	case "glances":
		pw, err := s.open(sv.Secret)
		if err != nil {
			return nil, err
		}
		return collect.Glances(ctx, sv.URL, sv.Username, pw)
	}
	return nil, fmt.Errorf("unknown kind %q", sv.Kind)
}

func (s *Server) poll(ctx context.Context, st *serverState) {
	st.mu.Lock()
	sv := st.srv
	st.mu.Unlock()
	every := s.interval(sv)
	if sv.Kind == "local" {
		// Rates need a previous reading; take one right away.
		_, _ = s.mon.local.Collect(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	for {
		st.mu.Lock()
		sv = st.srv
		st.mu.Unlock()
		snap, err := s.read(ctx, sv)
		if ctx.Err() != nil {
			return
		}
		s.record(ctx, st, snap, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func pointOf(snap *collect.Snapshot) store.Sample {
	p := store.Sample{T: snap.Time.Unix(), CPU: snap.CPU.Total, IOWait: snap.CPU.IOWait, Mem: snap.Mem.Percent, Swap: snap.Swap.Percent,
		Load1: snap.Load.One, Load5: snap.Load.Five, Load15: snap.Load.Fifteen}
	p.Rx, p.Tx = snap.NetTotals()
	p.Read, p.Write = snap.DiskTotals()
	p.Temp, _ = snap.Hottest()
	for _, x := range snap.Sensors {
		if x.Kind == "temp" && historySensor(x) {
			if p.Temps == nil {
				p.Temps = map[string]float64{}
			}
			p.Temps[x.Key] = x.Value
		}
	}
	return p
}

// historySensor keeps per-core CPU temperatures out of the history (the
// package temperature covers them).
func historySensor(x collect.Sensor) bool {
	return !strings.HasPrefix(x.Label, "CPU core ")
}

func (s *Server) record(ctx context.Context, st *serverState, snap *collect.Snapshot, err error) {
	now := time.Now()
	st.mu.Lock()
	sv := st.srv
	wasDown := s.statusOf(st, sv, now) == "down"
	if err != nil {
		msg := err.Error()
		changed := msg != st.err
		st.err = msg
		st.mu.Unlock()
		if changed {
			s.store.MarkSeen(ctx, sv.ID, "", msg)
			s.log.Debug("server reading failed", "server", sv.Name, "err", msg)
			s.touch()
		}
		return
	}
	hadErr := st.err != ""
	st.snap, st.seen, st.err = snap, now, ""
	p := pointOf(snap)
	st.ring = append(st.ring, p)
	cut := now.Add(-liveWindow).Unix()
	i := 0
	for i < len(st.ring) && st.ring[i].T < cut {
		i++
	}
	st.ring = st.ring[i:]
	minute := p.T / 60 * 60
	var flush []store.Sample
	var flushAt int64
	if st.minute != 0 && minute != st.minute && len(st.pts) > 0 {
		flush, flushAt = st.pts, st.minute
		st.pts = nil
	}
	st.minute = minute
	st.pts = append(st.pts, p)
	var names []store.SensorName
	for _, x := range snap.Sensors {
		if x.Kind == "temp" && historySensor(x) && st.sensors[x.Key] != x.Label {
			st.sensors[x.Key] = x.Label
			names = append(names, store.SensorName{Key: x.Key, Label: x.Label})
		}
	}
	for _, c := range snap.Containers {
		if c.State == "running" {
			st.running[c.Name] = true
		}
	}
	writeSeen := now.Sub(st.dbSeen) > 30*time.Second || hadErr
	if writeSeen {
		st.dbSeen = now
	}
	st.mu.Unlock()

	if len(flush) > 0 {
		avg := store.Average(flush, 60)
		if len(avg) > 0 {
			a := avg[0]
			a.T = flushAt
			if err := s.store.InsertSample(ctx, sv.ID, a); err != nil {
				s.log.Warn("store sample", "server", sv.Name, "err", err)
			}
		}
	}
	if len(names) > 0 {
		s.store.SaveSensorNames(ctx, sv.ID, names)
	}
	if writeSeen {
		s.store.MarkSeen(ctx, sv.ID, store.FormatTime(now), "")
	}
	if wasDown || hadErr {
		s.touch()
	}
	s.evaluate(ctx, sv, st, snap, now)
}

// statusOf: up | down | unknown | paused. Call with st.mu held (st may be nil).
func (s *Server) statusOf(st *serverState, sv store.Server, now time.Time) string {
	if !sv.Enabled {
		return "paused"
	}
	seen := store.ParseTime(sv.LastSeen)
	errMsg := sv.LastError
	if st != nil {
		seen, errMsg = st.seen, st.err
	}
	if seen.IsZero() {
		if errMsg != "" {
			return "down"
		}
		return "unknown"
	}
	grace := 3 * s.interval(sv)
	if grace < 30*time.Second {
		grace = 30 * time.Second
	}
	if now.Sub(seen) > grace {
		return "down"
	}
	return "up"
}

// ---------- alerts ----------

type alertKey struct {
	rule int64
	inst string
}

type alertEngine struct {
	mu      sync.Mutex
	pending map[alertKey]time.Time
	open    map[alertKey]*store.Alert
	rules   map[int64][]store.Rule // by server
	rulesAt time.Time
}

func newAlertEngine() *alertEngine {
	return &alertEngine{pending: map[alertKey]time.Time{}, open: map[alertKey]*store.Alert{}}
}

// loadAlerts picks up the alerts left open by the last run.
func (s *Server) loadAlerts(ctx context.Context) {
	open, err := s.store.OpenAlerts(ctx)
	if err != nil {
		s.log.Warn("load open alerts", "err", err)
		return
	}
	e := s.alerts
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range open {
		a := open[i]
		if a.RuleID == nil {
			s.store.ResolveAlert(ctx, a.ID)
			continue
		}
		e.open[alertKey{*a.RuleID, a.Target}] = &a
	}
}

// dropRules makes the engine reload rules on the next reading.
func (s *Server) dropRules() {
	s.alerts.mu.Lock()
	s.alerts.rules = nil
	s.alerts.mu.Unlock()
}

func (s *Server) rulesFor(ctx context.Context, serverID int64) []store.Rule {
	e := s.alerts
	e.mu.Lock()
	if e.rules != nil && time.Since(e.rulesAt) < time.Minute {
		r := e.rules[serverID]
		e.mu.Unlock()
		return r
	}
	e.mu.Unlock()
	all, err := s.store.ListRules(ctx, nil)
	if err != nil {
		return nil
	}
	by := map[int64][]store.Rule{}
	for _, r := range all {
		by[r.ServerID] = append(by[r.ServerID], r)
	}
	e.mu.Lock()
	e.rules, e.rulesAt = by, time.Now()
	e.mu.Unlock()
	return by[serverID]
}

type condition struct {
	inst  string
	label string // what fired: "/mnt/storage", "CPU package", "jellyfin"
	value float64
	fire  bool // over the threshold
	clear bool // far enough under it to resolve
}

// hysteresis is how far under the threshold a value must fall to resolve.
func hysteresis(metric string) float64 {
	switch metric {
	case "load":
		return 0.1
	case "cpu":
		return 5
	}
	return 2
}

func conditions(r store.Rule, snap *collect.Snapshot, running map[string]bool) []condition {
	over := func(inst, label string, v float64) condition {
		return condition{inst: inst, label: label, value: v, fire: v > r.Threshold, clear: v <= r.Threshold-hysteresis(r.Metric)}
	}
	var out []condition
	switch r.Metric {
	case "cpu":
		out = append(out, over("", "", snap.CPU.Total))
	case "mem":
		out = append(out, over("", "", snap.Mem.Percent))
	case "swap":
		if snap.Swap.Total > 0 {
			out = append(out, over("", "", snap.Swap.Percent))
		}
	case "load":
		th := snap.Host.Threads
		if th <= 0 {
			th = 1
		}
		out = append(out, over("", "", math.Round(snap.Load.One/float64(th)*100)/100))
	case "disk":
		for _, f := range snap.FS {
			if (r.Target == "" || r.Target == f.Mount) && f.Total > 0 && !f.ReadOnly {
				out = append(out, over(f.Mount, f.Mount, f.Percent))
			}
		}
	case "temp":
		pkg := false
		for _, x := range snap.Sensors {
			if x.Label == "CPU package" {
				pkg = true
			}
		}
		for _, x := range snap.Sensors {
			if x.Kind != "temp" || (r.Target != "" && r.Target != x.Key) {
				continue
			}
			if r.Target == "" && pkg && strings.HasPrefix(x.Label, "CPU core ") {
				continue
			}
			out = append(out, over(x.Key, x.Label, x.Value))
		}
	case "container":
		for _, c := range snap.Containers {
			if r.Target != "" && r.Target != c.Name {
				continue
			}
			bad := c.Health == "unhealthy" || c.State == "restarting" || c.State == "dead"
			if c.State == "exited" && running[c.Name] {
				code := collect.ExitCode(c.Status)
				bad = code != 0 && code != 137 && code != 143 && code != -1
			}
			out = append(out, condition{inst: c.Name, label: c.Name, value: 0, fire: bad, clear: !bad})
		}
	case "raid":
		for _, a := range snap.RAID {
			out = append(out, condition{inst: a.Name, label: a.Name, fire: a.Degraded, clear: !a.Degraded})
		}
	case "offline":
		out = append(out, condition{fire: false, clear: true})
	}
	return out
}

func (s *Server) evaluate(ctx context.Context, sv store.Server, st *serverState, snap *collect.Snapshot, now time.Time) {
	rules := s.rulesFor(ctx, sv.ID)
	st.mu.Lock()
	running := make(map[string]bool, len(st.running))
	for k, v := range st.running {
		running[k] = v
	}
	st.mu.Unlock()
	for _, r := range rules {
		if !r.Enabled {
			s.resolveRule(ctx, sv, r, nil, now)
			continue
		}
		conds := conditions(r, snap, running)
		s.apply(ctx, sv, r, conds, now, snap)
	}
}

// checkOffline fires offline rules for servers with no fresh reading.
func (s *Server) checkOffline(ctx context.Context) {
	now := time.Now()
	s.mon.mu.Lock()
	states := make([]*serverState, 0, len(s.mon.states))
	for _, st := range s.mon.states {
		states = append(states, st)
	}
	s.mon.mu.Unlock()
	for _, st := range states {
		st.mu.Lock()
		sv := st.srv
		since := st.seen
		if since.Before(st.started) {
			since = st.started // give a restarted box time for its first reading
		}
		errMsg := st.err
		st.mu.Unlock()
		for _, r := range s.rulesFor(ctx, sv.ID) {
			if r.Metric != "offline" || !r.Enabled {
				continue
			}
			gone := now.Sub(since)
			grace := 3 * s.interval(sv)
			if gone <= grace {
				continue
			}
			c := condition{fire: true, value: math.Round(gone.Seconds()), label: errMsg}
			// The rule's own for_s counts from the last reading, not from now.
			key := alertKey{r.ID, ""}
			s.alerts.mu.Lock()
			if _, ok := s.alerts.pending[key]; !ok {
				s.alerts.pending[key] = since
			}
			s.alerts.mu.Unlock()
			s.apply(ctx, sv, r, []condition{c}, now, nil)
		}
	}
}

func (s *Server) apply(ctx context.Context, sv store.Server, r store.Rule, conds []condition, now time.Time, snap *collect.Snapshot) {
	e := s.alerts
	seen := map[string]bool{}
	for _, c := range conds {
		seen[c.inst] = true
		key := alertKey{r.ID, c.inst}
		e.mu.Lock()
		open := e.open[key]
		if c.fire || (open != nil && !c.clear) {
			since, ok := e.pending[key]
			if !ok {
				since = now
				e.pending[key] = now
			}
			if open == nil && now.Sub(since) >= time.Duration(r.ForS)*time.Second {
				a := &store.Alert{ServerID: sv.ID, ServerName: sv.Name, RuleID: &r.ID, Metric: r.Metric, Target: c.inst, Severity: r.Severity,
					Value: c.value, Threshold: r.Threshold, StartedAt: store.FormatTime(since), Message: alertMessage(r, c, sv, snap)}
				e.open[key] = a
				e.mu.Unlock()
				if err := s.store.CreateAlert(ctx, a); err != nil {
					s.log.Warn("store alert", "err", err)
				}
				s.alertFired(ctx, sv, a)
				continue
			}
			if open != nil && c.value > open.Value && r.Metric != "offline" {
				open.Value = c.value
			}
			e.mu.Unlock()
			continue
		}
		delete(e.pending, key)
		e.mu.Unlock()
		if open != nil && c.clear {
			s.resolve(ctx, sv, key, open, c.value)
		}
	}
	s.resolveRule(ctx, sv, r, seen, now)
}

// resolveRule closes the rule's open alerts whose instance is gone (a removed
// container, an unmounted disk) — or all of them when keep is nil.
func (s *Server) resolveRule(ctx context.Context, sv store.Server, r store.Rule, keep map[string]bool, _ time.Time) {
	e := s.alerts
	e.mu.Lock()
	var gone []alertKey
	for k := range e.open {
		if k.rule == r.ID && (keep == nil || !keep[k.inst]) {
			gone = append(gone, k)
		}
	}
	for k := range e.pending {
		if k.rule == r.ID && (keep == nil || !keep[k.inst]) {
			delete(e.pending, k)
		}
	}
	e.mu.Unlock()
	for _, k := range gone {
		e.mu.Lock()
		a := e.open[k]
		e.mu.Unlock()
		if a != nil {
			s.resolve(ctx, sv, k, a, math.NaN())
		}
	}
}

func (s *Server) resolve(ctx context.Context, sv store.Server, key alertKey, a *store.Alert, value float64) {
	s.alerts.mu.Lock()
	delete(s.alerts.open, key)
	delete(s.alerts.pending, key)
	s.alerts.mu.Unlock()
	at := s.store.ResolveAlert(ctx, a.ID)
	a.ResolvedAt = &at
	s.alertResolved(ctx, sv, a, value)
}

func metricName(m string) string {
	switch m {
	case "cpu":
		return "CPU"
	case "mem":
		return "Memory"
	case "swap":
		return "Swap"
	case "load":
		return "Load"
	case "disk":
		return "Disk"
	case "temp":
		return "Temperature"
	case "offline":
		return "Offline"
	case "container":
		return "Container"
	case "raid":
		return "RAID"
	case "job":
		return "Scheduled job"
	case "speedtest":
		return "Speed test"
	case "speed_down":
		return "Download speed"
	case "speed_up":
		return "Upload speed"
	case "speed_ping":
		return "Ping"
	}
	return m
}

func fmtValue(metric string, v float64) string {
	switch metric {
	case "cpu", "mem", "swap", "disk":
		return fmt.Sprintf("%.0f%%", v)
	case "temp":
		return fmt.Sprintf("%.0f °C", v)
	case "load":
		return fmt.Sprintf("%.2f per thread", v)
	case "speed_down", "speed_up":
		if v < 10 {
			return fmt.Sprintf("%.1f Mbps", v)
		}
		return fmt.Sprintf("%.0f Mbps", v)
	case "speed_ping":
		return fmt.Sprintf("%.0f ms", v)
	}
	return fmt.Sprintf("%g", v)
}

func fmtFor(sec int) string {
	switch {
	case sec <= 0:
		return ""
	case sec < 120:
		return fmt.Sprintf(" for %d s", sec)
	case sec < 7200:
		return fmt.Sprintf(" for %d min", sec/60)
	}
	return fmt.Sprintf(" for %d h", sec/3600)
}

func alertMessage(r store.Rule, c condition, sv store.Server, snap *collect.Snapshot) string {
	switch r.Metric {
	case "offline":
		msg := fmt.Sprintf("%s is not answering (no reading for %s)", sv.Name, fmtDur(time.Duration(c.value)*time.Second))
		if c.label != "" {
			msg += ": " + c.label
		}
		return msg
	case "container":
		if snap != nil {
			for _, x := range snap.Containers {
				if x.Name == c.inst {
					return fmt.Sprintf("Container %s is %s (%s)", x.Name, firstNonEmpty(x.Health, x.State), x.Status)
				}
			}
		}
		return "Container " + c.inst + " is not healthy"
	case "raid":
		return "RAID array " + c.inst + " is degraded"
	case "job", "speedtest", "speed_down", "speed_up", "speed_ping":
		return c.label
	case "disk":
		return fmt.Sprintf("Disk %s is %s full (limit %s)", c.label, fmtValue("disk", c.value), fmtValue("disk", r.Threshold))
	case "temp":
		return fmt.Sprintf("%s at %s%s (limit %s)", c.label, fmtValue("temp", c.value), fmtFor(r.ForS), fmtValue("temp", r.Threshold))
	}
	return fmt.Sprintf("%s at %s%s (limit %s)", metricName(r.Metric), fmtValue(r.Metric, c.value), fmtFor(r.ForS), fmtValue(r.Metric, r.Threshold))
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < 2*time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// defaultRules are added to every new server.
func defaultRules(serverID int64) []store.Rule {
	return []store.Rule{
		{ServerID: serverID, Metric: "offline", ForS: 120, Severity: "critical", Enabled: true},
		{ServerID: serverID, Metric: "cpu", Threshold: 90, ForS: 600, Severity: "warning", Enabled: true},
		{ServerID: serverID, Metric: "mem", Threshold: 90, ForS: 300, Severity: "warning", Enabled: true},
		{ServerID: serverID, Metric: "disk", Threshold: 90, ForS: 60, Severity: "critical", Enabled: true},
		{ServerID: serverID, Metric: "temp", Threshold: 90, ForS: 120, Severity: "critical", Enabled: true},
		{ServerID: serverID, Metric: "container", ForS: 180, Severity: "warning", Enabled: true},
		{ServerID: serverID, Metric: "raid", ForS: 0, Severity: "critical", Enabled: true},
		{ServerID: serverID, Metric: "job", ForS: 0, Severity: "warning", Enabled: true},
		{ServerID: serverID, Metric: "load", Threshold: 2, ForS: 900, Severity: "warning", Enabled: false},
		{ServerID: serverID, Metric: "swap", Threshold: 90, ForS: 600, Severity: "warning", Enabled: false},
	}
}

func (m *monitor) snapshotStates() []*serverState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*serverState, 0, len(m.states))
	for _, st := range m.states {
		out = append(out, st)
	}
	return out
}

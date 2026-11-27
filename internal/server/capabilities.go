package server

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// What Pocket Search, Pocket Day and Pocket Hub ask Monitoring (the
// capabilities value in links.go): one route each, in the shape every Pocket
// app answers in. They read what the pollers and the uptime checks already
// measured and stored, as the request's person, and change nothing: no route
// here runs a check or reads a server. A person sees what /servers and
// /alerts show them: their own servers and the ones shared with them.

// The words the pages use (web/src/components/Status.tsx, server/Uptime.tsx).
var (
	serverStateWords = map[string]string{"up": "online", "down": "offline", "paused": "paused", "unknown": "waiting for data"}
	checkStateWords  = map[int]string{store.CheckDown: "down", store.CheckUp: "up", store.CheckPending: "retrying"}
)

// hasWord says whether there is a letter or a digit to look for: "??" and
// "*" are not words, and would match everything by accident.
func hasWord(q string) bool {
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// dots joins the parts of a line that are there: "up · 120 ms".
func dots(parts ...string) string {
	out := []string{}
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// countOf is "1 incident", "3 incidents".
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// lasted is a stretch of time in words: "under a minute", "12 min", "1 h 5 min".
func lasted(min float64) string {
	m := int(math.Round(min))
	switch {
	case min < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

// msWords is a response time as the Uptime tab writes it: "8.4 ms", "120 ms".
func msWords(v *float64) string {
	switch {
	case v == nil:
		return ""
	case *v < 10:
		return fmt.Sprintf("%.1f ms", *v)
	}
	return fmt.Sprintf("%.0f ms", *v)
}

// uptimeWords is a share of good results as the Uptime tab writes it: "100%", "99.95%", "97.3%".
func uptimeWords(v float64) string {
	p := v * 100
	switch {
	case p >= 99.995:
		return "100%"
	case p >= 99:
		return fmt.Sprintf("%.2f%%", p)
	}
	return fmt.Sprintf("%.1f%%", p)
}

// checkNow is a check with where the runner last saw it (newer than its row).
func (s *Server) checkNow(c store.Check) store.Check {
	s.checks.mu.Lock()
	defer s.checks.mu.Unlock()
	if live := s.checks.checks[c.ID]; live != nil {
		c.Status, c.Fails, c.LastAt, c.LastMs, c.LastMsg, c.ChangedAt, c.CertExpires = live.Status, live.Fails, live.LastAt, live.LastMs, live.LastMsg, live.ChangedAt, live.CertExpires
	}
	return c
}

// checkState is a check's state in a word: down, up, retrying, paused, not checked yet.
func checkState(c store.Check) string {
	switch {
	case !c.Enabled:
		return "paused"
	case c.LastAt == 0:
		return "not checked yet"
	}
	return checkStateWords[c.Status]
}

// checkPath opens a check on its server's Uptime tab.
func checkPath(c store.Check) string {
	return "/server/" + itoa64(c.ServerID) + "?tab=uptime&check=" + itoa64(c.ID)
}

// serverNow is where a server stands as its poller last saw it (up, down,
// paused, unknown), with the headline of the last reading and when that was.
func (s *Server) serverNow(sv store.Server, now time.Time) (string, *summary, time.Time) {
	st := s.mon.state(sv.ID)
	if st == nil {
		return s.statusOf(nil, sv, now), nil, store.ParseTime(sv.LastSeen)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	seen := st.seen
	if seen.IsZero() {
		seen = store.ParseTime(sv.LastSeen)
	}
	return s.statusOf(st, sv, now), summarize(st.snap), seen
}

// readingWords are a reading's headline numbers: "CPU 12% · memory 40% · disk 61%".
func readingWords(x *summary) string {
	if x == nil {
		return ""
	}
	return dots("CPU "+fmtValue("cpu", x.CPU), "memory "+fmtValue("mem", x.MemPct), "disk "+fmtValue("disk", x.FSPct))
}

// ---------- search ----------

// pocketSearch answers Pocket Search: uptime checks ("monitor") by name,
// group or what they check, and servers by name, a name match first. A
// check says where it stands, its last response time and its uptime since
// the first of the month; a server whether it answers and its last reading.
func (s *Server) pocketSearch(r *http.Request, q string, limit int) ([]core.SearchItem, error) {
	if !hasWord(q) {
		return nil, nil
	}
	ctx := r.Context()
	servers, err := s.store.VisibleServers(ctx, me(r))
	if err != nil {
		return nil, err
	}
	low := strings.ToLower(q)
	has := func(v string) bool { return strings.Contains(strings.ToLower(v), low) }
	now, loc := time.Now(), s.loc()
	month := time.Date(now.In(loc).Year(), now.In(loc).Month(), 1, 0, 0, 0, 0, loc)
	out := []core.SearchItem{}
	for _, sv := range servers {
		if has(sv.Name) {
			state, sum, _ := s.serverNow(sv, now)
			it := core.SearchItem{ID: "server:" + itoa64(sv.ID), Title: sv.Name, Kind: "server", URL: "/server/" + itoa64(sv.ID), Score: 3,
				Snippet: serverStateWords[state], Date: sv.LastSeen}
			if state == "up" {
				it.Snippet = dots(it.Snippet, readingWords(sum))
			}
			out = append(out, it)
		}
		checks, err := s.store.Checks(ctx, sv.ID)
		if err != nil {
			return nil, err
		}
		var uptime map[int64]float64
		for _, c := range checks {
			score := 0.0
			switch {
			case has(c.Name):
				score = 4
			case has(c.Group), has(c.Target):
				score = 1
			default:
				continue
			}
			if uptime == nil {
				uptime = s.store.Uptime(ctx, sv.ID, month, loc)
			}
			c = s.checkNow(c)
			it := core.SearchItem{ID: "monitor:" + itoa64(c.ID), Title: c.Name, Kind: "monitor", URL: checkPath(c), Score: score}
			it.Snippet = checkState(c)
			if c.Enabled && c.LastAt > 0 {
				if c.Status != store.CheckDown {
					it.Snippet = dots(it.Snippet, msWords(c.LastMs))
				}
				if v, ok := uptime[c.ID]; ok {
					it.Snippet = dots(it.Snippet, uptimeWords(v)+" this month")
				}
			}
			if at := c.ChangedAt; at > 0 {
				it.Date = time.Unix(at, 0).UTC().Format(time.RFC3339)
			}
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ---------- day ----------

// incident is one thing that went wrong on the day.
type incident struct {
	item    core.DayItem
	outage  bool
	start   time.Time
	summary string // its words in the day's line: "Jellyfin down 12 min"
}

// pocketDay answers Pocket Day: what went down that day, from when to when
// and for how many minutes of the day (uptime checks going down, a server
// that stopped answering), and the other alerts that started then (a disk
// filling up, a hot sensor, a failing drive, a certificate running out). A
// day when nothing went wrong is empty: no line at all. An outage over
// midnight counts on each day for its part of it. The day is the person's
// when the caller says their time zone, else the box's.
func (s *Server) pocketDay(r *http.Request, date string, tz *time.Location) (*core.Day, error) {
	ctx := r.Context()
	if tz == nil {
		tz = s.loc()
	}
	from, err := time.ParseInLocation("2006-01-02", date, tz)
	if err != nil {
		return nil, err
	}
	to := from.AddDate(0, 0, 1)
	now := time.Now()
	if !from.Before(now) {
		return nil, nil
	}
	servers, err := s.store.VisibleServers(ctx, me(r))
	if err != nil || len(servers) == 0 {
		return nil, err
	}
	ids := make([]int64, 0, len(servers))
	for _, sv := range servers {
		ids = append(ids, sv.ID)
	}

	// a stretch as the day sees it: its part of the day, the clock times, and
	// a word when it began before the day, ends after it or still goes on
	var list []incident
	clip := func(it *core.DayItem, start, end time.Time, ongoing bool) (time.Time, float64, bool) {
		a, b := start, end
		if a.Before(from) {
			a = from
		}
		if b.After(to) {
			b = to
		}
		if !b.After(a) {
			return a, 0, false
		}
		it.Time = a.In(tz).Format("15:04")
		edge := ""
		if start.Before(from) {
			edge = "since " + start.In(tz).Format("Jan 2 15:04")
		}
		switch {
		case ongoing:
			edge = dots(edge, "still going on")
		case !end.After(to):
			it.End = end.In(tz).Format("15:04")
		default:
			edge = dots(edge, "until "+end.In(tz).Format("Jan 2 15:04"))
		}
		it.Sub = dots(it.Sub, edge)
		return a, b.Sub(a).Minutes(), true
	}

	// an outage in the day's line: "Jellyfin down 12 min", "NAS offline since
	// 09:40" (today, still going on), "Jellyfin still down", "Jellyfin down all day"
	outageWords := func(name, verb string, start time.Time, mins float64, ongoing bool) string {
		switch {
		case ongoing && to.After(now) && start.Before(from):
			return name + " still " + verb
		case ongoing && to.After(now):
			return name + " " + verb + " since " + start.In(tz).Format("15:04")
		case mins >= to.Sub(from).Minutes()-1:
			return name + " " + verb + " all day"
		}
		return name + " " + verb + " " + lasted(mins)
	}

	// uptime checks: from each going down to its coming back
	names := map[int64]store.Check{}
	for _, sv := range servers {
		checks, err := s.store.Checks(ctx, sv.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range checks {
			names[c.ID] = s.checkNow(c)
		}
	}
	events, err := s.store.CheckEventsBetween(ctx, ids, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(events); i++ {
		e := events[i]
		c, ok := names[e.CheckID]
		if !ok || e.Status != store.CheckDown {
			continue
		}
		start := time.Unix(e.T, 0)
		var end time.Time
		ongoing := false
		if i+1 < len(events) && events[i+1].CheckID == e.CheckID {
			end = time.Unix(events[i+1].T, 0)
		} else if c.Enabled && c.Status == store.CheckDown {
			end, ongoing = now, true
		} else {
			// paused or deleted while down: it was down until it was last checked
			end = time.Unix(c.LastAt, 0)
		}
		it := core.DayItem{ID: "outage:" + itoa64(c.ID) + "-" + strconv.FormatInt(e.T, 10), Title: c.Name + " down", Kind: "outage", Sub: e.Msg, URL: checkPath(c)}
		at, mins, ok := clip(&it, start, end, ongoing)
		if !ok {
			continue
		}
		it.Minutes = math.Round(mins*10) / 10
		list = append(list, incident{item: it, outage: true, start: at, summary: outageWords(c.Name, "down", start, mins, ongoing)})
	}

	// the servers' alerts: a server not answering is an outage; the rest
	// (disk, temperature, drives, containers…) count on the day they began
	alerts, err := s.store.AlertsDuring(ctx, ids, from, to)
	if err != nil {
		return nil, err
	}
	for _, a := range alerts {
		if a.Metric == "check" {
			continue // the check's own ups and downs are above
		}
		start := store.ParseTime(a.StartedAt)
		end, ongoing := now, true
		if a.ResolvedAt != nil {
			end, ongoing = store.ParseTime(*a.ResolvedAt), false
		}
		if a.Metric == "offline" {
			it := core.DayItem{ID: "alert:" + itoa64(a.ID), Title: a.ServerName + " offline", Kind: "outage", URL: alertPath(a.ServerID, a.Metric, a.Target)}
			at, mins, ok := clip(&it, start, end, ongoing)
			if !ok {
				continue
			}
			it.Minutes = math.Round(mins*10) / 10
			list = append(list, incident{item: it, outage: true, start: at, summary: outageWords(a.ServerName, "offline", start, mins, ongoing)})
			continue
		}
		if start.Before(from) {
			continue
		}
		it := core.DayItem{ID: "alert:" + itoa64(a.ID), Title: a.Message, Kind: "alert", Sub: a.ServerName, URL: alertPath(a.ServerID, a.Metric, a.Target)}
		if _, _, ok := clip(&it, start, end, ongoing); !ok {
			continue
		}
		list = append(list, incident{item: it, start: start, summary: a.Message})
	}
	if len(list) == 0 {
		return nil, nil
	}

	sort.SliceStable(list, func(i, j int) bool { return list[i].start.Before(list[j].start) })
	day := &core.Day{Date: date}
	var outages, warnings []string
	down := 0.0
	for _, x := range list {
		day.Items = append(day.Items, x.item)
		if x.outage {
			outages = append(outages, x.summary)
			down += x.item.Minutes
		} else {
			warnings = append(warnings, x.summary)
		}
	}
	// "1 outage: Jellyfin down 12 min", "4 outages: A down 3 min, B down 1 h and 2 more; 1 alert"
	var parts []string
	if n := len(outages); n > 0 {
		shown := outages
		more := ""
		if n > 3 {
			shown, more = outages[:2], fmt.Sprintf(" and %d more", n-2)
		}
		parts = append(parts, countOf(n, "outage", "outages")+": "+strings.Join(shown, ", ")+more)
		day.Metrics = append(day.Metrics, core.Metric{Label: "Down", Value: lasted(down)})
	}
	switch n := len(warnings); {
	case n == 1 && len(outages) == 0:
		parts = append(parts, "1 alert: "+warnings[0])
	case n > 0:
		parts = append(parts, countOf(n, "alert", "alerts"))
	}
	day.Summary = strings.Join(parts, "; ")
	return day, nil
}

// ---------- widget ----------

// pocketWidget answers Pocket Hub: what is down now first (servers that do
// not answer, uptime checks that fail), then the open warnings (a disk nearly
// full, a hot sensor, a failing drive…), the worst first; a line that counts
// them, and CPU, memory, disk and temperature from the last reading of the
// box (the server the app runs on, else the person's first server that
// answers). Someone without a server gets nothing.
func (s *Server) pocketWidget(r *http.Request) (*core.WidgetData, error) {
	ctx := r.Context()
	servers, err := s.store.VisibleServers(ctx, me(r))
	if err != nil || len(servers) == 0 {
		return nil, err
	}
	now := time.Now()
	w := &core.WidgetData{URL: "/"}
	ids := make([]int64, 0, len(servers))
	watched, serversDown, checks, checksDown := 0, 0, 0, 0
	var box *summary
	boxLocal := false
	var downChecks []core.WidgetItem
	for _, sv := range servers {
		ids = append(ids, sv.ID)
		state, sum, seen := s.serverNow(sv, now)
		if state != "paused" {
			watched++
		}
		switch state {
		case "down":
			serversDown++
			detail := "offline"
			if t := seen.In(s.loc()); !seen.IsZero() {
				detail = "offline since " + t.Format("Jan 2 15:04")
				if t.Format("2006-01-02") == now.In(s.loc()).Format("2006-01-02") {
					detail = "offline since " + t.Format("15:04")
				}
			}
			w.Items = append(w.Items, core.WidgetItem{ID: "server:" + itoa64(sv.ID), Title: sv.Name, Detail: detail, URL: "/server/" + itoa64(sv.ID)})
		case "up":
			if sum != nil && (box == nil || (sv.Kind == "local" && !boxLocal)) {
				box, boxLocal = sum, sv.Kind == "local"
			}
		}
		list, err := s.store.Checks(ctx, sv.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range list {
			if !c.Enabled {
				continue
			}
			checks++
			c = s.checkNow(c)
			if c.Status != store.CheckDown || c.LastAt == 0 {
				continue
			}
			checksDown++
			detail := "down"
			if c.ChangedAt > 0 {
				detail = "down for " + lasted(now.Sub(time.Unix(c.ChangedAt, 0)).Minutes())
			}
			downChecks = append(downChecks, core.WidgetItem{ID: "monitor:" + itoa64(c.ID), Title: c.Name, Detail: dots(detail, c.LastMsg), URL: checkPath(c)})
		}
	}
	w.Items = append(w.Items, downChecks...)

	// open warnings; a check down and a server offline are above already
	open, err := s.store.ListAlerts(ctx, ids, true, 50)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(open, func(i, j int) bool { return open[i].Severity == "critical" && open[j].Severity != "critical" })
	warnings := 0
	for _, a := range open {
		if a.Metric == "check" || a.Metric == "offline" {
			continue
		}
		warnings++
		if len(w.Items) < 8 {
			w.Items = append(w.Items, core.WidgetItem{ID: "alert:" + itoa64(a.ID), Title: a.Message, Detail: a.ServerName, URL: alertPath(a.ServerID, a.Metric, a.Target)})
		}
	}
	if len(w.Items) > 8 {
		w.Items = w.Items[:8]
	}

	// "All 42 services up", "2 services down, 1 server offline, 1 warning";
	// servers are counted when there is more than the one, or no check
	var parts []string
	if serversDown == 0 && checksDown == 0 {
		var what []string
		n := 0
		if checks > 0 {
			what, n = append(what, countOf(checks, "service", "services")), checks
		}
		if watched > 1 || (checks == 0 && watched > 0) {
			what, n = append(what, countOf(watched, "server", "servers")), n+watched
		}
		if len(what) > 0 {
			line := strings.Join(what, " and ") + " up"
			if n > 1 {
				line = "All " + line
			}
			parts = append(parts, line)
		}
	}
	if checksDown > 0 {
		parts = append(parts, countOf(checksDown, "service down", "services down"))
	}
	if serversDown > 0 {
		parts = append(parts, countOf(serversDown, "server offline", "servers offline"))
	}
	if warnings > 0 {
		parts = append(parts, countOf(warnings, "warning", "warnings"))
	}
	w.Line = strings.Join(parts, ", ")

	if box != nil {
		w.Metrics = append(w.Metrics, core.Metric{Label: "CPU", Value: fmtValue("cpu", box.CPU)}, core.Metric{Label: "Memory", Value: fmtValue("mem", box.MemPct)})
		if box.FSMount != "" {
			w.Metrics = append(w.Metrics, core.Metric{Label: "Disk", Value: fmtValue("disk", box.FSPct)})
		}
		if box.Temp > 0 {
			w.Metrics = append(w.Metrics, core.Metric{Label: "Temperature", Value: fmtValue("temp", box.Temp)})
		}
	}
	return w, nil
}

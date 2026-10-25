package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/jobs"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// Scheduled jobs: every server whose agent reports them (and this box,
// through the jobs helper at JOBS_URL) is read every JOBS_INTERVAL. Runs are
// counted per day, failures and runs that wrote something are kept, and the
// "job" alert rule fires when a job fails, hangs or stops being scheduled.

func (s *Server) jobRoutes(r chi.Router) {
	r.Get("/servers/{id}/jobs", s.handleJobs)
	r.Get("/servers/{id}/jobs/runs", s.handleJobRuns)
	r.Get("/servers/{id}/jobs/days", s.handleJobDays)
	r.Get("/servers/{id}/jobs/log", s.handleJobLog)
	r.Post("/servers/{id}/jobs/action", s.handleJobAction)
	r.Put("/servers/{id}/jobs/settings", s.handleJobSettings)
}

type jobsState struct {
	mu        sync.Mutex
	status    *jobs.Status
	err       string
	errSince  time.Time
	checked   time.Time
	cursor    int64               // newest run end read (unix ms)
	last      map[string]jobs.Run // newest run per timer
	lastFail  map[string]jobs.Run // newest failed run per timer
	inflight  bool
	available bool // the source answers with jobs at all
}

type jobsHub struct {
	mu     sync.Mutex
	states map[int64]*jobsState
	kick   chan int64
}

func newJobsHub() *jobsHub {
	return &jobsHub{states: map[int64]*jobsState{}, kick: make(chan int64, 16)}
}

func (h *jobsHub) state(id int64) *jobsState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.states[id]
	if st == nil {
		st = &jobsState{last: map[string]jobs.Run{}, lastFail: map[string]jobs.Run{}}
		h.states[id] = st
	}
	return st
}

// jobSource says where a server's jobs come from ("" = nowhere).
func (s *Server) jobSource(sv store.Server) (jobs.Remote, string) {
	switch sv.Kind {
	case "local":
		if s.cfg.JobsURL == "" {
			return jobs.Remote{}, "This box's scheduled jobs are read by the jobs helper, which isn't set up: run `pocket-monitoring jobs-agent` on the host and set JOBS_URL and JOBS_TOKEN here."
		}
		return jobs.Remote{URL: s.cfg.JobsURL, Token: s.cfg.JobsToken}, ""
	case "agent":
		tok, err := s.open(sv.Secret)
		if err != nil {
			return jobs.Remote{}, err.Error()
		}
		return jobs.Remote{URL: sv.URL, Token: tok}, ""
	}
	return jobs.Remote{}, "Glances doesn't report scheduled jobs; run the Pocket Monitoring agent on that machine instead."
}

// jobsLoop reads every server's jobs on JOBS_INTERVAL (or right away when kicked).
func (s *Server) jobsLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.JobsInterval)
	defer t.Stop()
	s.pollAllJobs(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollAllJobs(ctx)
		case id := <-s.jobs.kick:
			if sv, err := s.store.GetServer(ctx, id); err == nil {
				s.pollJobs(ctx, *sv)
			}
		}
	}
}

func (s *Server) kickJobs(id int64) {
	select {
	case s.jobs.kick <- id:
	default:
	}
}

func (s *Server) pollAllJobs(ctx context.Context) {
	all, err := s.store.AllServers(ctx)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, sv := range all {
		if !sv.Enabled || sv.Kind == "glances" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(sv store.Server) {
			defer wg.Done()
			defer func() { <-sem }()
			s.pollJobs(ctx, sv)
		}(sv)
	}
	wg.Wait()
}

func toStoreRun(r jobs.Run) store.JobRun {
	return store.JobRun{Unit: r.Unit, Invocation: r.Invocation, Start: r.Start, End: r.End, OK: r.OK, Result: r.Result, ExitStatus: r.ExitStatus, Summary: r.Summary, Lines: r.Lines, Warnings: r.Warnings}
}

func (s *Server) pollJobs(ctx context.Context, sv store.Server) {
	src, why := s.jobSource(sv)
	st := s.jobs.state(sv.ID)
	st.mu.Lock()
	if st.inflight {
		st.mu.Unlock()
		return
	}
	st.inflight = true
	cursor := st.cursor
	st.mu.Unlock()
	defer func() { st.mu.Lock(); st.inflight = false; st.mu.Unlock() }()

	if why != "" {
		st.mu.Lock()
		st.status, st.err, st.available, st.checked = nil, "", false, time.Now()
		st.mu.Unlock()
		s.evaluateJobs(ctx, sv, st)
		return
	}
	if cursor == 0 {
		cursor = s.store.LastJobRunEnd(ctx, sv.ID)
	}
	since := cursor
	if since > 0 {
		since -= 2 * 60 * 1000 // overlap: a run already stored is skipped by its id
	}
	rctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	status, err := src.Status(rctx, since)
	cancel()
	now := time.Now()
	if err != nil {
		st.mu.Lock()
		noJobs := errors.Is(err, jobs.ErrNoJobs)
		if noJobs {
			st.status, st.available, st.err = nil, false, err.Error()
		} else {
			if st.err == "" {
				st.errSince = now
			}
			st.err = err.Error()
		}
		st.checked = now
		st.mu.Unlock()
		s.log.Debug("read jobs", "server", sv.Name, "err", err)
		s.evaluateJobs(ctx, sv, st)
		return
	}
	runs := make([]store.JobRun, 0, len(status.Runs))
	for _, r := range status.Runs {
		runs = append(runs, toStoreRun(r))
	}
	fresh, err := s.store.RecordJobRuns(ctx, sv.ID, runs, s.loc())
	if err != nil {
		s.log.Warn("store job runs", "server", sv.Name, "err", err)
	}
	st.mu.Lock()
	hadErr := st.err != ""
	st.status, st.err, st.errSince, st.checked, st.available = status, "", time.Time{}, now, true
	for _, r := range status.Runs {
		if r.End > st.cursor {
			st.cursor = r.End
		}
		if r.End >= st.last[r.Unit].End {
			st.last[r.Unit] = r
		}
		if !r.OK && r.End >= st.lastFail[r.Unit].End {
			st.lastFail[r.Unit] = r
		}
	}
	st.mu.Unlock()
	if len(fresh) > 0 || hadErr {
		s.touch()
	}
	s.evaluateJobs(ctx, sv, st)
}

func (s *Server) loc() *time.Location {
	if l, err := time.LoadLocation(s.cfg.Timezone); err == nil {
		return l
	}
	return time.Local
}

// ---------- what's wrong with a job ----------

func jobName(j jobs.Job, set store.JobSettings) string {
	if set.Label != "" {
		return set.Label
	}
	return strings.TrimSuffix(j.Unit, ".timer")
}

func jobRunning(j jobs.Job) bool {
	switch j.State {
	case "active", "activating", "deactivating", "reloading":
		return true
	}
	return false
}

// jobStatus: ok | running | paused | failed | stuck | overdue | unscheduled, and why.
func jobStatus(j jobs.Job, set store.JobSettings, now time.Time, stuck time.Duration, lastFail *jobs.Run) (string, string) {
	name := jobName(j, set)
	if jobRunning(j) {
		if j.LastStart > 0 && now.Sub(time.UnixMilli(j.LastStart)) > stuck {
			return "stuck", fmt.Sprintf("%s has been running for %s", name, fmtDur(now.Sub(time.UnixMilli(j.LastStart))))
		}
		return "running", ""
	}
	if j.Paused || (!j.Enabled && !j.Scheduled) {
		return "paused", ""
	}
	if j.State == "failed" || (j.Result != "" && j.Result != "success") {
		why := fmt.Sprintf("%s failed (%s", name, resultText(j.Result))
		if j.ExitStatus != 0 {
			why += fmt.Sprintf(", exit %d", j.ExitStatus)
		}
		why += ")"
		if lastFail != nil && lastFail.Summary != "" {
			why += ": " + lastFail.Summary
		}
		return "failed", why
	}
	if !j.Scheduled {
		return "unscheduled", fmt.Sprintf("%s is enabled but its timer isn't running, so it won't run again", name)
	}
	if j.Next > 0 && now.Sub(time.UnixMilli(j.Next)) > 15*time.Minute {
		return "overdue", fmt.Sprintf("%s should have run %s ago and didn't", name, fmtDur(now.Sub(time.UnixMilli(j.Next))))
	}
	return "ok", ""
}

func resultText(r string) string {
	switch r {
	case "exit-code":
		return "exited with an error"
	case "signal":
		return "killed by a signal"
	case "timeout":
		return "timed out"
	case "core-dump":
		return "crashed"
	case "oom-kill":
		return "out of memory"
	case "start-limit-hit":
		return "started too often"
	case "resources":
		return "couldn't start"
	case "":
		return "failed"
	}
	return r
}

// evaluateJobs runs the server's job rules against what was just read.
func (s *Server) evaluateJobs(ctx context.Context, sv store.Server, st *jobsState) {
	var rule *store.Rule
	for _, r := range s.rulesFor(ctx, sv.ID) {
		if r.Metric == "job" {
			r := r
			rule = &r
			break
		}
	}
	if rule == nil {
		return
	}
	now := time.Now()
	if !rule.Enabled {
		s.resolveRule(ctx, sv, *rule, nil, now)
		return
	}
	st.mu.Lock()
	status, errMsg, errSince, available := st.status, st.err, st.errSince, st.available
	fails := make(map[string]jobs.Run, len(st.lastFail))
	for k, v := range st.lastFail {
		fails[k] = v
	}
	st.mu.Unlock()
	var conds []condition
	switch {
	case errMsg != "" && available:
		// Can't read the jobs now: keep their alerts as they are, and say so
		// once it has lasted five minutes.
		keep := map[string]bool{"": true}
		s.alerts.mu.Lock()
		for k := range s.alerts.open {
			if k.rule == rule.ID {
				keep[k.inst] = true
			}
		}
		s.alerts.mu.Unlock()
		for inst := range keep {
			if inst != "" {
				conds = append(conds, condition{inst: inst, fire: false, clear: false})
			}
		}
		down := now.Sub(errSince) > 5*time.Minute
		conds = append(conds, condition{inst: "", label: "Can't read the scheduled jobs: " + errMsg, fire: down, clear: false})
	case status != nil:
		settings := s.store.JobSettingsFor(ctx, sv.ID)
		for _, j := range status.Jobs {
			set := settings[j.Unit]
			if set.Muted {
				continue
			}
			var lf *jobs.Run
			if r, ok := fails[j.Unit]; ok {
				lf = &r
			}
			code, why := jobStatus(j, set, now, s.cfg.JobsStuck, lf)
			bad := code == "failed" || code == "stuck" || code == "overdue" || code == "unscheduled"
			conds = append(conds, condition{inst: j.Unit, label: why, fire: bad, clear: !bad})
		}
	}
	s.apply(ctx, sv, *rule, conds, now, nil)
}

// ---------- API ----------

type jobView struct {
	jobs.Job
	Name    string         `json:"name"`
	Label   string         `json:"label"`
	Group   string         `json:"group"`
	Notes   string         `json:"notes"`
	Muted   bool           `json:"muted"`
	Status  string         `json:"status"`
	Problem string         `json:"problem"`
	Week    store.JobStats `json:"week"`
	LastRun *store.JobRun  `json:"last_run"`
	Kept    *store.JobRun  `json:"last_kept"` // newest run that wrote something or failed
}

func (s *Server) jobServer(w http.ResponseWriter, r *http.Request, ownerOnly bool) (*store.Server, bool) {
	sv, role, err := s.store.ServerFor(r.Context(), me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "get server")
		return nil, false
	}
	if ownerOnly && role != "owner" {
		writeErr(w, http.StatusForbidden, "only the server's owner can do that")
		return nil, false
	}
	return sv, true
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.jobServer(w, r, false)
	if !ok {
		return
	}
	ctx := r.Context()
	_, why := s.jobSource(*sv)
	st := s.jobs.state(sv.ID)
	st.mu.Lock()
	status, errMsg, checked, available := st.status, st.err, st.checked, st.available
	last := make(map[string]jobs.Run, len(st.last))
	for k, v := range st.last {
		last[k] = v
	}
	fails := make(map[string]jobs.Run, len(st.lastFail))
	for k, v := range st.lastFail {
		fails[k] = v
	}
	st.mu.Unlock()
	if status == nil && why == "" && errMsg == "" && checked.IsZero() {
		s.pollJobs(ctx, *sv) // first look before the loop got to it
		st.mu.Lock()
		status, errMsg, checked, available = st.status, st.err, st.checked, st.available
		st.mu.Unlock()
	}
	_, role, _ := s.store.ServerFor(ctx, me(r), sv.ID)
	out := map[string]any{"available": available && status != nil, "reason": why, "error": errMsg, "checked_at": store.FormatTime(checked), "owner": role == "owner", "jobs": []jobView{}, "groups": []string{}}
	if status == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	settings := s.store.JobSettingsFor(ctx, sv.ID)
	week := s.store.JobStatsSince(ctx, sv.ID, time.Now().In(s.loc()).AddDate(0, 0, -6).Format("2006-01-02"))
	kept := s.lastKeptRuns(ctx, sv.ID)
	now := time.Now()
	views := make([]jobView, 0, len(status.Jobs))
	groups := map[string]bool{}
	for _, j := range status.Jobs {
		set := settings[j.Unit]
		var lf *jobs.Run
		if x, ok := fails[j.Unit]; ok {
			lf = &x
		}
		code, why := jobStatus(j, set, now, s.cfg.JobsStuck, lf)
		v := jobView{Job: j, Name: jobName(j, set), Label: set.Label, Group: set.Group, Notes: set.Notes, Muted: set.Muted, Status: code, Problem: why, Week: week[j.Unit]}
		if v.Group == "" {
			v.Group = guessGroup(j.Unit)
		}
		groups[v.Group] = true
		if x, ok := last[j.Unit]; ok {
			sr := toStoreRun(x)
			v.LastRun = &sr
		}
		if x, ok := kept[j.Unit]; ok {
			x := x
			v.Kept = &x
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, k int) bool { return strings.ToLower(views[i].Name) < strings.ToLower(views[k].Name) })
	gl := make([]string, 0, len(groups))
	for g := range groups {
		gl = append(gl, g)
	}
	sort.Strings(gl)
	out["jobs"], out["groups"], out["host"], out["now"] = views, gl, status.Host, status.Now
	writeJSON(w, http.StatusOK, out)
}

// lastKeptRuns returns each job's newest stored run that failed or wrote something.
func (s *Server) lastKeptRuns(ctx context.Context, serverID int64) map[string]store.JobRun {
	out := map[string]store.JobRun{}
	rows, err := s.store.DB.QueryContext(ctx, `SELECT unit, invocation, started_ms, ended_ms, ok, result, exit_status, summary, lines, warnings FROM job_runs r
		WHERE server_id = ? AND (ok = 0 OR lines > 0) AND ended_ms = (SELECT MAX(ended_ms) FROM job_runs x WHERE x.server_id = r.server_id AND x.unit = r.unit AND (x.ok = 0 OR x.lines > 0))`, serverID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var x store.JobRun
		var ok int
		if rows.Scan(&x.Unit, &x.Invocation, &x.Start, &x.End, &ok, &x.Result, &x.ExitStatus, &x.Summary, &x.Lines, &x.Warnings) == nil {
			x.OK = ok == 1
			out[x.Unit] = x
		}
	}
	return out
}

var groupGuesses = []struct {
	group string
	re    *regexp.Regexp
}{
	{"Music", regexp.MustCompile(`(?i)music|lyric|ambient|slskd|soulseek|lidarr|bin-audit|library-twins|rip-import|navidrome|beets`)},
	{"Books", regexp.MustCompile(`(?i)book|ebook|chaptarr|readarr|calibre|kavita|comic|manga`)},
	{"Video & podcasts", regexp.MustCompile(`(?i)pinchflat|metube|youtube|podcast|feed|jellyfin|plex|emby`)},
	{"Downloads & media", regexp.MustCompile(`(?i)arr\b|arr-|sonarr|radarr|seerr|overseerr|recyclarr|prowlarr|import|download|stalled|search|torrent|sab|qbit`)},
	{"Backups", regexp.MustCompile(`(?i)backup|restic|borg|rsync|snapshot|sync`)},
	{"Games", regexp.MustCompile(`(?i)rom|game`)},
	{"System", regexp.MustCompile(`(?i)watchdog|wan|ip-|nas|mount|smart|temp|tmp|clean|sweep|update|link|cert|disk|scrub|health`)},
}

// guessGroup files a job by its name until someone picks a group.
func guessGroup(unit string) string {
	name := strings.TrimSuffix(unit, ".timer")
	for _, g := range groupGuesses {
		if g.re.MatchString(name) {
			return g.group
		}
	}
	return "Other"
}

func (s *Server) handleJobRuns(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.jobServer(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	unit := strings.TrimSpace(q.Get("unit"))
	runs, err := s.store.JobRuns(r.Context(), sv.ID, unit, queryBool(r, "failed"), queryBool(r, "busy") || unit == "", queryInt64(r, "before"), queryInt(r, "limit"))
	if err != nil {
		s.fail(w, err, "job runs")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleJobDays(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.jobServer(w, r, false)
	if !ok {
		return
	}
	days := queryInt(r, "days")
	if days <= 0 || days > 366 {
		days = 30
	}
	from := time.Now().In(s.loc()).AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	writeJSON(w, http.StatusOK, s.store.JobDays(r.Context(), sv.ID, strings.TrimSpace(r.URL.Query().Get("unit")), from))
}

// handleJobLog reads a job's journal. Owner only, like container logs.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.jobServer(w, r, true)
	if !ok {
		return
	}
	src, why := s.jobSource(*sv)
	if why != "" {
		writeErr(w, http.StatusNotImplemented, why)
		return
	}
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	lines, err := src.Log(ctx, q.Get("unit"), q.Get("invocation"), queryInt(r, "tail"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) handleJobAction(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.jobServer(w, r, true)
	if !ok {
		return
	}
	var in struct {
		Unit   string `json:"unit"`
		Action string `json:"action"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Action != "run" && in.Action != "pause" && in.Action != "resume" {
		writeErr(w, http.StatusBadRequest, "the action is run, pause or resume")
		return
	}
	src, why := s.jobSource(*sv)
	if why != "" {
		writeErr(w, http.StatusNotImplemented, why)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	j, err := src.Act(ctx, in.Unit, in.Action)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.log.Info("job action", "server", sv.Name, "unit", in.Unit, "action", in.Action, "by", me(r))
	s.pollJobs(ctx, *sv) // the list shows the new state right away
	// …and again shortly: a started run shows as running, then as its result.
	go func(id int64) {
		for _, d := range []time.Duration{5 * time.Second, 20 * time.Second} {
			time.Sleep(d)
			s.kickJobs(id)
		}
	}(sv.ID)
	s.touch()
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleJobSettings(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.jobServer(w, r, true)
	if !ok {
		return
	}
	var in struct {
		Unit  string  `json:"unit"`
		Group *string `json:"group"`
		Label *string `json:"label"`
		Notes *string `json:"notes"`
		Muted *bool   `json:"muted"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Unit) == "" {
		writeErr(w, http.StatusBadRequest, "unit is required")
		return
	}
	set := s.store.JobSettingsFor(r.Context(), sv.ID)[in.Unit]
	set.Unit = in.Unit
	if in.Group != nil {
		set.Group = clipStr(strings.TrimSpace(*in.Group), 60)
	}
	if in.Label != nil {
		set.Label = clipStr(strings.TrimSpace(*in.Label), 80)
	}
	if in.Notes != nil {
		set.Notes = clipStr(strings.TrimSpace(*in.Notes), 2000)
	}
	if in.Muted != nil {
		set.Muted = *in.Muted
	}
	if err := s.store.SaveJobSettings(r.Context(), sv.ID, set); err != nil {
		s.fail(w, err, "save job")
		return
	}
	s.evaluateJobs(r.Context(), *sv, s.jobs.state(sv.ID))
	s.touch()
	writeJSON(w, http.StatusOK, set)
}

func clipStr(v string, n int) string {
	r := []rune(v)
	if len(r) > n {
		return string(r[:n])
	}
	return v
}

// Package jobs reads a machine's scheduled jobs — systemd timers and the
// services they start — and what each run did, from systemctl and the
// journal. It runs where systemd is: the agent, or the small native helper
// (`pocket-monitoring jobs-agent`) next to a containerised hub.
package jobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Job is one timer and the service it starts.
type Job struct {
	Unit        string   `json:"unit"`    // backup.timer
	Service     string   `json:"service"` // backup.service
	Scope       string   `json:"scope"`   // user | system
	Description string   `json:"description"`
	Schedule    []string `json:"schedule"` // "OnCalendar=*-*-* 04:15:00", "OnUnitActiveSec=5min"
	Enabled     bool     `json:"enabled"`  // starts at boot
	FileState   string   `json:"file_state"`
	Scheduled   bool     `json:"scheduled"`
	Next        int64    `json:"next"`         // unix ms, 0 = nothing planned
	LastTrigger int64    `json:"last_trigger"` // unix ms
	State       string   `json:"state"`        // the service: inactive | activating | active | failed | deactivating
	Result      string   `json:"result"`       // success | exit-code | signal | timeout | core-dump | …
	ExitStatus  int      `json:"exit_status"`
	LastStart   int64    `json:"last_start"` // unix ms
	LastEnd     int64    `json:"last_end"`   // unix ms
	Control     bool     `json:"control"`    // this reader can run and pause it
	Paused      bool     `json:"paused"`     // paused here: the timer is kept stopped
}

// Run is one finished run of a job's service.
type Run struct {
	Unit       string `json:"unit"` // the timer
	Invocation string `json:"invocation"`
	Start      int64  `json:"start"` // unix ms (0 when the start fell outside the window)
	End        int64  `json:"end"`
	OK         bool   `json:"ok"`
	Result     string `json:"result"` // success | exit-code | …
	ExitStatus int    `json:"exit_status"`
	Summary    string `json:"summary"`  // the last line it wrote
	Lines      int    `json:"lines"`    // lines it wrote
	Warnings   int    `json:"warnings"` // of those, at warning level or worse
}

// Status is what a reader returns: the jobs now and the runs that ended after `since`.
type Status struct {
	Host string `json:"host"`
	Now  int64  `json:"now"`
	Jobs []Job  `json:"jobs"`
	Runs []Run  `json:"runs"`
}

// LogLine is one journal line of a job.
type LogLine struct {
	T          int64  `json:"t"` // unix ms
	Priority   int    `json:"p"`
	Msg        string `json:"m"`
	Invocation string `json:"i"`
	System     bool   `json:"sys,omitempty"` // written by systemd, not the job
}

// Options choose which jobs a reader sees.
type Options struct {
	User      bool // the user's own timers (systemctl --user)
	System    bool // system timers
	AllSystem bool // also the distribution's (apt, logrotate…), not only /etc and /usr/local
	Exclude   []string
	StateFile string // where paused jobs are remembered ("" = the default)
}

// Reader reads jobs through systemctl and journalctl.
//
// Pausing never touches unit files: `systemctl disable` deletes a unit that
// is a symlink into the unit directory (a common way to keep units in a git
// checkout), so a pause stops the timer and is remembered in StateFile; the
// reader stops it again whenever it finds it running (after a reboot).
type Reader struct {
	Opts   Options
	root   bool
	mu     sync.Mutex
	paused map[string]bool // scope + ":" + timer
}

// New returns a reader, or an error when systemd isn't here.
func New(o Options) (*Reader, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, errors.New("systemd isn't available here (no systemctl)")
	}
	r := &Reader{Opts: o, root: os.Geteuid() == 0, paused: map[string]bool{}}
	if r.Opts.StateFile == "" {
		r.Opts.StateFile = defaultStateFile(r.root)
	}
	if b, err := os.ReadFile(r.Opts.StateFile); err == nil {
		var list []string
		if json.Unmarshal(b, &list) == nil {
			for _, k := range list {
				r.paused[k] = true
			}
		}
	}
	return r, nil
}

func defaultStateFile(root bool) string {
	if root {
		return "/var/lib/pocket-monitoring/paused-jobs.json"
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "pocket-monitoring", "paused-jobs.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "pocket-monitoring", "paused-jobs.json")
}

func (r *Reader) isPaused(scope, unit string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused[scope+":"+unit]
}

func (r *Reader) setPaused(scope, unit string, on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := scope + ":" + unit
	if on {
		r.paused[k] = true
	} else {
		delete(r.paused, k)
	}
	list := make([]string, 0, len(r.paused))
	for x := range r.paused {
		list = append(list, x)
	}
	sort.Strings(list)
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.MkdirAll(filepath.Dir(r.Opts.StateFile), 0o755); err != nil {
		return err
	}
	tmp := r.Opts.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.Opts.StateFile)
}

// OptionsFromScope turns JOBS_SCOPE (auto | user | system | both | off) into options.
func OptionsFromScope(scope string, allSystem bool, exclude []string) (Options, bool) {
	o := Options{AllSystem: allSystem, Exclude: exclude}
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "off", "none", "false":
		return o, false
	case "user":
		o.User = true
	case "system":
		o.System = true
	case "both", "all":
		o.User, o.System = true, true
	default: // auto: root watches the system's timers, anyone else their own and the admin's
		o.System = true
		o.User = os.Geteuid() != 0
	}
	return o, true
}

const lookback = 2 * time.Hour // how far back a run's start and output are searched

func (r *Reader) scopes() []string {
	var out []string
	if r.Opts.User {
		out = append(out, "user")
	}
	if r.Opts.System {
		out = append(out, "system")
	}
	return out
}

func scopeArgs(scope string) []string {
	if scope == "user" {
		return []string{"--user"}
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return out, nil
}

// Status lists the jobs and the runs that ended after since (unix ms; 0 = the last day).
func (r *Reader) Status(ctx context.Context, since int64) (*Status, error) {
	st := &Status{Now: time.Now().UnixMilli(), Jobs: []Job{}, Runs: []Run{}}
	st.Host, _ = os.Hostname()
	if since <= 0 || since < time.Now().Add(-7*24*time.Hour).UnixMilli() {
		since = time.Now().Add(-24 * time.Hour).UnixMilli()
	}
	var errs []string
	for _, scope := range r.scopes() {
		jobs, err := r.list(ctx, scope)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		st.Jobs = append(st.Jobs, jobs...)
		if len(jobs) == 0 {
			continue
		}
		runs, err := r.runs(ctx, scope, jobs, since)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		st.Runs = append(st.Runs, runs...)
	}
	if len(st.Jobs) == 0 && len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	sort.Slice(st.Jobs, func(i, j int) bool { return st.Jobs[i].Unit < st.Jobs[j].Unit })
	sort.Slice(st.Runs, func(i, j int) bool { return st.Runs[i].End < st.Runs[j].End })
	return st, nil
}

type timerRow struct {
	Unit      string `json:"unit"`
	Activates string `json:"activates"`
	Next      int64  `json:"next"` // unix µs, also for monotonic timers (OnUnitActiveSec)
	Last      int64  `json:"last"`
}

// list reads one scope's timers and their services.
func (r *Reader) list(ctx context.Context, scope string) ([]Job, error) {
	out, err := run(ctx, "systemctl", append(scopeArgs(scope), "list-timers", "--all", "--no-pager", "-o", "json")...)
	if err != nil {
		return nil, err
	}
	var rows []timerRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("list-timers: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	units := []string{}
	for _, t := range rows {
		if t.Unit == "" || t.Activates == "" || r.excluded(t.Unit) {
			continue
		}
		units = append(units, t.Unit, t.Activates)
	}
	if len(units) == 0 {
		return nil, nil
	}
	props := "Id,Description,UnitFileState,ActiveState,SubState,Result,ExecMainStatus,ExecMainStartTimestamp,ExecMainExitTimestamp,NextElapseUSecRealtime,LastTriggerUSec,TimersCalendar,TimersMonotonic,FragmentPath"
	out, err = run(ctx, "systemctl", append(append(scopeArgs(scope), "show", "--no-pager", "--timestamp=unix", "-p", props), units...)...)
	if err != nil {
		return nil, err
	}
	blocks := parseShow(out)
	by := map[string]map[string][]string{}
	for _, b := range blocks {
		if id := first(b, "Id"); id != "" {
			by[id] = b
		}
	}
	var jobs []Job
	for _, t := range rows {
		tb, sb := by[t.Unit], by[t.Activates]
		if tb == nil || sb == nil {
			continue
		}
		if scope == "system" && !r.Opts.AllSystem && !adminMade(first(tb, "FragmentPath")) {
			continue
		}
		j := Job{Unit: t.Unit, Service: t.Activates, Scope: scope}
		j.Description = first(sb, "Description")
		if j.Description == "" || j.Description == t.Activates {
			j.Description = first(tb, "Description")
		}
		ufs := first(tb, "UnitFileState")
		j.FileState = ufs
		j.Enabled = ufs == "enabled" || ufs == "enabled-runtime" || ufs == "static" || ufs == "linked"
		j.Scheduled = first(tb, "ActiveState") == "active"
		j.Next = unixMs(first(tb, "NextElapseUSecRealtime"))
		if j.Next == 0 && t.Next > 0 {
			j.Next = t.Next / 1000
		}
		j.LastTrigger = unixMs(first(tb, "LastTriggerUSec"))
		if j.LastTrigger == 0 && t.Last > 0 {
			j.LastTrigger = t.Last / 1000
		}
		for _, k := range []string{"TimersCalendar", "TimersMonotonic"} {
			for _, v := range tb[k] {
				if s := scheduleOf(v); s != "" {
					j.Schedule = append(j.Schedule, s)
				}
			}
		}
		j.State = first(sb, "ActiveState")
		j.Result = first(sb, "Result")
		j.ExitStatus, _ = strconv.Atoi(first(sb, "ExecMainStatus"))
		j.LastStart = unixMs(first(sb, "ExecMainStartTimestamp"))
		j.LastEnd = unixMs(first(sb, "ExecMainExitTimestamp"))
		j.Control = scope == "user" || r.root
		if j.Paused = r.isPaused(scope, j.Unit); j.Paused && j.Scheduled && j.Control {
			// paused here but running again (a reboot, someone started it): stop it
			if _, err := run(ctx, "systemctl", append(scopeArgs(scope), "stop", j.Unit)...); err == nil {
				j.Scheduled, j.Next = false, 0
			}
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (r *Reader) excluded(unit string) bool {
	for _, x := range r.Opts.Exclude {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if ok, _ := pathMatch(x, unit); ok || x == strings.TrimSuffix(unit, ".timer") {
			return true
		}
	}
	return false
}

func pathMatch(pattern, name string) (bool, error) {
	re := "^" + strings.ReplaceAll(strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*"), `\?`, ".") + "$"
	return regexp.MatchString(re, name)
}

// adminMade: a timer someone put on this machine, not one the distribution ships.
func adminMade(path string) bool {
	return strings.HasPrefix(path, "/etc/") || strings.HasPrefix(path, "/usr/local/") || strings.HasPrefix(path, "/run/systemd/transient")
}

// parseShow splits `systemctl show` output into one map per unit (repeated keys kept).
func parseShow(out []byte) []map[string][]string {
	var blocks []map[string][]string
	cur := map[string][]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				blocks = append(blocks, cur)
				cur = map[string][]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			cur[k] = append(cur[k], v)
		}
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks
}

func first(b map[string][]string, k string) string {
	if v := b[k]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// unixMs reads "@1761372825" (--timestamp=unix); "" or "n/a" is 0.
func unixMs(v string) int64 {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "@"))
	if v == "" || v == "0" || v == "n/a" {
		return 0
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return int64(f * 1000)
	}
	return 0
}

var schedRe = regexp.MustCompile(`\{\s*([A-Za-z]+)=([^;]*?)\s*;`)

// scheduleOf turns "{ OnCalendar=*-*-* 04:15:00 ; next_elapse=… }" into "OnCalendar=*-*-* 04:15:00".
func scheduleOf(v string) string {
	m := schedRe.FindStringSubmatch(v)
	if m == nil {
		return ""
	}
	return m[1] + "=" + strings.TrimSpace(m[2])
}

// ---------- the journal ----------

// Message ids systemd's own lines carry (see systemd's catalog).
const (
	msgStarting    = "7d4958e842da4a758f6c1cdc7b36dcc5"
	msgJobDone     = "39f53479d3a045ac8e11786248231fbf" // Finished / Failed to start (JOB_RESULT)
	msgSucceeded   = "7ad2d189f7e94e70a38c781354912448" // Deactivated successfully
	msgFailed      = "d9b373ed55a64feb8242e02dbe79a49c" // Failed with result …
	msgProcessExit = "98e322203f7a4ed290d09fe03c09fe15" // Main process exited, status=…
	msgResources   = "ae8f7b866b0347b9af31fe1c80b127c0" // Consumed … CPU time
)

type entry struct {
	Realtime  string `json:"__REALTIME_TIMESTAMP"`
	Message   any    `json:"MESSAGE"` // a string, or bytes as a number array
	MessageID string `json:"MESSAGE_ID"`
	Priority  string `json:"PRIORITY"`
	// the unit a line is about (written by systemd) …
	UserUnit string `json:"USER_UNIT"`
	Unit     string `json:"UNIT"`
	UserInv  string `json:"USER_INVOCATION_ID"`
	Inv      string `json:"INVOCATION_ID"`
	// … or the unit that wrote it
	SysUserUnit string `json:"_SYSTEMD_USER_UNIT"`
	SysUnit     string `json:"_SYSTEMD_UNIT"`
	SysInv      string `json:"_SYSTEMD_INVOCATION_ID"`
	JobResult   string `json:"JOB_RESULT"`
	UnitResult  string `json:"UNIT_RESULT"`
	ExitStatus  string `json:"EXIT_STATUS"`
}

func (e entry) text() string {
	switch m := e.Message.(type) {
	case string:
		return m
	case []any:
		b := make([]byte, 0, len(m))
		for _, x := range m {
			if f, ok := x.(float64); ok {
				b = append(b, byte(f))
			}
		}
		return string(b)
	}
	return ""
}

func (e entry) ms() int64 {
	us, _ := strconv.ParseInt(e.Realtime, 10, 64)
	return us / 1000
}

// about returns the service a line concerns, its invocation, and whether systemd wrote it.
func (e entry) about(scope string) (unit, inv string, sys bool) {
	if scope == "user" {
		if e.UserUnit != "" {
			return e.UserUnit, e.UserInv, true
		}
		return e.SysUserUnit, e.SysInv, false
	}
	if e.Unit != "" {
		return e.Unit, e.Inv, true
	}
	return e.SysUnit, e.SysInv, false
}

var outputFields = "MESSAGE,MESSAGE_ID,PRIORITY,USER_UNIT,UNIT,USER_INVOCATION_ID,INVOCATION_ID,_SYSTEMD_USER_UNIT,_SYSTEMD_UNIT,_SYSTEMD_INVOCATION_ID,JOB_RESULT,UNIT_RESULT,EXIT_STATUS"

func (r *Reader) journal(ctx context.Context, scope string, services []string, from time.Time, extra ...string) ([]entry, error) {
	args := append(scopeArgs(scope), "-o", "json", "--no-pager", "--output-fields="+outputFields, "--since=@"+strconv.FormatInt(from.Unix(), 10))
	args = append(args, extra...)
	for i, s := range services {
		if i > 0 {
			args = append(args, "+")
		}
		if scope == "user" {
			args = append(args, "_SYSTEMD_USER_UNIT="+s, "+", "USER_UNIT="+s)
		} else {
			args = append(args, "_SYSTEMD_UNIT="+s, "+", "UNIT="+s)
		}
	}
	out, err := run(ctx, "journalctl", args...)
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return parseEntries(out), nil
}

func parseEntries(out []byte) []entry {
	var list []entry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 256<<10), 16<<20)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			list = append(list, e)
		}
	}
	return list
}

type building struct {
	Run
	done    bool
	jobDone bool
}

// runs rebuilds the finished runs that ended after since.
func (r *Reader) runs(ctx context.Context, scope string, jobs []Job, since int64) ([]Run, error) {
	timerOf := map[string]string{}
	active := map[string]bool{} // services running right now
	services := []string{}
	for _, j := range jobs {
		timerOf[j.Service] = j.Unit
		services = append(services, j.Service)
		if j.State == "active" || j.State == "activating" || j.State == "deactivating" || j.State == "reloading" {
			active[j.Service] = true
		}
	}
	from := time.UnixMilli(since).Add(-lookback)
	list, err := r.journal(ctx, scope, services, from)
	if err != nil {
		return nil, err
	}
	return assemble(list, scope, timerOf, active, since), nil
}

// assemble groups journal lines into runs by invocation id.
func assemble(list []entry, scope string, timerOf map[string]string, active map[string]bool, since int64) []Run {
	by := map[string]*building{}
	order := []string{}
	for _, e := range list {
		unit, inv, sys := e.about(scope)
		timer, ok := timerOf[unit]
		if !ok || inv == "" {
			continue
		}
		b := by[inv]
		if b == nil {
			b = &building{Run: Run{Unit: timer, Invocation: inv, OK: true, Result: "success"}}
			by[inv] = b
			order = append(order, inv)
		}
		t := e.ms()
		if !sys {
			msg := strings.TrimSpace(e.text())
			if msg == "" {
				continue
			}
			b.Lines++
			if p, err := strconv.Atoi(e.Priority); err == nil && p <= 4 {
				b.Warnings++
			}
			b.Summary = clip(msg, 300)
			continue
		}
		switch e.MessageID {
		case msgStarting:
			b.Start = t
		case msgJobDone:
			b.jobDone = true
			if e.JobResult != "" && e.JobResult != "done" {
				b.OK = false
				if b.Result == "success" {
					b.Result = e.JobResult
				}
				b.done, b.End = true, t
			} else if t > b.End {
				b.End = t
			}
		case msgSucceeded, msgResources:
			b.done = true
			if t > b.End {
				b.End = t
			}
		case msgFailed:
			b.done, b.OK = true, false
			if e.UnitResult != "" {
				b.Result = e.UnitResult
			} else if b.Result == "success" {
				b.Result = "failed"
			}
			if t > b.End {
				b.End = t
			}
		case msgProcessExit:
			if n, err := strconv.Atoi(e.ExitStatus); err == nil {
				b.ExitStatus = n
			}
		}
	}
	var out []Run
	for _, inv := range order {
		b := by[inv]
		// A oneshot's "Finished" is its end; the same line for a long-running
		// service only means it started, so wait while that service still runs.
		if !b.done && !(b.jobDone && !active[serviceOf(b.Unit, timerOf)]) {
			continue
		}
		if b.End <= since {
			continue
		}
		if b.ExitStatus != 0 && b.OK && b.Result == "success" {
			b.OK = false
			b.Result = "exit-code"
		}
		out = append(out, b.Run)
	}
	return out
}

func serviceOf(timer string, timerOf map[string]string) string {
	for s, t := range timerOf {
		if t == timer {
			return s
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// Log returns a job's journal lines: one run (invocation) or the last tail lines.
func (r *Reader) Log(ctx context.Context, unit, invocation string, tail int) ([]LogLine, error) {
	j, err := r.find(ctx, unit)
	if err != nil {
		return nil, err
	}
	if tail <= 0 || tail > 5000 {
		tail = 500
	}
	from := time.Now().Add(-30 * 24 * time.Hour)
	var list []entry
	if invocation != "" {
		if !invRe.MatchString(invocation) {
			return nil, errors.New("that is not an invocation id")
		}
		list, err = r.journal(ctx, j.Scope, []string{j.Service}, from)
	} else {
		list, err = r.journal(ctx, j.Scope, []string{j.Service}, from, "-n", strconv.Itoa(tail))
	}
	if err != nil {
		return nil, err
	}
	out := []LogLine{}
	for _, e := range list {
		_, inv, sys := e.about(j.Scope)
		if invocation != "" && inv != invocation {
			continue
		}
		msg := e.text()
		if sys && e.MessageID == "" && msg == "" {
			continue
		}
		p, _ := strconv.Atoi(e.Priority)
		out = append(out, LogLine{T: e.ms(), Priority: p, Msg: clip(msg, 16<<10), Invocation: inv, System: sys})
	}
	if len(out) > tail {
		out = out[len(out)-tail:]
	}
	return out, nil
}

var invRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// find looks a timer up among the jobs this reader sees (nothing else can be touched).
func (r *Reader) find(ctx context.Context, unit string) (*Job, error) {
	if !strings.HasSuffix(unit, ".timer") {
		unit += ".timer"
	}
	for _, scope := range r.scopes() {
		jobs, err := r.list(ctx, scope)
		if err != nil {
			continue
		}
		for i := range jobs {
			if jobs[i].Unit == unit {
				return &jobs[i], nil
			}
		}
	}
	return nil, ErrUnknown
}

// ErrUnknown: no such job here.
var ErrUnknown = errors.New("no such job")

// ErrNotAllowed: the job is visible but this reader may not change it.
var ErrNotAllowed = errors.New("this job belongs to the system; only an agent running as root can run or pause it")

// Act runs a job now, or pauses / resumes its timer.
func (r *Reader) Act(ctx context.Context, unit, action string) (*Job, error) {
	j, err := r.find(ctx, unit)
	if err != nil {
		return nil, err
	}
	if !j.Control {
		return nil, ErrNotAllowed
	}
	sc := scopeArgs(j.Scope)
	switch action {
	case "run":
		_, err = run(ctx, "systemctl", append(sc, "start", "--no-block", j.Service)...)
	case "pause":
		if err = r.setPaused(j.Scope, j.Unit, true); err == nil {
			_, err = run(ctx, "systemctl", append(sc, "stop", j.Unit)...)
		}
	case "resume":
		if err = r.setPaused(j.Scope, j.Unit, false); err == nil {
			if j.Enabled {
				_, err = run(ctx, "systemctl", append(sc, "start", j.Unit)...)
			} else { // disabled by hand: enabling only adds the boot link
				_, err = run(ctx, "systemctl", append(sc, "enable", "--now", j.Unit)...)
			}
		}
	default:
		return nil, errors.New("the action is run, pause or resume")
	}
	if err != nil {
		return nil, err
	}
	return r.find(ctx, j.Unit)
}

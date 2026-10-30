package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/jobs"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// ---- Pocket apps' own scheduled jobs (docs/jobs.md) ----
//
// The box's server (kind local) shows, next to its systemd timers, the jobs
// Pocket apps run themselves. Monitoring asks each app through a Pocket Core
// app link acting as the server's owner, so they see their own jobs (and the
// box's only when they are an admin there). Their units are
// "app:<slug>:<job id>"; pausing stays in the app's own settings.

// jobApps are the apps that implement the jobs contract.
var jobApps = []struct{ slug, name string }{
	{"money", "Pocket Money"},
	{"backups", "Pocket Backups"},
}

func jobAppUses() []core.Use {
	out := make([]core.Use, 0, len(jobApps))
	for _, a := range jobApps {
		out = append(out, core.Use{App: a.slug, Scopes: []string{"jobs:read", "jobs:run"}, Reason: "Show its scheduled jobs on the Jobs tab next to the server's timers, and run one now"})
	}
	return out
}

type appJob struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Schedule    string  `json:"schedule"`
	Enabled     bool    `json:"enabled"`
	Next        int64   `json:"next"`
	Running     bool    `json:"running"`
	Started     int64   `json:"started"`
	Last        *appRun `json:"last"`
	URL         string  `json:"url"`
	CanRun      bool    `json:"can_run"`
}

type appRun struct {
	Job     string `json:"job"`
	ID      string `json:"id"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
}

func appUnit(slug, id string) string { return "app:" + slug + ":" + id }

func parseAppUnit(unit string) (slug, id string, ok bool) {
	rest, found := strings.CutPrefix(unit, "app:")
	if !found {
		return "", "", false
	}
	slug, id, ok = strings.Cut(rest, ":")
	return slug, id, ok && slug != "" && id != ""
}

var appClient = &http.Client{Timeout: 30 * time.Second}

// appCall makes one contract call in an app as the server's owner.
func (s *Server) appCall(ctx context.Context, sv store.Server, slug, scope, method, path string, out any) (*core.LinkToken, error) {
	if !s.core.Configured() {
		return nil, core.ErrNotConfigured
	}
	u, err := s.store.GetUser(ctx, sv.UserID)
	if err != nil {
		return nil, err
	}
	tok, err := s.core.ActAs(ctx, u.Email, slug, scope)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(tok.URL, "/")+path, nil)
	if err != nil {
		return tok, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	res, err := appClient.Do(req)
	if err != nil {
		return tok, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return tok, errors.New(e.Error)
	}
	if out != nil {
		return tok, json.Unmarshal(raw, out)
	}
	return tok, nil
}

// skipApp: the app is not on this box, doesn't offer jobs yet, or the link is off.
func skipApp(err error) bool {
	var le *core.LinkError
	return errors.Is(err, core.ErrNotConfigured) || (errors.As(err, &le) && le.Reason != "unknown_user")
}

// appJobs reads every job app's jobs for a server, and their runs that
// ended after what is stored. Errors are per app ("Pocket Money: …").
func (s *Server) appJobs(ctx context.Context, sv store.Server) ([]jobs.Job, []jobs.Run, []string) {
	var js []jobs.Job
	var rs []jobs.Run
	var errs []string
	for _, a := range jobApps {
		since := s.lastAppRunEnd(ctx, sv.ID, a.slug)
		if since > 0 {
			since -= 2 * 60 * 1000
		}
		var ans struct {
			Jobs []appJob `json:"jobs"`
			Runs []appRun `json:"runs"`
		}
		actx, cancel := context.WithTimeout(ctx, 30*time.Second)
		tok, err := s.appCall(actx, sv, a.slug, "jobs:read", http.MethodGet, "/api/v1/scheduled-jobs?since="+strconv.FormatInt(since, 10), &ans)
		cancel()
		if err != nil {
			if !skipApp(err) {
				errs = append(errs, a.name+": "+err.Error())
			}
			continue
		}
		public := strings.TrimRight(firstNonEmpty(tok.PublicURL, tok.URL), "/")
		for _, aj := range ans.Jobs {
			js = append(js, toJob(a.slug, a.name, public, aj))
		}
		for _, r := range ans.Runs {
			rs = append(rs, jobs.Run{Unit: appUnit(a.slug, r.Job), Invocation: appUnit(a.slug, r.ID), Start: r.Start, End: r.End, OK: r.OK, Result: map[bool]string{true: "success", false: "failed"}[r.OK], Summary: clipStr(r.Summary, 500), Lines: min(len(r.Summary), 1)}) // a summary counts as output, so the run is kept like one that wrote something
		}
	}
	return js, rs, errs
}

func toJob(slug, name, public string, aj appJob) jobs.Job {
	j := jobs.Job{Unit: appUnit(slug, aj.ID), Scope: "app", Name: aj.Name, App: slug, AppName: name, Description: aj.Description,
		Enabled: aj.Enabled, Scheduled: aj.Enabled, Next: aj.Next, Control: aj.CanRun, Paused: !aj.Enabled, State: "inactive", FileState: "app"}
	if aj.Schedule != "" {
		j.Schedule = []string{aj.Schedule}
	}
	if aj.URL != "" {
		j.URL = aj.URL
		if strings.HasPrefix(aj.URL, "/") && public != "" {
			j.URL = public + aj.URL
		}
	}
	if aj.Last != nil {
		j.LastTrigger, j.LastStart, j.LastEnd = aj.Last.Start, aj.Last.Start, aj.Last.End
		if aj.Last.OK {
			j.Result = "success"
		} else {
			j.Result, j.State = "failed", "failed"
		}
	}
	if aj.Running {
		j.State, j.LastStart = "active", aj.Started
	}
	return j
}

func (s *Server) lastAppRunEnd(ctx context.Context, serverID int64, slug string) int64 {
	var n *int64
	_ = s.store.DB.QueryRowContext(ctx, `SELECT MAX(ended_ms) FROM job_runs WHERE server_id = ? AND unit LIKE ?`, serverID, "app:"+slug+":%").Scan(&n)
	if n == nil {
		return 0
	}
	return *n
}

// appJobAction runs an app's job now; pausing is the app's own setting.
func (s *Server) appJobAction(ctx context.Context, sv store.Server, unit, action string) error {
	slug, id, ok := parseAppUnit(unit)
	if !ok {
		return errors.New("unknown job")
	}
	name := slug
	for _, a := range jobApps {
		if a.slug == slug {
			name = a.name
		}
	}
	if action != "run" {
		return fmt.Errorf("switch this job off or on in %s's own settings", name)
	}
	_, err := s.appCall(ctx, sv, slug, "jobs:run", http.MethodPost, "/api/v1/scheduled-jobs/"+id+"/run", nil)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/config"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/jobs"
)

// AgentAuthorized checks the bearer token (or Basic password) against tok.
func AgentAuthorized(r *http.Request, tok string) bool {
	got := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		got = strings.TrimSpace(h[7:])
	} else if _, pass, ok := r.BasicAuth(); ok {
		got = pass
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1
}

// RunAgent serves this machine's readings to a Pocket Monitoring hub: no
// database, no UI, one endpoint. It reads every interval on its own so the
// rates are smooth whatever the hub's polling interval is. With jobsOnly it
// is the jobs helper: only the scheduled jobs, for a hub in a container on
// the same box (which can't reach systemd itself).
func RunAgent(ctx context.Context, cfg *config.Config, log *slog.Logger, jobsOnly bool) error {
	if cfg.AgentToken == "" {
		return errors.New("AGENT_TOKEN is required (the hub sends it as a bearer token)")
	}
	if len(cfg.AgentToken) < 16 {
		return errors.New("AGENT_TOKEN must be at least 16 characters")
	}
	var jr *jobs.Reader
	if o, on := jobs.OptionsFromScope(cfg.JobsScope, cfg.JobsAllSystem, cfg.JobsExclude); on {
		r, err := jobs.New(o)
		if err != nil && jobsOnly {
			return err
		}
		if err != nil {
			log.Info("scheduled jobs off", "why", err)
		}
		jr = r
	} else if jobsOnly {
		return errors.New("JOBS_SCOPE is off; the jobs helper has nothing to serve")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		mode := "agent"
		if jobsOnly {
			mode = "jobs"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": Version, "mode": mode, "jobs": jr != nil})
	})
	if jr != nil {
		serveAgentJobs(mux, cfg.AgentToken, jr)
	}
	if jobsOnly {
		log.Info("pocket-monitoring jobs helper started", "version", Version, "addr", agentAddr(cfg), "user", jr.Opts.User, "system", jr.Opts.System)
		return listenAgent(ctx, agentAddr(cfg), mux)
	}
	l := collect.NewLocal(Version)
	l.Docker = collect.NewDocker(cfg.DockerHost)
	go l.Docker.Run(ctx, cfg.DockerInterval)
	var mu sync.Mutex
	var last *collect.Snapshot
	go func() {
		_, _ = l.Collect(ctx) // rates need a previous reading
		time.Sleep(time.Second)
		for {
			snap, err := l.Collect(ctx)
			if err == nil {
				mu.Lock()
				last = snap
				mu.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.LocalInterval):
			}
		}
	}()
	mux.HandleFunc("GET /agent/v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if !AgentAuthorized(r, cfg.AgentToken) {
			time.Sleep(300 * time.Millisecond)
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		mu.Lock()
		snap := last
		mu.Unlock()
		if snap == nil {
			writeErr(w, http.StatusServiceUnavailable, "first reading not ready yet")
			return
		}
		writeJSON(w, http.StatusOK, snap)
	})
	mux.HandleFunc("GET /agent/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		if !AgentAuthorized(r, cfg.AgentToken) {
			time.Sleep(300 * time.Millisecond)
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		serveAgentLogs(w, r, l.Docker)
	})
	log.Info("pocket-monitoring agent started", "version", Version, "addr", agentAddr(cfg), "docker", !l.Docker.Missing(), "root", l.P.Root, "jobs", jr != nil)
	return listenAgent(ctx, agentAddr(cfg), mux)
}

func agentAddr(cfg *config.Config) string { return cfg.Host + ":" + itoa64(int64(cfg.AgentPort)) }

func listenAgent(ctx context.Context, addr string, mux *http.ServeMux) error {
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// serveAgentJobs answers a hub's questions about this machine's scheduled jobs.
func serveAgentJobs(mux *http.ServeMux, token string, jr *jobs.Reader) {
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !AgentAuthorized(r, token) {
				time.Sleep(300 * time.Millisecond)
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /agent/v1/jobs", auth(func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		st, err := jr.Status(r.Context(), since)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	}))
	mux.HandleFunc("GET /agent/v1/jobs/log", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tail, _ := strconv.Atoi(q.Get("tail"))
		lines, err := jr.Log(r.Context(), q.Get("unit"), q.Get("invocation"), tail)
		if err != nil {
			writeJobErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
	}))
	mux.HandleFunc("POST /agent/v1/jobs/action", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		j, err := jr.Act(r.Context(), q.Get("unit"), q.Get("action"))
		if err != nil {
			writeJobErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, j)
	}))
}

func writeJobErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobs.ErrUnknown):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, jobs.ErrNotAllowed):
		writeErr(w, http.StatusForbidden, err.Error())
	default:
		writeErr(w, http.StatusBadGateway, err.Error())
	}
}

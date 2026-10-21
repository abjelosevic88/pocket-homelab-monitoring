package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/config"
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
// rates are smooth whatever the hub's polling interval is.
func RunAgent(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if cfg.AgentToken == "" {
		return errors.New("AGENT_TOKEN is required (the hub sends it as a bearer token)")
	}
	if len(cfg.AgentToken) < 16 {
		return errors.New("AGENT_TOKEN must be at least 16 characters")
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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": Version, "mode": "agent"})
	})
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
	addr := cfg.Host + ":" + itoa64(int64(cfg.AgentPort))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("pocket-monitoring agent started", "version", Version, "addr", addr, "docker", !l.Docker.Missing(), "root", l.P.Root)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

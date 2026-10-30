// Package server wires the HTTP API, the pollers that read every server,
// history, alerts, auth, Pocket Core and the embedded SPA.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/crypto/bcrypt"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/config"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/speedtest"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// Version is injected at build time.
var Version = "dev"

// Server holds dependencies.
type Server struct {
	cfg   *config.Config
	store *store.Store
	log   *slog.Logger
	webFS fs.FS
	http  *http.Client

	started  time.Time
	requests atomic.Int64
	errors   atomic.Int64
	changed  atomic.Int64 // bumps on every write; GET /changes

	core    *core.Client
	coreMu  sync.Mutex
	coreOK  bool
	coreErr string

	oidc   *oidcProvider
	oidcMu sync.Mutex

	linkApps sync.Map // pcx_ token → acting app slug

	mon    *monitor
	alerts *alertEngine
	jobs   *jobsHub
	speed  *speedHub

	baseCtx context.Context // the app's lifetime (background work outlives requests)
}

// New creates a server.
func New(cfg *config.Config, st *store.Store, log *slog.Logger, webFS fs.FS) *Server {
	ua := "PocketMonitoring/" + Version + " (+https://github.com/abjelosevic88/pocket-homelab-monitoring)"
	s := &Server{cfg: cfg, store: st, log: log, webFS: webFS, started: time.Now(), alerts: newAlertEngine(), jobs: newJobsHub()}
	home, _ := filepath.Abs(cfg.DataDir)
	s.speed = newSpeedHub(speedtest.Runner{Bin: cfg.SpeedtestBin, Home: home})
	s.http = &http.Client{Timeout: 60 * time.Second}
	s.core = core.New(cfg.CoreURL, core.Options{DB: st.DB, TokenFile: cfg.DataDir + "/core.token", UserAgent: ua, ServiceCache: time.Minute, ServiceStale: 15 * time.Minute})
	s.oidc = &oidcProvider{issuerURL: cfg.OIDCIssuer}
	var local *collect.Local
	if cfg.LocalCollector {
		local = collect.NewLocal(Version)
		local.Docker = collect.NewDocker(cfg.DockerHost)
	}
	s.mon = newMonitor(local)
	return s
}

// isOwner reports whether uid is the account the box was set up with.
func (s *Server) isOwner(ctx context.Context, uid int64) bool {
	return uid > 0 && uid == s.store.OwnerID(ctx)
}

// Bootstrap seeds the first admin (if configured), demo data and starts the workers.
func (s *Server) Bootstrap(ctx context.Context) error {
	s.baseCtx = ctx
	if err := s.core.Start(ctx); err != nil {
		return err
	}
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n == 0 && s.cfg.AuthMode == "local" && s.cfg.AdminEmail != "" && s.cfg.AdminPassword != "" {
		if len(s.cfg.AdminPassword) < 8 {
			return errors.New("ADMIN_PASSWORD must be at least 8 characters")
		}
		h, _ := bcrypt.GenerateFromPassword([]byte(s.cfg.AdminPassword), bcrypt.DefaultCost)
		u := &store.User{Email: s.cfg.AdminEmail, Name: s.cfg.AdminName, PasswordHash: string(h), Role: "admin"}
		if err := s.store.CreateUser(ctx, u); err != nil {
			return err
		}
		s.seedUser(ctx, u)
		st, _ := s.store.GetSettings(ctx, 0)
		st.SetupComplete = true
		if err := s.store.SaveBoxSettings(ctx, st); err != nil {
			return err
		}
		s.log.Info("admin user created from environment", "email", u.Email)
	}
	_ = s.store.PurgeSessions(ctx)
	s.changed.Store(time.Now().UnixNano())
	if err := s.loadSecretKey(); err != nil {
		return err
	}
	if owner := s.store.OwnerID(ctx); owner > 0 {
		if u, err := s.store.GetUser(ctx, owner); err == nil {
			s.seedUser(ctx, u)
		}
	}
	if s.cfg.DemoData {
		s.seedDemo(ctx)
	}
	s.loadAlerts(ctx)
	if s.mon.local != nil && s.mon.local.Docker != nil {
		go s.mon.local.Docker.Run(ctx, s.cfg.DockerInterval)
	}
	go s.monitorLoop(ctx)
	go s.jobsLoop(ctx)
	go s.speedLoop(ctx)
	go s.housekeeping(ctx)
	if s.core.Configured() {
		go s.coreLoop(ctx)
	}
	return nil
}

// seedUser gives the box owner "This server" (once: deleting it sticks).
func (s *Server) seedUser(ctx context.Context, u *store.User) {
	if !s.cfg.LocalCollector || u.ID != s.store.OwnerID(ctx) || s.store.HasLocalServer(ctx) || s.store.BoxSetting(ctx, "local_removed") == "1" {
		return
	}
	name := s.cfg.LocalName
	if name == "" {
		if h, err := os.ReadFile("/proc/sys/kernel/hostname"); err == nil {
			name = strings.TrimSpace(string(h))
		}
	}
	if name == "" {
		name = "This server"
	}
	sv := &store.Server{UserID: u.ID, Name: name, Kind: "local", Enabled: true, IntervalS: int(s.cfg.LocalInterval.Seconds()), Color: "#2a78d6"}
	if err := s.store.CreateServer(ctx, sv); err != nil {
		s.log.Warn("create local server", "err", err)
		return
	}
	for _, r := range defaultRules(sv.ID) {
		_ = s.store.SaveRule(ctx, &r)
	}
	s.dropRules()
	s.reconcile()
	s.log.Info("monitoring this box", "server", name, "owner", u.Email)
}

// housekeeping rolls history up, prunes it and old alerts, and sessions.
func (s *Server) housekeeping(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	n := 0
	for {
		if err := s.store.Rollup(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("history roll-up", "err", err)
		}
		if n%12 == 0 {
			s.store.PruneSamples(ctx)
			s.store.PruneAlerts(ctx, 180)
			s.store.PruneJobRuns(ctx, s.cfg.JobsKeepDays)
			s.store.PruneSpeedTests(ctx)
			_ = s.store.PurgeSessions(ctx)
		}
		n++
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// touch records that something changed (the UI polls /changes).
func (s *Server) touch() { s.changed.Store(time.Now().UnixNano()) }

// Router builds the chi router.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	if s.cfg.TrustProxy {
		r.Use(middleware.RealIP)
	}
	r.Use(middleware.RequestID)
	r.Use(s.logging)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5, "application/json", "text/html", "text/css", "application/javascript", "text/plain"))

	r.Get("/pocket-ask.js", s.core.AskWidget("monitoring"))
	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)
	r.Get("/api/health", s.handleHealth)
	if s.cfg.MetricsEnabled {
		r.Get("/metrics", s.handleMetrics)
	}

	r.Get("/agent/v1/snapshot", s.handleAgentSnapshot)
	r.Get("/agent/v1/logs", s.handleAgentLogs)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(middleware.Timeout(180 * time.Second))
		r.Post("/auth/login", s.handleLogin)
		r.Post("/auth/setup", s.handleSetup)
		r.Get("/auth/status", s.handleAuthStatus)
		r.Get("/auth/oidc/start", s.handleOIDCStart)
		r.Get("/auth/oidc/callback", s.handleOIDCCallback)
		r.Post("/client-error", s.handleClientError)

		r.Group(func(r chi.Router) {
			r.Use(s.requireAuth)
			r.Post("/auth/logout", s.handleLogout)
			r.Get("/auth/me", s.handleMe)
			r.Put("/auth/me", s.handleUpdateMe)
			r.Get("/changes", s.handleChanges)

			s.serverRoutes(r)
			s.alertRoutes(r)
			s.jobRoutes(r)
			s.speedRoutes(r)

			r.Get("/users", s.handleListUsers)
			r.Post("/users", s.handleCreateUser)
			r.Put("/users/{id}", s.handleUpdateUser)
			r.Delete("/users/{id}", s.handleDeleteUser)

			r.Get("/settings", s.handleGetSettings)
			r.Put("/settings", s.handlePutSettings)
			r.Get("/settings/system", s.handleSystemInfo)
			r.Post("/notify/test", s.handleTestNotify)

			r.Get("/tokens", s.handleListTokens)
			r.Post("/tokens", s.handleCreateToken)
			r.Delete("/tokens/{id}", s.handleDeleteToken)

			r.Get("/backup", s.handleBackup)
		})
	})

	r.NotFound(s.handleSPA)
	return r
}

// ---------- helpers ----------

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.requests.Add(1)
		if ww.Status() >= 500 {
			s.errors.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			return
		}
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", ww.Status(), "dur", time.Since(start).String(), "ip", r.RemoteAddr)
	})
}

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func badRequest(msg string) error { return userError{msg} }

func (s *Server) fail(w http.ResponseWriter, err error, context string) {
	var ue userError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "already there")
		return
	case errors.As(err, &ue):
		writeErr(w, http.StatusBadRequest, ue.msg)
		return
	}
	s.log.Error(context, "err", err)
	writeErr(w, http.StatusInternalServerError, context+": "+err.Error())
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<20))
	return dec.Decode(v)
}

func idParam(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	return n
}

func queryInt(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

func queryInt64(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
	return n
}

func queryBool(r *http.Request, name string) bool {
	v := strings.ToLower(r.URL.Query().Get(name))
	return v == "1" || v == "true" || v == "yes"
}

func me(r *http.Request) int64 { return actorID(r.Context()) }

// ---------- Pocket Core ----------

// coreLoop registers with Core on start (retrying until it answers) and
// re-registers every 30 minutes so version and last-seen stay fresh.
func (s *Server) coreLoop(ctx context.Context) {
	wait := 5 * time.Second
	for {
		err := s.registerWithCore(ctx)
		s.coreMu.Lock()
		s.coreOK, s.coreErr = err == nil, ""
		if err != nil {
			s.coreErr = err.Error()
		}
		s.coreMu.Unlock()
		if err != nil {
			s.log.Warn("pocket core: registration failed, retrying", "err", err, "in", wait.String())
		} else {
			wait = 30 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err != nil && wait < 5*time.Minute {
			wait *= 2
		}
	}
}

func (s *Server) registerWithCore(ctx context.Context) error {
	m := core.Manifest{
		Slug: "monitoring", Name: "Pocket Monitoring",
		Description: "Watches this server and any others you add: CPU, memory, swap, load, network, disks, file systems, temperatures, processes, containers and the internet connection's speed, live and as history, with alerts sent to your phone when something runs hot, fills up, crashes or goes offline.",
		Icon:        "/favicon.svg", Version: Version,
		URL: s.cfg.CoreAppURL, PublicURL: s.cfg.BaseURL, HealthPath: "/healthz", EnvPrefix: "MONITORING",
		Publishes: []string{"monitoring.alert", "monitoring.resolved", "monitoring.server_added", "monitoring.server_removed", "monitoring.speedtest"},
		Offers:    linkOffers,
		Uses:      jobAppUses(),
	}
	if s.cfg.BaseURL != "" {
		m.OIDC = &core.OIDCRequest{RedirectURIs: []string{s.cfg.BaseURL + "/api/v1/auth/oidc/callback"}}
	}
	reg, err := s.core.Register(ctx, s.cfg.CoreRegistrationKey, m)
	if err != nil {
		return err
	}
	if reg.Token != "" {
		s.log.Info("pocket core: registered, app token stored", "core", s.cfg.CoreURL)
	} else {
		s.log.Debug("pocket core: re-registered", "core", s.cfg.CoreURL)
	}
	return nil
}

// publish persists an event locally; the SDK retries network delivery.

// coreStatus summarises the Pocket Core link for the About page.
func (s *Server) coreStatus() map[string]any {
	s.coreMu.Lock()
	defer s.coreMu.Unlock()
	return map[string]any{"configured": s.core.Configured(), "url": s.cfg.CoreURL, "registered": s.coreOK, "error": s.coreErr}
}

// handleClientError writes a crash of the web app into the server's log.
func (s *Server) handleClientError(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Message, Stack, Component, URL, Where, Agent string
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	who := ""
	if u, err := s.resolveUser(r); err == nil && u != nil {
		who = u.Email
	}
	s.log.Warn("web app crashed", "user", who, "where", in.Where, "url", in.URL, "message", in.Message, "stack", in.Stack, "component", in.Component, "agent", in.Agent)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"stamp": s.changed.Load()})
}

// ---------- health / metrics ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"core_outbox": s.coreOutboxHealth(r.Context()), "status": "ok", "version": Version, "uptime": time.Since(s.started).Round(time.Second).String()})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DB.PingContext(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts := s.store.Counts(ctx)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP pocket_monitoring_info Build info.\n# TYPE pocket_monitoring_info gauge\npocket_monitoring_info{version=%q} 1\n", Version)
	fmt.Fprintf(w, "# HELP pocket_monitoring_uptime_seconds Process uptime.\n# TYPE pocket_monitoring_uptime_seconds gauge\npocket_monitoring_uptime_seconds %d\n", int(time.Since(s.started).Seconds()))
	fmt.Fprintf(w, "# HELP pocket_monitoring_http_requests_total HTTP requests served.\n# TYPE pocket_monitoring_http_requests_total counter\npocket_monitoring_http_requests_total %d\n", s.requests.Load())
	fmt.Fprintf(w, "# HELP pocket_monitoring_http_errors_total HTTP 5xx responses.\n# TYPE pocket_monitoring_http_errors_total counter\npocket_monitoring_http_errors_total %d\n", s.errors.Load())
	fmt.Fprintf(w, "# HELP pocket_monitoring_rows Rows per table.\n# TYPE pocket_monitoring_rows gauge\n")
	for k, v := range counts {
		fmt.Fprintf(w, "pocket_monitoring_rows{table=%q} %d\n", k, v)
	}
	s.speedMetrics(ctx, w)
}

// ---------- static / SPA ----------

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/agent/") {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if s.webFS == nil {
		http.Error(w, "web UI not built (run `make web`)", http.StatusNotFound)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if f, err := s.webFS.Open(path); err == nil {
		f.Close()
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.FileServerFS(s.webFS).ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	r.URL.Path = "/"
	http.FileServerFS(s.webFS).ServeHTTP(w, r)
}

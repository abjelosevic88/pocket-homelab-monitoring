// Command pocket-monitoring watches this server and any others you add:
// CPU, memory, disks, temperatures, processes and containers, live and as
// history, with alerts. One binary, one SQLite file, one volume. Run as
// `pocket-monitoring agent` it is the small reader on another machine.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/config"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/server"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
	"github.com/abjelosevic88/pocket-homelab-monitoring/web"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("pocket-monitoring", version)
			return
		case "agent":
			runAgent()
			return
		case "backup":
			runBackup(os.Args[2:])
			return
		case "reset-password":
			runResetPassword(os.Args[2:])
			return
		case "help", "--help", "-h":
			fmt.Println(`pocket-monitoring — server monitoring with history and alerts

Usage:
  pocket-monitoring                     start the server (default)
  pocket-monitoring agent               serve this machine's readings to a hub (AGENT_TOKEN, AGENT_PORT)
  pocket-monitoring backup [-o FILE]    write a consistent SQLite backup
  pocket-monitoring reset-password [-email USER] [-password NEW]
                                       set a user's password (default: first user, random password printed)
  pocket-monitoring version             print version

Configuration is read from environment variables, see docs/configuration.md.`)
			return
		}
	}
	serve()
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func mustLoad() (*config.Config, *slog.Logger, *store.Store) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := newLogger(cfg)
	st, err := store.Open(cfg.DBPath, log)
	if err != nil {
		log.Error("open database", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	return cfg, log, st
}

func serve() {
	server.Version = version
	cfg, log, st := mustLoad()
	defer st.Close()

	var webFS fs.FS
	if sub, err := fs.Sub(web.Dist, "dist"); err == nil {
		if f, err := sub.Open("index.html"); err == nil {
			f.Close()
			webFS = sub
		}
	}
	if webFS == nil {
		log.Warn("web UI not embedded; API only (run `make web` before building)")
	}

	srv := server.New(cfg, st, log, webFS)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Bootstrap(ctx); err != nil {
		log.Error("bootstrap", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Info("pocket-monitoring started", "version", version, "addr", cfg.Addr(), "base_url", cfg.BaseURL, "db", cfg.DBPath, "auth_mode", cfg.AuthMode)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer c2()
	_ = httpSrv.Shutdown(shutdownCtx)
}

func runAgent() {
	server.Version = version
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := newLogger(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := server.RunAgent(ctx, cfg, log); err != nil {
		log.Error("agent", "err", err)
		os.Exit(1)
	}
}

func runBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("o", "", "output file (default: DATA_DIR/backups/pocket-monitoring-<timestamp>.db)")
	_ = fs.Parse(args)
	cfg, log, st := mustLoad()
	defer st.Close()
	dst := *out
	if dst == "" {
		_ = os.MkdirAll(cfg.DataDir+"/backups", 0o755)
		dst = fmt.Sprintf("%s/backups/pocket-monitoring-%s.db", cfg.DataDir, time.Now().Format("20060102-150405"))
	}
	if err := st.Backup(context.Background(), dst); err != nil {
		log.Error("backup failed", "err", err)
		os.Exit(1)
	}
	fmt.Println(dst)
}

func runResetPassword(args []string) {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	email := fs.String("email", "", "user email (default: the first user)")
	password := fs.String("password", "", "new password (default: generate a random one)")
	_ = fs.Parse(args)
	_, log, st := mustLoad()
	defer st.Close()
	ctx := context.Background()
	var u *store.User
	var err error
	if *email != "" {
		u, err = st.GetUserByEmail(ctx, *email)
	} else {
		var users []store.User
		users, err = st.ListUsers(ctx)
		if err == nil && len(users) > 0 {
			u = &users[0]
		} else if err == nil {
			err = errors.New("no users found")
		}
	}
	if err != nil {
		log.Error("reset-password: user lookup failed", "err", err)
		os.Exit(1)
	}
	pw := *password
	if pw == "" {
		const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			log.Error("reset-password: random", "err", err)
			os.Exit(1)
		}
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		pw = string(b)
	}
	if len(pw) < 8 {
		log.Error("reset-password: password must be at least 8 characters")
		os.Exit(1)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		log.Error("reset-password: hash", "err", err)
		os.Exit(1)
	}
	u.PasswordHash = string(h)
	if err := st.UpdateUser(ctx, u); err != nil {
		log.Error("reset-password: update", "err", err)
		os.Exit(1)
	}
	fmt.Printf("password for %s set", u.Email)
	if *password == "" {
		fmt.Printf(": %s", pw)
	}
	fmt.Println()
}

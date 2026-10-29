// Package config loads runtime configuration from environment variables.
//
// Every option has a sensible default so that `docker run` with a single
// volume works out of the box. See docs/configuration.md for the full list.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime settings.
type Config struct {
	Host           string
	Port           int
	BaseURL        string
	DataDir        string
	DBPath         string
	LogLevel       string
	LogFormat      string // text | json
	TrustProxy     bool
	MetricsEnabled bool
	Timezone       string // IANA; the box default for people who did not choose one

	AuthMode             string // local | proxy | none
	AuthProxyHeader      string
	AuthProxyEmailHeader string
	SessionTTL           time.Duration
	SecureCookies        bool
	AdminEmail           string
	AdminPassword        string
	AdminName            string

	// Sign in with an OpenID Connect provider. On a box with Pocket Core
	// none of these are needed: the app uses the client Core issued.
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCName         string
	OIDCAutoCreate   bool

	// Pocket Core: when CORE_URL is set the app registers itself on start,
	// publishes events and accepts the tokens Core hands to paired phones.
	CoreURL             string
	CoreRegistrationKey string
	CoreAppURL          string

	// Notifications: alerts and recoveries (each person can set their own;
	// this route is the owner's until they choose one).
	NotifyURL  string // Apprise route, ntfy topic URL or webhook
	NotifyKind string // apprise | ntfy | webhook

	// This box: read its /proc, /sys and Docker socket as "This server".
	LocalCollector bool
	LocalName      string        // name of the local server ("" = its host name)
	LocalInterval  time.Duration // how often this box is read
	DockerHost     string        // unix:///var/run/docker.sock (default) or tcp://…
	DockerInterval time.Duration // how often container stats are read

	// Agent: with AGENT_TOKEN set, /agent/v1/snapshot answers other Pocket
	// Monitoring hubs (and `pocket-monitoring agent` listens on AGENT_PORT).
	AgentToken string
	AgentPort  int

	// Scheduled jobs (systemd timers). The agent and the jobs helper read
	// them (JOBS_SCOPE, JOBS_ALL_SYSTEM, JOBS_EXCLUDE); a hub in a container
	// reads this box's through the helper at JOBS_URL with JOBS_TOKEN.
	JobsScope     string
	JobsAllSystem bool
	JobsExclude   []string
	JobsURL       string
	JobsToken     string
	JobsInterval  time.Duration // how often the hub reads every server's jobs
	JobsStuck     time.Duration // a run longer than this is reported as stuck
	JobsKeepDays  int           // kept runs (failures and runs that wrote something)

	// Internet speed tests: the Ookla Speedtest CLI on the machine the web
	// app runs on (the image ships it). SPEEDTEST_BIN = its name or path.
	SpeedtestBin string

	DemoData bool
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(env(key, "")) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	if v := env(key, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := env(key, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	dataDir := env("DATA_DIR", "./data")
	c := &Config{
		Host:           env("HOST", "0.0.0.0"),
		Port:           envInt("PORT", 8080),
		BaseURL:        strings.TrimRight(env("BASE_URL", ""), "/"),
		DataDir:        dataDir,
		DBPath:         env("DB_PATH", dataDir+"/pocket-monitoring.db"),
		LogLevel:       strings.ToLower(env("LOG_LEVEL", "info")),
		LogFormat:      strings.ToLower(env("LOG_FORMAT", "text")),
		TrustProxy:     envBool("TRUST_PROXY", true),
		MetricsEnabled: envBool("METRICS_ENABLED", true),
		Timezone:       env("TZ", "UTC"),

		AuthMode:             strings.ToLower(env("AUTH_MODE", "local")),
		AuthProxyHeader:      env("AUTH_PROXY_HEADER", "Remote-User"),
		AuthProxyEmailHeader: env("AUTH_PROXY_EMAIL_HEADER", "Remote-Email"),
		SessionTTL:           envDuration("SESSION_TTL", 720*time.Hour),
		SecureCookies:        envBool("SECURE_COOKIES", false),
		AdminEmail:           strings.ToLower(env("ADMIN_EMAIL", "")),
		AdminPassword:        env("ADMIN_PASSWORD", ""),
		AdminName:            env("ADMIN_NAME", "Admin"),

		OIDCIssuer:       strings.TrimRight(env("OIDC_ISSUER", ""), "/"),
		OIDCClientID:     env("OIDC_CLIENT_ID", ""),
		OIDCClientSecret: env("OIDC_CLIENT_SECRET", ""),
		OIDCName:         env("OIDC_NAME", "single sign-on"),
		OIDCAutoCreate:   envBool("OIDC_AUTO_CREATE", true),

		CoreURL:             strings.TrimRight(env("CORE_URL", ""), "/"),
		CoreRegistrationKey: env("CORE_REGISTRATION_KEY", ""),
		CoreAppURL:          strings.TrimRight(env("CORE_APP_URL", ""), "/"),

		NotifyURL:  env("NOTIFY_URL", ""),
		NotifyKind: strings.ToLower(env("NOTIFY_KIND", "apprise")),

		LocalCollector: envBool("LOCAL_COLLECTOR", true),
		LocalName:      env("LOCAL_NAME", ""),
		LocalInterval:  envDuration("LOCAL_INTERVAL", 5*time.Second),
		DockerHost:     env("DOCKER_HOST", ""),
		DockerInterval: envDuration("DOCKER_INTERVAL", 15*time.Second),
		AgentToken:     env("AGENT_TOKEN", ""),
		AgentPort:      envInt("AGENT_PORT", 8193),

		JobsScope:     strings.ToLower(env("JOBS_SCOPE", "auto")),
		JobsAllSystem: envBool("JOBS_ALL_SYSTEM", false),
		JobsExclude:   splitList(env("JOBS_EXCLUDE", "")),
		JobsURL:       strings.TrimRight(env("JOBS_URL", ""), "/"),
		JobsToken:     env("JOBS_TOKEN", ""),
		JobsInterval:  envDuration("JOBS_INTERVAL", time.Minute),
		JobsStuck:     envDuration("JOBS_STUCK_AFTER", 6*time.Hour),
		JobsKeepDays:  envInt("JOBS_KEEP_DAYS", 30),

		SpeedtestBin: env("SPEEDTEST_BIN", "speedtest"),

		DemoData: envBool("DEMO_DATA", false),
	}
	if c.CoreAppURL == "" {
		c.CoreAppURL = c.BaseURL
	}
	switch c.AuthMode {
	case "local", "proxy", "none":
	default:
		return nil, fmt.Errorf("AUTH_MODE must be one of local, proxy, none (got %q)", c.AuthMode)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return nil, fmt.Errorf("PORT out of range: %d", c.Port)
	}
	if c.OIDCIssuer != "" && (c.OIDCClientID == "" || c.BaseURL == "") {
		return nil, fmt.Errorf("OIDC_ISSUER needs OIDC_CLIENT_ID and BASE_URL (the callback is BASE_URL/api/v1/auth/oidc/callback)")
	}
	if c.LocalInterval < time.Second {
		c.LocalInterval = time.Second
	}
	if c.DockerInterval < 5*time.Second {
		c.DockerInterval = 5 * time.Second
	}
	if c.JobsInterval < 15*time.Second {
		c.JobsInterval = 15 * time.Second
	}
	if c.JobsStuck < 5*time.Minute {
		c.JobsStuck = 5 * time.Minute
	}
	if c.JobsKeepDays < 1 {
		c.JobsKeepDays = 1
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return nil, fmt.Errorf("TZ %q is not a known time zone", c.Timezone)
	}
	return c, nil
}

func splitList(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Addr returns host:port.
func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

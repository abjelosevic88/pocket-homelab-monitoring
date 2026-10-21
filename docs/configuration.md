# Configuration

Everything is an environment variable; nothing is required.

| Variable | Default | |
|---|---|---|
| `PORT` / `HOST` | `8080` / `0.0.0.0` | where the web app listens |
| `BASE_URL` | – | public address, for links in notifications and sign-in |
| `DATA_DIR` | `./data` (`/data` in the image) | database, `secret.key`, backups |
| `TZ` | `UTC` | |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` / `ADMIN_NAME` | – | create the first admin instead of the setup screen |
| `AUTH_MODE` | `local` | `local`, `proxy` (trust `Remote-User`) or `none` (no login, one person) |
| `SECURE_COOKIES` | `false` | `true` behind HTTPS |
| `SESSION_TTL` | `720h` | |
| `LOCAL_COLLECTOR` | `true` | watch the box the app runs on |
| `LOCAL_NAME` | host name | name of that server |
| `LOCAL_INTERVAL` | `5s` | how often this box is read |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | or `tcp://host:2375`; Podman's socket works too |
| `DOCKER_INTERVAL` | `15s` | how often container stats are read |
| `HOST_PROC` / `HOST_SYS` / `HOST_ROOT` | `/proc` / `/sys` / `/host/root` if mounted | where the host is visible |
| `AGENT_TOKEN` | – | agent mode: required; web app: lets another hub read this box at `/agent/v1/snapshot` |
| `AGENT_PORT` | `8193` | agent mode only |
| `NOTIFY_URL` / `NOTIFY_KIND` | – / `apprise` | the owner's alert route until they set their own (`apprise`, `ntfy`, `webhook`) |
| `CORE_URL` / `CORE_REGISTRATION_KEY` / `CORE_APP_URL` | – | Pocket Core |
| `OIDC_ISSUER` / `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` / `OIDC_NAME` | – | sign in with any OpenID Connect provider |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `text` | |
| `METRICS_ENABLED` | `true` | Prometheus text at `/metrics` (the app's own counters) |
| `DEMO_DATA` | `false` | a made-up day of history for an empty server |

## Seeing the host from a container

The local collector reads `/proc`, `/sys`, the mount table and the Docker socket. In a container that
needs `pid: host` (processes, CPU and memory are host-wide anyway), `network_mode: host` (the real
interfaces), `/` mounted read-only at `/host/root` (free space of every file system, `/etc/os-release`,
user names) and the Docker socket (containers). The image runs as uid 1000: add the docker group's id
with `group_add`, or run the agent as root (it reads other users' process I/O too, like Glances does).

## Commands

```
pocket-monitoring                    the web app (default)
pocket-monitoring agent              serve this machine's readings to a hub
pocket-monitoring backup [-o FILE]   consistent SQLite copy (VACUUM INTO)
pocket-monitoring reset-password [-email E] [-password P]
pocket-monitoring version
```

## Files in the volume

- `pocket-monitoring.db` — servers, rules, alerts, history (minutes for 8 days, quarter hours for 400 days).
- `secret.key` — seals agent tokens and Glances passwords. Lose it and you retype them.
- `core.token`, `core.token.oidc.json` — Pocket Core registration.
- `backups/` — `pocket-monitoring backup` output.

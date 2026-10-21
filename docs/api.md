# API

All JSON under `/api/v1`. Auth: session cookie (web), `Authorization: Bearer pmon_…` (personal token),
or a Pocket Core token. Shapes are in `web/src/lib/types.ts` (TypeScript) and `internal/collect/types.go` /
`internal/store/models.go` (Go).

Every server belongs to the person who added it. Others see it only when it is shared with them
(read-only, role `viewer`); anyone else's server id answers 404. Admins get no view of other people's servers.

## Auth & account (same as every Pocket app)

| Method | Path | |
|---|---|---|
| GET | `/auth/status` | `AuthStatus` |
| POST | `/auth/setup` | `{email,password,name}` first admin |
| POST | `/auth/login` / `/auth/logout` | |
| GET/PUT | `/auth/me` | `User`; PUT `{name,email,current_password,new_password}` |
| GET | `/auth/oidc/start` | Sign in with Pocket Core |
| GET | `/changes` | `{stamp}` bumps when servers, statuses or alerts change |
| GET/POST/PUT/DELETE | `/users`, `/users/{id}` | admin only |
| GET/POST/DELETE | `/tokens`, `/tokens/{id}` | personal tokens (`pmon_…`, shown once) |
| GET/PUT | `/settings` | `SettingsResponse`; PUT any `Settings` field (+ `notify_token`) |
| GET | `/settings/system` | version, uptime, counts, Core link |
| POST | `/notify/test` | sends a test notification |
| GET | `/people` | `Person[]` everyone on the box (share picker) |
| GET | `/backup` | admin, only while alone on the box |

## Servers

| Method | Path | |
|---|---|---|
| GET | `/servers` | `Server[]` mine + shared with me, each with `summary`, `spark`, `status` |
| POST | `/servers` | `{name, kind: agent\|glances, url, token?, username?, password?, color?, interval_s?}` → `Server` (default alert rules are added) |
| POST | `/servers/test` | same body → `{ok, hostname, platform, error}` (nothing stored) |
| GET | `/servers/{id}` | `Server` |
| PUT | `/servers/{id}` | partial: `name, url, token, username, password, color, interval_s, enabled` (owner) |
| DELETE | `/servers/{id}` | owner; history and alerts go with it |
| POST | `/servers/reorder` | `{ids: number[]}` |
| GET | `/servers/{id}/snapshot` | `Snapshot` latest full reading (404 `{"error":"no data yet"}` before the first) |
| GET | `/servers/{id}/live` | `{step_s, points: Point[]}` last 15 minutes at the collection interval (from memory) |
| GET | `/servers/{id}/history?range=1h\|6h\|24h\|7d\|30d\|90d` | `History` |
| GET | `/servers/{id}/logs?container=&tail=&since=` | `Logs` container logs, owner only (403 for viewers, 501 for Glances). `container` = name or id, '' = every running container merged by time; `tail` 1–5000 (500); `since` = the `t` of the last line you have → only newer lines (follow by polling) |
| GET | `/servers/{id}/shares` | `Share[]` (owner) |
| POST | `/servers/{id}/shares` | `{user_id}` or `{everyone:true}` → `Share` |
| DELETE | `/shares/{sid}` | owner |
| POST | `/servers/{id}/leave` | a viewer removes a shared server from their list |

`kind` = `local` (this box, made automatically for the owner, cannot be added by hand), `agent`
(another machine running `pocket-monitoring agent`, `url` like `http://10.0.0.5:8193`, `token` = its
AGENT_TOKEN) or `glances` (a Glances 4 web server, `url` like `http://10.0.0.5:61208`, optional
`username`/`password`).

## Alerts

| Method | Path | |
|---|---|---|
| GET | `/rules?server_id=` | `Rule[]` of my servers (owner only) |
| POST | `/rules` | `{server_id, metric, target, threshold, for_s, severity, enabled}` |
| PUT | `/rules/{id}` | partial |
| DELETE | `/rules/{id}` | |
| GET | `/alerts?state=active\|all&server_id=&limit=` | `Alert[]` newest first, of servers I can see |

Metrics and what `threshold` means:

| metric | threshold | target |
|---|---|---|
| cpu | percent | – |
| mem | percent | – |
| swap | percent | – |
| load | load1 per thread (1.0 = every thread busy) | – |
| disk | percent used | mount point or '' for any |
| temp | °C | sensor key or '' for any |
| offline | – (fires when no reading for `for_s`) | – |
| container | – (fires when a container is unhealthy, or was running and stopped) | container name or '' for any |
| raid | – (fires when an array is degraded) | – |

## Agent (on the monitored machine)

`GET /agent/v1/snapshot` with `Authorization: Bearer <AGENT_TOKEN>` → `Snapshot`.
`GET /agent/v1/logs?container=&tail=&since=` (same token) → `Logs`.
`GET /healthz` → `{status, version}`.

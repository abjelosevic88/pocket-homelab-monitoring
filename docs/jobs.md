# Pocket apps' scheduled jobs in Pocket Monitoring

Pocket Monitoring's **Jobs** tab shows every scheduled job of a box in one place: the systemd timers (through the jobs helper or the agent) and the jobs Pocket apps run themselves (a nightly review, a product check, backups…). The apps keep running and configuring their own jobs; Monitoring only shows them, keeps their run history, alerts when one fails, hangs or misses its time, and can start one now.

An app takes part by implementing the small **jobs** contract below and offering two Pocket Core link scopes. Monitoring reaches it through a Core app link acting as the person who owns the box's server in Monitoring (a five-minute `pcx_` token per call, nothing to paste), so the person sees their own jobs, and admin-only jobs only when they are an admin there.

## Scopes

Offer in the app's manifest:

| Scope | Name | Description |
|---|---|---|
| `jobs:read` | See scheduled jobs | List the jobs this app runs on a schedule, when they ran, how it went and when they run next. |
| `jobs:run` | Run jobs now | Start one of those jobs right away. |

Link routes (a path of its own, so it never clashes with an app's own `/jobs`):

```
GET  ^/api/v1/scheduled-jobs$              jobs:read
POST ^/api/v1/scheduled-jobs/[^/]+/run$    jobs:run
```

Monitoring declares `uses` for each app it reads (`internal/server/appjobs.go`); add the app there when it gains the contract.

## `GET /api/v1/scheduled-jobs?since=<unix ms>`

```json
{
  "jobs": [{
    "id": "day-review",                  // stable, [a-z0-9-]
    "name": "Day review",
    "description": "The AI looks over the past day's bookings",
    "schedule": "every day at 00:00",    // in words, the app's own settings
    "enabled": true,                     // false: switched off in the app (Monitoring shows it paused, no alerts)
    "next": 1761865200000,               // unix ms of the next run, 0 = not on a clock (only by hand, on an event)
    "running": false,
    "started": 0,                        // unix ms the running run started
    "last": {"start": 1761778800000, "end": 1761778805000, "ok": true, "summary": "2 checked · 1 to look at"},
    "url": "/review",                    // where its settings are, a path in the app or a full URL
    "can_run": true                      // this person may start it (POST …/run)
  }],
  "runs": [{
    "job": "day-review",
    "id": "dr-17",                       // unique and stable within the app
    "start": 1761778800000,
    "end": 1761778805000,
    "ok": true,
    "summary": "2 checked · 1 to look at"   // one line, shown in the history and in a failure alert
  }]
}
```

- `runs` are the runs that **ended** after `since` (all the app keeps when `since` is 0), at most 200, oldest first. Runs that end after Monitoring last read are picked up on the next read (it overlaps by two minutes and skips ids it has).
- A job that ran but found nothing to do may report a run or not; a job that **should** have run and didn't must keep its old `next`, so Monitoring can say it is overdue (15 minutes late).
- `ok: false` with a `summary` that says what went wrong.
- Only what the person may see: per-person jobs as that person, box jobs only to admins.

## `POST /api/v1/scheduled-jobs/{id}/run`

Starts the job now, in the background. `202 {"started": true}`; `409 {"error": "…"}` when it is running already; `403` when the person may not; `404` unknown job. Monitoring reads the jobs again right after.

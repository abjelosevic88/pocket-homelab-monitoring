#!/usr/bin/env python3
"""Export Uptime Kuma (1.x) monitors *and their history* for Pocket Monitoring.

Kuma's own Settings → Backup → Export carries the monitors only. This reads a
copy of kuma.db (stop Kuma first, or copy kuma.db + kuma.db-wal together) and
writes the same JSON shape plus a "pocket" block: status page membership and
groups, uptime per local day, every up/down change, the last week of results
and where each monitor stood. Import it on the Uptime tab (⋯ → Import from
Uptime Kuma).

    python3 kuma-export.py /path/to/kuma.db > kuma.json
    TZ=Europe/Sarajevo python3 kuma-export.py kuma.db --beats-days 7 > kuma.json
"""
import argparse
import calendar
import json
import os
import sqlite3
import sys
import time


def ts(s):
    """Kuma stores UTC as 'YYYY-MM-DD HH:MM:SS.mmm'."""
    return calendar.timegm(time.strptime(s[:19], "%Y-%m-%d %H:%M:%S"))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("db")
    ap.add_argument("--beats-days", type=int, default=7, help="days of single results to carry over (default 7)")
    ap.add_argument("--slug", default="", help="status page to take public monitors from (default: the first)")
    a = ap.parse_args()
    db = sqlite3.connect(f"file:{a.db}?mode=ro", uri=True)
    db.row_factory = sqlite3.Row
    cols = {r[1] for r in db.execute("PRAGMA table_info(monitor)")}

    def col(r, name, default=None):
        return r[name] if name in cols else default

    monitors = []
    for r in db.execute("SELECT * FROM monitor ORDER BY id"):
        try:
            codes = json.loads(col(r, "accepted_statuscodes_json") or '["200-299"]')
        except ValueError:
            codes = ["200-299"]
        monitors.append({
            "id": r["id"], "name": r["name"], "type": r["type"], "url": col(r, "url") or "", "method": col(r, "method") or "GET",
            "hostname": col(r, "hostname") or "", "port": col(r, "port") or 0, "interval": r["interval"],
            "retryInterval": col(r, "retry_interval") or r["interval"], "maxretries": col(r, "maxretries") or 0,
            "keyword": col(r, "keyword") or "", "invertKeyword": bool(col(r, "invert_keyword")), "ignoreTls": bool(col(r, "ignore_tls")),
            "maxredirects": col(r, "maxredirects", 10), "upsideDown": bool(col(r, "upside_down")), "accepted_statuscodes": codes,
            "dns_resolve_server": col(r, "dns_resolve_server") or "", "dns_resolve_type": col(r, "dns_resolve_type") or "A",
            "pushToken": col(r, "push_token") or "", "active": r["active"], "timeout": col(r, "timeout") or 0,
            "description": col(r, "description") or "",
        })

    pocket = {"public": [], "groups": {}, "days": {}, "events": {}, "beats": {}, "state": {}}
    pages = list(db.execute("SELECT id, slug, title, description FROM status_page ORDER BY id"))
    page = next((p for p in pages if p["slug"] == a.slug), pages[0] if pages else None)
    if page:
        pocket["status_page"] = {"slug": page["slug"], "title": page["title"], "description": page["description"] or ""}
        for r in db.execute("""SELECT mg.monitor_id, g.name FROM monitor_group mg JOIN "group" g ON g.id = mg.group_id
                               WHERE g.status_page_id = ? ORDER BY g.weight, mg.weight""", (page["id"],)):
            pocket["public"].append(r["monitor_id"])
            if r["name"] and r["name"] not in ("Services", ""):
                pocket["groups"][str(r["monitor_id"])] = r["name"]

    # sqlite's 'localtime' follows TZ, so days split where the hub splits them
    if "TZ" not in os.environ:
        print("note: TZ not set, days use this machine's zone", file=sys.stderr)
    for r in db.execute("""SELECT monitor_id, date(time, 'localtime') AS day, SUM(status <> 0) AS up, SUM(status = 0) AS down,
                           AVG(CASE WHEN status = 1 AND ping IS NOT NULL THEN ping END) AS ms
                           FROM heartbeat GROUP BY monitor_id, day ORDER BY monitor_id, day"""):
        pocket["days"].setdefault(str(r["monitor_id"]), []).append({"day": r["day"], "up": r["up"], "down": r["down"], "avg_ms": round(r["ms"] or 0, 1)})
    for r in db.execute("SELECT monitor_id, time, status, msg FROM heartbeat WHERE important = 1 AND status IN (0, 1) ORDER BY time"):
        pocket["events"].setdefault(str(r["monitor_id"]), []).append({"t": ts(r["time"]), "s": r["status"], "msg": r["msg"] or ""})
    since = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime(time.time() - a.beats_days * 86400))
    for r in db.execute("SELECT monitor_id, time, status, ping, msg FROM heartbeat WHERE time >= ? ORDER BY time", (since,)):
        pocket["beats"].setdefault(str(r["monitor_id"]), []).append({"t": ts(r["time"]), "s": r["status"], "ms": r["ping"], "msg": (r["msg"] or "")[:300]})
    for r in db.execute("""SELECT h.monitor_id, h.time, h.status, h.ping, h.msg FROM heartbeat h
                           JOIN (SELECT monitor_id, MAX(id) AS id FROM heartbeat GROUP BY monitor_id) l ON l.id = h.id"""):
        ev = pocket["events"].get(str(r["monitor_id"]))
        pocket["state"][str(r["monitor_id"])] = {"status": r["status"], "last_at": ts(r["time"]), "last_ms": r["ping"],
                                                 "last_msg": (r["msg"] or "")[:300], "changed_at": ev[-1]["t"] if ev else 0}

    json.dump({"version": "pocket-export-1", "monitorList": monitors, "pocket": pocket}, sys.stdout, separators=(",", ":"))
    print(f"{len(monitors)} monitors, {sum(len(v) for v in pocket['beats'].values())} results, "
          f"{sum(len(v) for v in pocket['days'].values())} days", file=sys.stderr)


if __name__ == "__main__":
    main()

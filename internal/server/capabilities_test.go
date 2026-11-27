package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/config"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// fakeCapCore is a Pocket Core that vouches for link tokens
// pcx_<alice|bob>_<search|day|widget|capture|read> from Pocket Hub and
// remembers the manifest Monitoring registers with.
type fakeCapCore struct {
	*httptest.Server
	mu       sync.Mutex
	manifest map[string]any
}

func newCapHarness(t *testing.T) (*harness, *Server, *fakeCapCore) {
	t.Helper()
	fc := &fakeCapCore{}
	scopes := map[string]string{"search": core.ScopeSearch, "day": core.ScopeDay, "widget": core.ScopeWidget, "capture": core.ScopeCapture, "read": "monitoring:read"}
	fc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/apps/register":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			fc.mu.Lock()
			fc.manifest = m
			fc.mu.Unlock()
			io.WriteString(w, `{}`)
		case "/api/v1/auth/introspect":
			var in struct{ Token string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			parts := strings.Split(in.Token, "_")
			if len(parts) != 3 || parts[0] != "pcx" || scopes[parts[2]] == "" {
				io.WriteString(w, `{"active":false}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "kind": "delegated", "audience": "monitoring", "scope": scopes[parts[2]],
				"user": map[string]any{"id": 1, "sub": parts[1], "email": parts[1] + "@example.com", "name": parts[1], "role": "member"},
				"app":  map[string]any{"id": 9, "slug": "hub", "name": "Pocket Hub"}})
		default:
			io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(fc.Close)
	h, s := newHarnessWith(t, func(c *config.Config) { c.CoreURL, c.CoreRegistrationKey = fc.URL, "key" })
	return h, s, fc
}

// boxIDs are what seedBox made.
type boxIDs struct {
	box, nas, pi, bobBox             int64
	jellyfin, nextcloud, requests    int64
	paused, bobSite                  int64
	alice, bob, carol                string // API tokens
	nasOffline, boxDisk, boxTemp, ck int64
}

// putCheck stores a check with where it stands, in the database and in the
// runner (whose copy is the newer one), and keeps the runner from running it.
func putCheck(t *testing.T, s *Server, c *store.Check) {
	t.Helper()
	ctx := context.Background()
	st := *c
	if err := s.store.SaveCheck(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.Status, c.Fails, c.LastAt, c.LastMs, c.LastMsg, c.ChangedAt = st.Status, st.Fails, st.LastAt, st.LastMs, st.LastMsg, st.ChangedAt
	if err := s.store.SaveCheckState(ctx, c); err != nil {
		t.Fatal(err)
	}
	cp := *c
	s.checks.mu.Lock()
	s.checks.checks[c.ID] = &cp
	s.checks.next[c.ID] = time.Now().Add(time.Hour)
	s.checks.mu.Unlock()
}

// putAlert stores an alert that started (and maybe ended) then.
func putAlert(t *testing.T, s *Server, a store.Alert, started time.Time, resolved *time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	a.StartedAt = started.UTC().Format(time.RFC3339Nano)
	if err := s.store.CreateAlert(ctx, &a); err != nil {
		t.Fatal(err)
	}
	if resolved != nil {
		if _, err := s.store.DB.ExecContext(ctx, `UPDATE alerts SET resolved_at = ? WHERE id = ?`, resolved.UTC().Format(time.RFC3339Nano), a.ID); err != nil {
			t.Fatal(err)
		}
	}
	return a.ID
}

func at(v string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", v)
	if err != nil {
		panic(err)
	}
	return t
}

// seedBox gives Alice the box the app runs on (with uptime checks), a NAS
// that stopped answering an hour ago and a Pi with a fresh reading; outages
// and alerts on 10 March 2026 (UTC); Bob his own box with one check; Carol
// nothing.
func seedBox(t *testing.T, h *harness, s *Server) boxIDs {
	t.Helper()
	ctx := context.Background()
	var ids boxIDs
	ids.alice = h.user(s, "alice@example.com", "admin")
	ids.bob = h.user(s, "bob@example.com", "member")
	ids.carol = h.user(s, "carol@example.com", "member")
	alice, _ := s.store.GetUserByEmail(ctx, "alice@example.com")
	bob, _ := s.store.GetUserByEmail(ctx, "bob@example.com")
	now := time.Now()

	server := func(owner int64, name, kind string, seen time.Time) int64 {
		sv := &store.Server{UserID: owner, Name: name, Kind: kind, Enabled: true, IntervalS: 10, Color: "#000"}
		if kind == "agent" {
			sv.URL = "http://127.0.0.1:9"
		}
		if err := s.store.CreateServer(ctx, sv); err != nil {
			t.Fatal(err)
		}
		s.store.MarkSeen(ctx, sv.ID, store.FormatTime(seen), "")
		return sv.ID
	}
	ids.box = server(alice.ID, "Box", "local", now)
	ids.nas = server(alice.ID, "NAS", "agent", now.Add(-time.Hour))
	ids.pi = server(alice.ID, "Pi", "agent", now)
	ids.bobBox = server(bob.ID, "Bob's box", "local", now)

	// the Pi's last reading, as its poller keeps it
	pi, _ := s.store.GetServer(ctx, ids.pi)
	snap := &collect.Snapshot{Time: now, CPU: collect.CPU{Total: 12}, Mem: collect.Mem{Percent: 40},
		FS: []collect.FS{{Mount: "/", Percent: 61}}, Sensors: []collect.Sensor{{Kind: "temp", Label: "CPU", Value: 48}}}
	s.mon.mu.Lock()
	s.mon.states[ids.pi] = &serverState{srv: *pi, snap: snap, seen: now, started: now, cancel: func() {}, running: map[string]bool{}, sensors: map[string]string{}}
	s.mon.mu.Unlock()

	ms := func(v float64) *float64 { return &v }
	jf := &store.Check{ServerID: ids.box, Name: "Jellyfin", Kind: "http", Target: "https://watch.example", Group: "Media", IntervalS: 60, Enabled: true,
		Status: store.CheckUp, LastAt: now.Unix(), LastMs: ms(120), ChangedAt: at("2026-03-10 09:12").Unix()}
	putCheck(t, s, jf)
	nc := &store.Check{ServerID: ids.box, Name: "Nextcloud", Kind: "http", Target: "https://cloud.example", IntervalS: 60, Enabled: true,
		Status: store.CheckDown, LastAt: now.Unix(), LastMsg: "502 - Bad Gateway", ChangedAt: now.Add(-12 * time.Minute).Unix()}
	putCheck(t, s, nc)
	rq := &store.Check{ServerID: ids.box, Name: "Requests", Kind: "http", Target: "https://jellyfin.example/requests", IntervalS: 60, Enabled: true,
		Status: store.CheckUp, LastAt: now.Unix(), LastMs: ms(8.44)}
	putCheck(t, s, rq)
	old := &store.Check{ServerID: ids.box, Name: "Old blog", Kind: "http", Target: "https://blog.example", IntervalS: 60, Enabled: false, Status: store.CheckDown, LastAt: 1}
	putCheck(t, s, old)
	bs := &store.Check{ServerID: ids.bobBox, Name: "Bob's site", Kind: "http", Target: "https://jellyfin.bob.example", IntervalS: 60, Enabled: true,
		Status: store.CheckUp, LastAt: now.Unix()}
	putCheck(t, s, bs)
	ids.jellyfin, ids.nextcloud, ids.requests, ids.paused, ids.bobSite = jf.ID, nc.ID, rq.ID, old.ID, bs.ID

	// Jellyfin this month: three good results and one bad
	for i, st := range []int{store.CheckUp, store.CheckUp, store.CheckDown, store.CheckUp} {
		if err := s.store.AddBeat(ctx, jf.ID, store.Beat{T: now.Add(time.Duration(i-4) * time.Second).Unix(), Status: st}, false, s.loc()); err != nil {
			t.Fatal(err)
		}
	}
	// the ups and downs
	event := func(id int64, when time.Time, st int, msg string) {
		s.store.ImportCheckEvent(ctx, store.CheckEvent{CheckID: id, T: when.Unix(), Status: st, Msg: msg})
	}
	event(jf.ID, at("2026-03-10 09:00"), store.CheckDown, "timeout")
	event(jf.ID, at("2026-03-10 09:12"), store.CheckUp, "200 - OK")
	event(nc.ID, at("2026-03-09 23:50"), store.CheckDown, "502 - Bad Gateway")
	event(nc.ID, at("2026-03-10 00:20"), store.CheckUp, "200 - OK")
	event(nc.ID, now.Add(-12*time.Minute), store.CheckDown, "502 - Bad Gateway")
	event(bs.ID, at("2026-03-10 11:00"), store.CheckDown, "timeout")
	event(bs.ID, at("2026-03-10 11:30"), store.CheckUp, "200 - OK")

	// the alerts
	end := at("2026-03-10 14:03")
	ids.nasOffline = putAlert(t, s, store.Alert{ServerID: ids.nas, Metric: "offline", Severity: "critical", Message: "NAS is not answering (no reading for 1 min)"}, at("2026-03-10 14:00"), &end)
	ids.boxDisk = putAlert(t, s, store.Alert{ServerID: ids.box, Metric: "disk", Target: "/", Severity: "warning", Message: "Disk / is 92% full (limit 90%)"}, at("2026-03-10 16:00"), nil)
	ids.boxTemp = putAlert(t, s, store.Alert{ServerID: ids.box, Metric: "temp", Target: "cpu", Severity: "critical", Message: "CPU at 91 °C (limit 85 °C)"}, at("2026-03-08 10:00"), nil)
	ids.ck = putAlert(t, s, store.Alert{ServerID: ids.box, Metric: "check", Target: itoa64(nc.ID), Severity: "critical", Message: "Nextcloud is down: 502 - Bad Gateway"}, at("2026-03-10 09:01"), nil)
	putAlert(t, s, store.Alert{ServerID: ids.bobBox, Metric: "disk", Target: "/", Severity: "warning", Message: "Disk / of Bob is 95% full (limit 90%)"}, at("2026-03-10 12:00"), nil)
	return ids
}

// capItems are the rows of a capability answer.
func capItems(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	l, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("no items list in %v", m)
	}
	out := []map[string]any{}
	for _, v := range l {
		out = append(out, v.(map[string]any))
	}
	return out
}

// capItem finds the row with that title.
func capItem(t *testing.T, m map[string]any, title string) map[string]any {
	t.Helper()
	for _, it := range capItems(t, m) {
		if it["title"] == title {
			return it
		}
	}
	t.Fatalf("no %q in %v", title, m["items"])
	return nil
}

// capMetrics are a capability answer's numbers by label.
func capMetrics(m map[string]any) map[string]string {
	out := map[string]string{}
	l, _ := m["metrics"].([]any)
	for _, v := range l {
		x := v.(map[string]any)
		out[x["label"].(string)], _ = x["value"].(string)
	}
	return out
}

func (h *harness) get(tok, path string) map[string]any {
	h.t.Helper()
	var out map[string]any
	if code := h.do(tok, "GET", path, nil, &out); code != http.StatusOK {
		h.t.Fatalf("GET %s: %d %v", path, code, out)
	}
	return out
}

func TestPocketSearch(t *testing.T) {
	h, s := newHarness(t)
	ids := seedBox(t, h, s)
	box := itoa64(ids.box)

	// a check by its name leads; one that only checks an address with the word comes after
	all := capItems(t, h.get(ids.alice, "/pocket/search?q=jellyfin"))
	if len(all) != 2 || all[0]["title"] != "Jellyfin" || all[1]["title"] != "Requests" {
		t.Fatalf("jellyfin: %v", all)
	}
	jf := all[0]
	if jf["id"] != "monitor:"+itoa64(ids.jellyfin) || jf["kind"] != "monitor" || jf["url"] != "/server/"+box+"?tab=uptime&check="+itoa64(ids.jellyfin) ||
		jf["snippet"] != "up · 120 ms · 75.0% this month" || !strings.HasPrefix(jf["date"].(string), "2026-03-10T09:12") {
		t.Fatalf("Jellyfin: %v", jf)
	}
	if rq := all[1]; rq["snippet"] != "up · 8.4 ms" {
		t.Fatalf("Requests: %v", rq)
	}
	if nc := capItem(t, h.get(ids.alice, "/pocket/search?q=nextcloud"), "Nextcloud"); nc["snippet"] != "down" {
		t.Fatalf("a check that is down: %v", nc)
	}
	if old := capItem(t, h.get(ids.alice, "/pocket/search?q=blog"), "Old blog"); old["snippet"] != "paused" {
		t.Fatalf("a paused check: %v", old)
	}
	// by its group
	if got := capItems(t, h.get(ids.alice, "/pocket/search?q=media")); len(got) != 1 || got[0]["title"] != "Jellyfin" {
		t.Fatalf("by group: %v", got)
	}
	// servers: whether they answer, and the last reading of one that does
	if nas := capItem(t, h.get(ids.alice, "/pocket/search?q=nas"), "NAS"); nas["kind"] != "server" || nas["id"] != "server:"+itoa64(ids.nas) ||
		nas["url"] != "/server/"+itoa64(ids.nas) || nas["snippet"] != "offline" {
		t.Fatalf("NAS: %v", nas)
	}
	if pi := capItem(t, h.get(ids.alice, "/pocket/search?q=pi"), "Pi"); pi["snippet"] != "online · CPU 12% · memory 40% · disk 61%" {
		t.Fatalf("Pi: %v", pi)
	}
	if got := capItems(t, h.get(ids.alice, "/pocket/search?q=jellyfin&limit=1")); len(got) != 1 || got[0]["title"] != "Jellyfin" {
		t.Fatalf("limit 1: %v", got)
	}
	// nothing to look for
	for _, q := range []string{"", "%3F%3F", "*", "%25", "_", "+"} {
		if got := capItems(t, h.get(ids.alice, "/pocket/search?q="+q)); len(got) != 0 {
			t.Fatalf("q=%q finds %v", q, got)
		}
	}
	// another person's servers and checks never appear
	if got := capItems(t, h.get(ids.bob, "/pocket/search?q=jellyfin")); len(got) != 1 || got[0]["title"] != "Bob's site" {
		t.Fatalf("bob: %v", got)
	}
	for _, q := range []string{"nas", "pi", "nextcloud", "media", "box"} {
		for _, it := range capItems(t, h.get(ids.bob, "/pocket/search?q="+q)) {
			if it["title"] != "Bob's box" {
				t.Fatalf("bob finds alice's %q: %v", q, it)
			}
		}
	}
	if got := capItems(t, h.get(ids.carol, "/pocket/search?q=box")); len(got) != 0 {
		t.Fatalf("carol: %v", got)
	}
	// a server shared with someone is theirs to find, like on /servers
	carol, _ := s.store.GetUserByEmail(context.Background(), "carol@example.com")
	if _, err := s.store.CreateShare(context.Background(), ids.nas, carol.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := capItems(t, h.get(ids.carol, "/pocket/search?q=nas")); len(got) != 1 || got[0]["title"] != "NAS" {
		t.Fatalf("shared NAS: %v", got)
	}
	if c := h.do("", "GET", "/pocket/search?q=nas", nil, nil); c != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", c)
	}
}

func TestPocketDay(t *testing.T) {
	h, s := newHarness(t)
	ids := seedBox(t, h, s)
	box := itoa64(ids.box)

	d := h.get(ids.alice, "/pocket/day/2026-03-10?tz=UTC")
	if d["summary"] != "3 outages: Nextcloud down 20 min, Jellyfin down 12 min, NAS offline 3 min; 1 alert" {
		t.Fatalf("summary: %v", d["summary"])
	}
	if m := capMetrics(d); m["Down"] != "35 min" {
		t.Fatalf("metrics: %v", m)
	}
	list := capItems(t, d)
	if len(list) != 4 {
		t.Fatalf("items: %v", list)
	}
	nc := list[0]
	if nc["title"] != "Nextcloud down" || nc["kind"] != "outage" || nc["time"] != "00:00" || nc["end"] != "00:20" || nc["minutes"] != 20.0 ||
		nc["sub"] != "502 - Bad Gateway · since Mar 9 23:50" || nc["url"] != "/server/"+box+"?tab=uptime&check="+itoa64(ids.nextcloud) ||
		!strings.HasPrefix(nc["id"].(string), "outage:"+itoa64(ids.nextcloud)+"-") {
		t.Fatalf("Nextcloud: %v", nc)
	}
	if jf := list[1]; jf["title"] != "Jellyfin down" || jf["time"] != "09:00" || jf["end"] != "09:12" || jf["minutes"] != 12.0 || jf["sub"] != "timeout" {
		t.Fatalf("Jellyfin: %v", jf)
	}
	if nas := list[2]; nas["title"] != "NAS offline" || nas["kind"] != "outage" || nas["time"] != "14:00" || nas["end"] != "14:03" || nas["minutes"] != 3.0 ||
		nas["url"] != "/server/"+itoa64(ids.nas) || nas["id"] != "alert:"+itoa64(ids.nasOffline) {
		t.Fatalf("NAS: %v", nas)
	}
	// an alert that began that day and still goes on; a check's own alert is its outage above
	if disk := list[3]; disk["title"] != "Disk / is 92% full (limit 90%)" || disk["kind"] != "alert" || disk["time"] != "16:00" || disk["end"] != nil ||
		disk["sub"] != "Box · still going on" || disk["url"] != "/server/"+box || disk["minutes"] != nil {
		t.Fatalf("disk: %v", disk)
	}
	// the person's own time zone
	if jf := capItem(t, h.get(ids.alice, "/pocket/day/2026-03-10?tz=Europe/Sarajevo"), "Jellyfin down"); jf["time"] != "10:00" || jf["end"] != "10:12" {
		t.Fatalf("Sarajevo: %v", jf)
	}
	// an outage over midnight counts on the day before for its part
	prev := h.get(ids.alice, "/pocket/day/2026-03-09?tz=UTC")
	if prev["summary"] != "1 outage: Nextcloud down 10 min" {
		t.Fatalf("9 March: %v", prev)
	}
	if nc := capItems(t, prev)[0]; nc["time"] != "23:50" || nc["end"] != nil || nc["sub"] != "502 - Bad Gateway · until Mar 10 00:20" || nc["minutes"] != 10.0 {
		t.Fatalf("9 March Nextcloud: %v", nc)
	}
	// a day with nothing wrong is empty, even with an alert still open from before
	for _, day := range []string{"2026-03-11", "2026-03-07", "2099-01-01"} {
		if e := h.get(ids.alice, "/pocket/day/"+day+"?tz=UTC"); e["summary"] != nil || len(capItems(t, e)) != 0 {
			t.Fatalf("%s: %v", day, e)
		}
	}
	// the one alert of a day says what it was
	if d := h.get(ids.alice, "/pocket/day/2026-03-08?tz=UTC"); d["summary"] != "1 alert: CPU at 91 °C (limit 85 °C)" {
		t.Fatalf("8 March: %v", d)
	}
	// today: Nextcloud went down twelve minutes ago and is still down
	now := time.Now().UTC()
	today := h.get(ids.alice, "/pocket/day/"+now.Format("2006-01-02")+"?tz=UTC")
	want := "1 outage: Nextcloud down since " + now.Add(-12*time.Minute).Format("15:04")
	if now.Add(-12*time.Minute).Day() != now.Day() {
		want = "1 outage: Nextcloud still down"
	}
	if today["summary"] != want {
		t.Fatalf("today: %v, want %q", today["summary"], want)
	}
	if nc := capItem(t, today, "Nextcloud down"); nc["end"] != nil || !strings.Contains(nc["sub"].(string), "still going on") {
		t.Fatalf("today's Nextcloud: %v", nc)
	}
	// another person's outages and alerts never appear
	bob := h.get(ids.bob, "/pocket/day/2026-03-10?tz=UTC")
	if bob["summary"] != "1 outage: Bob's site down 30 min; 1 alert" || len(capItems(t, bob)) != 2 {
		t.Fatalf("bob: %v", bob)
	}
	if e := h.get(ids.carol, "/pocket/day/2026-03-10?tz=UTC"); len(capItems(t, e)) != 0 {
		t.Fatalf("carol: %v", e)
	}
	var bad map[string]any
	if c := h.do(ids.alice, "GET", "/pocket/day/2026-02-30", nil, &bad); c != http.StatusBadRequest {
		t.Fatalf("a day that does not exist: %d %v", c, bad)
	}
}

func TestPocketWidget(t *testing.T) {
	h, s := newHarness(t)
	ids := seedBox(t, h, s)
	box := itoa64(ids.box)

	w := h.get(ids.alice, "/pocket/widget")
	if w["line"] != "1 service down, 1 server offline, 2 warnings" || w["url"] != "/" {
		t.Fatalf("line: %v", w)
	}
	if m := capMetrics(w); len(m) != 4 || m["CPU"] != "12%" || m["Memory"] != "40%" || m["Disk"] != "61%" || m["Temperature"] != "48 °C" {
		t.Fatalf("metrics: %v", w["metrics"])
	}
	list := capItems(t, w)
	if len(list) != 4 {
		t.Fatalf("items: %v", list)
	}
	// what is down first: the server, the check; then the warnings, the worst first
	seen := time.Now().Add(-time.Hour).In(s.loc())
	since := "offline since " + seen.Format("15:04")
	if seen.Format("2006-01-02") != time.Now().In(s.loc()).Format("2006-01-02") {
		since = "offline since " + seen.Format("Jan 2 15:04")
	}
	if nas := list[0]; nas["title"] != "NAS" || nas["detail"] != since || nas["url"] != "/server/"+itoa64(ids.nas) || nas["id"] != "server:"+itoa64(ids.nas) {
		t.Fatalf("NAS: %v", nas)
	}
	if nc := list[1]; nc["title"] != "Nextcloud" || nc["detail"] != "down for 12 min · 502 - Bad Gateway" || nc["url"] != "/server/"+box+"?tab=uptime&check="+itoa64(ids.nextcloud) {
		t.Fatalf("Nextcloud: %v", nc)
	}
	if hot := list[2]; hot["title"] != "CPU at 91 °C (limit 85 °C)" || hot["detail"] != "Box" || hot["id"] != "alert:"+itoa64(ids.boxTemp) {
		t.Fatalf("temperature: %v", hot)
	}
	if disk := list[3]; disk["title"] != "Disk / is 92% full (limit 90%)" || disk["url"] != "/server/"+box {
		t.Fatalf("disk: %v", disk)
	}

	// all well: Bob's one check on his one box
	if b := h.get(ids.bob, "/pocket/widget"); b["line"] != "1 service up, 1 warning" || len(capItems(t, b)) != 1 || b["metrics"] != nil {
		t.Fatalf("bob: %v", b)
	}
	ctx := context.Background()
	if _, err := s.store.DB.ExecContext(ctx, `UPDATE alerts SET resolved_at = ? WHERE resolved_at IS NULL`, store.Now()); err != nil {
		t.Fatal(err)
	}
	if b := h.get(ids.bob, "/pocket/widget"); b["line"] != "1 service up" || len(capItems(t, b)) != 0 {
		t.Fatalf("bob all well: %v", b)
	}
	// Alice with every check up and the NAS back
	s.checks.mu.Lock()
	s.checks.checks[ids.nextcloud].Status = store.CheckUp
	s.checks.mu.Unlock()
	s.store.MarkSeen(ctx, ids.nas, store.Now(), "")
	s.mon.mu.Lock()
	if st := s.mon.states[ids.nas]; st != nil {
		st.mu.Lock()
		st.seen, st.err = time.Now(), ""
		st.mu.Unlock()
	}
	s.mon.mu.Unlock()
	if a := h.get(ids.alice, "/pocket/widget"); a["line"] != "All 3 services and 3 servers up" || len(capItems(t, a)) != 0 {
		t.Fatalf("alice all well: %v", a)
	}
	// someone without a server gets nothing
	if c := h.get(ids.carol, "/pocket/widget"); c["line"] != nil || len(capItems(t, c)) != 0 {
		t.Fatalf("carol: %v", c)
	}
}

func TestPocketLinkTokens(t *testing.T) {
	h, s, _ := newCapHarness(t)
	ids := seedBox(t, h, s)
	sid := itoa64(ids.box)
	const day = "/pocket/day/2026-03-10?tz=UTC"
	for _, c := range []struct {
		tok, method, path string
		want              int
	}{
		{"pcx_alice_search", "GET", "/pocket/search?q=jellyfin", 200},
		{"pcx_alice_day", "GET", day, 200},
		{"pcx_alice_widget", "GET", "/pocket/widget", 200},
		{"pcx_alice_widget", "GET", "/auth/me", 200},
		// another capability's route
		{"pcx_alice_search", "GET", day, 403},
		{"pcx_alice_search", "GET", "/pocket/widget", 403},
		{"pcx_alice_day", "GET", "/pocket/search?q=nas", 403},
		{"pcx_alice_day", "GET", "/pocket/widget", 403},
		{"pcx_alice_widget", "GET", "/pocket/search?q=nas", 403},
		{"pcx_alice_widget", "GET", day, 403},
		// nothing can be kept here: there is no such route, and the capture scope opens nothing else
		{"pcx_alice_capture", "POST", "/pocket/capture", 404},
		{"pcx_alice_capture", "GET", "/pocket/widget", 403},
		// the rest of the app
		{"pcx_alice_widget", "GET", "/servers", 403},
		{"pcx_alice_widget", "GET", "/alerts", 403},
		{"pcx_alice_search", "GET", "/servers/" + sid + "/checks", 403},
		{"pcx_alice_day", "GET", "/servers/" + sid + "/checks/events", 403},
		{"pcx_alice_widget", "GET", "/settings", 403},
		// nothing runs a check or changes one
		{"pcx_alice_widget", "POST", "/servers/" + sid + "/checks/" + itoa64(ids.jellyfin) + "/run", 403},
		{"pcx_alice_search", "PUT", "/servers/" + sid + "/checks/" + itoa64(ids.jellyfin), 403},
		{"pcx_alice_widget", "POST", "/pocket/widget", 405},
		// the app's own scope does not open the new routes, and still reads what it read
		{"pcx_alice_read", "GET", "/pocket/search?q=jellyfin", 403},
		{"pcx_alice_read", "GET", day, 403},
		{"pcx_alice_read", "GET", "/pocket/widget", 403},
		{"pcx_alice_read", "GET", "/servers", 200},
		{"pcx_alice_read", "GET", "/servers/" + sid + "/checks", 200},
		{"pcx_nobody", "GET", "/pocket/widget", 401},
	} {
		var body any
		if c.method != "GET" {
			body = map[string]any{"request_id": "req-0000000000000001", "text": "Jellyfin"}
		}
		if code := h.do(c.tok, c.method, c.path, body, nil); code != c.want {
			t.Errorf("%s %s %s → %d, want %d", c.tok, c.method, c.path, code, c.want)
		}
	}
	// a person signed in finds no capture route
	if code := h.do(ids.alice, "POST", "/pocket/capture", map[string]any{"request_id": "req-0000000000000002", "text": "x"}, nil); code != http.StatusNotFound {
		t.Errorf("capture as a person: %d", code)
	}
	// the token acts for its person only
	if got := capItems(t, h.get("pcx_alice_search", "/pocket/search?q=jellyfin")); len(got) != 2 || got[0]["title"] != "Jellyfin" {
		t.Fatalf("alice through the link: %v", got)
	}
	if got := capItems(t, h.get("pcx_bob_search", "/pocket/search?q=nextcloud")); len(got) != 0 {
		t.Fatalf("bob through the link finds alice's check: %v", got)
	}
	if w := h.get("pcx_alice_widget", "/pocket/widget"); w["line"] != "1 service down, 1 server offline, 2 warnings" {
		t.Fatalf("alice's widget through the link: %v", w)
	}
	if w := h.get("pcx_bob_widget", "/pocket/widget"); w["line"] != "1 service up, 1 warning" {
		t.Fatalf("bob's widget through the link: %v", w)
	}
	if d := h.get("pcx_alice_day", day); !strings.HasPrefix(d["summary"].(string), "3 outages:") {
		t.Fatalf("alice's day through the link: %v", d)
	}
	if d := h.get("pcx_bob_day", day); d["summary"] != "1 outage: Bob's site down 30 min; 1 alert" {
		t.Fatalf("bob's day through the link: %v", d)
	}
}

func TestPocketLinkScopes(t *testing.T) {
	req := func(method, path string) *http.Request { return httptest.NewRequest(method, path, nil) }
	for _, c := range []struct {
		scope, method, path string
		want                bool
	}{
		{core.ScopeSearch, "GET", "/api/v1/pocket/search?q=nas", true},
		{core.ScopeDay, "GET", "/api/v1/pocket/day/2026-03-10", true},
		{core.ScopeWidget, "GET", "/api/v1/pocket/widget", true},
		{core.ScopeWidget, "GET", "/api/v1/auth/me", true},
		{core.ScopeDay, "GET", "/api/v1/pocket/day/2026-03-10/more", false},
		{core.ScopeSearch, "POST", "/api/v1/pocket/search", false},
		{core.ScopeWidget, "DELETE", "/api/v1/pocket/widget", false},
		{core.ScopeCapture, "POST", "/api/v1/pocket/capture", false},
		{core.ScopeWidget, "GET", "/api/v1/servers", false},
		{"monitoring:read", "GET", "/api/v1/pocket/widget", false},
		{"monitoring:read", "GET", "/api/v1/pocket/search", false},
		{"monitoring:read", "GET", "/api/v1/servers", true},
	} {
		if got := linkAllows([]string{c.scope}, req(c.method, c.path)); got != c.want {
			t.Errorf("%s %s %s = %v, want %v", c.scope, c.method, c.path, got, c.want)
		}
	}
}

func TestManifestCapabilities(t *testing.T) {
	_, s, fc := newCapHarness(t)
	if err := s.registerWithCore(context.Background()); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	m := fc.manifest
	fc.mu.Unlock()
	if m["slug"] != "monitoring" || m["category"] != "System" {
		t.Fatalf("manifest: %v %v", m["slug"], m["category"])
	}
	search, _ := m["search"].(map[string]any)
	day, _ := m["day"].(map[string]any)
	widget, _ := m["widget"].(map[string]any)
	if search == nil || day == nil || day["label"] != "Monitoring" || day["one"] != "incident" || day["many"] != "incidents" ||
		widget == nil || widget["title"] != "Box status" || m["capture"] != nil {
		t.Fatalf("capabilities: %v %v %v %v", search, day, widget, m["capture"])
	}
	bangs := map[string]bool{}
	for _, b := range search["bangs"].([]any) {
		bangs[b.(string)] = true
	}
	kinds, _ := search["kinds"].([]any)
	if len(bangs) != 2 || !bangs["monitoring"] || !bangs["status"] || len(kinds) != 2 || kinds[0] != "monitor" {
		t.Fatalf("search: %v", search)
	}
	offered := map[string]bool{}
	for _, o := range m["offers"].([]any) {
		offered[o.(map[string]any)["scope"].(string)] = true
	}
	for _, sc := range []string{"monitoring:read", core.ScopeSearch, core.ScopeDay, core.ScopeWidget} {
		if !offered[sc] {
			t.Errorf("%s is not offered", sc)
		}
	}
	if offered[core.ScopeCapture] {
		t.Error("a quick line is offered, but nothing can be kept here")
	}
}

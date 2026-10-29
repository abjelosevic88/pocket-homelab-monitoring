package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// fakeSpeedCLI points the hub at a script that prints the recorded run
// (ok) or fails like the CLI does when the connection is down.
func fakeSpeedCLI(t *testing.T, s *Server, ok bool) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "speedtest")
	fixture, _ := filepath.Abs("../speedtest/testdata/run.jsonl")
	script := "#!/bin/sh\ncat " + fixture + "\n"
	if !ok {
		script = "#!/bin/sh\necho '{\"type\":\"log\",\"message\":\"Cannot open socket\",\"level\":\"error\"}' >&2\nexit 2\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s.speed.runner.Bin = bin
}

func waitSpeedIdle(t *testing.T, s *Server) {
	t.Helper()
	for i := 0; i < 200; i++ {
		s.speed.mu.Lock()
		idle := s.speed.live == nil
		s.speed.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("speed test never finished")
}

func localServer(t *testing.T, s *Server, email string) int64 {
	t.Helper()
	ctx := context.Background()
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	sv := &store.Server{UserID: u.ID, Name: "Home", Kind: "local", Enabled: true, IntervalS: 5}
	if err := s.store.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	return sv.ID
}

func TestSpeedTests(t *testing.T) {
	h, s := newHarness(t)
	owner := h.user(s, "owner@example.com", "admin")
	viewer := h.user(s, "viewer@example.com", "member")
	stranger := h.user(s, "stranger@example.com", "member")
	id := localServer(t, s, "owner@example.com")
	base := "/servers/" + itoa64(id) + "/speedtest"
	fakeSpeedCLI(t, s, true)

	var info struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
		Owner     bool   `json:"owner"`
	}
	if c := h.do(owner, "GET", base, nil, &info); c != http.StatusOK || !info.Available || !info.Owner {
		t.Fatalf("info: %d %+v", c, info)
	}

	// turning the schedule on adds the speed rules, from the minimums
	if c := h.do(owner, "PUT", base+"/settings", map[string]any{"enabled": true, "every_min": 180, "offset_min": 7, "plan_down": 150, "plan_up": 75, "min_down": 90, "min_up": 45, "max_ping": 30}, nil); c != http.StatusOK {
		t.Fatalf("settings: %d", c)
	}
	var rules []store.Rule
	h.do(owner, "GET", "/rules?server_id="+itoa64(id), nil, &rules)
	speedRules := map[string]store.Rule{}
	for _, r := range rules {
		if isSpeedMetric(r.Metric) {
			speedRules[r.Metric] = r
		}
	}
	if len(speedRules) != 4 || speedRules["speed_down"].Threshold != 90 || !speedRules["speed_down"].Enabled || speedRules["speedtest"].ForS != 2 {
		t.Fatalf("rules: %+v", speedRules)
	}
	if c := h.do(owner, "PUT", base+"/settings", map[string]any{"every_min": 7}, nil); c != http.StatusBadRequest {
		t.Fatalf("odd interval accepted: %d", c)
	}

	// run one by hand
	if c := h.do(owner, "POST", base+"/run", map[string]any{}, nil); c != http.StatusAccepted {
		t.Fatalf("run: %d", c)
	}
	waitSpeedIdle(t, s)
	var list []store.SpeedTest
	if c := h.do(owner, "GET", base+"/results?range=24h", nil, &list); c != http.StatusOK || len(list) != 1 {
		t.Fatalf("results: %d %d", c, len(list))
	}
	got := list[0]
	if !got.OK || got.Trigger != "manual" || *got.DownMbps < 150 || got.TestServer != "Example Net" || got.ExternalIP != "198.51.100.7" {
		t.Fatalf("result: %+v", got)
	}

	// sharing: a viewer reads but can't run, a stranger sees nothing
	vu, _ := s.store.GetUserByEmail(context.Background(), "viewer@example.com")
	if c := h.do(owner, "POST", "/servers/"+itoa64(id)+"/shares", map[string]any{"user_id": vu.ID}, nil); c != http.StatusCreated && c != http.StatusOK {
		t.Fatalf("share: %d", c)
	}
	if c := h.do(viewer, "GET", base+"/results", nil, &list); c != http.StatusOK || len(list) != 1 {
		t.Fatalf("viewer results: %d %d", c, len(list))
	}
	if c := h.do(viewer, "POST", base+"/run", nil, nil); c != http.StatusForbidden {
		t.Fatalf("viewer run: %d", c)
	}
	if c := h.do(stranger, "GET", base+"/results", nil, nil); c != http.StatusNotFound {
		t.Fatalf("stranger: %d", c)
	}

	// two failures in a row open the "speed test" alert; a good test resolves it
	fakeSpeedCLI(t, s, false)
	for i := 0; i < 2; i++ {
		if c := h.do(owner, "POST", base+"/run", nil, nil); c != http.StatusAccepted {
			t.Fatalf("run %d: %d", i, c)
		}
		waitSpeedIdle(t, s)
	}
	var alerts []store.Alert
	h.do(owner, "GET", "/alerts", nil, &alerts)
	if len(alerts) != 1 || alerts[0].Metric != "speedtest" {
		t.Fatalf("alerts after failures: %+v", alerts)
	}
	fakeSpeedCLI(t, s, true)
	h.do(owner, "POST", base+"/run", nil, nil)
	waitSpeedIdle(t, s)
	h.do(owner, "GET", "/alerts", nil, &alerts)
	if len(alerts) != 0 {
		t.Fatalf("alert not resolved: %+v", alerts)
	}

	// notes, delete, CSV
	h.do(owner, "GET", base+"/results", nil, &list)
	if c := h.do(owner, "PUT", base+"/results/"+itoa64(list[0].ID), map[string]any{"note": "router rebooted"}, nil); c != http.StatusOK {
		t.Fatalf("note: %d", c)
	}
	if c := h.do(owner, "DELETE", base+"/results/"+itoa64(list[1].ID), nil, nil); c != http.StatusNoContent {
		t.Fatalf("delete: %d", c)
	}
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1"+base+"/export.csv", nil)
	req.Header.Set("Authorization", "Bearer "+owner)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if lines := bytes.Count(b, []byte("\n")); lines != 4 || !bytes.Contains(b, []byte("router rebooted")) {
		t.Fatalf("csv (%d lines):\n%s", lines, b)
	}
}

func TestSpeedImportFromSpeedtestTracker(t *testing.T) {
	h, s := newHarness(t)
	owner := h.user(s, "owner@example.com", "admin")
	id := localServer(t, s, "owner@example.com")

	path := filepath.Join(t.TempDir(), "database.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := os.ReadFile("../speedtest/testdata/run.jsonl")
	last := result[bytes.LastIndexByte(bytes.TrimRight(result, "\n"), '\n')+1:]
	for _, q := range []string{
		`CREATE TABLE results (id integer primary key, service varchar, ping float, download integer, upload integer, comments text, data text, status varchar not null, scheduled tinyint(1), created_at datetime, updated_at datetime)`,
		`INSERT INTO results (ping, download, upload, data, status, scheduled, created_at) VALUES (1.9, 19023437, 9528508, '` + string(bytes.TrimSpace(last)) + `', 'completed', 1, '2026-10-29 21:11:00')`,
		`INSERT INTO results (data, status, scheduled, created_at) VALUES ('{"type":"log","level":"error","message":"Configuration - Could not retrieve or read configuration (ConfigurationError)"}', 'failed', 1, '2026-10-29 18:07:00')`,
		`INSERT INTO results (status, created_at) VALUES ('running', '2026-10-29 21:30:00')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	upload := func() map[string]int {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "database.sqlite")
		b, _ := os.ReadFile(path)
		_, _ = fw.Write(b)
		mw.Close()
		req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/servers/"+itoa64(id)+"/speedtest/import", &body)
		req.Header.Set("Authorization", "Bearer "+owner)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]int
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("import: %d %s", res.StatusCode, b)
		}
		_ = json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if got := upload(); got["added"] != 2 || got["failed"] != 1 {
		t.Fatalf("first import: %v", got)
	}
	if got := upload(); got["added"] != 0 || got["skipped"] != 2 {
		t.Fatalf("second import should skip: %v", got)
	}
	var list []store.SpeedTest
	h.do(owner, "GET", "/servers/"+itoa64(id)+"/speedtest/results", nil, &list)
	if len(list) != 2 || list[0].OK || list[1].Trigger != "import" || !list[1].OK || list[1].TestServer != "Example Net" {
		t.Fatalf("imported: %+v", list)
	}
}

func TestSpeedSlots(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Sarajevo")
	st := store.SpeedSettings{EveryMin: 180, OffsetMin: 7}
	at := func(s string) time.Time { x, _ := time.ParseInLocation("2006-01-02 15:04", s, loc); return x }
	for _, c := range []struct{ now, slot, next string }{
		{"2026-10-29 21:08", "2026-10-29 21:07", "2026-10-30 00:07"},
		{"2026-10-29 21:06", "2026-10-29 18:07", "2026-10-29 21:07"},
		{"2026-10-30 00:03", "2026-10-29 21:07", "2026-10-30 00:07"},
	} {
		if got := speedSlot(at(c.now), st, loc); !got.Equal(at(c.slot)) {
			t.Errorf("slot(%s) = %s, want %s", c.now, got.In(loc).Format("2006-01-02 15:04"), c.slot)
		}
		if got := speedNext(at(c.now), st, loc); !got.Equal(at(c.next)) {
			t.Errorf("next(%s) = %s, want %s", c.now, got.In(loc).Format("2006-01-02 15:04"), c.next)
		}
	}
}

func TestSpeedConditions(t *testing.T) {
	ok := func(down float64) store.SpeedTest {
		return store.SpeedTest{OK: true, DownMbps: &down, UpMbps: fp(70), PingMs: fp(2)}
	}
	rule := store.Rule{Metric: "speed_down", Threshold: 90, ForS: 2}
	// newest first: one slow test isn't enough, two are
	if c := speedConditions(rule, []store.SpeedTest{ok(40), ok(150)}); c[0].fire {
		t.Fatal("one slow test fired")
	}
	c := speedConditions(rule, []store.SpeedTest{ok(40), {OK: false}, ok(60), ok(150)})
	if !c[0].fire || c[0].value != 40 {
		t.Fatalf("two slow tests (a failure between doesn't count): %+v", c)
	}
	// clears only a little above the line
	if c := speedConditions(rule, []store.SpeedTest{ok(91)}); c[0].clear {
		t.Fatal("cleared within the margin")
	}
	if c := speedConditions(rule, []store.SpeedTest{ok(120)}); !c[0].clear {
		t.Fatal("didn't clear")
	}
	fail := store.Rule{Metric: "speedtest", ForS: 2}
	if c := speedConditions(fail, []store.SpeedTest{{OK: false, Error: "no route"}, {OK: false}, ok(150)}); !c[0].fire || c[0].value != 2 {
		t.Fatalf("failures: %+v", c)
	}
}

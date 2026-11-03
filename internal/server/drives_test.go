package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

func testDrive(key, status string, errs int64) collect.Drive {
	wear := 12.0
	d := collect.Drive{Key: key, Name: "/dev/" + key, Model: "Disk " + key, Serial: strings.ToUpper(key), Status: status, Errors: errs, Reallocated: errs,
		Temp: 40, TempLimit: 55, PowerOnHours: 100, Wear: &wear, ReadAt: time.Now().UTC(), Attrs: []collect.DriveAttr{{Key: "5", ID: 5, Name: "Reallocated Sector Ct", Raw: errs}}}
	if status == "failed" {
		d.Problems = []string{"the drive's own SMART health check says it is failing"}
	}
	return d
}

func TestDriveAlerts(t *testing.T) {
	h, s := newHarness(t)
	tok := h.user(s, "owner@example.com", "admin")
	ctx := context.Background()
	var me struct{ ID int64 }
	h.do(tok, "GET", "/auth/me", nil, &me)
	sv := &store.Server{UserID: me.ID, Name: "NAS", Kind: "agent", URL: "http://127.0.0.1:9", Enabled: false, IntervalS: 10, Color: "#000"}
	if err := s.store.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	for _, r := range defaultRules(sv.ID) {
		_ = s.store.SaveRule(ctx, &r)
	}
	s.dropRules()
	st := &serverState{srv: *sv, running: map[string]bool{}, sensors: map[string]string{}, drives: s.store.DrivesSeen(ctx, sv.ID)}
	s.mon.mu.Lock()
	s.mon.states[sv.ID] = st
	s.mon.mu.Unlock()

	now := time.Now()
	pass := func(at time.Time, drives ...collect.Drive) {
		snap := &collect.Snapshot{Time: at, Drives: drives, DrivesAt: at}
		s.record(ctx, st, snap, nil)
	}
	open := func() map[string]string {
		list, _ := s.store.ListAlerts(ctx, []int64{sv.ID}, true, 0)
		out := map[string]string{}
		for _, a := range list {
			out[a.Metric+":"+a.Target] = a.Message
		}
		return out
	}

	pass(now, testDrive("sda", "passed", 0), testDrive("sdb", "failed", 3))
	got := open()
	if len(got) != 2 || !strings.Contains(got["drive:sdb"], "is failing") || !strings.Contains(got["drive_errors:sdb"], "3 reallocated") {
		t.Fatalf("after the first pass: %v", got)
	}

	// An agent restarting has no SMART pass yet: alerts stay as they are.
	s.record(ctx, st, &collect.Snapshot{Time: now.Add(time.Minute)}, nil)
	if len(open()) != 2 {
		t.Fatalf("a reading without SMART changed the alerts: %v", open())
	}

	// sdb dies and drops off the list: its alerts stay, the drive rule turns into "missing" after an hour.
	pass(now.Add(2*time.Minute), testDrive("sda", "passed", 0))
	if len(open()) != 2 {
		t.Fatalf("a vanished drive resolved its alerts: %v", open())
	}
	st.mu.Lock()
	k := st.drives["sdb"]
	k.LastSeen = now.Add(-2 * time.Hour).Unix()
	st.drives["sdb"] = k
	st.mu.Unlock()
	s.alerts.mu.Lock()
	for key, a := range s.alerts.open {
		if a.Metric == "drive" {
			delete(s.alerts.open, key) // as if it had never been failing: let "missing" fire on its own
			s.store.ResolveAlert(ctx, a.ID)
		}
	}
	s.alerts.mu.Unlock()
	pass(now.Add(3*time.Minute), testDrive("sda", "passed", 0))
	if m := open()["drive:sdb"]; !strings.Contains(m, "has not answered") {
		t.Fatalf("missing drive: %q (%v)", m, open())
	}

	// The API lists both, sdb as missing; forgetting it resolves its alerts.
	var list []struct {
		Missing bool `json:"missing"`
		Drive   struct {
			Key string `json:"key"`
		} `json:"drive"`
	}
	if c := h.do(tok, "GET", "/drives", nil, &list); c != http.StatusOK || len(list) != 2 {
		t.Fatalf("list: %d %v", c, list)
	}
	for _, d := range list {
		if d.Missing != (d.Drive.Key == "sdb") {
			t.Fatalf("missing flags: %+v", list)
		}
	}
	var det struct {
		History []store.DriveSample `json:"history"`
	}
	if c := h.do(tok, "GET", "/servers/"+itoa64(sv.ID)+"/drives/sda", nil, &det); c != http.StatusOK || len(det.History) == 0 || det.History[0].Attrs["5"] != 0 {
		t.Fatalf("detail: %d %+v", c, det)
	}
	if c := h.do(tok, "DELETE", "/servers/"+itoa64(sv.ID)+"/drives/sdb", nil, nil); c != http.StatusNoContent {
		t.Fatalf("forget: %d", c)
	}
	pass(now.Add(4*time.Minute), testDrive("sda", "passed", 0))
	if len(open()) != 0 {
		t.Fatalf("forgotten drive kept its alerts: %v", open())
	}
}

func TestDriveTempUsesTheDrivesOwnLimit(t *testing.T) {
	r := store.Rule{Metric: "drive_temp"}
	d := testDrive("sda", "passed", 0)
	d.Temp = 58
	c := driveConditions(r, &collect.Snapshot{Drives: []collect.Drive{d}, DrivesAt: time.Now()}, nil, time.Now())
	if len(c) != 1 || !c[0].fire {
		t.Fatalf("58 °C over a 55 °C limit: %+v", c)
	}
	r.Threshold = 60
	c = driveConditions(r, &collect.Snapshot{Drives: []collect.Drive{d}, DrivesAt: time.Now()}, nil, time.Now())
	if c[0].fire {
		t.Fatal("an explicit 60 °C limit fired at 58")
	}
	d.Standby = true
	r.Threshold = 0
	c = driveConditions(r, &collect.Snapshot{Drives: []collect.Drive{d}, DrivesAt: time.Now()}, nil, time.Now())
	if c[0].fire || c[0].clear {
		t.Fatal("a sleeping drive's old temperature decided the alert")
	}
}

func TestScrutinyImport(t *testing.T) {
	scrutiny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/summary":
			_, _ = w.Write([]byte(`{"data":{"summary":{"0x5000":{"device":{"wwn":"0x5000","serial_number":"SDA","model_name":"Disk sda","host_id":"nas"}},
				"nvme1":{"device":{"wwn":"nvme1","serial_number":"ELSEWHERE","model_name":"Other"}}}}}`))
		case strings.HasPrefix(r.URL.Path, "/api/device/0x5000/details"):
			day := "2026-09-01T00:00:00Z"
			if r.URL.Query().Get("duration_key") == "week" {
				day = "2026-10-30T00:00:00Z"
			}
			_, _ = w.Write([]byte(`{"data":{"smart_results":[{"date":"` + day + `","Status":0,"temp":31,"power_on_hours":50,"device_protocol":"ATA",
				"attrs":{"5":{"raw_value":0},"197":{"raw_value":2},"194":{"raw_value":124554051617}}}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer scrutiny.Close()
	h, s := newHarness(t)
	tok := h.user(s, "owner@example.com", "admin")
	ctx := context.Background()
	var me struct{ ID int64 }
	h.do(tok, "GET", "/auth/me", nil, &me)
	sv := &store.Server{UserID: me.ID, Name: "NAS", Kind: "agent", URL: "http://127.0.0.1:9", IntervalS: 10, Color: "#000"}
	_ = s.store.CreateServer(ctx, sv)
	s.saveDrives(ctx, *sv, []collect.Drive{testDrive("sda", "passed", 0)}, time.Now())

	var res struct {
		Points int `json:"points"`
		Drives []struct {
			Matched bool `json:"matched"`
			Points  int  `json:"points"`
		} `json:"drives"`
	}
	if c := h.do(tok, "POST", "/drives/import/scrutiny", map[string]any{"url": scrutiny.URL}, &res); c != http.StatusOK {
		t.Fatalf("import: %d", c)
	}
	if res.Points != 2 || len(res.Drives) != 2 {
		t.Fatalf("import result: %+v", res)
	}
	hist, _ := s.store.DriveHistory(ctx, sv.ID, "sda", 0)
	if len(hist) != 3 || hist[0].Temp == nil || *hist[0].Temp != 31 || hist[0].Errors == nil || *hist[0].Errors != 2 || hist[0].Attrs["194"] != 33 {
		t.Fatalf("history: %+v", hist[0])
	}
	// importing again adds nothing
	h.do(tok, "POST", "/drives/import/scrutiny", map[string]any{"url": scrutiny.URL}, &res)
	if res.Points != 0 {
		t.Fatalf("second import added %d points", res.Points)
	}
}

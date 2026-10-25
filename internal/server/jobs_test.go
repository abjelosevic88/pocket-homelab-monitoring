package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/jobs"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// fakeAgent answers the jobs endpoints like an agent with two timers, one failed.
func fakeAgent(t *testing.T, token string) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	actions := []string{}
	now := time.Now().UnixMilli()
	status := jobs.Status{Host: "box", Now: now, Jobs: []jobs.Job{
		{Unit: "backup.timer", Service: "backup.service", Scope: "user", Enabled: true, Scheduled: true, Next: now + 3600e3, State: "inactive", Result: "success", Control: true, Schedule: []string{"OnCalendar=*-*-* 03:00:00"}},
		{Unit: "music-scan.timer", Service: "music-scan.service", Scope: "user", Enabled: true, Scheduled: true, Next: now + 600e3, State: "failed", Result: "exit-code", ExitStatus: 2, Control: true},
	}, Runs: []jobs.Run{
		{Unit: "backup.timer", Invocation: "11111111111111111111111111111111", Start: now - 60e3, End: now - 50e3, OK: true, Result: "success", Summary: "3 snapshots", Lines: 4},
		{Unit: "backup.timer", Invocation: "22222222222222222222222222222222", Start: now - 40e3, End: now - 39e3, OK: true, Result: "success"},
		{Unit: "music-scan.timer", Invocation: "33333333333333333333333333333333", Start: now - 20e3, End: now - 10e3, OK: false, Result: "exit-code", ExitStatus: 2, Summary: "navidrome unreachable", Lines: 1, Warnings: 1},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/agent/v1/jobs":
			_ = json.NewEncoder(w).Encode(status)
		case "/agent/v1/jobs/action":
			mu.Lock()
			actions = append(actions, r.URL.Query().Get("action")+" "+r.URL.Query().Get("unit"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(status.Jobs[0])
		case "/agent/v1/jobs/log":
			_ = json.NewEncoder(w).Encode(map[string]any{"lines": []jobs.LogLine{{T: now, Msg: "hello", Invocation: r.URL.Query().Get("invocation")}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &actions
}

func TestJobs(t *testing.T) {
	h, s := newHarness(t)
	owner := h.user(s, "owner@example.com", "admin")
	other := h.user(s, "other@example.com", "member")
	const tok = "0123456789abcdef0123"
	agent, actions := fakeAgent(t, tok)

	var sv struct{ ID int64 }
	if c := h.do(owner, "POST", "/servers", map[string]any{"kind": "agent", "name": "Box", "url": agent.URL, "token": tok}, &sv); c != http.StatusCreated {
		t.Fatalf("create: %d", c)
	}
	id := itoa64(sv.ID)
	var list struct {
		Available bool `json:"available"`
		Owner     bool `json:"owner"`
		Jobs      []struct {
			Unit, Status, Problem, Group, Name string
			Week                               struct{ Runs, Failed, Busy int }
			LastKept                           *struct{ Summary string } `json:"last_kept"`
		} `json:"jobs"`
	}
	if c := h.do(owner, "GET", "/servers/"+id+"/jobs", nil, &list); c != http.StatusOK || !list.Available || len(list.Jobs) != 2 {
		t.Fatalf("jobs: %d %+v", c, list)
	}
	b, m := list.Jobs[0], list.Jobs[1]
	if b.Status != "ok" || b.Week.Runs != 2 || b.Week.Busy != 1 || b.LastKept == nil || b.LastKept.Summary != "3 snapshots" || b.Group != "Backups" {
		t.Errorf("backup job: %+v", b)
	}
	if m.Status != "failed" || m.Week.Failed != 1 || m.Group != "Music" {
		t.Errorf("music job: %+v", m)
	}

	// Read twice: runs count once.
	s.pollJobs(t.Context(), mustServer(t, s, sv.ID))
	h.do(owner, "GET", "/servers/"+id+"/jobs", nil, &list)
	if list.Jobs[0].Week.Runs != 2 {
		t.Errorf("a run read twice counted twice: %+v", list.Jobs[0].Week)
	}

	// The failure opened a job alert.
	var alerts []struct{ Metric, Target, Message string }
	h.do(owner, "GET", "/alerts?server_id="+id, nil, &alerts)
	found := false
	for _, a := range alerts {
		if a.Metric == "job" && a.Target == "music-scan.timer" {
			found = true
		}
	}
	if !found {
		t.Errorf("no alert for the failed job: %+v", alerts)
	}
	// Muting it closes the alert.
	if c := h.do(owner, "PUT", "/servers/"+id+"/jobs/settings", map[string]any{"unit": "music-scan.timer", "muted": true, "group": "Library"}, nil); c != http.StatusOK {
		t.Fatalf("settings: %d", c)
	}
	h.do(owner, "GET", "/alerts?server_id="+id, nil, &alerts)
	for _, a := range alerts {
		if a.Metric == "job" {
			t.Errorf("a muted job still alerts: %+v", a)
		}
	}

	var runs []struct{ Unit, Summary string }
	h.do(owner, "GET", "/servers/"+id+"/jobs/runs", nil, &runs)
	if len(runs) != 2 { // the quiet success isn't listed with all jobs
		t.Errorf("runs: %+v", runs)
	}
	h.do(owner, "GET", "/servers/"+id+"/jobs/runs?unit=backup.timer", nil, &runs)
	if len(runs) != 2 {
		t.Errorf("one job's runs include quiet ones: %+v", runs)
	}

	if c := h.do(owner, "POST", "/servers/"+id+"/jobs/action", map[string]any{"unit": "backup.timer", "action": "run"}, nil); c != http.StatusOK || len(*actions) != 1 || (*actions)[0] != "run backup.timer" {
		t.Errorf("run: %d %v", c, *actions)
	}
	if c := h.do(owner, "POST", "/servers/"+id+"/jobs/action", map[string]any{"unit": "backup.timer", "action": "rm -rf"}, nil); c != http.StatusBadRequest {
		t.Errorf("bad action: %d", c)
	}

	// Someone else: not found. A viewer: sees the list, not the log, can't act.
	for _, p := range []string{"/servers/" + id + "/jobs", "/servers/" + id + "/jobs/runs", "/servers/" + id + "/jobs/log?unit=backup.timer"} {
		if c := h.do(other, "GET", p, nil, nil); c != http.StatusNotFound {
			t.Errorf("GET %s as another person: %d", p, c)
		}
	}
	var me struct{ ID int64 }
	h.do(other, "GET", "/auth/me", nil, &me)
	h.do(owner, "POST", "/servers/"+id+"/shares", map[string]any{"user_id": me.ID}, nil)
	if c := h.do(other, "GET", "/servers/"+id+"/jobs", nil, &list); c != http.StatusOK || list.Owner {
		t.Errorf("viewer list: %d owner=%v", c, list.Owner)
	}
	if c := h.do(other, "GET", "/servers/"+id+"/jobs/log?unit=backup.timer", nil, nil); c != http.StatusForbidden {
		t.Errorf("viewer log: %d", c)
	}
	if c := h.do(other, "POST", "/servers/"+id+"/jobs/action", map[string]any{"unit": "backup.timer", "action": "pause"}, nil); c != http.StatusForbidden {
		t.Errorf("viewer action: %d", c)
	}
}

func mustServer(t *testing.T, s *Server, id int64) store.Server {
	t.Helper()
	sv, err := s.store.GetServer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return *sv
}

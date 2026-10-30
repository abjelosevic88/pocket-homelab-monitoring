package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/abjelosevic88/pocket-homelab-core/sdk"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// A Pocket app's own jobs show on the box's Jobs tab, read through a Core
// link as the server's owner; Run now goes to the app, pausing does not.
func TestAppJobs(t *testing.T) {
	h, s := newHarness(t)
	owner := h.user(s, "owner@example.com", "admin")
	now := time.Now().UnixMilli()

	var mu sync.Mutex
	var ran []string
	var sinceSeen []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pcx_money" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/scheduled-jobs":
			mu.Lock()
			sinceSeen = append(sinceSeen, r.URL.Query().Get("since"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{
					{"id": "day-review", "name": "Day review", "description": "AI looks over the day", "schedule": "every day at 00:00", "enabled": true, "next": now + 3600e3,
						"last": map[string]any{"start": now - 60e3, "end": now - 55e3, "ok": false, "summary": "AI timed out"}, "url": "/review", "can_run": true},
					{"id": "product-check", "name": "Product check", "schedule": "every day at 04:00 (off)", "enabled": false, "can_run": true},
				},
				"runs": []map[string]any{{"job": "day-review", "id": "dr-1", "start": now - 60e3, "end": now - 55e3, "ok": false, "summary": "AI timed out"}},
			})
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/scheduled-jobs/"):
			mu.Lock()
			ran = append(ran, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"started":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(app.Close)
	fakeCore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			User, App string
			Scopes    []string
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/links/token" || in.User != "owner@example.com" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if in.App != "money" { // Pocket Backups is not on this box
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such app","reason":"target_missing"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "pcx_money", "expires_in": 300, "scope": strings.Join(in.Scopes, " "), "app": "money", "url": app.URL, "public_url": "https://money.example"})
	}))
	t.Cleanup(fakeCore.Close)
	s.core = core.New(fakeCore.URL, core.Options{Token: "pca_monitoring"})

	ctx := context.Background()
	u, _ := s.store.GetUserByEmail(ctx, "owner@example.com")
	sv := &store.Server{UserID: u.ID, Name: "Home server", Kind: "local", Enabled: true, IntervalS: 5}
	if err := s.store.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	s.pollJobs(ctx, *sv)

	var list struct {
		Available bool `json:"available"`
		Jobs      []struct {
			Unit, Status, Problem, Group, Name, Scope, URL string
			AppName                                         string `json:"app_name"`
		} `json:"jobs"`
	}
	id := itoa64(sv.ID)
	if c := h.do(owner, "GET", "/servers/"+id+"/jobs", nil, &list); c != 200 || !list.Available || len(list.Jobs) != 2 {
		t.Fatalf("jobs: %d %+v", c, list)
	}
	dr, pc := list.Jobs[0], list.Jobs[1]
	if dr.Unit != "app:money:day-review" || dr.Status != "failed" || dr.Problem != "Day review (Pocket Money) failed: AI timed out" || dr.Group != "Pocket Money" ||
		dr.Scope != "app" || dr.URL != "https://money.example/review" {
		t.Errorf("day review: %+v", dr)
	}
	if pc.Status != "paused" {
		t.Errorf("switched off in the app: %+v", pc)
	}
	var runs []store.JobRun
	h.do(owner, "GET", "/servers/"+id+"/jobs/runs?unit=app:money:day-review", nil, &runs)
	if len(runs) != 1 || runs[0].Summary != "AI timed out" || runs[0].OK {
		t.Fatalf("runs %+v", runs)
	}
	// the next read asks only for runs after the stored one
	s.pollJobs(ctx, *sv)
	mu.Lock()
	if len(sinceSeen) < 2 || sinceSeen[0] != "0" || sinceSeen[len(sinceSeen)-1] == "0" {
		t.Errorf("since %v", sinceSeen)
	}
	mu.Unlock()

	if c := h.do(owner, "POST", "/servers/"+id+"/jobs/action", map[string]any{"unit": "app:money:day-review", "action": "run"}, nil); c != 200 {
		t.Fatalf("run: %d", c)
	}
	mu.Lock()
	if len(ran) != 1 || ran[0] != "/api/v1/scheduled-jobs/day-review/run" {
		t.Errorf("ran %v", ran)
	}
	mu.Unlock()
	if c := h.do(owner, "POST", "/servers/"+id+"/jobs/action", map[string]any{"unit": "app:money:day-review", "action": "pause"}, nil); c != http.StatusBadGateway {
		t.Fatalf("pause an app's job: %d", c)
	}
	if c := h.do(owner, "GET", "/servers/"+id+"/jobs/log?unit=app:money:day-review", nil, nil); c != http.StatusNotImplemented {
		t.Fatalf("log of an app's job: %d", c)
	}
}

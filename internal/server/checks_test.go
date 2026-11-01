package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

func TestNextStatus(t *testing.T) {
	cases := []struct {
		prev, fails, retries int
		ok                   bool
		want, wantFails      int
	}{
		{store.CheckUp, 0, 1, false, store.CheckPending, 1}, // first failure retries
		{store.CheckPending, 1, 1, false, store.CheckDown, 2},
		{store.CheckUp, 0, 0, false, store.CheckDown, 1}, // no retries: down at once
		{store.CheckDown, 5, 3, false, store.CheckDown, 6},
		{store.CheckDown, 5, 3, true, store.CheckUp, 0},
		{store.CheckPending, 1, 2, true, store.CheckUp, 0},
	}
	for _, c := range cases {
		got, fails := nextStatus(c.prev, c.fails, c.retries, c.ok)
		if got != c.want || fails != c.wantFails {
			t.Errorf("nextStatus(%d,%d,%d,%v) = %d,%d; want %d,%d", c.prev, c.fails, c.retries, c.ok, got, fails, c.want, c.wantFails)
		}
	}
}

func TestCheckConditions(t *testing.T) {
	now := time.Now()
	checks := []store.Check{
		{ID: 1, Name: "Web", Kind: "http", Target: "https://a.example", Enabled: true, Notify: true, Status: store.CheckDown, LastAt: 1, LastMsg: "502 - Bad Gateway", CertExpires: now.Add(5 * 24 * time.Hour).Unix()},
		{ID: 2, Name: "Muted", Kind: "http", Target: "https://b.example", Enabled: true, Notify: false, Status: store.CheckDown, LastAt: 1},
		{ID: 3, Name: "Retrying", Kind: "tcp", Target: "x", Port: 1, Enabled: true, Notify: true, Status: store.CheckPending, LastAt: 1},
		{ID: 4, Name: "Plain http", Kind: "http", Target: "http://c.example", Enabled: true, Notify: true, Status: store.CheckUp, LastAt: 1, CertExpires: now.Unix()},
	}
	down := checkConditions(store.Rule{Metric: "check"}, checks, now)
	if len(down) != 3 {
		t.Fatalf("muted checks stay out: got %d conditions", len(down))
	}
	if !down[0].fire || down[0].label != "Web is down: 502 - Bad Gateway" {
		t.Errorf("down check: %+v", down[0])
	}
	if down[1].fire || down[1].clear {
		t.Errorf("a pending check neither fires nor clears: %+v", down[1])
	}
	cert := checkConditions(store.Rule{Metric: "cert", Threshold: 14}, checks, now)
	if len(cert) != 1 || !cert[0].fire || cert[0].value != 4 && cert[0].value != 5 {
		t.Errorf("cert: %+v", cert)
	}
	only := checkConditions(store.Rule{Metric: "check", Target: "3"}, checks, now)
	if len(only) != 1 || only[0].inst != "3" {
		t.Errorf("a rule's target narrows it to one check: %+v", only)
	}
}

func TestChecksAPIAndKumaCompat(t *testing.T) {
	h, s := newHarness(t)
	tok := h.user(s, "owner@example.com", "admin")
	other := h.user(s, "other@example.com", "user")
	ctx := context.Background()
	owner, _ := s.store.GetUserByEmail(ctx, "owner@example.com")
	sv := &store.Server{UserID: owner.ID, Name: "Box", Kind: "local", Enabled: true, IntervalS: 5, Color: "#000"}
	if err := s.store.CreateServer(ctx, sv); err != nil {
		t.Fatal(err)
	}
	base := "/servers/" + strconv.FormatInt(sv.ID, 10)

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("hello pocket"))
	}))
	defer site.Close()

	var c store.Check
	if code := h.do(tok, "POST", base+"/checks", map[string]any{"name": "Site", "kind": "http", "target": site.URL, "keyword": "pocket", "interval_s": 60}, &c); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if code := h.do(tok, "POST", base+"/checks", map[string]any{"name": "Bad", "kind": "http", "target": "ftp://x"}, nil); code != http.StatusBadRequest {
		t.Errorf("a non-http URL is refused, got %d", code)
	}
	if code := h.do(other, "GET", base+"/checks", nil, nil); code != http.StatusNotFound {
		t.Errorf("someone else's checks are invisible, got %d", code)
	}
	var ran store.Check
	if code := h.do(tok, "POST", base+"/checks/"+strconv.FormatInt(c.ID, 10)+"/run", nil, &ran); code != http.StatusOK || ran.Status != store.CheckUp {
		t.Fatalf("run: %d %+v", code, ran)
	}

	// a push check, fed through Kuma's URL
	var p store.Check
	h.do(tok, "POST", base+"/checks", map[string]any{"name": "Nightly", "kind": "push", "interval_s": 3600, "public": true}, &p)
	if p.PushToken == "" {
		t.Fatal("push checks get a token")
	}
	res, err := http.Get(h.srv.URL + "/api/push/" + p.PushToken + "?status=up&msg=done&ping=12")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("push: %v %v", err, res)
	}
	res, _ = http.Get(h.srv.URL + "/api/push/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown token is 404, got %d", res.StatusCode)
	}

	// status page in Uptime Kuma's shape
	if code := h.do(tok, "PUT", base+"/status-page", map[string]any{"slug": "home", "title": "Home"}, nil); code != http.StatusOK {
		t.Fatalf("status page: %d", code)
	}
	var page struct {
		PublicGroupList []struct {
			MonitorList []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"monitorList"`
		} `json:"publicGroupList"`
	}
	res, _ = http.Get(h.srv.URL + "/api/status-page/home")
	_ = json.NewDecoder(res.Body).Decode(&page)
	res.Body.Close()
	if len(page.PublicGroupList) != 1 || len(page.PublicGroupList[0].MonitorList) != 1 || page.PublicGroupList[0].MonitorList[0].Name != "Nightly" {
		t.Errorf("only public checks are on the page: %+v", page)
	}
	var hb struct {
		HeartbeatList map[string][]struct {
			Status int `json:"status"`
		} `json:"heartbeatList"`
		UptimeList map[string]float64 `json:"uptimeList"`
	}
	res, _ = http.Get(h.srv.URL + "/api/status-page/heartbeat/home")
	_ = json.NewDecoder(res.Body).Decode(&hb)
	res.Body.Close()
	id := strconv.FormatInt(p.ID, 10)
	if b := hb.HeartbeatList[id]; len(b) != 1 || b[0].Status != 1 || hb.UptimeList[id+"_24"] != 1 {
		t.Errorf("heartbeats: %+v", hb)
	}

	// metrics in Kuma's names
	res, _ = http.Get(h.srv.URL + "/metrics")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `monitor_status{monitor_name="Nightly"`) {
		t.Errorf("metrics lack monitor_status:\n%s", body)
	}

	// the alert rule fires when a check goes down
	var bad store.Check
	h.do(tok, "POST", base+"/checks", map[string]any{"name": "Broken", "kind": "http", "target": site.URL + "/bad", "retries": 0}, &bad)
	h.do(tok, "POST", base+"/checks/"+strconv.FormatInt(bad.ID, 10)+"/run", nil, &ran)
	if ran.Status != store.CheckDown || !strings.HasPrefix(ran.LastMsg, "502") {
		t.Fatalf("broken check: %+v", ran)
	}
	var alerts []store.Alert
	h.do(tok, "GET", "/alerts", nil, &alerts)
	found := false
	for _, a := range alerts {
		found = found || (a.Metric == "check" && a.Target == strconv.FormatInt(bad.ID, 10))
	}
	if !found {
		t.Errorf("no alert for the down check: %+v", alerts)
	}
	// deleting it resolves the alert
	h.do(tok, "DELETE", base+"/checks/"+strconv.FormatInt(bad.ID, 10), nil, nil)
	h.do(tok, "GET", "/alerts", nil, &alerts)
	for _, a := range alerts {
		if a.Metric == "check" {
			t.Errorf("alert still open after delete: %+v", a)
		}
	}
}

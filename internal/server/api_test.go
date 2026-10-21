package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/config"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
}

func newHarness(t *testing.T) (*harness, *Server) {
	t.Helper()
	t.Setenv("LOCAL_COLLECTOR", "false")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	cfg.DBPath = cfg.DataDir + "/test.db"
	cfg.AuthMode = "local"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(cfg.DBPath, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(cfg, st, log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: httptest.NewServer(s.Router())}
	t.Cleanup(h.srv.Close)
	return h, s
}

// user makes an account and returns its API token.
func (h *harness) user(s *Server, email, role string) string {
	h.t.Helper()
	u := &store.User{Email: email, Name: email, Role: role}
	if err := s.store.CreateUser(context.Background(), u); err != nil {
		h.t.Fatal(err)
	}
	tok, _, err := s.store.CreateAPIToken(context.Background(), u.ID, "test")
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func (h *harness) do(tok, method, path string, body any, out any) int {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestServersArePerPerson(t *testing.T) {
	h, s := newHarness(t)
	admin := h.user(s, "owner@example.com", "admin")
	sani := h.user(s, "sani@example.com", "member")

	var sv struct {
		ID   int64  `json:"id"`
		Role string `json:"role"`
	}
	if c := h.do(sani, "POST", "/servers", map[string]any{"kind": "glances", "name": "Sani's NAS", "url": "http://127.0.0.1:9"}, &sv); c != http.StatusCreated {
		t.Fatalf("create: %d", c)
	}
	var list []map[string]any
	h.do(admin, "GET", "/servers", nil, &list)
	if len(list) != 0 {
		t.Fatalf("the admin sees someone else's server: %v", list)
	}
	id := itoa64(sv.ID)
	for _, p := range []string{"/servers/" + id + "/logs", "/servers/" + id, "/servers/" + id + "/snapshot", "/servers/" + id + "/live", "/servers/" + id + "/history", "/servers/" + id + "/shares"} {
		if c := h.do(admin, "GET", p, nil, nil); c != http.StatusNotFound {
			t.Errorf("GET %s as another person: %d, want 404", p, c)
		}
	}
	if c := h.do(admin, "PUT", "/servers/"+id, map[string]any{"name": "mine now"}, nil); c != http.StatusNotFound {
		t.Errorf("PUT as another person: %d", c)
	}
	if c := h.do(admin, "DELETE", "/servers/"+id, nil, nil); c != http.StatusNotFound {
		t.Errorf("DELETE as another person: %d", c)
	}
	var rules []store.Rule
	h.do(sani, "GET", "/rules?server_id="+id, nil, &rules)
	if len(rules) == 0 {
		t.Fatal("a new server should come with default rules")
	}
	h.do(admin, "GET", "/rules", nil, &list)
	if len(list) != 0 {
		t.Fatalf("the admin sees someone else's rules")
	}
	rid := itoa64(rules[0].ID)
	if c := h.do(admin, "PUT", "/rules/"+rid, map[string]any{"enabled": false}, nil); c != http.StatusNotFound {
		t.Errorf("PUT rule as another person: %d", c)
	}
	if c := h.do(admin, "POST", "/rules", map[string]any{"server_id": sv.ID, "metric": "cpu", "threshold": 50}, nil); c != http.StatusNotFound {
		t.Errorf("POST rule on someone else's server: %d", c)
	}

	// Shared read-only: the viewer sees it but cannot change it.
	var me struct{ ID int64 }
	h.do(admin, "GET", "/auth/me", nil, &me)
	if c := h.do(sani, "POST", "/servers/"+id+"/shares", map[string]any{"user_id": me.ID}, nil); c != http.StatusCreated {
		t.Fatalf("share: %d", c)
	}
	h.do(admin, "GET", "/servers", nil, &list)
	if len(list) != 1 || list[0]["role"] != "viewer" {
		t.Fatalf("shared server: %v", list)
	}
	if c := h.do(admin, "GET", "/servers/"+id+"/history?range=24h", nil, nil); c != http.StatusOK {
		t.Errorf("viewer history: %d", c)
	}
	if c := h.do(admin, "GET", "/servers/"+id+"/logs", nil, nil); c != http.StatusForbidden {
		t.Errorf("viewer logs: %d, want 403", c)
	}
	if c := h.do(admin, "PUT", "/servers/"+id, map[string]any{"name": "x"}, nil); c != http.StatusForbidden {
		t.Errorf("viewer PUT: %d, want 403", c)
	}
	if c := h.do(admin, "GET", "/rules?server_id="+id, nil, &rules); c != http.StatusOK || len(rules) != 0 {
		t.Errorf("viewer sees the owner's rules: %d %d", c, len(rules))
	}
	if c := h.do(admin, "POST", "/servers/"+id+"/leave", nil, nil); c != http.StatusNoContent {
		t.Errorf("leave: %d", c)
	}
	h.do(admin, "GET", "/servers", nil, &list)
	if len(list) != 0 {
		t.Fatalf("left server still listed")
	}
}

func TestAgentEndpointNeedsToken(t *testing.T) {
	r := httptest.NewRequest("GET", "/agent/v1/snapshot", nil)
	if AgentAuthorized(r, "secret-token-123456") {
		t.Fatal("no token accepted")
	}
	r.Header.Set("Authorization", "Bearer secret-token-123456")
	if !AgentAuthorized(r, "secret-token-123456") {
		t.Fatal("right token refused")
	}
}

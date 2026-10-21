package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
)

func logQuery(r *http.Request) collect.LogQuery {
	return collect.LogQuery{Container: strings.TrimSpace(r.URL.Query().Get("container")), Tail: queryInt(r, "tail"), Since: r.URL.Query().Get("since")}
}

// handleLogs serves container logs. Only the server's owner reads them:
// logs carry tokens and passwords more often than not, and a share is for
// looking at the numbers.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	sv, role, err := s.store.ServerFor(r.Context(), me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "get server")
		return
	}
	if role != "owner" {
		writeErr(w, http.StatusForbidden, "only the server's owner can read its container logs")
		return
	}
	q := logQuery(r)
	if err := q.Clamp(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var out *collect.Logs
	switch sv.Kind {
	case "local":
		if s.mon.local == nil {
			err = errors.New("the local collector is off (LOCAL_COLLECTOR=false)")
			break
		}
		out, err = s.mon.local.Docker.Logs(ctx, q)
	case "agent":
		var tok string
		if tok, err = s.open(sv.Secret); err == nil {
			out, err = collect.AgentLogs(ctx, sv.URL, tok, q)
		}
	default:
		writeErr(w, http.StatusNotImplemented, "Glances doesn't serve container logs; run the Pocket Monitoring agent on that machine to read them here")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentLogs lets another hub read this box's container logs (AGENT_TOKEN).
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AgentToken == "" || s.mon.local == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !AgentAuthorized(r, s.cfg.AgentToken) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	serveAgentLogs(w, r, s.mon.local.Docker)
}

func serveAgentLogs(w http.ResponseWriter, r *http.Request, d *collect.Docker) {
	q := logQuery(r)
	if err := q.Clamp(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := d.Logs(ctx, q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

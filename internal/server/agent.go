package server

import "net/http"

// handleAgentSnapshot lets another Pocket Monitoring read this box like an
// agent (AGENT_TOKEN must be set; off otherwise).
func (s *Server) handleAgentSnapshot(w http.ResponseWriter, r *http.Request) {
	tok := s.cfg.AgentToken
	if tok == "" || s.mon.local == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !AgentAuthorized(r, tok) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	for _, st := range s.mon.snapshotStates() {
		st.mu.Lock()
		snap := st.snap
		st.mu.Unlock()
		if snap != nil && snap.Source == "local" {
			writeJSON(w, http.StatusOK, snap)
			return
		}
	}
	snap, err := s.mon.local.Collect(r.Context())
	if err != nil {
		s.fail(w, err, "collect")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

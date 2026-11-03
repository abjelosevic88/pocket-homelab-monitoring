package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/collect"
	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// Drive health: servers report every disk's SMART reading (collect.Drive)
// every few minutes. The hub keeps each drive's latest reading and an hourly
// history, judges the drive rules, and notices a drive that stopped
// answering — a dead disk drops off smartctl's list instead of failing it.

// driveGone is how long a drive may be missing before the drive rule fires.
const driveGone = time.Hour

func (s *Server) driveRoutes(r chi.Router) {
	r.Get("/drives", s.handleListDrives)
	r.Post("/drives/import/scrutiny", s.handleImportScrutiny)
	r.Get("/servers/{id}/drives", s.handleServerDrives)
	r.Get("/servers/{id}/drives/{key}", s.handleGetDrive)
	r.Delete("/servers/{id}/drives/{key}", s.handleDeleteDrive)
}

func isDriveMetric(m string) bool {
	return m == "drive" || m == "drive_errors" || m == "drive_wear" || m == "drive_temp"
}

// noteDrives takes a new SMART pass from a reading (call with st.mu held);
// it returns the drives to store, nil when the pass was already stored.
func noteDrives(st *serverState, snap *collect.Snapshot, now time.Time) []collect.Drive {
	if snap.DrivesAt.IsZero() || snap.DrivesAt.Equal(st.drivesAt) {
		return nil
	}
	st.drivesAt = snap.DrivesAt
	if st.drives == nil {
		st.drives = map[string]store.DriveSeen{}
	}
	for _, d := range snap.Drives {
		st.drives[d.Key] = store.DriveSeen{Label: store.DriveLabel(d.Model, d.Name), LastSeen: now.Unix()}
	}
	if snap.Drives == nil {
		return []collect.Drive{}
	}
	return snap.Drives
}

func driveSample(d collect.Drive) store.DriveSample {
	p := store.DriveSample{T: d.ReadAt.Unix(), Status: d.Status, Temp: &d.Temp, Hours: &d.PowerOnHours, Wear: d.Wear, Spare: d.Spare, Errors: &d.Errors, Attrs: map[string]int64{}}
	if d.Written > 0 {
		w := int64(d.Written)
		p.Written = &w
	}
	if d.Read > 0 {
		r := int64(d.Read)
		p.Read = &r
	}
	for _, a := range d.Attrs {
		p.Attrs[a.Key] = a.Raw
	}
	return p
}

// saveDrives stores one SMART pass. A drive asleep or unreadable this time
// still counts as present but adds no history (its values are old).
func (s *Server) saveDrives(ctx context.Context, sv store.Server, drives []collect.Drive, now time.Time) {
	added := false
	for _, d := range drives {
		data, _ := json.Marshal(d)
		row := store.DriveRow{Key: d.Key, Name: d.Name, Model: d.Model, Serial: d.Serial, Status: d.Status, Data: data, LastSeen: now.Unix()}
		if _, err := s.store.GetDrive(ctx, sv.ID, d.Key); err != nil {
			added = true
		}
		var p *store.DriveSample
		if !d.Standby && d.Error == "" && !d.ReadAt.IsZero() {
			x := driveSample(d)
			p = &x
		}
		if err := s.store.SaveDrive(ctx, sv.ID, row, p); err != nil {
			s.log.Warn("store drive", "server", sv.Name, "drive", d.Key, "err", err)
		}
	}
	if added {
		s.touch()
	}
}

// driveConditions judges the drive rules; known are the drives the server
// has had, so a drive that vanished keeps (or raises) its alert.
func driveConditions(r store.Rule, snap *collect.Snapshot, known map[string]store.DriveSeen, now time.Time) []condition {
	var out []condition
	here := map[string]bool{}
	for _, d := range snap.Drives {
		here[d.Key] = true
		if r.Target != "" && r.Target != d.Key {
			continue
		}
		label := store.DriveLabel(d.Model, d.Name)
		switch r.Metric {
		case "drive":
			bad := d.Status == "failed"
			out = append(out, condition{inst: d.Key, label: label, fire: bad, clear: !bad})
		case "drive_errors":
			v := float64(d.Errors)
			out = append(out, condition{inst: d.Key, label: label, value: v, fire: v > r.Threshold, clear: v <= r.Threshold})
		case "drive_wear":
			if d.Wear != nil {
				out = append(out, condition{inst: d.Key, label: label, value: *d.Wear, fire: *d.Wear > r.Threshold, clear: *d.Wear <= r.Threshold})
			}
		case "drive_temp":
			limit := r.Threshold
			if limit <= 0 {
				limit = d.TempLimit
			}
			if d.Standby || limit <= 0 || d.Temp <= 0 {
				out = append(out, condition{inst: d.Key, label: label}) // nothing new: leave it as it is
				continue
			}
			out = append(out, condition{inst: d.Key, label: label, value: d.Temp, fire: d.Temp > limit, clear: d.Temp <= limit-2})
		}
	}
	for key, k := range known {
		if here[key] || (r.Target != "" && r.Target != key) {
			continue
		}
		// Not in this pass: keep its alerts as they are. The drive rule fires
		// once it's been gone a while; value carries when it was last seen
		// so the message tells "missing" from "failing".
		c := condition{inst: key, label: k.Label}
		if r.Metric == "drive" {
			c.value, c.fire = float64(k.LastSeen), now.Unix()-k.LastSeen > int64(driveGone.Seconds())
		}
		out = append(out, c)
	}
	return out
}

func driveMessage(r store.Rule, c condition, snap *collect.Snapshot) string {
	var d *collect.Drive
	if snap != nil {
		for i := range snap.Drives {
			if snap.Drives[i].Key == c.inst {
				d = &snap.Drives[i]
			}
		}
	}
	switch r.Metric {
	case "drive":
		if d == nil {
			return fmt.Sprintf("Drive %s has not answered since %s: it may have died or been pulled", c.label, time.Unix(int64(c.value), 0).Format("2 Jan 15:04"))
		}
		return fmt.Sprintf("Drive %s is failing: %s", c.label, strings.Join(d.Problems, "; "))
	case "drive_errors":
		what := fmt.Sprintf("%g errors", c.value)
		if d != nil {
			var parts []string
			for _, x := range []struct {
				n    int64
				what string
			}{{d.Reallocated, "reallocated"}, {d.Pending, "pending"}, {d.Uncorrectable, "uncorrectable"}, {d.MediaErrors, "media errors"}} {
				if x.n > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", x.n, x.what))
				}
			}
			if len(parts) > 0 {
				what = strings.Join(parts, ", ")
			}
		}
		return fmt.Sprintf("Drive %s reports bad sectors: %s (allowed %g)", c.label, what, r.Threshold)
	case "drive_wear":
		return fmt.Sprintf("Drive %s has used %s of its rated write endurance (limit %s)", c.label, fmtValue("drive_wear", c.value), fmtValue("drive_wear", r.Threshold))
	case "drive_temp":
		limit := r.Threshold
		if limit <= 0 && d != nil {
			limit = d.TempLimit
		}
		return fmt.Sprintf("Drive %s at %s%s (limit %s)", c.label, fmtValue("temp", c.value), fmtFor(r.ForS), fmtValue("temp", limit))
	}
	return c.label
}

func driveResolved(a *store.Alert, took string) string {
	label := a.Target
	if i := strings.Index(a.Message, "Drive "); i == 0 {
		rest := a.Message[len("Drive "):]
		for _, sep := range []string{" is failing", " has not answered", " reports", " has used", " at "} {
			if j := strings.Index(rest, sep); j > 0 {
				label = rest[:j]
				break
			}
		}
	}
	switch a.Metric {
	case "drive":
		return fmt.Sprintf("Drive %s is healthy again%s.", label, took)
	case "drive_errors":
		return fmt.Sprintf("Drive %s is within its error allowance again%s.", label, took)
	case "drive_wear":
		return fmt.Sprintf("Drive %s wear alert cleared%s.", label, took)
	}
	return fmt.Sprintf("Drive %s has cooled down%s.", label, took)
}

// ---------- API ----------

type driveView struct {
	ServerID    int64           `json:"server_id"`
	ServerName  string          `json:"server_name"`
	ServerColor string          `json:"server_color"`
	Role        string          `json:"role"`
	Drive       json.RawMessage `json:"drive"`
	FirstSeen   int64           `json:"first_seen"`
	LastSeen    int64           `json:"last_seen"`
	// Missing: the server's last SMART pass didn't find it.
	Missing bool `json:"missing"`
}

func (s *Server) driveViews(ctx context.Context, servers []store.Server, roles map[int64]string) ([]driveView, error) {
	ids := make([]int64, 0, len(servers))
	byID := map[int64]store.Server{}
	for _, sv := range servers {
		ids = append(ids, sv.ID)
		byID[sv.ID] = sv
	}
	rows, err := s.store.Drives(ctx, ids)
	if err != nil {
		return nil, err
	}
	present := map[int64]map[string]bool{}
	for _, id := range ids {
		if st := s.mon.state(id); st != nil {
			st.mu.Lock()
			if st.snap != nil && !st.snap.DrivesAt.IsZero() {
				m := map[string]bool{}
				for _, d := range st.snap.Drives {
					m[d.Key] = true
				}
				present[id] = m
			}
			st.mu.Unlock()
		}
	}
	out := make([]driveView, 0, len(rows))
	for _, d := range rows {
		sv := byID[d.ServerID]
		v := driveView{ServerID: sv.ID, ServerName: sv.Name, ServerColor: sv.Color, Role: roles[sv.ID], Drive: d.Data, FirstSeen: d.FirstSeen, LastSeen: d.LastSeen}
		if m, ok := present[sv.ID]; ok && !m[d.Key] {
			v.Missing = true
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) handleListDrives(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := s.store.VisibleServers(ctx, me(r))
	if err != nil {
		s.fail(w, err, "list servers")
		return
	}
	roles := map[int64]string{}
	for _, sv := range list {
		roles[sv.ID] = "viewer"
		if sv.UserID == me(r) {
			roles[sv.ID] = "owner"
		}
	}
	out, err := s.driveViews(ctx, list, roles)
	if err != nil {
		s.fail(w, err, "list drives")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleServerDrives(w http.ResponseWriter, r *http.Request) {
	sv, role, err := s.store.ServerFor(r.Context(), me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "get server")
		return
	}
	out, err := s.driveViews(r.Context(), []store.Server{*sv}, map[int64]string{sv.ID: role})
	if err != nil {
		s.fail(w, err, "list drives")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetDrive(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sv, role, err := s.store.ServerFor(ctx, me(r), idParam(r, "id"))
	if err != nil {
		s.fail(w, err, "get server")
		return
	}
	key := chi.URLParam(r, "key")
	views, err := s.driveViews(ctx, []store.Server{*sv}, map[int64]string{sv.ID: role})
	if err != nil {
		s.fail(w, err, "get drive")
		return
	}
	var v *driveView
	for i := range views {
		var d struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(views[i].Drive, &d)
		if d.Key == key {
			v = &views[i]
		}
	}
	if v == nil {
		writeErr(w, http.StatusNotFound, "no such drive")
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	from := time.Now().AddDate(0, 0, -days).Unix()
	if r.URL.Query().Get("days") == "all" {
		from = 0
	}
	hist, err := s.store.DriveHistory(ctx, sv.ID, key, from)
	if err != nil {
		s.fail(w, err, "drive history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drive": v, "history": hist})
}

func (s *Server) handleDeleteDrive(w http.ResponseWriter, r *http.Request) {
	sv := s.ownServer(w, r)
	if sv == nil {
		return
	}
	key := chi.URLParam(r, "key")
	if err := s.store.DeleteDrive(r.Context(), sv.ID, key); err != nil {
		s.fail(w, err, "forget drive")
		return
	}
	if st := s.mon.state(sv.ID); st != nil {
		st.mu.Lock()
		delete(st.drives, key)
		st.mu.Unlock()
	}
	s.touch()
	w.WriteHeader(http.StatusNoContent)
}

// ---------- Scrutiny import ----------

type scrutinyAttr struct {
	RawValue *int64   `json:"raw_value"`
	Value    *float64 `json:"value"`
}

type scrutinyResult struct {
	Date         time.Time               `json:"date"`
	Status       int                     `json:"Status"`
	Temp         *float64                `json:"temp"`
	PowerOnHours *int64                  `json:"power_on_hours"`
	Protocol     string                  `json:"device_protocol"`
	Attrs        map[string]scrutinyAttr `json:"attrs"`
}

// scrutinySample turns one Scrutiny result into a history point.
func scrutinySample(x scrutinyResult) store.DriveSample {
	p := store.DriveSample{T: x.Date.Unix(), Temp: x.Temp, Hours: x.PowerOnHours, Status: "passed", Attrs: map[string]int64{}}
	if x.Status != 0 {
		p.Status = "failed"
	}
	val := func(k string) (int64, bool) {
		a, ok := x.Attrs[k]
		switch {
		case !ok:
			return 0, false
		case a.RawValue != nil:
			return *a.RawValue, true
		case a.Value != nil:
			return int64(*a.Value), true
		}
		return 0, false
	}
	for k := range x.Attrs {
		if v, ok := val(k); ok {
			if k == "188" {
				v &= 0xffff
			}
			if k == "194" || k == "190" {
				v &= 0xff
			}
			p.Attrs[k] = v
		}
	}
	var errs int64
	if strings.EqualFold(x.Protocol, "NVMe") {
		if v, ok := val("percentage_used"); ok {
			f := float64(v)
			p.Wear = &f
		}
		if v, ok := val("available_spare"); ok {
			f := float64(v)
			p.Spare = &f
		}
		if v, ok := val("data_units_written"); ok {
			w := v * 512000
			p.Written = &w
		}
		if v, ok := val("data_units_read"); ok {
			rd := v * 512000
			p.Read = &rd
		}
		errs, _ = val("media_errors")
	} else {
		for _, k := range []string{"5", "197", "198", "187"} {
			v, _ := val(k)
			errs += v
		}
	}
	p.Errors = &errs
	return p
}

func (s *Server) handleImportScrutiny(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		URL string `json:"url"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	base, err := cleanURL(in.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mine := s.ownedServerIDs(ctx, me(r))
	get := func(path string, v any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		res, err := s.http.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("%s answered HTTP %d", base+path, res.StatusCode)
		}
		return json.NewDecoder(res.Body).Decode(v)
	}
	var sum struct {
		Data struct {
			Summary map[string]struct {
				Device struct {
					WWN    string `json:"wwn"`
					Serial string `json:"serial_number"`
					Model  string `json:"model_name"`
					Name   string `json:"device_name"`
					Host   string `json:"host_id"`
				} `json:"device"`
			} `json:"summary"`
		} `json:"data"`
	}
	if err := get("/api/summary", &sum); err != nil {
		writeErr(w, http.StatusBadGateway, "couldn't read Scrutiny: "+err.Error())
		return
	}
	type result struct {
		Drive   string `json:"drive"`
		Server  string `json:"server,omitempty"`
		Points  int    `json:"points"`
		Matched bool   `json:"matched"`
	}
	out := []result{}
	total := 0
	keys := make([]string, 0, len(sum.Data.Summary))
	for k := range sum.Data.Summary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, wwn := range keys {
		dev := sum.Data.Summary[wwn].Device
		label := strings.TrimSpace(dev.Model + " " + dev.Serial)
		if dev.Host != "" {
			label = dev.Host + " · " + label
		}
		row := s.store.DriveBySerial(ctx, mine, dev.Serial)
		if row == nil {
			out = append(out, result{Drive: label})
			continue
		}
		sv, _ := s.store.GetServer(ctx, row.ServerID)
		res := result{Drive: label, Matched: true}
		if sv != nil {
			res.Server = sv.Name
		}
		// Scrutiny thins old history: every range adds points the others don't have.
		for _, span := range []string{"week", "month", "year", "forever"} {
			var det struct {
				Data struct {
					Results []scrutinyResult `json:"smart_results"`
				} `json:"data"`
			}
			if err := get("/api/device/"+url.PathEscape(wwn)+"/details?duration_key="+span, &det); err != nil {
				continue
			}
			for _, x := range det.Data.Results {
				if x.Date.IsZero() {
					continue
				}
				if ok, err := s.store.ImportDriveSample(ctx, row.ServerID, row.Key, scrutinySample(x)); err == nil && ok {
					res.Points++
				}
			}
		}
		total += res.Points
		out = append(out, res)
	}
	s.log.Info("imported Scrutiny history", "points", total)
	writeJSON(w, http.StatusOK, map[string]any{"drives": out, "points": total})
}

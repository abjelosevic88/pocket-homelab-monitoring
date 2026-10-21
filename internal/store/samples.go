package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

// Minute history is kept for MinuteDays, the 15-minute roll-up for RollupDays.
const (
	MinuteDays = 8
	RollupDays = 400
)

type sampleExtra struct {
	Temps map[string]float64 `json:"temps,omitempty"`
}

// InsertSample stores (or replaces) one minute of a server.
func (s *Store) InsertSample(ctx context.Context, serverID int64, p Sample) error {
	return s.insertSample(ctx, "samples", serverID, p)
}

func (s *Store) insertSample(ctx context.Context, table string, serverID int64, p Sample) error {
	extra := ""
	if len(p.Temps) > 0 {
		b, _ := json.Marshal(sampleExtra{Temps: p.Temps})
		extra = string(b)
	}
	_, err := s.DB.ExecContext(ctx, `INSERT OR REPLACE INTO `+table+` (server_id, ts, cpu, iowait, mem, swap, load1, load5, load15, rx, tx, rd, wr, temp, extra)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, serverID, p.T, p.CPU, p.IOWait, p.Mem, p.Swap, p.Load1, p.Load5, p.Load15, p.Rx, p.Tx, p.Read, p.Write, p.Temp, extra)
	return err
}

func (s *Store) readSamples(ctx context.Context, table string, serverID, from, to int64, withTemps bool) ([]Sample, error) {
	cols := `ts, COALESCE(cpu,0), COALESCE(iowait,0), COALESCE(mem,0), COALESCE(swap,0), COALESCE(load1,0), COALESCE(load5,0), COALESCE(load15,0), COALESCE(rx,0), COALESCE(tx,0), COALESCE(rd,0), COALESCE(wr,0), COALESCE(temp,0), `
	if withTemps {
		cols += `extra`
	} else {
		cols += `''`
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+cols+` FROM `+table+` WHERE server_id = ? AND ts >= ? AND ts < ? ORDER BY ts`, serverID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var p Sample
		var extra string
		if err := rows.Scan(&p.T, &p.CPU, &p.IOWait, &p.Mem, &p.Swap, &p.Load1, &p.Load5, &p.Load15, &p.Rx, &p.Tx, &p.Read, &p.Write, &p.Temp, &extra); err != nil {
			return nil, err
		}
		if extra != "" {
			var x sampleExtra
			if json.Unmarshal([]byte(extra), &x) == nil {
				p.Temps = x.Temps
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Average folds points into buckets of step seconds (aligned to step).
func Average(points []Sample, step int64) []Sample {
	if step <= 0 || len(points) == 0 {
		return points
	}
	type acc struct {
		p     Sample
		n     float64
		temps map[string][2]float64
	}
	buckets := map[int64]*acc{}
	var keys []int64
	for _, p := range points {
		b := p.T / step * step
		a, ok := buckets[b]
		if !ok {
			a = &acc{p: Sample{T: b}, temps: map[string][2]float64{}}
			buckets[b] = a
			keys = append(keys, b)
		}
		a.n++
		a.p.CPU += p.CPU
		a.p.IOWait += p.IOWait
		a.p.Mem += p.Mem
		a.p.Swap += p.Swap
		a.p.Load1 += p.Load1
		a.p.Load5 += p.Load5
		a.p.Load15 += p.Load15
		a.p.Rx += p.Rx
		a.p.Tx += p.Tx
		a.p.Read += p.Read
		a.p.Write += p.Write
		if p.Temp > a.p.Temp {
			a.p.Temp = p.Temp // the hottest reading is the useful one
		}
		for k, v := range p.Temps {
			t := a.temps[k]
			a.temps[k] = [2]float64{t[0] + v, t[1] + 1}
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]Sample, 0, len(keys))
	r := func(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
	for _, k := range keys {
		a := buckets[k]
		p := a.p
		p.CPU, p.IOWait, p.Mem, p.Swap = r(p.CPU/a.n), r(p.IOWait/a.n), r(p.Mem/a.n), r(p.Swap/a.n)
		p.Load1, p.Load5, p.Load15 = r(p.Load1/a.n), r(p.Load5/a.n), r(p.Load15/a.n)
		p.Rx, p.Tx, p.Read, p.Write = float64(int64(p.Rx/a.n)), float64(int64(p.Tx/a.n)), float64(int64(p.Read/a.n)), float64(int64(p.Write/a.n))
		if len(a.temps) > 0 {
			p.Temps = make(map[string]float64, len(a.temps))
			for key, t := range a.temps {
				p.Temps[key] = r(t[0] / t[1])
			}
		}
		out = append(out, p)
	}
	return out
}

// History returns points between from and to (unix seconds), averaged to step.
// Ranges longer than the minute history come from the 15-minute roll-up.
func (s *Store) History(ctx context.Context, serverID, from, to, step int64) ([]Sample, error) {
	table := "samples"
	if time.Now().Unix()-from > MinuteDays*86400-3600 || step >= 900 {
		table = "samples_15m"
	}
	pts, err := s.readSamples(ctx, table, serverID, from, to, true)
	if err != nil {
		return nil, err
	}
	if table == "samples_15m" {
		// The roll-up lags up to 15 minutes; fill the tail from minutes.
		var last int64
		if len(pts) > 0 {
			last = pts[len(pts)-1].T + 900
		} else {
			last = from
		}
		if tail, err := s.readSamples(ctx, "samples", serverID, last, to, true); err == nil {
			pts = append(pts, tail...)
		}
	}
	return Average(pts, step), nil
}

// Spark returns the last n minutes of CPU, oldest first.
func (s *Store) Spark(ctx context.Context, serverID int64, minutes int) []float64 {
	rows, err := s.DB.QueryContext(ctx, `SELECT COALESCE(cpu,0) FROM samples WHERE server_id = ? AND ts >= ? ORDER BY ts`, serverID, time.Now().Unix()-int64(minutes*60))
	if err != nil {
		return []float64{}
	}
	defer rows.Close()
	out := []float64{}
	for rows.Next() {
		var v float64
		if rows.Scan(&v) == nil {
			out = append(out, v)
		}
	}
	return out
}

// Rollup writes the 15-minute averages of every finished quarter hour not
// rolled up yet.
func (s *Store) Rollup(ctx context.Context) error {
	ids := []int64{}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM servers`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	end := time.Now().Unix() / 900 * 900 // the quarter in progress waits
	for _, id := range ids {
		var last int64
		_ = s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts), 0) FROM samples_15m WHERE server_id = ?`, id).Scan(&last)
		from := last + 900
		if last == 0 {
			from = end - MinuteDays*86400
		}
		if from >= end {
			continue
		}
		pts, err := s.readSamples(ctx, "samples", id, from, end, true)
		if err != nil {
			return err
		}
		for _, p := range Average(pts, 900) {
			if err := s.insertSample(ctx, "samples_15m", id, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// PruneSamples forgets history past its retention.
func (s *Store) PruneSamples(ctx context.Context) {
	now := time.Now().Unix()
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM samples WHERE ts < ?`, now-MinuteDays*86400)
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM samples_15m WHERE ts < ?`, now-RollupDays*86400)
}

// SaveSensorNames remembers sensor labels for the history legend.
func (s *Store) SaveSensorNames(ctx context.Context, serverID int64, names []SensorName) {
	for _, n := range names {
		_, _ = s.DB.ExecContext(ctx, `INSERT INTO sensors (server_id, key, label) VALUES (?, ?, ?) ON CONFLICT(server_id, key) DO UPDATE SET label = excluded.label`, serverID, n.Key, n.Label)
	}
}

// SensorNames lists the sensors a server has reported.
func (s *Store) SensorNames(ctx context.Context, serverID int64) map[string]string {
	out := map[string]string{}
	rows, err := s.DB.QueryContext(ctx, `SELECT key, label FROM sensors WHERE server_id = ?`, serverID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, l string
		if rows.Scan(&k, &l) == nil {
			out[k] = l
		}
	}
	return out
}

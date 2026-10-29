package server

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"github.com/abjelosevic88/pocket-homelab-monitoring/internal/store"
)

// seedDemo fills a day of made-up history for the owner's servers when the
// box is empty (DEMO_DATA=true; for screenshots and trying the app out).
func (s *Server) seedDemo(ctx context.Context) {
	owner := s.store.OwnerID(ctx)
	if owner == 0 {
		return
	}
	list, _ := s.store.VisibleServers(ctx, owner)
	for _, sv := range list {
		if len(s.store.Spark(ctx, sv.ID, 24*60)) > 0 {
			continue
		}
		now := time.Now().Unix() / 60 * 60
		for i := int64(24 * 60); i > 0; i-- {
			t := now - i*60
			h := float64(t%86400) / 3600
			wave := (math.Sin((h-9)/24*2*math.Pi) + 1) / 2
			p := store.Sample{T: t, CPU: 8 + 30*wave + rand.Float64()*8, IOWait: rand.Float64() * 3, Mem: 46 + 6*wave + rand.Float64(), Swap: 40,
				Load1: 0.8 + 2*wave, Load5: 0.8 + 1.8*wave, Load15: 0.8 + 1.5*wave, Rx: 2e5 + 4e6*wave*rand.Float64(), Tx: 1e5 + 1e6*wave*rand.Float64(),
				Read: 5e4 + 2e6*rand.Float64()*wave, Write: 1e5 + 3e6*rand.Float64()*wave, Temp: 48 + 20*wave + rand.Float64()*3}
			p.Temps = map[string]float64{"demo/cpu": p.Temp, "demo/nvme0": 34 + 6*wave, "demo/nvme1": 36 + 5*wave}
			_ = s.store.InsertSample(ctx, sv.ID, p)
		}
		s.store.SaveSensorNames(ctx, sv.ID, []store.SensorName{{Key: "demo/cpu", Label: "CPU package"}, {Key: "demo/nvme0", Label: "NVMe nvme0"}, {Key: "demo/nvme1", Label: "NVMe nvme1"}})
		if sv.Kind == "local" {
			s.seedDemoSpeed(ctx, sv.ID)
		}
	}
}

// seedDemoSpeed: 60 days of tests every 3 hours on a 150/75 plan, slower in
// the evening, a few failures and one slow week.
func (s *Server) seedDemoSpeed(ctx context.Context, id int64) {
	if s.store.CountSpeedTests(ctx, id) > 0 {
		return
	}
	st := store.SpeedSettings{Enabled: true, EveryMin: 180, OffsetMin: 7, PlanDown: 150, PlanUp: 75, MinDown: 90, MinUp: 45, MaxPing: 30}
	_ = s.store.SaveSpeedSettings(ctx, id, st)
	s.store.EnsureSpeedRules(ctx, id, st)
	loc := s.loc()
	now := time.Now()
	slot := speedSlot(now, st, loc)
	ips := []string{"198.51.100.7", "198.51.100.42", "203.0.113.19"}
	for i := 60 * 8; i >= 0; i-- {
		at := slot.Add(-time.Duration(i) * 3 * time.Hour)
		t := &store.SpeedTest{ServerID: id, T: at.Unix(), Trigger: "schedule", TestServerID: 1234, TestServer: "Example Net", TestLocation: "Springfield", ISP: "Example ISP", ExternalIP: ips[(i/150)%len(ips)]}
		if rand.IntN(60) == 0 {
			t.OK, t.Error = false, "Cannot read from socket: Connection reset by peer"
			_, _ = s.store.AddSpeedTest(ctx, t)
			continue
		}
		h := float64(at.In(loc).Hour())
		evening := math.Max(0, 1-math.Abs(h-21)/4) // the 18–24 h dip
		slow := 1.0
		if i > 20*8 && i < 27*8 {
			slow = 0.55
		}
		down := (151 - 45*evening*rand.Float64()) * slow
		up := 76 - 6*evening*rand.Float64()
		ping := 1.9 + rand.Float64()*0.6 + 8*evening*rand.Float64()
		t.OK = true
		t.DownMbps, t.UpMbps, t.PingMs, t.JitterMs = fp(math.Round(down*100)/100), fp(math.Round(up*100)/100), fp(math.Round(ping*100)/100), fp(math.Round(rand.Float64()*40)/100)
		t.DownLatencyMs, t.UpLatencyMs = fp(math.Round((ping+rand.Float64()*4)*100)/100), fp(math.Round((ping+20+rand.Float64()*25)*100)/100)
		t.LossPct = fp(0)
		t.DownBytes, t.UpBytes, t.DurationMs = int64(down*1.3e6), int64(up*1.3e6), 20000+rand.Int64N(2000)
		busy := math.Round(rand.Float64()*rand.Float64()*40*10) / 10
		t.BusyMbps = &busy
		_, _ = s.store.AddSpeedTest(ctx, t)
	}
}

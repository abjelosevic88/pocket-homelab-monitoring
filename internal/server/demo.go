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
	}
}

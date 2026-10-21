package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

var remoteClient = &http.Client{Timeout: 12 * time.Second}

func getJSON(ctx context.Context, url string, auth func(*http.Request), v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if auth != nil {
		auth(req)
	}
	res, err := remoteClient.Do(req)
	if err != nil {
		var ue *neturl.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the `Get "http://…":` prefix; the address is on the page anyway
		}
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the server refused the credentials (HTTP %d)", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(v)
}

// Agent reads another Pocket Monitoring running as `pocket-monitoring agent`.
func Agent(ctx context.Context, baseURL, token string) (*Snapshot, error) {
	var s Snapshot
	err := getJSON(ctx, strings.TrimRight(baseURL, "/")+"/agent/v1/snapshot", func(r *http.Request) {
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}, &s)
	if err != nil {
		return nil, err
	}
	s.Source = "agent"
	if s.Time.IsZero() {
		s.Time = time.Now().UTC()
	}
	return &s, nil
}

// Glances reads a Glances 4 web server (glances -w) through its REST API.
func Glances(ctx context.Context, baseURL, user, pass string) (*Snapshot, error) {
	base := strings.TrimRight(baseURL, "/")
	if !strings.Contains(base, "/api/") {
		base += "/api/4"
	}
	auth := func(r *http.Request) {
		if user != "" || pass != "" {
			r.SetBasicAuth(user, pass)
		}
	}
	plugins := map[string]any{
		"system": &gSystem{}, "uptime": new(any), "cpu": &gCPU{}, "percpu": &[]gPerCPU{}, "load": &gLoad{}, "mem": &gMem{},
		"memswap": &gSwap{}, "network": &[]gNet{}, "diskio": &[]gDisk{}, "fs": &[]gFS{}, "sensors": &[]gSensor{},
		"processcount": &gProcCount{}, "processlist": &[]gProc{}, "containers": &[]gCtr{}, "core": &gCore{}, "quicklook": &gQuick{},
		"raid": &map[string]gRAID{},
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := map[string]error{}
	for name, dst := range plugins {
		wg.Add(1)
		go func(name string, dst any) {
			defer wg.Done()
			if err := getJSON(ctx, base+"/"+name, auth, dst); err != nil {
				mu.Lock()
				errs[name] = err
				mu.Unlock()
			}
		}(name, dst)
	}
	wg.Wait()
	if err, ok := errs["cpu"]; ok {
		return nil, err
	}
	s := &Snapshot{Time: time.Now().UTC(), Source: "glances", Net: []NetIf{}, DiskIO: []DiskIO{}, FS: []FS{}, Sensors: []Sensor{}, Containers: []Container{}, RAID: []RAID{}}
	for name, err := range errs {
		if name != "raid" && name != "containers" && name != "quicklook" {
			s.Errors = append(s.Errors, name+": "+err.Error())
		}
	}
	sort.Strings(s.Errors)

	sys := plugins["system"].(*gSystem)
	core := plugins["core"].(*gCore)
	q := plugins["quicklook"].(*gQuick)
	s.Host = Host{Hostname: sys.Hostname, OS: strings.ToLower(sys.OSName), Platform: strings.TrimSpace(sys.HRName), Kernel: sys.OSVersion, Arch: sys.Platform, Cores: core.Phys, Threads: core.Log, CPUModel: q.CPUName, IPs: []string{}}
	if s.Host.Platform == "" {
		s.Host.Platform = strings.TrimSpace(sys.LinuxDistro)
	}
	if up, ok := (*plugins["uptime"].(*any)).(string); ok {
		s.Host.UptimeSec = parseGlancesUptime(up)
		s.Host.BootTime = time.Now().Unix() - s.Host.UptimeSec
	}
	if q.CPUHzCurrent > 0 {
		s.CPU.FreqMHz = float64(int64(q.CPUHzCurrent / 1e6))
	}

	c := plugins["cpu"].(*gCPU)
	s.CPU.Total, s.CPU.User, s.CPU.System, s.CPU.Nice = round1(c.Total), round1(c.User), round1(c.System), round1(c.Nice)
	s.CPU.IOWait, s.CPU.IRQ, s.CPU.SoftIRQ, s.CPU.Steal, s.CPU.Idle = round1(c.IOWait), round1(c.IRQ), round1(c.SoftIRQ), round1(c.Steal), round1(c.Idle)
	s.CPU.CtxSwitch, s.CPU.Interrupts = c.CtxRate, c.IntrRate
	for _, p := range *plugins["percpu"].(*[]gPerCPU) {
		if p.User+p.System+p.Idle == 0 {
			p.Total = 0 // Glances answers 100 before its first refresh
		}
		s.CPU.PerCore = append(s.CPU.PerCore, round1(p.Total))
	}
	if s.CPU.PerCore == nil {
		s.CPU.PerCore = []float64{}
	}
	if s.Host.Threads == 0 {
		s.Host.Threads = len(s.CPU.PerCore)
	}
	l := plugins["load"].(*gLoad)
	s.Load = Load{l.Min1, l.Min5, l.Min15}
	m := plugins["mem"].(*gMem)
	s.Mem = Mem{Total: m.Total, Used: m.Used, Available: m.Available, Free: m.Free, Buffers: m.Buffers, Cached: m.Cached, Shared: m.Shared, Percent: round1(m.Percent)}
	sw := plugins["memswap"].(*gSwap)
	s.Swap = Swap{Total: sw.Total, Used: sw.Used, Percent: round1(sw.Percent)}

	for _, n := range *plugins["network"].(*[]gNet) {
		virt := false
		for _, pre := range virtualPrefixes {
			if strings.HasPrefix(n.Name, pre) {
				virt = true
			}
		}
		up := true
		if n.IsUp != nil {
			up = *n.IsUp
		}
		s.Net = append(s.Net, NetIf{Name: n.Name, RxBps: n.RxRate, TxBps: n.TxRate, RxTotal: n.RxGauge, TxTotal: n.TxGauge, Up: up, SpeedMb: int(n.Speed / 1048576), Virtual: virt, Addrs: []string{}})
	}
	sort.SliceStable(s.Net, func(i, j int) bool {
		if s.Net[i].Virtual != s.Net[j].Virtual {
			return !s.Net[i].Virtual
		}
		return s.Net[i].Name < s.Net[j].Name
	})
	for _, d := range *plugins["diskio"].(*[]gDisk) {
		if strings.HasPrefix(d.Name, "loop") || strings.HasPrefix(d.Name, "dm-") || strings.HasPrefix(d.Name, "ram") || strings.HasPrefix(d.Name, "zram") || isPartition(d.Name) {
			continue
		}
		s.DiskIO = append(s.DiskIO, DiskIO{Name: d.Name, ReadBps: d.ReadRate, WriteBps: d.WriteRate, ReadIOPS: round1(d.ReadCountRate), WriteIOPS: round1(d.WriteCountRate), ReadTotal: d.ReadGauge, WriteTot: d.WriteGauge})
	}
	for _, f := range *plugins["fs"].(*[]gFS) {
		x := FS{Device: f.Device, Mount: f.Mount, Type: f.Type, Total: f.Size, Used: f.Used, Free: f.Free, Percent: round1(f.Percent), NetworkFS: netFS[f.Type]}
		if strings.HasPrefix(x.Mount, "/etc/") || strings.HasPrefix(x.Mount, "/var/lib/docker") {
			continue
		}
		s.FS = append(s.FS, x)
	}
	seen := map[string]int{}
	for _, x := range *plugins["sensors"].(*[]gSensor) {
		kind, unit := "temp", "°C"
		switch {
		case strings.HasPrefix(x.Type, "fan"):
			kind, unit = "fan", "RPM"
		case strings.HasPrefix(x.Type, "battery"):
			continue
		}
		v := toFloat(x.Value)
		if kind == "temp" && (v <= -40 || v >= 150) {
			continue
		}
		key := strings.ToLower(x.Type + "/" + x.Label)
		if n := seen[key]; n > 0 {
			key += fmt.Sprintf("#%d", n+1)
		}
		seen[key]++
		label := x.Label
		switch {
		case strings.HasPrefix(label, "Package id"):
			label = "CPU package"
		case strings.HasPrefix(label, "Core "):
			label = "CPU core " + strings.TrimPrefix(label, "Core ")
		case strings.HasPrefix(label, "Composite"):
			label = "NVMe " + strings.TrimSpace(strings.TrimPrefix(label, "Composite"))
		}
		hi, crit := toFloat(x.Warning), toFloat(x.Critical)
		if hi > 150 {
			hi = 0
		}
		if crit > 150 {
			crit = 0
		}
		s.Sensors = append(s.Sensors, Sensor{Key: key, Label: strings.TrimSpace(label), Chip: x.Type, Kind: kind, Unit: unit, Value: round1(v), High: hi, Critical: crit})
	}
	pc := plugins["processcount"].(*gProcCount)
	s.Procs = Procs{Total: pc.Total, Running: pc.Running, Sleeping: pc.Sleeping, Threads: pc.Thread}
	for _, p := range *plugins["processlist"].(*[]gProc) {
		cmd := strings.Join(p.Cmdline, " ")
		if cmd == "" {
			cmd = "[" + p.Name + "]"
		}
		x := Proc{PID: p.PID, PPID: p.PPID, Name: p.Name, User: p.Username, CPU: round1(p.CPU), MemPct: round1(p.Mem), RSS: p.MemInfo.RSS, Threads: p.Threads, State: p.Status, Cmd: cmd, Started: int64(p.Created)}
		if len(p.IO) >= 4 && p.TimeSince > 0 {
			x.ReadBps = float64(int64(float64(sub(p.IO[0], p.IO[2])) / p.TimeSince))
			x.WriteBps = float64(int64(float64(sub(p.IO[1], p.IO[3])) / p.TimeSince))
		}
		s.Procs.Top = append(s.Procs.Top, x)
	}
	sort.Slice(s.Procs.Top, func(i, j int) bool { return s.Procs.Top[i].CPU > s.Procs.Top[j].CPU })
	if len(s.Procs.Top) > 160 {
		s.Procs.Top = s.Procs.Top[:160]
	}
	if s.Procs.Top == nil {
		s.Procs.Top = []Proc{}
	}
	if _, ok := errs["containers"]; !ok {
		s.ContainersAt = s.Time
	}
	for _, c := range *plugins["containers"].(*[]gCtr) {
		x := Container{ID: c.ID, Name: c.Name, Status: c.Status, State: "running", CPU: round1(c.CPUPct), MemUsed: c.MemUsage, MemLimit: c.MemLimit, Ports: c.Ports}
		if len(c.Image) > 0 {
			x.Image = c.Image[0]
		}
		switch strings.ToLower(c.Status) {
		case "healthy", "unhealthy", "starting":
			x.Health = strings.ToLower(c.Status)
		case "running", "":
		default:
			x.State = strings.ToLower(c.Status)
		}
		if c.Network.TimeSince > 0 {
			x.RxBps = float64(int64(float64(c.Network.Rx) / c.Network.TimeSince))
			x.TxBps = float64(int64(float64(c.Network.Tx) / c.Network.TimeSince))
		}
		if c.IO.TimeSince > 0 {
			x.ReadBps = float64(int64(float64(c.IO.Ior) / c.IO.TimeSince))
			x.WriteBps = float64(int64(float64(c.IO.Iow) / c.IO.TimeSince))
		}
		s.Containers = append(s.Containers, x)
	}
	sort.Slice(s.Containers, func(i, j int) bool { return s.Containers[i].Name < s.Containers[j].Name })
	for name, r := range *plugins["raid"].(*map[string]gRAID) {
		x := RAID{Name: name, Level: r.Type, State: r.Status, Devices: r.Available + (r.Used - r.Available), Active: r.Used}
		if r.Available > 0 {
			x.Devices = r.Available
		}
		x.Degraded = r.Status == "active" && r.Used < r.Available
		s.RAID = append(s.RAID, x)
	}
	return s, nil
}

func isPartition(name string) bool {
	if strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "mmcblk") {
		return strings.Contains(name, "p") && strings.LastIndex(name, "p") > strings.LastIndex(name, "n")
	}
	return len(name) > 0 && name[len(name)-1] >= '0' && name[len(name)-1] <= '9' && (strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "vd") || strings.HasPrefix(name, "hd") || strings.HasPrefix(name, "xvd"))
}

// parseGlancesUptime reads "4 days, 3:12:05" or "3:12:05".
func parseGlancesUptime(s string) int64 {
	var days, h, m, sec int64
	if a, b, ok := strings.Cut(s, ","); ok {
		fmt.Sscanf(strings.TrimSpace(a), "%d", &days)
		s = strings.TrimSpace(b)
	}
	fmt.Sscanf(s, "%d:%d:%d", &h, &m, &sec)
	return days*86400 + h*3600 + m*60 + sec
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

type gSystem struct {
	Hostname    string `json:"hostname"`
	OSName      string `json:"os_name"`
	OSVersion   string `json:"os_version"`
	Platform    string `json:"platform"`
	LinuxDistro string `json:"linux_distro"`
	HRName      string `json:"hr_name"`
}
type gCore struct {
	Phys int `json:"phys"`
	Log  int `json:"log"`
}
type gQuick struct {
	CPUName      string  `json:"cpu_name"`
	CPUHzCurrent float64 `json:"cpu_hz_current"`
}
type gCPU struct {
	Total    float64 `json:"total"`
	User     float64 `json:"user"`
	System   float64 `json:"system"`
	Nice     float64 `json:"nice"`
	Idle     float64 `json:"idle"`
	IOWait   float64 `json:"iowait"`
	IRQ      float64 `json:"irq"`
	SoftIRQ  float64 `json:"softirq"`
	Steal    float64 `json:"steal"`
	CtxRate  float64 `json:"ctx_switches_rate_per_sec"`
	IntrRate float64 `json:"interrupts_rate_per_sec"`
}
type gPerCPU struct {
	Total  float64 `json:"total"`
	User   float64 `json:"user"`
	System float64 `json:"system"`
	Idle   float64 `json:"idle"`
}
type gLoad struct {
	Min1  float64 `json:"min1"`
	Min5  float64 `json:"min5"`
	Min15 float64 `json:"min15"`
}
type gMem struct {
	Total     uint64  `json:"total"`
	Available uint64  `json:"available"`
	Percent   float64 `json:"percent"`
	Used      uint64  `json:"used"`
	Free      uint64  `json:"free"`
	Buffers   uint64  `json:"buffers"`
	Cached    uint64  `json:"cached"`
	Shared    uint64  `json:"shared"`
}
type gSwap struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}
type gNet struct {
	Name    string  `json:"interface_name"`
	RxRate  float64 `json:"bytes_recv_rate_per_sec"`
	TxRate  float64 `json:"bytes_sent_rate_per_sec"`
	RxGauge uint64  `json:"bytes_recv_gauge"`
	TxGauge uint64  `json:"bytes_sent_gauge"`
	Speed   float64 `json:"speed"`
	IsUp    *bool   `json:"is_up"`
}
type gDisk struct {
	Name           string  `json:"disk_name"`
	ReadRate       float64 `json:"read_bytes_rate_per_sec"`
	WriteRate      float64 `json:"write_bytes_rate_per_sec"`
	ReadCountRate  float64 `json:"read_count_rate_per_sec"`
	WriteCountRate float64 `json:"write_count_rate_per_sec"`
	ReadGauge      uint64  `json:"read_bytes_gauge"`
	WriteGauge     uint64  `json:"write_bytes_gauge"`
}
type gFS struct {
	Device  string  `json:"device_name"`
	Mount   string  `json:"mnt_point"`
	Type    string  `json:"fs_type"`
	Size    uint64  `json:"size"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	Percent float64 `json:"percent"`
}
type gSensor struct {
	Label    string `json:"label"`
	Value    any    `json:"value"`
	Warning  any    `json:"warning"`
	Critical any    `json:"critical"`
	Type     string `json:"type"`
}
type gProcCount struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Sleeping int `json:"sleeping"`
	Thread   int `json:"thread"`
}
type gProc struct {
	PID      int32    `json:"pid"`
	PPID     int32    `json:"ppid"`
	Name     string   `json:"name"`
	Username string   `json:"username"`
	CPU      float64  `json:"cpu_percent"`
	Mem      float64  `json:"memory_percent"`
	Threads  int32    `json:"num_threads"`
	Status   string   `json:"status"`
	Cmdline  []string `json:"cmdline"`
	Created  float64  `json:"create_time"`
	MemInfo  struct {
		RSS uint64 `json:"rss"`
	} `json:"memory_info"`
	IO        []uint64 `json:"io_counters"`
	TimeSince float64  `json:"time_since_update"`
}
type gCtr struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Image    []string `json:"image"`
	CPUPct   float64  `json:"cpu_percent"`
	MemUsage uint64   `json:"memory_usage"`
	MemLimit uint64   `json:"memory_limit"`
	Ports    string   `json:"ports"`
	Network  struct {
		Rx        uint64  `json:"rx"`
		Tx        uint64  `json:"tx"`
		TimeSince float64 `json:"time_since_update"`
	} `json:"network"`
	IO struct {
		Ior       uint64  `json:"ior"`
		Iow       uint64  `json:"iow"`
		TimeSince float64 `json:"time_since_update"`
	} `json:"io"`
}
type gRAID struct {
	Status    string `json:"status"`
	Type      string `json:"type"`
	Available int    `json:"available"`
	Used      int    `json:"used"`
}

// Package collect reads what a machine is doing right now: CPU, memory,
// swap, load, network, disks, file systems, sensors, processes and
// containers. One Snapshot is one moment; rates (bytes per second, CPU
// percent) are worked out from the previous reading.
//
// The same Snapshot comes from three places: the local collector (this
// box's /proc, /sys and Docker socket), a Pocket Monitoring agent on
// another machine (GET /agent/v1/snapshot), or a Glances server.
package collect

import "time"

// Snapshot is everything known about one machine at one moment.
type Snapshot struct {
	Time       time.Time   `json:"time"`
	Source     string      `json:"source"` // local | agent | glances
	Host       Host        `json:"host"`
	CPU        CPU         `json:"cpu"`
	Load       Load        `json:"load"`
	Mem        Mem         `json:"mem"`
	Swap       Swap        `json:"swap"`
	Net        []NetIf     `json:"net"`
	DiskIO     []DiskIO    `json:"disk_io"`
	FS         []FS        `json:"fs"`
	Sensors    []Sensor    `json:"sensors"`
	Procs      Procs       `json:"procs"`
	Containers []Container `json:"containers"`
	RAID       []RAID      `json:"raid"`
	// ContainersAt is when the container list was last read (it is read
	// less often than the rest); zero when there is no container engine.
	ContainersAt time.Time `json:"containers_at"`
	// Errors are the parts that could not be read ("containers: permission denied").
	Errors []string `json:"errors,omitempty"`
}

// Host describes the machine.
type Host struct {
	Hostname       string   `json:"hostname"`
	OS             string   `json:"os"`       // linux
	Platform       string   `json:"platform"` // ubuntu 24.04
	Kernel         string   `json:"kernel"`
	Arch           string   `json:"arch"`
	Virtualization string   `json:"virtualization,omitempty"`
	CPUModel       string   `json:"cpu_model"`
	Cores          int      `json:"cores"`   // physical
	Threads        int      `json:"threads"` // logical
	BootTime       int64    `json:"boot_time"`
	UptimeSec      int64    `json:"uptime_s"`
	IPs            []string `json:"ips"`
	AgentVersion   string   `json:"agent_version,omitempty"`
}

// CPU is the whole-machine CPU use in percent of all threads.
type CPU struct {
	Total      float64   `json:"total"`
	User       float64   `json:"user"`
	System     float64   `json:"system"`
	Nice       float64   `json:"nice"`
	IOWait     float64   `json:"iowait"`
	IRQ        float64   `json:"irq"`
	SoftIRQ    float64   `json:"softirq"`
	Steal      float64   `json:"steal"`
	Idle       float64   `json:"idle"`
	PerCore    []float64 `json:"per_core"`
	FreqMHz    float64   `json:"freq_mhz"`
	CtxSwitch  float64   `json:"ctx_switches_s"`
	Interrupts float64   `json:"interrupts_s"`
}

// Load is the run-queue average.
type Load struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// Mem is physical memory in bytes. Used = Total - Available.
type Mem struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Free      uint64  `json:"free"`
	Buffers   uint64  `json:"buffers"`
	Cached    uint64  `json:"cached"`
	Shared    uint64  `json:"shared"`
	Percent   float64 `json:"percent"`
}

// Swap is swap space in bytes, with paging rates.
type Swap struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
	InBps   float64 `json:"in_bps"`
	OutBps  float64 `json:"out_bps"`
}

// NetIf is one network interface.
type NetIf struct {
	Name     string   `json:"name"`
	RxBps    float64  `json:"rx_bps"` // bytes per second
	TxBps    float64  `json:"tx_bps"`
	RxTotal  uint64   `json:"rx_total"`
	TxTotal  uint64   `json:"tx_total"`
	RxErrors uint64   `json:"rx_errors"`
	TxErrors uint64   `json:"tx_errors"`
	Drops    uint64   `json:"drops"`
	Up       bool     `json:"up"`
	SpeedMb  int      `json:"speed_mbps"` // link speed, 0 when unknown
	MTU      int      `json:"mtu"`
	Addrs    []string `json:"addrs"`
	Virtual  bool     `json:"virtual"` // loopback, docker bridges, veth, tailscale…
}

// DiskIO is one block device.
type DiskIO struct {
	Name      string  `json:"name"`
	ReadBps   float64 `json:"read_bps"`
	WriteBps  float64 `json:"write_bps"`
	ReadIOPS  float64 `json:"read_iops"`
	WriteIOPS float64 `json:"write_iops"`
	Busy      float64 `json:"busy"` // percent of time the device had I/O in flight
	ReadTotal uint64  `json:"read_total"`
	WriteTot  uint64  `json:"write_total"`
}

// FS is one mounted file system.
type FS struct {
	Device      string  `json:"device"`
	Mount       string  `json:"mount"`
	Type        string  `json:"type"`
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Free        uint64  `json:"free"`
	Percent     float64 `json:"percent"`
	InodesPct   float64 `json:"inodes_percent"`
	ReadOnly    bool    `json:"read_only"`
	NetworkFS   bool    `json:"network"` // nfs, cifs, fuse.sshfs…
	ReservedPct float64 `json:"-"`
}

// Sensor is one temperature, fan or power reading.
type Sensor struct {
	Key      string  `json:"key"`   // stable id: chip/label
	Label    string  `json:"label"` // "CPU package", "NVMe nvme0 (KIOXIA…)"
	Chip     string  `json:"chip"`  // coretemp, nvme, acpitz…
	Kind     string  `json:"kind"`  // temp | fan | power | voltage
	Value    float64 `json:"value"` // °C, RPM, W, V
	High     float64 `json:"high,omitempty"`
	Critical float64 `json:"critical,omitempty"`
	Unit     string  `json:"unit"`
}

// Procs summarises the process table and keeps the busiest processes.
type Procs struct {
	Total    int    `json:"total"`
	Running  int    `json:"running"`
	Sleeping int    `json:"sleeping"`
	Zombie   int    `json:"zombie"`
	Threads  int    `json:"threads"`
	Top      []Proc `json:"top"`
}

// Proc is one process.
type Proc struct {
	PID       int32   `json:"pid"`
	PPID      int32   `json:"ppid"`
	Name      string  `json:"name"`
	User      string  `json:"user"`
	CPU       float64 `json:"cpu"`     // percent of one thread (htop style: 400 = four busy threads)
	MemPct    float64 `json:"mem_pct"` // percent of physical memory
	RSS       uint64  `json:"rss"`
	Threads   int32   `json:"threads"`
	State     string  `json:"state"` // R S D Z T I
	Cmd       string  `json:"cmd"`
	Started   int64   `json:"started"` // unix seconds
	Container string  `json:"container,omitempty"`
	ReadBps   float64 `json:"read_bps"`
	WriteBps  float64 `json:"write_bps"`
}

// Container is one Docker / Podman container.
type Container struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Image    string  `json:"image"`
	State    string  `json:"state"`  // running | exited | paused | restarting | created | dead
	Status   string  `json:"status"` // "Up 3 days (healthy)"
	Health   string  `json:"health"` // healthy | unhealthy | starting | ""
	Project  string  `json:"project,omitempty"`
	CPU      float64 `json:"cpu"` // percent of one thread
	MemUsed  uint64  `json:"mem_used"`
	MemLimit uint64  `json:"mem_limit"`
	RxBps    float64 `json:"rx_bps"`
	TxBps    float64 `json:"tx_bps"`
	ReadBps  float64 `json:"read_bps"`
	WriteBps float64 `json:"write_bps"`
	Started  int64   `json:"started"` // unix seconds, 0 when not running
	Restarts int     `json:"restarts"`
	Ports    string  `json:"ports,omitempty"`
}

// RAID is one Linux software RAID array (/proc/mdstat).
type RAID struct {
	Name     string  `json:"name"`
	Level    string  `json:"level"`
	State    string  `json:"state"` // active | inactive
	Devices  int     `json:"devices"`
	Active   int     `json:"active"`
	Degraded bool    `json:"degraded"`
	Sync     float64 `json:"sync,omitempty"` // resync / recovery progress, percent
}

// Hottest returns the highest temperature reading (0 when none).
func (s *Snapshot) Hottest() (float64, string) {
	best, label := 0.0, ""
	for _, x := range s.Sensors {
		if x.Kind == "temp" && x.Value > best && x.Value < 150 {
			best, label = x.Value, x.Label
		}
	}
	return best, label
}

// NetTotals sums the physical interfaces (virtual ones would count traffic twice).
func (s *Snapshot) NetTotals() (rx, tx float64) {
	any := false
	for _, n := range s.Net {
		if !n.Virtual {
			rx += n.RxBps
			tx += n.TxBps
			any = true
		}
	}
	if !any {
		for _, n := range s.Net {
			if n.Name != "lo" {
				rx += n.RxBps
				tx += n.TxBps
			}
		}
	}
	return
}

// DiskTotals sums the whole disks (partitions would count twice).
func (s *Snapshot) DiskTotals() (r, w float64) {
	for _, d := range s.DiskIO {
		r += d.ReadBps
		w += d.WriteBps
	}
	return
}

// FullestFS returns the highest usage among real file systems.
func (s *Snapshot) FullestFS() (float64, string) {
	best, mount := 0.0, ""
	for _, f := range s.FS {
		if f.Percent > best {
			best, mount = f.Percent, f.Mount
		}
	}
	return best, mount
}

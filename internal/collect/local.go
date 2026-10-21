package collect

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Paths is where the host's /proc, /sys and root file system are visible.
// In a container: run with --pid host --network host, mount / at
// /host/root read-only, and /proc and /sys are the host's already.
type Paths struct {
	Proc string // /proc
	Sys  string // /sys
	Root string // "" or /host/root: prefix for statfs and /etc files
}

// DefaultPaths reads HOST_PROC / HOST_SYS / HOST_ROOT, and picks
// /host/root when it exists.
func DefaultPaths() Paths {
	p := Paths{Proc: env("HOST_PROC", "/proc"), Sys: env("HOST_SYS", "/sys"), Root: os.Getenv("HOST_ROOT")}
	if p.Root == "" {
		if fi, err := os.Stat("/host/root"); err == nil && fi.IsDir() {
			p.Root = "/host/root"
		}
	}
	p.Root = strings.TrimRight(p.Root, "/")
	return p
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// Local reads this machine. It is safe for concurrent use; every call to
// Collect works out rates against the previous call.
type Local struct {
	P        Paths
	Docker   *Docker
	TopProcs int
	Version  string

	mu      sync.Mutex
	prev    *rawState
	procs   procTracker
	users   map[uint32]string
	usersAt time.Time
	static  *Host
}

// NewLocal makes a collector with the default paths and Docker socket.
func NewLocal(version string) *Local {
	l := &Local{P: DefaultPaths(), TopProcs: 80, Version: version}
	l.Docker = NewDocker(os.Getenv("DOCKER_HOST"))
	return l
}

type cpuTimes struct{ user, nice, system, idle, iowait, irq, softirq, steal uint64 }

func (c cpuTimes) total() uint64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

type rawState struct {
	at      time.Time
	cpu     cpuTimes
	cores   []cpuTimes
	ctxt    uint64
	intr    uint64
	pswpin  uint64
	pswpout uint64
	net     map[string][2]uint64
	disks   map[string]diskRaw
}

type diskRaw struct{ rd, wr, rdSec, wrSec, ioMs uint64 }

// Collect takes one reading.
func (l *Local) Collect(ctx context.Context) (*Snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	s := &Snapshot{Time: now.UTC(), Source: "local", Net: []NetIf{}, DiskIO: []DiskIO{}, FS: []FS{}, Sensors: []Sensor{}, Containers: []Container{}, RAID: []RAID{}}
	cur := &rawState{at: now, net: map[string][2]uint64{}, disks: map[string]diskRaw{}}
	fail := func(part string, err error) {
		if err != nil {
			s.Errors = append(s.Errors, part+": "+err.Error())
		}
	}

	fail("host", l.readHost(s))
	fail("cpu", l.readStat(s, cur))
	fail("memory", l.readMem(s))
	l.readVMStat(cur)
	fail("load", l.readLoad(s))
	fail("network", l.readNet(s, cur))
	fail("disks", l.readDisks(s, cur))
	fail("file systems", l.readFS(ctx, s))
	l.readSensors(s)
	l.readRAID(s)
	fail("processes", l.readProcs(s, now))

	if p := l.prev; p != nil {
		dt := now.Sub(p.at).Seconds()
		if dt > 0 {
			s.Swap.InBps = float64(sub(cur.pswpin, p.pswpin)) * 4096 / dt
			s.Swap.OutBps = float64(sub(cur.pswpout, p.pswpout)) * 4096 / dt
		}
	}
	l.prev = cur
	if l.Docker != nil {
		cs, at, err := l.Docker.Latest()
		s.Containers, s.ContainersAt = cs, at
		if err != nil && !l.Docker.Missing() {
			fail("containers", err)
		}
		l.tagContainers(s)
	}
	return s, nil
}

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func pct(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return round1(float64(part) / float64(whole) * 100)
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func (l *Local) proc(p ...string) string { return filepath.Join(append([]string{l.P.Proc}, p...)...) }
func (l *Local) sys(p ...string) string  { return filepath.Join(append([]string{l.P.Sys}, p...)...) }
func (l *Local) root(p string) string    { return l.P.Root + p }

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func atou(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// ---------- host ----------

func (l *Local) readHost(s *Snapshot) error {
	if l.static == nil {
		h := Host{OS: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: l.Version}
		h.Kernel = readTrim(l.proc("sys/kernel/osrelease"))
		h.Platform = osRelease(l.root("/etc/os-release"))
		if h.Platform == "" {
			h.Platform = osRelease("/etc/os-release")
		}
		if f, err := os.Open(l.proc("cpuinfo")); err == nil {
			cores := map[string]bool{}
			phys := ""
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				k, v, ok := strings.Cut(sc.Text(), ":")
				if !ok {
					continue
				}
				k, v = strings.TrimSpace(k), strings.TrimSpace(v)
				switch k {
				case "processor":
					h.Threads++
				case "model name", "Model", "Hardware":
					if h.CPUModel == "" {
						h.CPUModel = v
					}
				case "physical id":
					phys = v
				case "core id":
					cores[phys+"/"+v] = true
				case "flags":
					if strings.Contains(" "+v+" ", " hypervisor ") {
						h.Virtualization = "vm"
					}
				}
			}
			f.Close()
			h.Cores = len(cores)
			if h.Cores == 0 {
				h.Cores = h.Threads
			}
		}
		if h.Threads == 0 {
			h.Threads = runtime.NumCPU()
		}
		if v := readTrim(l.sys("class/dmi/id/product_name")); v != "" && h.Virtualization != "" {
			h.Virtualization = v
		}
		l.static = &h
	}
	h := *l.static
	h.Hostname = readTrim(l.proc("sys/kernel/hostname"))
	if h.Hostname == "" {
		h.Hostname, _ = os.Hostname()
	}
	if up := strings.Fields(readTrim(l.proc("uptime"))); len(up) > 0 {
		h.UptimeSec = int64(atof(up[0]))
		h.BootTime = time.Now().Unix() - h.UptimeSec
	}
	h.IPs = []string{}
	s.Host = h
	return nil
}

func osRelease(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			vals[k] = strings.Trim(v, `"'`)
		}
	}
	if v := vals["PRETTY_NAME"]; v != "" {
		return v
	}
	return strings.TrimSpace(vals["NAME"] + " " + vals["VERSION_ID"])
}

// ---------- cpu ----------

func parseCPU(f []string) cpuTimes {
	var v [8]uint64
	for i := 0; i < 8 && i+1 < len(f); i++ {
		v[i] = atou(f[i+1])
	}
	return cpuTimes{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}
}

func (l *Local) readStat(s *Snapshot, cur *rawState) error {
	b, err := os.ReadFile(l.proc("stat"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch {
		case f[0] == "cpu":
			cur.cpu = parseCPU(f)
		case strings.HasPrefix(f[0], "cpu"):
			cur.cores = append(cur.cores, parseCPU(f))
		case f[0] == "ctxt":
			cur.ctxt = atou(f[1])
		case f[0] == "intr":
			cur.intr = atou(f[1])
		}
	}
	s.CPU.PerCore = make([]float64, len(cur.cores))
	p := l.prev
	if p == nil {
		return nil
	}
	d := func(a, b cpuTimes) (cpuTimes, float64) {
		x := cpuTimes{sub(a.user, b.user), sub(a.nice, b.nice), sub(a.system, b.system), sub(a.idle, b.idle), sub(a.iowait, b.iowait), sub(a.irq, b.irq), sub(a.softirq, b.softirq), sub(a.steal, b.steal)}
		return x, float64(x.total())
	}
	x, t := d(cur.cpu, p.cpu)
	if t > 0 {
		f := func(v uint64) float64 { return round1(float64(v) / t * 100) }
		s.CPU.User, s.CPU.Nice, s.CPU.System, s.CPU.IOWait = f(x.user), f(x.nice), f(x.system), f(x.iowait)
		s.CPU.IRQ, s.CPU.SoftIRQ, s.CPU.Steal, s.CPU.Idle = f(x.irq), f(x.softirq), f(x.steal), f(x.idle)
		s.CPU.Total = round1(100 - float64(x.idle+x.iowait)/t*100)
		if s.CPU.Total < 0 {
			s.CPU.Total = 0
		}
	}
	for i := range cur.cores {
		if i < len(p.cores) {
			x, t := d(cur.cores[i], p.cores[i])
			if t > 0 {
				s.CPU.PerCore[i] = round1(100 - float64(x.idle+x.iowait)/t*100)
			}
		}
	}
	if dt := cur.at.Sub(p.at).Seconds(); dt > 0 {
		s.CPU.CtxSwitch = float64(int64(float64(sub(cur.ctxt, p.ctxt)) / dt))
		s.CPU.Interrupts = float64(int64(float64(sub(cur.intr, p.intr)) / dt))
	}
	// Average current frequency over the cores that report one.
	var sum float64
	var n int
	for i := range cur.cores {
		if v := atof(readTrim(l.sys("devices/system/cpu", "cpu"+strconv.Itoa(i), "cpufreq/scaling_cur_freq"))); v > 0 {
			sum += v / 1000
			n++
		}
	}
	if n > 0 {
		s.CPU.FreqMHz = float64(int64(sum / float64(n)))
	}
	return nil
}

// ---------- memory ----------

func (l *Local) readMem(s *Snapshot) error {
	f, err := os.Open(l.proc("meminfo"))
	if err != nil {
		return err
	}
	defer f.Close()
	m := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fs := strings.Fields(v)
		if len(fs) == 0 {
			continue
		}
		n := atou(fs[0])
		if len(fs) > 1 && fs[1] == "kB" {
			n *= 1024
		}
		m[k] = n
	}
	mem := Mem{Total: m["MemTotal"], Free: m["MemFree"], Buffers: m["Buffers"], Cached: m["Cached"] + m["SReclaimable"], Shared: m["Shmem"]}
	mem.Available = m["MemAvailable"]
	if mem.Available == 0 {
		mem.Available = mem.Free + mem.Buffers + mem.Cached
	}
	mem.Used = sub(mem.Total, mem.Available)
	mem.Percent = pct(mem.Used, mem.Total)
	s.Mem = mem
	s.Swap.Total = m["SwapTotal"]
	s.Swap.Used = sub(m["SwapTotal"], m["SwapFree"]+m["SwapCached"])
	s.Swap.Percent = pct(s.Swap.Used, s.Swap.Total)
	return nil
}

func (l *Local) readVMStat(cur *rawState) {
	b, _ := os.ReadFile(l.proc("vmstat"))
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "pswpin":
			cur.pswpin = atou(f[1])
		case "pswpout":
			cur.pswpout = atou(f[1])
		}
	}
}

func (l *Local) readLoad(s *Snapshot) error {
	f := strings.Fields(readTrim(l.proc("loadavg")))
	if len(f) < 3 {
		return os.ErrNotExist
	}
	s.Load = Load{atof(f[0]), atof(f[1]), atof(f[2])}
	return nil
}

// ---------- network ----------

var virtualPrefixes = []string{"lo", "veth", "docker", "br-", "virbr", "vnet", "tun", "tap", "tailscale", "wg", "zt", "cni", "flannel", "cali", "kube", "lxc", "vmnet", "dummy", "bond-slave"}

func (l *Local) readNet(s *Snapshot, cur *rawState) error {
	b, err := os.ReadFile(l.proc("net/dev"))
	if err != nil {
		return err
	}
	addrs := map[string][]string{}
	if ifs, err := net.Interfaces(); err == nil {
		for _, i := range ifs {
			as, _ := i.Addrs()
			for _, a := range as {
				addrs[i.Name] = append(addrs[i.Name], a.String())
			}
		}
	}
	p := l.prev
	var dt float64
	if p != nil {
		dt = cur.at.Sub(p.at).Seconds()
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		if strings.HasPrefix(name, "veth") {
			continue // one per container; the container stats carry their traffic
		}
		rx, tx := atou(f[0]), atou(f[8])
		cur.net[name] = [2]uint64{rx, tx}
		virt := false
		if target, err := os.Readlink(l.sys("class/net", name)); err == nil && strings.Contains(target, "/virtual/") {
			virt = true
		}
		for _, pre := range virtualPrefixes {
			if strings.HasPrefix(name, pre) {
				virt = true
			}
		}
		n := NetIf{Name: name, RxTotal: rx, TxTotal: tx, RxErrors: atou(f[2]), TxErrors: atou(f[10]), Drops: atou(f[3]) + atou(f[11]), Virtual: virt, Addrs: addrs[name]}
		if n.Addrs == nil {
			n.Addrs = []string{}
		}
		n.Up = readTrim(l.sys("class/net", name, "operstate")) == "up" || (name == "lo")
		if sp, err := strconv.Atoi(readTrim(l.sys("class/net", name, "speed"))); err == nil && sp > 0 {
			n.SpeedMb = sp
		}
		n.MTU, _ = strconv.Atoi(readTrim(l.sys("class/net", name, "mtu")))
		if p != nil && dt > 0 {
			if pv, ok := p.net[name]; ok {
				n.RxBps = float64(int64(float64(sub(rx, pv[0])) / dt))
				n.TxBps = float64(int64(float64(sub(tx, pv[1])) / dt))
			}
		}
		s.Net = append(s.Net, n)
		if !virt || strings.HasPrefix(name, "tailscale") || strings.HasPrefix(name, "wg") {
			for _, a := range n.Addrs {
				ip, _, _ := strings.Cut(a, "/")
				if pip := net.ParseIP(ip); pip != nil && !pip.IsLinkLocalUnicast() && !pip.IsLoopback() {
					s.Host.IPs = append(s.Host.IPs, ip)
				}
			}
		}
	}
	sort.SliceStable(s.Net, func(i, j int) bool {
		if s.Net[i].Virtual != s.Net[j].Virtual {
			return !s.Net[i].Virtual
		}
		return s.Net[i].Name < s.Net[j].Name
	})
	return nil
}

// ---------- disks ----------

func (l *Local) readDisks(s *Snapshot, cur *rawState) error {
	b, err := os.ReadFile(l.proc("diskstats"))
	if err != nil {
		return err
	}
	p := l.prev
	var dt float64
	if p != nil {
		dt = cur.at.Sub(p.at).Seconds()
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		name := f[2]
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "sr") {
			continue
		}
		// Whole disks only: partitions are not listed in /sys/block.
		if _, err := os.Stat(l.sys("block", name)); err != nil {
			continue
		}
		r := diskRaw{rd: atou(f[3]), rdSec: atou(f[5]), wr: atou(f[7]), wrSec: atou(f[9]), ioMs: atou(f[12])}
		cur.disks[name] = r
		d := DiskIO{Name: name, ReadTotal: r.rdSec * 512, WriteTot: r.wrSec * 512}
		if p != nil && dt > 0 {
			if pv, ok := p.disks[name]; ok {
				d.ReadBps = float64(int64(float64(sub(r.rdSec, pv.rdSec)) * 512 / dt))
				d.WriteBps = float64(int64(float64(sub(r.wrSec, pv.wrSec)) * 512 / dt))
				d.ReadIOPS = round1(float64(sub(r.rd, pv.rd)) / dt)
				d.WriteIOPS = round1(float64(sub(r.wr, pv.wr)) / dt)
				d.Busy = round1(float64(sub(r.ioMs, pv.ioMs)) / (dt * 1000) * 100)
				if d.Busy > 100 {
					d.Busy = 100
				}
			}
		}
		s.DiskIO = append(s.DiskIO, d)
	}
	return nil
}

// ---------- file systems ----------

var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true, "f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true, "nfs": true, "nfs4": true, "cifs": true, "smb3": true, "fuse.sshfs": true, "fuse.mergerfs": true, "fuse.rclone": true, "9p": true, "virtiofs": true, "apfs": true, "hfs": true}

var netFS = map[string]bool{"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "fuse.sshfs": true, "fuse.rclone": true, "9p": true}

func (l *Local) readFS(ctx context.Context, s *Snapshot) error {
	src := l.proc("1/mountinfo")
	b, err := os.ReadFile(src)
	if err != nil {
		if b, err = os.ReadFile(l.proc("self/mountinfo")); err != nil {
			return err
		}
	}
	type mnt struct {
		dev, mount, typ, src string
		ro                   bool
	}
	byDev := map[string]mnt{}
	order := []string{}
	for _, line := range strings.Split(string(b), "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		a, c := strings.Fields(pre), strings.Fields(post)
		if len(a) < 6 || len(c) < 2 {
			continue
		}
		typ := c[0]
		if !realFS[typ] {
			continue
		}
		mp := unescapeMount(a[4])
		if strings.HasPrefix(mp, "/var/lib/docker") || strings.HasPrefix(mp, "/run/") || strings.HasPrefix(mp, "/snap/") || strings.HasPrefix(mp, "/var/snap/") || strings.HasPrefix(mp, "/nix/store") {
			continue
		}
		// One row per file system: bind mounts share the device id (and a
		// subtree root); keep the shortest mount point.
		key := a[2] + "|" + c[1]
		if typ == "zfs" || netFS[typ] {
			key = c[1]
		}
		m := mnt{dev: a[2], mount: mp, typ: typ, src: c[1], ro: strings.HasPrefix(a[5], "ro")}
		if old, ok := byDev[key]; ok {
			if len(mp) < len(old.mount) || a[3] == "/" && old.mount != "/" && len(mp) <= len(old.mount) {
				byDev[key] = m
			}
			continue
		}
		byDev[key] = m
		order = append(order, key)
	}
	for _, k := range order {
		m := byDev[k]
		st, ok := statfs(ctx, l.root(m.mount), netFS[m.typ])
		if !ok || st.Blocks == 0 {
			continue
		}
		bs := st.Bsize
		total, free, avail := st.Blocks*bs, st.Bfree*bs, st.Bavail*bs
		used := sub(total, free)
		f := FS{Device: m.src, Mount: m.mount, Type: m.typ, Total: total, Used: used, Free: avail, ReadOnly: m.ro, NetworkFS: netFS[m.typ]}
		if used+avail > 0 {
			f.Percent = round1(float64(used) / float64(used+avail) * 100)
		}
		if st.Files > 0 {
			f.InodesPct = pct(sub(st.Files, st.Ffree), st.Files)
		}
		s.FS = append(s.FS, f)
	}
	sort.SliceStable(s.FS, func(i, j int) bool { return s.FS[i].Mount < s.FS[j].Mount })
	return nil
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---------- sensors ----------

func (l *Local) readSensors(s *Snapshot) {
	dirs, _ := filepath.Glob(l.sys("class/hwmon/hwmon*"))
	sort.Slice(dirs, func(i, j int) bool { return dirs[i] < dirs[j] })
	seen := map[string]int{}
	for _, d := range dirs {
		chip := readTrim(filepath.Join(d, "name"))
		dev := ""
		if t, err := filepath.EvalSymlinks(filepath.Join(d, "device")); err == nil {
			dev = filepath.Base(t)
		}
		prefix := chipLabel(l, chip, dev)
		inputs, _ := filepath.Glob(filepath.Join(d, "*_input"))
		sort.Slice(inputs, func(i, j int) bool { return natLess(inputs[i], inputs[j]) })
		for _, in := range inputs {
			base := strings.TrimSuffix(filepath.Base(in), "_input")
			kind, unit, scale := "", "", 1.0
			switch {
			case strings.HasPrefix(base, "temp"):
				kind, unit, scale = "temp", "°C", 1000
			case strings.HasPrefix(base, "fan"):
				kind, unit = "fan", "RPM"
			case strings.HasPrefix(base, "power"):
				kind, unit, scale = "power", "W", 1e6
			default:
				continue
			}
			raw := readTrim(in)
			if raw == "" {
				continue
			}
			v := atof(raw) / scale
			if kind == "temp" && (v <= -40 || v >= 150) {
				continue
			}
			label := readTrim(filepath.Join(d, base+"_label"))
			if label == "" {
				label = base
			}
			x := Sensor{Chip: chip, Kind: kind, Unit: unit, Value: round1(v), Label: niceSensor(prefix, chip, label)}
			if kind == "temp" {
				x.High = round1(atof(readTrim(filepath.Join(d, base+"_max"))) / 1000)
				x.Critical = round1(atof(readTrim(filepath.Join(d, base+"_crit"))) / 1000)
				if x.High > 150 || x.High < 0 {
					x.High = 0
				}
				if x.Critical > 150 || x.Critical < 0 {
					x.Critical = 0
				}
			}
			key := strings.ToLower(chip + "/" + firstNonEmpty(dev, chip) + "/" + label)
			if n := seen[key]; n > 0 {
				key += "#" + strconv.Itoa(n+1)
			}
			seen[key]++
			x.Key = key
			s.Sensors = append(s.Sensors, x)
		}
	}
	if len(s.Sensors) > 0 {
		return
	}
	// No hwmon (some ARM boards, VMs): thermal zones.
	zones, _ := filepath.Glob(l.sys("class/thermal/thermal_zone*"))
	for _, z := range zones {
		v := atof(readTrim(filepath.Join(z, "temp"))) / 1000
		if v <= 0 || v >= 150 {
			continue
		}
		t := firstNonEmpty(readTrim(filepath.Join(z, "type")), filepath.Base(z))
		s.Sensors = append(s.Sensors, Sensor{Key: "thermal/" + filepath.Base(z), Label: t, Chip: "thermal", Kind: "temp", Unit: "°C", Value: round1(v)})
	}
}

func chipLabel(l *Local, chip, dev string) string {
	switch {
	case chip == "nvme":
		model := readTrim(l.sys("class/nvme", dev, "model"))
		if dev != "" && model != "" {
			return "NVMe " + dev + " · " + model
		}
		return "NVMe " + dev
	case chip == "drivetemp":
		return "Disk " + dev
	}
	return chip
}

func niceSensor(prefix, chip, label string) string {
	switch chip {
	case "coretemp", "k10temp", "zenpower":
		switch {
		case strings.HasPrefix(label, "Package id"), label == "Tctl", label == "Tdie":
			return "CPU package"
		case strings.HasPrefix(label, "Core "):
			return "CPU core " + strings.TrimPrefix(label, "Core ")
		}
		return "CPU " + label
	case "nvme":
		if label == "Composite" {
			return prefix
		}
		return prefix + " " + label
	case "acpitz":
		return "ACPI " + label
	case "pch_cannonlake", "pch_cometlake", "pch_skylake":
		return "Chipset"
	}
	if strings.HasPrefix(chip, "iwlwifi") {
		return "Wi-Fi card"
	}
	if strings.HasPrefix(chip, "pch_") {
		return "Chipset"
	}
	if label == "" || strings.HasPrefix(label, "temp") {
		return prefix + " " + label
	}
	return prefix + " " + label
}

// natLess orders temp2_input before temp10_input.
func natLess(a, b string) bool {
	na, nb := trailingNum(a), trailingNum(b)
	if na != nb && strings.TrimRight(stripNum(a), "0123456789") == strings.TrimRight(stripNum(b), "0123456789") {
		return na < nb
	}
	return a < b
}

func stripNum(s string) string { return strings.TrimSuffix(filepath.Base(s), "_input") }

func trailingNum(s string) int {
	s = stripNum(s)
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(s[i:])
	return n
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------- raid ----------

func (l *Local) readRAID(s *Snapshot) {
	b, err := os.ReadFile(l.proc("mdstat"))
	if err != nil {
		return
	}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		name, rest, ok := strings.Cut(line, " : ")
		if !ok || !strings.HasPrefix(name, "md") {
			continue
		}
		f := strings.Fields(rest)
		r := RAID{Name: strings.TrimSpace(name)}
		if len(f) > 0 {
			r.State = f[0]
		}
		for _, x := range f[1:] {
			if strings.HasPrefix(x, "raid") || x == "linear" {
				r.Level = x
			} else if strings.Contains(x, "[") {
				r.Devices++
			}
		}
		if i+1 < len(lines) {
			nx := lines[i+1]
			if a := strings.LastIndex(nx, "["); a >= 0 {
				if c := strings.Index(nx[a:], "]"); c > 0 {
					st := nx[a+1 : a+c]
					if strings.Contains(st, "U") || strings.Contains(st, "_") {
						r.Active = strings.Count(st, "U")
						r.Degraded = strings.Contains(st, "_")
					}
				}
			}
		}
		if i+2 < len(lines) {
			if j := strings.Index(lines[i+2], "%"); j > 0 {
				k := strings.LastIndex(lines[i+2][:j], "=")
				if k < 0 {
					k = strings.LastIndex(lines[i+2][:j], " ")
				}
				if k >= 0 {
					r.Sync = atof(lines[i+2][k+1 : j])
				}
			}
		}
		s.RAID = append(s.RAID, r)
	}
}

// ---------- processes ----------

type procPrev struct {
	ticks  uint64
	at     time.Time
	rd, wr uint64
}

type procTracker struct {
	prev map[int32]procPrev
}

const clkTck = 100

func (l *Local) userName(uid uint32) string {
	if l.users == nil || time.Since(l.usersAt) > 10*time.Minute {
		l.users = map[uint32]string{}
		l.usersAt = time.Now()
		for _, p := range []string{l.root("/etc/passwd"), "/etc/passwd"} {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Split(line, ":")
				if len(f) > 2 {
					if n, err := strconv.ParseUint(f[2], 10, 32); err == nil {
						if _, ok := l.users[uint32(n)]; !ok {
							l.users[uint32(n)] = f[0]
						}
					}
				}
			}
		}
	}
	if n, ok := l.users[uid]; ok {
		return n
	}
	return strconv.FormatUint(uint64(uid), 10)
}

func (l *Local) readProcs(s *Snapshot, now time.Time) error {
	d, err := os.Open(l.P.Proc)
	if err != nil {
		return err
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return err
	}
	if l.procs.prev == nil {
		l.procs.prev = map[int32]procPrev{}
	}
	next := make(map[int32]procPrev, len(names))
	pageSize := uint64(os.Getpagesize())
	var all []Proc
	var uptime float64
	if up := strings.Fields(readTrim(l.proc("uptime"))); len(up) > 0 {
		uptime = atof(up[0])
	}
	bootUnix := float64(now.Unix()) - uptime
	for _, n := range names {
		if n[0] < '0' || n[0] > '9' {
			continue
		}
		pid64, err := strconv.ParseInt(n, 10, 32)
		if err != nil {
			continue
		}
		pid := int32(pid64)
		b, err := os.ReadFile(l.proc(n, "stat"))
		if err != nil {
			continue
		}
		open, close := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
		if open < 0 || close < open {
			continue
		}
		comm := string(b[open+1 : close])
		f := strings.Fields(string(b[close+1:]))
		if len(f) < 22 {
			continue
		}
		// f[0]=state f[1]=ppid f[11]=utime f[12]=stime f[17]=threads f[19]=starttime f[21]=rss
		state := f[0]
		ticks := atou(f[11]) + atou(f[12])
		threads := atou(f[17])
		s.Procs.Total++
		s.Procs.Threads += int(threads)
		switch state {
		case "R":
			s.Procs.Running++
		case "S", "I", "D":
			s.Procs.Sleeping++
		case "Z":
			s.Procs.Zombie++
		}
		p := Proc{PID: pid, Name: comm, State: state, Threads: int32(threads), RSS: atou(f[21]) * pageSize}
		pp, _ := strconv.ParseInt(f[1], 10, 32)
		p.PPID = int32(pp)
		p.Started = int64(bootUnix + float64(atou(f[19]))/clkTck)
		if prev, ok := l.procs.prev[pid]; ok {
			if dt := now.Sub(prev.at).Seconds(); dt > 0 {
				p.CPU = round1(float64(sub(ticks, prev.ticks)) / clkTck / dt * 100)
			}
		}
		next[pid] = procPrev{ticks: ticks, at: now}
		if s.Mem.Total > 0 {
			p.MemPct = round1(float64(p.RSS) / float64(s.Mem.Total) * 100)
		}
		all = append(all, p)
	}
	// Keep the busiest by CPU and the biggest by memory.
	keep := map[int32]bool{}
	sort.Slice(all, func(i, j int) bool { return all[i].CPU > all[j].CPU })
	for i := 0; i < len(all) && i < l.TopProcs; i++ {
		keep[all[i].PID] = true
	}
	sort.Slice(all, func(i, j int) bool { return all[i].RSS > all[j].RSS })
	for i := 0; i < len(all) && i < l.TopProcs; i++ {
		keep[all[i].PID] = true
	}
	top := make([]Proc, 0, len(keep))
	for _, p := range all {
		if !keep[p.PID] {
			continue
		}
		pid := strconv.Itoa(int(p.PID))
		if fi, err := os.Stat(l.proc(pid)); err == nil {
			if uid, ok := fileUID(fi); ok {
				p.User = l.userName(uid)
			}
		}
		if cmd, err := os.ReadFile(l.proc(pid, "cmdline")); err == nil && len(cmd) > 0 {
			p.Cmd = strings.TrimSpace(strings.ReplaceAll(string(bytes.TrimRight(cmd, "\x00")), "\x00", " "))
			if len(p.Cmd) > 400 {
				p.Cmd = p.Cmd[:400] + "…"
			}
		}
		if p.Cmd == "" {
			p.Cmd = "[" + p.Name + "]"
		}
		if cg, err := os.ReadFile(l.proc(pid, "cgroup")); err == nil {
			p.Container = containerID(string(cg))
		}
		if io, err := os.ReadFile(l.proc(pid, "io")); err == nil {
			var rd, wr uint64
			for _, line := range strings.Split(string(io), "\n") {
				k, v, _ := strings.Cut(line, ": ")
				switch k {
				case "read_bytes":
					rd = atou(v)
				case "write_bytes":
					wr = atou(v)
				}
			}
			if prev, ok := l.procs.prev[p.PID]; ok && (prev.rd > 0 || prev.wr > 0) {
				if dt := now.Sub(prev.at).Seconds(); dt > 0 {
					p.ReadBps = float64(int64(float64(sub(rd, prev.rd)) / dt))
					p.WriteBps = float64(int64(float64(sub(wr, prev.wr)) / dt))
				}
			}
			x := next[p.PID]
			x.rd, x.wr = rd, wr
			next[p.PID] = x
		}
		top = append(top, p)
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].CPU != top[j].CPU {
			return top[i].CPU > top[j].CPU
		}
		return top[i].RSS > top[j].RSS
	})
	s.Procs.Top = top
	l.procs.prev = next
	return nil
}

// containerID pulls a 64-hex container id out of /proc/<pid>/cgroup.
func containerID(cg string) string {
	for _, line := range strings.Split(cg, "\n") {
		for _, part := range strings.FieldsFunc(line, func(r rune) bool { return r == '/' || r == ':' || r == '-' || r == '.' }) {
			if len(part) == 64 && isHex(part) {
				return part
			}
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// tagContainers swaps container ids on processes for container names.
func (l *Local) tagContainers(s *Snapshot) {
	if len(s.Containers) == 0 {
		for i := range s.Procs.Top {
			if len(s.Procs.Top[i].Container) == 64 {
				s.Procs.Top[i].Container = s.Procs.Top[i].Container[:12]
			}
		}
		return
	}
	names := map[string]string{}
	for _, c := range s.Containers {
		names[c.ID] = c.Name
	}
	for i := range s.Procs.Top {
		if id := s.Procs.Top[i].Container; id != "" {
			if n, ok := names[id]; ok {
				s.Procs.Top[i].Container = n
			} else if len(id) == 64 {
				s.Procs.Top[i].Container = id[:12]
			}
		}
	}
}

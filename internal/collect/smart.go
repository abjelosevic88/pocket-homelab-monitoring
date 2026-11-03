package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// SMART reads the health of every drive through smartctl (smartmontools 7+,
// JSON output). Reading SMART is slow and wakes sleeping disks, so it runs on
// its own loop (Run) every few minutes and Collect only takes the latest list.
// A disk asleep at read time keeps its last values (marked standby) and is
// read anyway once a day.
type SMART struct {
	Bin     string   // smartctl
	Devices []string // "/dev/sda" or "/dev/sda:sat"; empty = smartctl --scan-open
	Exclude []string // device names to leave out

	mu      sync.Mutex
	list    []Drive
	at      time.Time
	err     error
	missing bool
	prev    map[string]Drive     // by device name
	full    map[string]time.Time // last read that wasn't skipped for standby
}

// NewSMART finds smartctl ("" = on the PATH or in /usr/sbin).
func NewSMART(bin string, devices, exclude []string) *SMART {
	m := &SMART{Devices: devices, Exclude: exclude, prev: map[string]Drive{}, full: map[string]time.Time{}}
	if bin == "" {
		bin = "smartctl"
	}
	if p, err := exec.LookPath(bin); err == nil {
		m.Bin = p
	} else if _, err := os.Stat("/usr/sbin/smartctl"); err == nil && bin == "smartctl" {
		m.Bin = "/usr/sbin/smartctl"
	} else {
		m.missing = true
	}
	return m
}

// Missing reports that smartctl isn't installed.
func (m *SMART) Missing() bool { return m == nil || m.missing }

// Latest returns the drives of the last pass and when it finished.
func (m *SMART) Latest() ([]Drive, time.Time, error) {
	if m == nil {
		return nil, time.Time{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Drive, len(m.list))
	copy(out, m.list)
	return out, m.at, m.err
}

// Run reads every drive every interval until ctx ends.
func (m *SMART) Run(ctx context.Context, every time.Duration) {
	if m.Missing() {
		return
	}
	for {
		list, err := m.Read(ctx)
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		if list != nil {
			m.list, m.at = list, time.Now().UTC()
		}
		m.err = err
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

type smartDev struct{ name, typ string }

// Read takes one pass over every drive. A drive that can't be read this
// time keeps its last reading with the error; only a drive that is no longer
// there at all drops out.
func (m *SMART) Read(ctx context.Context) ([]Drive, error) {
	devs, err := m.devices(ctx)
	if err != nil {
		return nil, err
	}
	out := []Drive{}
	seen := map[string]bool{}
	var errs []string
	for _, dv := range devs {
		if seen[dv.name] || m.excluded(dv.name) {
			continue
		}
		seen[dv.name] = true
		d, err := m.readOne(ctx, dv)
		if err != nil {
			errs = append(errs, dv.name+": "+err.Error())
			continue
		}
		if d != nil {
			out = append(out, *d)
		}
	}
	for name := range m.prev {
		if !seen[name] {
			delete(m.prev, name)
			delete(m.full, name)
		}
	}
	// one disk can show up under two names (multipath, a type override)
	byKey := map[string]bool{}
	uniq := out[:0]
	for _, d := range out {
		if !byKey[d.Key] {
			byKey[d.Key] = true
			uniq = append(uniq, d)
		}
	}
	out = uniq
	sort.Slice(out, func(i, j int) bool { return natLess(out[i].Name, out[j].Name) })
	if len(out) == 0 && len(errs) > 0 {
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

func (m *SMART) excluded(name string) bool {
	for _, x := range m.Exclude {
		if x == name || "/dev/"+x == name {
			return true
		}
	}
	return false
}

func (m *SMART) devices(ctx context.Context) ([]smartDev, error) {
	if len(m.Devices) > 0 {
		out := []smartDev{}
		for _, d := range m.Devices {
			name, typ, _ := strings.Cut(strings.TrimSpace(d), ":")
			if name == "" {
				continue
			}
			if !strings.HasPrefix(name, "/") {
				name = "/dev/" + name
			}
			out = append(out, smartDev{name, typ})
		}
		return out, nil
	}
	b, _ := m.run(ctx, 30*time.Second, "-j", "--scan-open")
	var scan struct {
		Devices []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Protocol string `json:"protocol"`
			OpenErr  string `json:"open_error"`
		} `json:"devices"`
		Smartctl smartctlInfo `json:"smartctl"`
	}
	if err := json.Unmarshal(b, &scan); err != nil {
		return nil, fmt.Errorf("smartctl --scan-open: %w", err)
	}
	out := []smartDev{}
	var openErr string
	for _, d := range scan.Devices {
		if d.OpenErr != "" {
			openErr = d.OpenErr
			continue
		}
		out = append(out, smartDev{d.Name, d.Type})
	}
	if len(out) == 0 && openErr != "" {
		return nil, errors.New(openErr + " (SMART needs root and the disks: run privileged, see the README)")
	}
	if len(out) == 0 {
		if msg := scan.Smartctl.message(); msg != "" {
			return nil, errors.New(msg)
		}
	}
	return out, nil
}

func (m *SMART) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, m.Bin, args...)
	cmd.Stdout = &out
	err := cmd.Run()
	return out.Bytes(), err // smartctl's exit status is a bit mask: the JSON says more
}

func (m *SMART) readOne(ctx context.Context, dv smartDev) (*Drive, error) {
	prev, hasPrev := m.prev[dv.name]
	// Don't spin up a sleeping disk just to read it — except once a day.
	gentle := hasPrev && time.Since(m.full[dv.name]) < 24*time.Hour
	args := []string{"-j", "-x"}
	if gentle {
		args = append(args, "-n", "standby")
	}
	if dv.typ != "" {
		args = append(args, "-d", dv.typ)
	}
	args = append(args, dv.name)
	b, _ := m.run(ctx, 90*time.Second, args...)
	var raw smartJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		if hasPrev {
			prev.Error = "smartctl gave no answer"
			return &prev, nil
		}
		return nil, errors.New("smartctl gave no answer")
	}
	if raw.Smartctl.standby() {
		if hasPrev {
			prev.Standby = true
			prev.Error = ""
			return &prev, nil
		}
		return nil, nil
	}
	d := parseDrive(&raw, dv)
	if d == nil {
		if msg := raw.Smartctl.message(); msg != "" && hasPrev {
			prev.Error = msg
			return &prev, nil
		}
		return nil, nil // no SMART here (a USB stick behind an unknown bridge)
	}
	d.ReadAt = time.Now().UTC()
	m.prev[dv.name] = *d
	m.full[dv.name] = d.ReadAt
	return d, nil
}

// ---------- the reading ----------

// Drive is the SMART health of one disk.
type Drive struct {
	Key        string `json:"key"`  // stable id: the serial number
	Name       string `json:"name"` // /dev/sda
	Type       string `json:"type"` // smartctl -d: sat, nvme, scsi…
	Protocol   string `json:"protocol"`
	Model      string `json:"model"`
	Family     string `json:"family,omitempty"`
	Serial     string `json:"serial"`
	WWN        string `json:"wwn,omitempty"`
	Firmware   string `json:"firmware,omitempty"`
	Capacity   uint64 `json:"capacity"`
	RPM        int    `json:"rpm"` // 0 = solid state
	FormFactor string `json:"form_factor,omitempty"`
	Interface  string `json:"interface,omitempty"`

	// Status is passed, warning or failed; Problems say why it failed,
	// Warnings what to keep an eye on.
	Status      string   `json:"status"`
	Problems    []string `json:"problems,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	SmartPassed *bool    `json:"smart_passed"`

	Temp            float64  `json:"temp"`
	TempMax         float64  `json:"temp_max,omitempty"` // highest in its life, when the drive keeps it
	TempLimit       float64  `json:"temp_limit"`         // over this it runs hot
	PowerOnHours    int64    `json:"power_on_hours"`
	PowerCycles     int64    `json:"power_cycles"`
	UnsafeShutdowns int64    `json:"unsafe_shutdowns,omitempty"`
	Wear            *float64 `json:"wear,omitempty"` // percent of the rated write endurance used
	Spare           *float64 `json:"spare,omitempty"`
	SpareThreshold  float64  `json:"spare_threshold,omitempty"`
	CriticalWarning int      `json:"critical_warning,omitempty"`
	MediaErrors     int64    `json:"media_errors,omitempty"`
	ErrorLog        int64    `json:"error_log,omitempty"` // entries in the drive's error log
	Reallocated     int64    `json:"reallocated,omitempty"`
	Pending         int64    `json:"pending,omitempty"`
	Uncorrectable   int64    `json:"uncorrectable,omitempty"`
	CRCErrors       int64    `json:"crc_errors,omitempty"`
	// Errors sums the signs of a dying surface: reallocated, pending and
	// uncorrectable sectors, reported uncorrectable errors, NVMe media errors.
	Errors  int64  `json:"errors"`
	Written uint64 `json:"written,omitempty"` // bytes over its life
	Read    uint64 `json:"read,omitempty"`

	Attrs     []DriveAttr `json:"attrs"`
	SelfTests []SelfTest  `json:"self_tests,omitempty"`

	Standby bool      `json:"standby,omitempty"` // asleep at the last pass: these are older values
	Error   string    `json:"error,omitempty"`   // the last pass couldn't read it: these are older values
	ReadAt  time.Time `json:"read_at"`
}

// DriveAttr is one SMART attribute (ATA) or health log field (NVMe).
type DriveAttr struct {
	Key     string `json:"key"` // "5" or "media_errors": the history keys
	ID      int    `json:"id,omitempty"`
	Name    string `json:"name"`
	Value   *int   `json:"value,omitempty"` // normalised (ATA): 1–253, higher is better
	Worst   *int   `json:"worst,omitempty"`
	Thresh  *int   `json:"thresh,omitempty"`
	Raw     int64  `json:"raw"`
	RawStr  string `json:"raw_str,omitempty"`
	Prefail bool   `json:"prefail,omitempty"`
	Failed  string `json:"failed,omitempty"` // now | past: under the maker's threshold
	Status  string `json:"status,omitempty"` // warn | fail
	Note    string `json:"note,omitempty"`
}

// SelfTest is one entry of the drive's self-test log, newest first.
type SelfTest struct {
	Type    string `json:"type"`
	Result  string `json:"result"`
	Passed  bool   `json:"passed"`
	Failed  bool   `json:"failed"`
	Hours   int64  `json:"hours"`
	Running bool   `json:"running,omitempty"`
}

// ---------- smartctl -j -x ----------

type smartctlInfo struct {
	Messages []struct {
		String   string `json:"string"`
		Severity string `json:"severity"`
	} `json:"messages"`
	ExitStatus int `json:"exit_status"`
}

func (s smartctlInfo) message() string {
	for _, m := range s.Messages {
		if m.Severity == "error" || m.Severity == "" {
			return strings.TrimSpace(m.String)
		}
	}
	return ""
}

func (s smartctlInfo) standby() bool {
	for _, m := range s.Messages {
		u := strings.ToUpper(m.String)
		if strings.Contains(u, "STANDBY") || strings.Contains(u, "SLEEP") {
			return true
		}
	}
	return false
}

type jStatus struct {
	Value  int    `json:"value"`
	String string `json:"string"`
	Passed *bool  `json:"passed"`
}

type smartJSON struct {
	Smartctl smartctlInfo `json:"smartctl"`
	Device   struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Protocol string `json:"protocol"`
	} `json:"device"`
	ModelFamily  string `json:"model_family"`
	ModelName    string `json:"model_name"`
	ScsiModel    string `json:"scsi_model_name"`
	ScsiVendor   string `json:"scsi_vendor"`
	SerialNumber string `json:"serial_number"`
	WWN          *struct {
		NAA uint64 `json:"naa"`
		OUI uint64 `json:"oui"`
		ID  uint64 `json:"id"`
	} `json:"wwn"`
	Firmware     string `json:"firmware_version"`
	UserCapacity struct {
		Bytes uint64 `json:"bytes"`
	} `json:"user_capacity"`
	NVMeTotalCapacity uint64 `json:"nvme_total_capacity"`
	LogicalBlockSize  uint64 `json:"logical_block_size"`
	RotationRate      *int   `json:"rotation_rate"`
	FormFactor        struct {
		Name string `json:"name"`
	} `json:"form_factor"`
	SATAVersion struct {
		String string `json:"string"`
	} `json:"sata_version"`
	InterfaceSpeed struct {
		Current struct {
			String string `json:"string"`
		} `json:"current"`
	} `json:"interface_speed"`
	NVMeVersion struct {
		String string `json:"string"`
	} `json:"nvme_version"`
	SmartSupport *struct {
		Available bool `json:"available"`
		Enabled   bool `json:"enabled"`
	} `json:"smart_support"`
	SmartStatus *struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	Temperature struct {
		Current     *float64 `json:"current"`
		LifetimeMax float64  `json:"lifetime_max"`
		OpLimitMax  float64  `json:"op_limit_max"`
	} `json:"temperature"`
	PowerOnTime struct {
		Hours int64 `json:"hours"`
	} `json:"power_on_time"`
	PowerCycleCount int64 `json:"power_cycle_count"`

	ATAAttrs struct {
		Table []struct {
			ID         int    `json:"id"`
			Name       string `json:"name"`
			Value      int    `json:"value"`
			Worst      int    `json:"worst"`
			Thresh     int    `json:"thresh"`
			WhenFailed string `json:"when_failed"`
			Flags      struct {
				Prefailure bool `json:"prefailure"`
			} `json:"flags"`
			Raw struct {
				Value  int64  `json:"value"`
				String string `json:"string"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	ATADevStats struct {
		Pages []struct {
			Name  string `json:"name"`
			Table []struct {
				Name  string `json:"name"`
				Value *int64 `json:"value"`
			} `json:"table"`
		} `json:"pages"`
	} `json:"ata_device_statistics"`
	ATAErrorLog struct {
		Extended struct {
			Count int64 `json:"count"`
		} `json:"extended"`
		Summary struct {
			Count int64 `json:"count"`
		} `json:"summary"`
	} `json:"ata_smart_error_log"`
	ATASelfTests struct {
		Extended struct {
			Table []ataTest `json:"table"`
		} `json:"extended"`
		Standard struct {
			Table []ataTest `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	ATASmartData struct {
		SelfTest struct {
			Status jStatus `json:"status"`
		} `json:"self_test"`
	} `json:"ata_smart_data"`

	NVMeLog *struct {
		CriticalWarning  int      `json:"critical_warning"`
		Temperature      *float64 `json:"temperature"`
		AvailableSpare   *float64 `json:"available_spare"`
		SpareThreshold   float64  `json:"available_spare_threshold"`
		PercentageUsed   *float64 `json:"percentage_used"`
		DataUnitsRead    uint64   `json:"data_units_read"`
		DataUnitsWritten uint64   `json:"data_units_written"`
		HostReads        uint64   `json:"host_reads"`
		HostWrites       uint64   `json:"host_writes"`
		ControllerBusy   int64    `json:"controller_busy_time"`
		PowerCycles      int64    `json:"power_cycles"`
		PowerOnHours     int64    `json:"power_on_hours"`
		UnsafeShutdowns  int64    `json:"unsafe_shutdowns"`
		MediaErrors      int64    `json:"media_errors"`
		ErrLogEntries    int64    `json:"num_err_log_entries"`
		WarningTempTime  int64    `json:"warning_temp_time"`
		CriticalCompTime int64    `json:"critical_comp_time"`
	} `json:"nvme_smart_health_information_log"`
	NVMeSelfTestLog *struct {
		Current struct {
			Value  int    `json:"value"`
			String string `json:"string"`
		} `json:"current_self_test_operation"`
		Table []struct {
			Code struct {
				String string `json:"string"`
			} `json:"self_test_code"`
			Result struct {
				Value  int    `json:"value"`
				String string `json:"string"`
			} `json:"self_test_result"`
			Hours int64 `json:"power_on_hours"`
		} `json:"table"`
	} `json:"nvme_self_test_log"`
	SCSIGrownDefects *int64 `json:"scsi_grown_defect_list"`
}

type ataTest struct {
	Type struct {
		String string `json:"string"`
	} `json:"type"`
	Status        jStatus `json:"status"`
	LifetimeHours int64   `json:"lifetime_hours"`
}

var keyClean = regexp.MustCompile(`[^a-z0-9._-]+`)

// DriveKey turns a serial number into the drive's id.
func DriveKey(serial string) string {
	return strings.Trim(keyClean.ReplaceAllString(strings.ToLower(strings.TrimSpace(serial)), "-"), "-")
}

func ip(v int) *int { return &v }

// ataNames are the attributes worth a word of explanation.
var ataNotes = map[int]string{
	5:   "Sectors the drive gave up on and replaced from its spare area. Any at all is an early sign of a failing surface; a growing count means replace it.",
	10:  "Retries to spin the platters up to speed. Points at a weak motor or power supply.",
	184: "Data corrupted between the drive's cache and its interface.",
	187: "Errors the drive could not correct with its error-correcting code.",
	188: "Commands that timed out, often a cable, power or controller problem.",
	196: "How often the drive had to reallocate a sector.",
	197: "Sectors waiting to be reallocated because they couldn't be read. They clear when rewritten; if they stay, the surface is going.",
	198: "Sectors found unreadable by the drive's own offline scan.",
	199: "Transfer errors on the cable. Almost always the cable or the port, not the disk.",
}

// criticalATA are the attributes whose raw count should stay at zero
// (Backblaze's list plus spin retries and end-to-end errors).
var criticalATA = map[int]bool{5: true, 10: true, 184: true, 187: true, 188: true, 196: true, 197: true, 198: true}

func parseDrive(j *smartJSON, dv smartDev) *Drive {
	model := strings.TrimSpace(firstNonEmpty(j.ModelName, strings.TrimSpace(j.ScsiVendor+" "+j.ScsiModel)))
	if j.SerialNumber == "" || (j.SmartSupport != nil && !j.SmartSupport.Available && j.NVMeLog == nil) {
		return nil
	}
	d := &Drive{Key: DriveKey(j.SerialNumber), Name: firstNonEmpty(j.Device.Name, dv.name), Type: firstNonEmpty(j.Device.Type, dv.typ), Protocol: j.Device.Protocol,
		Model: model, Family: j.ModelFamily, Serial: strings.TrimSpace(j.SerialNumber), Firmware: j.Firmware, Capacity: j.UserCapacity.Bytes,
		FormFactor: j.FormFactor.Name, Attrs: []DriveAttr{}}
	if d.Capacity == 0 {
		d.Capacity = j.NVMeTotalCapacity
	}
	if j.WWN != nil && j.WWN.NAA > 0 {
		d.WWN = fmt.Sprintf("0x%x%03x%09x", j.WWN.NAA, j.WWN.OUI, j.WWN.ID)
	}
	if j.RotationRate != nil {
		d.RPM = *j.RotationRate
	}
	switch {
	case j.NVMeVersion.String != "":
		d.Interface = "NVMe " + j.NVMeVersion.String
	case j.SATAVersion.String != "":
		d.Interface = j.SATAVersion.String
		if s := j.InterfaceSpeed.Current.String; s != "" {
			d.Interface += " · " + s
		}
	}
	if j.SmartStatus != nil {
		d.SmartPassed = j.SmartStatus.Passed
	}
	if j.Temperature.Current != nil {
		d.Temp = *j.Temperature.Current
	}
	d.TempMax = j.Temperature.LifetimeMax
	d.PowerOnHours = j.PowerOnTime.Hours
	d.PowerCycles = j.PowerCycleCount
	bs := j.LogicalBlockSize
	if bs == 0 {
		bs = 512
	}

	if n := j.NVMeLog; n != nil {
		parseNVMe(d, j)
	} else {
		parseATA(d, j, bs)
	}
	switch {
	case j.NVMeLog != nil:
		d.TempLimit = 70
		if j.Temperature.OpLimitMax > 0 { // the drive's warning temperature (WCTEMP)
			d.TempLimit = j.Temperature.OpLimitMax
		}
	case d.RPM > 0:
		d.TempLimit = 55 // spinning disks age fast above ~50 °C, whatever their data sheet allows
		if j.Temperature.OpLimitMax > 0 && j.Temperature.OpLimitMax < d.TempLimit {
			d.TempLimit = j.Temperature.OpLimitMax
		}
	default:
		d.TempLimit = 70
		if j.Temperature.OpLimitMax > 0 {
			d.TempLimit = j.Temperature.OpLimitMax
		}
	}
	if j.SCSIGrownDefects != nil {
		d.Reallocated = *j.SCSIGrownDefects
		d.Errors += *j.SCSIGrownDefects
	}
	Assess(d)
	return d
}

func parseNVMe(d *Drive, j *smartJSON) {
	n := j.NVMeLog
	if d.Protocol == "" {
		d.Protocol = "NVMe"
	}
	if n.Temperature != nil && d.Temp == 0 {
		d.Temp = *n.Temperature
	}
	if d.PowerOnHours == 0 {
		d.PowerOnHours = n.PowerOnHours
	}
	if d.PowerCycles == 0 {
		d.PowerCycles = n.PowerCycles
	}
	d.Wear, d.Spare, d.SpareThreshold = n.PercentageUsed, n.AvailableSpare, n.SpareThreshold
	d.CriticalWarning = n.CriticalWarning
	d.MediaErrors, d.ErrorLog, d.UnsafeShutdowns = n.MediaErrors, n.ErrLogEntries, n.UnsafeShutdowns
	d.Errors = n.MediaErrors
	d.Written, d.Read = n.DataUnitsWritten*512000, n.DataUnitsRead*512000
	add := func(key, name string, v int64, note string) {
		d.Attrs = append(d.Attrs, DriveAttr{Key: key, Name: name, Raw: v, Note: note})
	}
	add("critical_warning", "Critical warning", int64(n.CriticalWarning), "Flags the drive raises itself: spare space low, too hot, reliability degraded, read-only. Should be 0.")
	if n.Temperature != nil {
		add("temperature", "Temperature (°C)", int64(*n.Temperature), "")
	}
	if n.AvailableSpare != nil {
		add("available_spare", "Available spare (%)", int64(*n.AvailableSpare), fmt.Sprintf("Spare blocks left to replace worn ones. The drive counts as failing under %g %%.", n.SpareThreshold))
	}
	add("available_spare_threshold", "Available spare threshold (%)", int64(n.SpareThreshold), "")
	if n.PercentageUsed != nil {
		add("percentage_used", "Percentage used", int64(*n.PercentageUsed), "The maker's estimate of how much of the rated write endurance is used. It can pass 100.")
	}
	add("data_units_written", "Data written", int64(n.DataUnitsWritten), "In units of 512,000 bytes.")
	add("data_units_read", "Data read", int64(n.DataUnitsRead), "In units of 512,000 bytes.")
	add("host_writes", "Write commands", int64(n.HostWrites), "")
	add("host_reads", "Read commands", int64(n.HostReads), "")
	add("controller_busy_time", "Controller busy (min)", n.ControllerBusy, "")
	add("power_cycles", "Power cycles", n.PowerCycles, "")
	add("power_on_hours", "Power-on hours", n.PowerOnHours, "")
	add("unsafe_shutdowns", "Unsafe shutdowns", n.UnsafeShutdowns, "Power lost without a clean shutdown. Harmless on its own, but each one risks the data in flight.")
	add("media_errors", "Media errors", n.MediaErrors, "Data the drive could not read back intact. Should be 0.")
	add("num_err_log_entries", "Error log entries", n.ErrLogEntries, "Errors the controller logged; many are harmless (a command the drive didn't support).")
	add("warning_temp_time", "Time over warning temp (min)", n.WarningTempTime, "")
	add("critical_comp_time", "Time over critical temp (min)", n.CriticalCompTime, "")
	for i := range d.Attrs {
		a := &d.Attrs[i]
		switch a.Key {
		case "media_errors":
			if a.Raw > 0 {
				a.Status = "warn"
			}
		case "critical_warning":
			if a.Raw&^0x02 != 0 {
				a.Status = "fail"
			} else if a.Raw != 0 {
				a.Status = "warn"
			}
		case "available_spare":
			if n.SpareThreshold > 0 && float64(a.Raw) <= n.SpareThreshold {
				a.Status = "fail"
			}
		case "percentage_used":
			if a.Raw >= 100 {
				a.Status = "warn"
			}
		}
	}
	if t := j.NVMeSelfTestLog; t != nil {
		if t.Current.Value != 0 {
			d.SelfTests = append(d.SelfTests, SelfTest{Type: "Self-test", Result: t.Current.String, Running: true})
		}
		for _, e := range t.Table {
			r := e.Result.Value
			d.SelfTests = append(d.SelfTests, SelfTest{Type: e.Code.String, Result: e.Result.String, Hours: e.Hours, Passed: r == 0, Failed: r >= 5 && r <= 7})
		}
	}
}

func parseATA(d *Drive, j *smartJSON, bs uint64) {
	if d.Protocol == "" {
		d.Protocol = "ATA"
	}
	raw := map[int]int64{}
	norm := map[int]int{}
	for _, t := range j.ATAAttrs.Table {
		r := t.Raw.Value
		if t.ID == 188 {
			r &= 0xffff // Seagate packs three counters; the low word is the total
		}
		if t.ID == 194 || t.ID == 190 {
			r &= 0xff // the low byte is the temperature, the rest min/max
		}
		raw[t.ID], norm[t.ID] = r, t.Value
		a := DriveAttr{Key: fmt.Sprint(t.ID), ID: t.ID, Name: strings.ReplaceAll(t.Name, "_", " "), Value: ip(t.Value), Worst: ip(t.Worst), Thresh: ip(t.Thresh),
			Raw: r, Prefail: t.Flags.Prefailure, Note: ataNotes[t.ID]}
		if t.Raw.String != "" && t.Raw.String != fmt.Sprint(t.Raw.Value) {
			a.RawStr = t.Raw.String
		}
		switch {
		case t.WhenFailed == "now" || (t.Thresh > 0 && t.Value <= t.Thresh && t.Value > 0):
			a.Failed, a.Status = "now", "fail"
		case t.WhenFailed == "past" || t.WhenFailed == "In_the_past":
			a.Failed, a.Status = "past", "warn"
		case criticalATA[t.ID] && r > 0:
			a.Status = "warn"
		}
		d.Attrs = append(d.Attrs, a)
	}
	d.Reallocated, d.Pending, d.Uncorrectable, d.CRCErrors = raw[5], raw[197], raw[198], raw[199]
	d.Errors = raw[5] + raw[197] + raw[198] + raw[187]
	if d.Temp == 0 {
		d.Temp = float64(raw[194])
	}
	stat := func(page, name string) (int64, bool) {
		for _, p := range j.ATADevStats.Pages {
			if p.Name != page {
				continue
			}
			for _, t := range p.Table {
				if t.Name == name && t.Value != nil {
					return *t.Value, true
				}
			}
		}
		return 0, false
	}
	if v, ok := stat("General Statistics", "Logical Sectors Written"); ok {
		d.Written = uint64(v) * bs
	} else if v, ok := raw[241]; ok && d.RPM > 0 {
		d.Written = uint64(v) * bs
	}
	if v, ok := stat("General Statistics", "Logical Sectors Read"); ok {
		d.Read = uint64(v) * bs
	} else if v, ok := raw[242]; ok && d.RPM > 0 {
		d.Read = uint64(v) * bs
	}
	if d.RPM == 0 { // solid state: how worn
		if v, ok := stat("Solid State Device Statistics", "Percentage Used Endurance Indicator"); ok {
			f := float64(v)
			d.Wear = &f
		} else {
			for _, id := range []int{231, 233, 202, 177, 173} {
				if v, ok := norm[id]; ok && v > 0 && v <= 100 {
					f := float64(100 - v)
					d.Wear = &f
					break
				}
			}
		}
	}
	d.ErrorLog = j.ATAErrorLog.Extended.Count
	if d.ErrorLog == 0 {
		d.ErrorLog = j.ATAErrorLog.Summary.Count
	}
	tests := j.ATASelfTests.Extended.Table
	if len(tests) == 0 {
		tests = j.ATASelfTests.Standard.Table
	}
	if st := j.ATASmartData.SelfTest.Status; st.Value>>4 == 0xf || st.Value == 0xf {
		d.SelfTests = append(d.SelfTests, SelfTest{Type: "Self-test", Result: st.String, Running: true})
	}
	for _, t := range tests {
		v := t.Status.Value & 0xf
		if t.Status.Value > 0xf {
			v = t.Status.Value >> 4
		}
		passed := t.Status.Passed != nil && *t.Status.Passed
		d.SelfTests = append(d.SelfTests, SelfTest{Type: t.Type.String, Result: t.Status.String, Hours: t.LifetimeHours, Passed: passed,
			Failed: !passed && v >= 3 && v <= 8})
	}
}

// Assess works out the drive's status from its reading.
func Assess(d *Drive) {
	d.Problems, d.Warnings = nil, nil
	fail := func(f string, a ...any) { d.Problems = append(d.Problems, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { d.Warnings = append(d.Warnings, fmt.Sprintf(f, a...)) }
	if d.SmartPassed != nil && !*d.SmartPassed {
		fail("the drive's own SMART health check says it is failing")
	}
	if c := d.CriticalWarning; c != 0 {
		var what []string
		for bit, s := range map[int]string{0x01: "spare space below its threshold", 0x04: "reliability degraded", 0x08: "read-only", 0x10: "volatile memory backup failed", 0x20: "persistent memory read-only"} {
			if c&bit != 0 {
				what = append(what, s)
			}
		}
		sort.Strings(what)
		if len(what) > 0 {
			fail("critical warning: %s", strings.Join(what, ", "))
		}
		if c&0x02 != 0 {
			warn("critical warning: over its temperature threshold")
		}
	}
	if d.Spare != nil && d.SpareThreshold > 0 && *d.Spare <= d.SpareThreshold {
		fail("spare space down to %g %% (threshold %g %%)", *d.Spare, d.SpareThreshold)
	}
	for _, a := range d.Attrs {
		if a.ID == 0 {
			continue
		}
		switch {
		case a.Failed == "now":
			fail("%s is at the maker's failure threshold (%d ≤ %d)", a.Name, deref(a.Value), deref(a.Thresh))
		case a.Failed == "past":
			warn("%s fell to the maker's failure threshold in the past", a.Name)
		case a.Status == "warn":
			warn("%s: %d", a.Name, a.Raw)
		}
	}
	if d.MediaErrors > 0 {
		warn("%d media errors", d.MediaErrors)
	}
	if d.Wear != nil {
		if *d.Wear >= 90 {
			warn("%g %% of its rated write endurance used", *d.Wear)
		}
	}
	if d.TempLimit > 0 && d.Temp > d.TempLimit {
		warn("running hot: %g °C (limit %g °C)", d.Temp, d.TempLimit)
	}
	for _, t := range d.SelfTests {
		if t.Running {
			continue
		}
		if t.Failed {
			fail("the last self-test failed: %s", t.Result)
		}
		break // only the newest finished test counts
	}
	switch {
	case len(d.Problems) > 0:
		d.Status = "failed"
	case len(d.Warnings) > 0:
		d.Status = "warning"
	default:
		d.Status = "passed"
	}
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

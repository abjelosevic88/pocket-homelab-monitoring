package collect

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func loadSmart(t *testing.T, name string) *smartJSON {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var j smartJSON
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	return &j
}

func attr(d *Drive, key string) *DriveAttr {
	for i := range d.Attrs {
		if d.Attrs[i].Key == key {
			return &d.Attrs[i]
		}
	}
	return nil
}

func TestParseHDD(t *testing.T) {
	d := parseDrive(loadSmart(t, "smart-hdd.json"), smartDev{name: "/dev/sda", typ: "sat"})
	if d == nil {
		t.Fatal("no drive")
	}
	if d.Key != "ztest001" || d.Model != "ST12000VN0008-3MH101" || d.Family != "Seagate IronWolf" || d.Protocol != "ATA" || d.RPM != 7200 {
		t.Fatalf("identity: %+v", d)
	}
	if d.Status != "passed" || len(d.Problems)+len(d.Warnings) != 0 {
		t.Fatalf("status %s %v %v", d.Status, d.Problems, d.Warnings)
	}
	if d.Temp != 33 || d.TempLimit != 55 || d.PowerOnHours != 2290 || d.PowerCycles != 16 {
		t.Fatalf("temp %g limit %g hours %d cycles %d", d.Temp, d.TempLimit, d.PowerOnHours, d.PowerCycles)
	}
	if d.Written != 35921223080*512 || d.Wear != nil {
		t.Fatalf("written %d wear %v", d.Written, d.Wear)
	}
	if a := attr(d, "194"); a == nil || a.Raw != 33 {
		t.Fatalf("temperature attribute keeps only its low byte: %+v", a)
	}
	if len(d.SelfTests) == 0 || !d.SelfTests[0].Passed || d.SelfTests[0].Type != "Extended offline" {
		t.Fatalf("self tests: %+v", d.SelfTests)
	}
	if !strings.HasPrefix(d.Interface, "SATA 3.3") || d.WWN == "" {
		t.Fatalf("interface %q wwn %q", d.Interface, d.WWN)
	}
}

func TestParseSATASSD(t *testing.T) {
	d := parseDrive(loadSmart(t, "smart-ssd.json"), smartDev{name: "/dev/sdb", typ: "sat"})
	if d == nil || d.RPM != 0 || d.Status != "passed" {
		t.Fatalf("drive %+v", d)
	}
	if d.Wear == nil || *d.Wear != 0 {
		t.Fatalf("wear from Wear_Leveling_Count: %v", d.Wear)
	}
	if d.TempLimit != 70 {
		t.Fatalf("temp limit %g", d.TempLimit)
	}
}

func TestParseNVMe(t *testing.T) {
	d := parseDrive(loadSmart(t, "smart-nvme.json"), smartDev{name: "/dev/nvme0", typ: "nvme"})
	if d == nil || d.Protocol != "NVMe" || d.Key != "testnvme0001" {
		t.Fatalf("drive %+v", d)
	}
	if d.Wear == nil || *d.Wear != 7 || d.Spare == nil || *d.Spare != 100 || d.SpareThreshold != 5 {
		t.Fatalf("wear %v spare %v/%g", d.Wear, d.Spare, d.SpareThreshold)
	}
	if d.TempLimit != 79 || d.Temp == 0 || d.Written == 0 || d.UnsafeShutdowns == 0 {
		t.Fatalf("limit %g temp %g written %d unsafe %d", d.TempLimit, d.Temp, d.Written, d.UnsafeShutdowns)
	}
	if d.Status != "passed" {
		t.Fatalf("status %s %v %v", d.Status, d.Problems, d.Warnings)
	}
	if len(d.SelfTests) != 1 || !d.SelfTests[0].Passed {
		t.Fatalf("self tests %+v", d.SelfTests)
	}
}

func TestAssess(t *testing.T) {
	no := false
	bad := 3
	thr := 10
	one := 1
	cases := []struct {
		name   string
		d      Drive
		status string
		say    string
	}{
		{"smart failed", Drive{SmartPassed: &no}, "failed", "SMART health check"},
		{"pending sectors", Drive{Attrs: []DriveAttr{{ID: 197, Name: "Current Pending Sector", Raw: 8, Status: "warn"}}}, "warning", "Current Pending Sector: 8"},
		{"under threshold", Drive{Attrs: []DriveAttr{{ID: 5, Name: "Reallocated Sector Ct", Value: &bad, Thresh: &thr, Failed: "now", Status: "fail"}}}, "failed", "failure threshold"},
		{"spare gone", Drive{Spare: ptr(4), SpareThreshold: 5}, "failed", "spare space"},
		{"critical warning", Drive{CriticalWarning: 0x04}, "failed", "reliability degraded"},
		{"too hot", Drive{Temp: 61, TempLimit: 55}, "warning", "running hot"},
		{"worn", Drive{Wear: ptr(95)}, "warning", "endurance"},
		{"media errors", Drive{MediaErrors: 2}, "warning", "media errors"},
		{"self-test failed", Drive{SelfTests: []SelfTest{{Type: "Short", Result: "Completed: read failure", Failed: true}, {Passed: true}}}, "failed", "self-test"},
		{"old failed self-test", Drive{SelfTests: []SelfTest{{Passed: true}, {Failed: true}}}, "passed", ""},
		{"running self-test", Drive{SelfTests: []SelfTest{{Running: true}, {Passed: true}}}, "passed", ""},
		{"fine", Drive{Attrs: []DriveAttr{{ID: 1, Value: &one, Thresh: new(int)}}}, "passed", ""},
	}
	for _, c := range cases {
		d := c.d
		Assess(&d)
		if d.Status != c.status {
			t.Errorf("%s: status %s, want %s (%v %v)", c.name, d.Status, c.status, d.Problems, d.Warnings)
		}
		all := strings.Join(append(d.Problems, d.Warnings...), "; ")
		if c.say != "" && !strings.Contains(all, c.say) {
			t.Errorf("%s: %q doesn't mention %q", c.name, all, c.say)
		}
	}
}

func ptr(f float64) *float64 { return &f }

func TestDriveKey(t *testing.T) {
	for in, want := range map[string]string{"ZZ00H58N": "zz00h58n", " S4X 9/A ": "s4x-9-a", "BTHP01340UXD1P0D": "bthp01340uxd1p0d"} {
		if got := DriveKey(in); got != want {
			t.Errorf("DriveKey(%q) = %q, want %q", in, got, want)
		}
	}
}

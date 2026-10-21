package collect

import "testing"

func TestExitCode(t *testing.T) {
	for in, want := range map[string]int{"Exited (137) 3 hours ago": 137, "Exited (0) 2 days ago": 0, "Up 4 days": -1} {
		if got := ExitCode(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

func TestContainerID(t *testing.T) {
	id := "719a09cd555e241beed837798d40a160a165c0721cb254b994a8d1716d0466f9"
	if got := containerID("0::/system.slice/docker-" + id + ".scope\n"); got != id {
		t.Fatalf("got %q", got)
	}
	if containerID("0::/user.slice/user-1000.slice/session-3.scope") != "" {
		t.Fatal("no container expected")
	}
}

func TestPartition(t *testing.T) {
	for in, want := range map[string]bool{"nvme0n1": false, "nvme0n1p2": true, "sda": false, "sda1": true, "mmcblk0p1": true, "md0": false} {
		if isPartition(in) != want {
			t.Errorf("%s: want %v", in, want)
		}
	}
}

func TestGlancesUptime(t *testing.T) {
	if got := parseGlancesUptime("4 days, 3:12:05"); got != 4*86400+3*3600+12*60+5 {
		t.Fatalf("got %d", got)
	}
}

func TestParseLogStream(t *testing.T) {
	frame := func(kind byte, s string) []byte {
		b := []byte{kind, 0, 0, 0, 0, 0, 0, byte(len(s))}
		return append(b, s...)
	}
	var b []byte
	b = append(b, frame(1, "2026-10-21T15:00:00.000000001Z hello\n2026-10-21T15:00:00.0000")...)
	b = append(b, frame(1, "00002Z split line\n")...)
	b = append(b, frame(2, "2026-10-21T15:00:01.000000000Z oops\n")...)
	got := parseLogStream(b, "web")
	if len(got) != 3 || got[1].Msg != "split line" || got[2].Stream != "err" || got[0].T != "2026-10-21T15:00:00.000000001Z" || got[2].Container != "web" {
		t.Fatalf("got %+v", got)
	}
	raw := parseLogStream([]byte("2026-10-21T15:00:00Z tty line\r\n"), "tty")
	if len(raw) != 1 || raw[0].Msg != "tty line" {
		t.Fatalf("raw: %+v", raw)
	}
}

func TestLogQueryRejectsPaths(t *testing.T) {
	for _, c := range []string{"../etc", "a/b", "x?y=1"} {
		q := LogQuery{Container: c}
		if q.Clamp() == nil {
			t.Errorf("%q accepted", c)
		}
	}
}

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

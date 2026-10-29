package speedtest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI writes a script that prints body (or fails with stderr) like the Ookla CLI.
func fakeCLI(t *testing.T, stdout, stderr string, code int) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "out.jsonl")
	errf := filepath.Join(dir, "err.txt")
	_ = os.WriteFile(out, []byte(stdout), 0o644)
	_ = os.WriteFile(errf, []byte(stderr), 0o644)
	bin := filepath.Join(dir, "speedtest")
	script := "#!/bin/sh\ncat " + out + "\ncat " + errf + " >&2\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestRunParsesProgressAndResult(t *testing.T) {
	body, err := os.ReadFile("testdata/run.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	r := Runner{Bin: fakeCLI(t, "Speedtest by Ookla\n\n"+string(body), "", 0)}
	if !r.Available() {
		t.Fatal("fake CLI not found")
	}
	var phases []string
	res, err := r.Run(context.Background(), 1234, func(p Progress) { phases = append(phases, p.Phase) })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(phases, ","); got != "connecting,ping,ping,download,download,upload,upload" {
		t.Fatalf("phases = %s", got)
	}
	if res.DownMbps < 150 || res.DownMbps > 155 || res.UpMbps < 75 || res.UpMbps > 78 {
		t.Fatalf("speeds = %v / %v", res.DownMbps, res.UpMbps)
	}
	if res.PingMs <= 0 || res.UpLatencyMs <= res.PingMs || res.LossPct != -1 {
		t.Fatalf("latency = %+v", res)
	}
	if res.Server.ID != 1234 || res.Server.Name != "Example Net" || res.ISP != "Example ISP" || res.ExternalIP != "198.51.100.7" || res.ResultURL == "" {
		t.Fatalf("details = %+v", res)
	}
	if res.Time.IsZero() || res.DurationMs < 20000 {
		t.Fatalf("time = %v, duration %d", res.Time, res.DurationMs)
	}
}

func TestRunReportsCLIError(t *testing.T) {
	r := Runner{Bin: fakeCLI(t, "", `{"type":"log","timestamp":"2026-10-29T20:00:00Z","message":"Cannot read from socket: Connection reset by peer","level":"error"}`+"\n", 2)}
	_, err := r.Run(context.Background(), 0, nil)
	if err == nil || !strings.Contains(err.Error(), "Cannot read from socket") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseResultRejectsOtherLines(t *testing.T) {
	if _, err := ParseResult([]byte(`{"type":"log","message":"x"}`)); err == nil {
		t.Fatal("a log line is not a result")
	}
}

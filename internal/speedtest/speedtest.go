// Package speedtest runs the Ookla Speedtest CLI and reads its JSON lines:
// progress while the test runs (ping, download, upload) and the result.
package speedtest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runner runs the CLI found at Bin (a name on PATH or a path).
type Runner struct {
	Bin  string
	Home string // where the CLI keeps its license acceptance ("" = $HOME)
}

// Available reports whether the CLI can be found.
func (r Runner) Available() bool {
	_, err := exec.LookPath(r.bin())
	return err == nil
}

func (r Runner) bin() string {
	if r.Bin == "" {
		return "speedtest"
	}
	return r.Bin
}

// Server is one Ookla test server.
type Server struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Country  string `json:"country"`
	Host     string `json:"host"`
}

// Result is one finished test. Speeds are megabits per second, times milliseconds.
type Result struct {
	Time          time.Time `json:"time"`
	DownMbps      float64   `json:"down_mbps"`
	UpMbps        float64   `json:"up_mbps"`
	PingMs        float64   `json:"ping_ms"`
	JitterMs      float64   `json:"jitter_ms"`
	LossPct       float64   `json:"loss_pct"` // -1 = not measured
	DownLatencyMs float64   `json:"down_latency_ms"`
	UpLatencyMs   float64   `json:"up_latency_ms"`
	DownBytes     int64     `json:"down_bytes"`
	UpBytes       int64     `json:"up_bytes"`
	DurationMs    int64     `json:"duration_ms"`
	ISP           string    `json:"isp"`
	ExternalIP    string    `json:"external_ip"`
	VPN           bool      `json:"vpn"`
	Server        Server    `json:"server"`
	ResultURL     string    `json:"result_url"`
}

// Progress is what the test is doing right now.
type Progress struct {
	Phase    string  `json:"phase"`    // connecting | ping | download | upload
	Progress float64 `json:"progress"` // 0…1 within the phase
	Mbps     float64 `json:"mbps"`     // current speed (download/upload)
	PingMs   float64 `json:"ping_ms"`
	Server   *Server `json:"server,omitempty"`
	ISP      string  `json:"isp,omitempty"`
}

// ---------- the JSON the CLI writes ----------

type wireServer struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Country  string `json:"country"`
	Host     string `json:"host"`
}

type wireLatency struct {
	IQM    float64 `json:"iqm"`
	Low    float64 `json:"low"`
	High   float64 `json:"high"`
	Jitter float64 `json:"jitter"`
}

type wireTransfer struct {
	Bandwidth float64      `json:"bandwidth"` // bytes per second
	Bytes     int64        `json:"bytes"`
	Elapsed   int64        `json:"elapsed"`
	Progress  float64      `json:"progress"`
	Latency   *wireLatency `json:"latency"`
}

type wireLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Ping      *struct {
		Jitter   float64 `json:"jitter"`
		Latency  float64 `json:"latency"`
		Progress float64 `json:"progress"`
	} `json:"ping"`
	Download   *wireTransfer `json:"download"`
	Upload     *wireTransfer `json:"upload"`
	PacketLoss *float64      `json:"packetLoss"`
	ISP        string        `json:"isp"`
	Interface  *struct {
		ExternalIP string `json:"externalIp"`
		IsVPN      bool   `json:"isVpn"`
	} `json:"interface"`
	Server  *wireServer  `json:"server"`
	Servers []wireServer `json:"servers"`
	Result  *struct {
		URL string `json:"url"`
	} `json:"result"`
}

func mbps(bytesPerSec float64) float64 { return round2(bytesPerSec * 8 / 1e6) }

func round2(v float64) float64 {
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 2, 64), 64)
	return f
}

func (w wireServer) server() Server {
	return Server{ID: w.ID, Name: w.Name, Location: w.Location, Country: w.Country, Host: w.Host}
}

// ParseResult reads a "result" line (also the data Speedtest Tracker stored).
func ParseResult(b []byte) (*Result, error) {
	var l wireLine
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	return l.result()
}

func (l wireLine) result() (*Result, error) {
	if l.Type != "result" || l.Download == nil || l.Upload == nil || l.Ping == nil {
		return nil, errors.New("not a speed test result")
	}
	r := &Result{
		DownMbps: mbps(l.Download.Bandwidth), UpMbps: mbps(l.Upload.Bandwidth),
		PingMs: round2(l.Ping.Latency), JitterMs: round2(l.Ping.Jitter), LossPct: -1,
		DownBytes: l.Download.Bytes, UpBytes: l.Upload.Bytes, DurationMs: l.Download.Elapsed + l.Upload.Elapsed,
		ISP: l.ISP,
	}
	if t, err := time.Parse(time.RFC3339, l.Timestamp); err == nil {
		r.Time = t
	}
	if l.Download.Latency != nil {
		r.DownLatencyMs = round2(l.Download.Latency.IQM)
	}
	if l.Upload.Latency != nil {
		r.UpLatencyMs = round2(l.Upload.Latency.IQM)
	}
	if l.PacketLoss != nil {
		r.LossPct = round2(*l.PacketLoss)
	}
	if l.Interface != nil {
		r.ExternalIP, r.VPN = l.Interface.ExternalIP, l.Interface.IsVPN
	}
	if l.Server != nil {
		r.Server = l.Server.server()
	}
	if l.Result != nil {
		r.ResultURL = l.Result.URL
	}
	return r, nil
}

// progress turns a progress line into what the test is doing (false = not one).
func (l wireLine) progress() (Progress, bool) {
	switch l.Type {
	case "testStart":
		p := Progress{Phase: "connecting", ISP: l.ISP}
		if l.Server != nil {
			s := l.Server.server()
			p.Server = &s
		}
		return p, true
	case "ping":
		if l.Ping != nil {
			return Progress{Phase: "ping", Progress: l.Ping.Progress, PingMs: round2(l.Ping.Latency)}, true
		}
	case "download":
		if l.Download != nil {
			return Progress{Phase: "download", Progress: l.Download.Progress, Mbps: mbps(l.Download.Bandwidth)}, true
		}
	case "upload":
		if l.Upload != nil {
			return Progress{Phase: "upload", Progress: l.Upload.Progress, Mbps: mbps(l.Upload.Bandwidth)}, true
		}
	}
	return Progress{}, false
}

// ---------- running it ----------

func (r Runner) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.bin(), append([]string{"--accept-license", "--accept-gdpr"}, args...)...)
	if r.Home != "" {
		_ = os.MkdirAll(r.Home, 0o700)
		cmd.Env = append(os.Environ(), "HOME="+r.Home)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// Run tests the connection against serverID (0 = the CLI picks the nearest),
// calling progress for every step it reports.
func (r Runner) Run(ctx context.Context, serverID int, progress func(Progress)) (*Result, error) {
	args := []string{"-f", "jsonl", "--progress=yes"}
	if serverID > 0 {
		args = append(args, "-s", strconv.Itoa(serverID))
	}
	cmd := r.command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the speedtest CLI: %w", err)
	}
	var res *Result
	var lastErr string
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var l wireLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		if l.Type == "log" && l.Level == "error" {
			lastErr = l.Message
			continue
		}
		if l.Type == "result" {
			if x, err := l.result(); err == nil {
				res = x
			}
			continue
		}
		if p, ok := l.progress(); ok && progress != nil {
			progress(p)
		}
	}
	werr := cmd.Wait()
	if res != nil {
		if res.Time.IsZero() {
			res.Time = start
		}
		return res, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if lastErr == "" {
		lastErr = cliError(stderr.Bytes())
	}
	if lastErr == "" && werr != nil {
		lastErr = werr.Error()
	}
	if lastErr == "" {
		lastErr = "the test ended without a result"
	}
	return nil, errors.New(lastErr)
}

// cliError finds the message in what the CLI wrote to stderr (JSON log lines or text).
func cliError(b []byte) string {
	msg := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var l wireLine
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &l) == nil {
			if l.Message != "" {
				msg = l.Message
			}
			continue
		}
		if strings.Contains(strings.ToLower(line), "error") {
			msg = strings.TrimPrefix(strings.TrimPrefix(line, "[error]"), "error:")
		}
	}
	return strings.TrimSpace(msg)
}

// Servers lists the test servers near this machine.
func (r Runner) Servers(ctx context.Context) ([]Server, error) {
	cmd := r.command(ctx, "-L", "-f", "json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		if m := cliError(stderr.Bytes()); m != "" {
			return nil, errors.New(m)
		}
		return nil, err
	}
	var l wireLine
	if err := json.Unmarshal(bytes.TrimSpace(b), &l); err != nil {
		return nil, fmt.Errorf("read the server list: %w", err)
	}
	out := make([]Server, 0, len(l.Servers))
	for _, s := range l.Servers {
		out = append(out, s.server())
	}
	return out, nil
}

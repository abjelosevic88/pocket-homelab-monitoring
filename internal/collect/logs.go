package collect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogLine is one line a container wrote.
type LogLine struct {
	T         string `json:"t"` // RFC3339Nano from the engine
	Container string `json:"c"` // container name
	Stream    string `json:"s"` // out | err
	Msg       string `json:"m"`
}

// Logs is what a log request returns.
type Logs struct {
	Lines     []LogLine `json:"lines"`
	Truncated bool      `json:"truncated"` // more lines matched than were returned
	Now       string    `json:"now"`       // the source's clock: follow from here when nothing came back
}

// LogQuery asks for the last Tail lines, or (with Since) everything after a
// moment; Container "" means every container, merged by time.
type LogQuery struct {
	Container string
	Tail      int
	Since     string // RFC3339Nano of the last line the caller has
}

const (
	maxLogLines = 5000
	maxLineLen  = 16 << 10
)

var validRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// ErrNoLogs means the source cannot serve logs at all.
var ErrNoLogs = errors.New("container logs are not available here")

// Clamp keeps a query in bounds.
func (q *LogQuery) Clamp() error {
	if q.Container != "" && !validRef.MatchString(q.Container) {
		return errors.New("that is not a container name or id")
	}
	if q.Tail <= 0 {
		q.Tail = 500
	}
	if q.Tail > maxLogLines {
		q.Tail = maxLogLines
	}
	if q.Since != "" {
		if _, err := time.Parse(time.RFC3339Nano, q.Since); err != nil {
			return errors.New("since is a timestamp like 2026-10-21T15:00:00.123456789Z")
		}
	}
	return nil
}

// Logs reads one container's log, or every container's merged.
func (d *Docker) Logs(ctx context.Context, q LogQuery) (*Logs, error) {
	if d.Missing() {
		return nil, errors.New("no container engine on this machine")
	}
	if err := q.Clamp(); err != nil {
		return nil, err
	}
	if q.Container != "" {
		name := q.Container
		for _, c := range d.snapshotList() {
			if c.ID == q.Container || c.Name == q.Container || strings.HasPrefix(c.ID, q.Container) {
				name = c.Name
				break
			}
		}
		lines, err := d.containerLogs(ctx, q.Container, name, q.Tail, q.Since)
		if err != nil {
			return nil, err
		}
		return capLines(lines, q.Tail), nil
	}
	// Everything: each running container (and ones that stopped in the last
	// hour) gives its share, merged by time.
	list := d.snapshotList()
	if len(list) == 0 {
		var items []dockerListItem
		if err := d.get(ctx, "/containers/json", &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			name := it.ID[:12]
			if len(it.Names) > 0 {
				name = strings.TrimPrefix(it.Names[0], "/")
			}
			list = append(list, Container{ID: it.ID, Name: name, State: it.State})
		}
	}
	per := q.Tail / 4
	if per < 30 {
		per = 30
	}
	var mu sync.Mutex
	var all []LogLine
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, c := range list {
		if c.State != "running" && c.State != "restarting" {
			continue
		}
		wg.Add(1)
		go func(c Container) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			lines, err := d.containerLogs(ctx, c.ID, c.Name, per, q.Since)
			if err != nil {
				return
			}
			mu.Lock()
			all = append(all, lines...)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	sort.SliceStable(all, func(i, j int) bool { return all[i].T < all[j].T })
	return capLines(all, q.Tail), nil
}

func (d *Docker) snapshotList() []Container {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Container, len(d.list))
	copy(out, d.list)
	return out
}

func capLines(lines []LogLine, n int) *Logs {
	out := &Logs{Lines: lines, Now: time.Now().UTC().Format(time.RFC3339Nano)}
	if len(lines) > n {
		out.Lines = lines[len(lines)-n:]
		out.Truncated = true
	}
	if out.Lines == nil {
		out.Lines = []LogLine{}
	}
	return out
}

func (d *Docker) containerLogs(ctx context.Context, id, name string, tail int, since string) ([]LogLine, error) {
	v := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {strconv.Itoa(tail)}}
	if since != "" {
		t, _ := time.Parse(time.RFC3339Nano, since)
		v.Set("since", fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond()))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/containers/"+url.PathEscape(id)+"/logs?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, errors.New("no such container")
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return nil, fmt.Errorf("docker logs: HTTP %d %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	lines := parseLogStream(body, name)
	if since != "" {
		// The engine's since is inclusive: drop what the caller already has.
		kept := lines[:0]
		for _, l := range lines {
			if l.T > since {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	return lines, nil
}

// parseLogStream reads Docker's multiplexed log format (8-byte frame
// headers) or, for containers with a TTY, the raw stream.
func parseLogStream(b []byte, name string) []LogLine {
	var out []LogLine
	add := func(stream string, chunk []byte) {
		sc := bufio.NewScanner(bytes.NewReader(chunk))
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			out = append(out, logLine(name, stream, sc.Text()))
		}
	}
	muxed := len(b) >= 8 && (b[0] == 1 || b[0] == 2) && b[1] == 0 && b[2] == 0 && b[3] == 0
	if !muxed {
		add("out", b)
		return out
	}
	// Frames may split a line; join per stream before splitting into lines.
	var outBuf, errBuf bytes.Buffer
	flush := func() {
		if outBuf.Len() > 0 {
			add("out", outBuf.Bytes())
			outBuf.Reset()
		}
		if errBuf.Len() > 0 {
			add("err", errBuf.Bytes())
			errBuf.Reset()
		}
	}
	last := byte(0)
	for len(b) >= 8 {
		kind := b[0]
		size := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		if size > len(b) {
			size = len(b)
		}
		if last != 0 && kind != last {
			flush() // keep stdout and stderr in the order they came
		}
		if kind == 2 {
			errBuf.Write(b[:size])
		} else {
			outBuf.Write(b[:size])
		}
		last = kind
		b = b[size:]
	}
	flush()
	return out
}

func logLine(name, stream, text string) LogLine {
	l := LogLine{Container: name, Stream: stream}
	if ts, msg, ok := strings.Cut(text, " "); ok && len(ts) >= 20 && ts[4] == '-' && ts[10] == 'T' {
		l.T, l.Msg = ts, msg
	} else {
		l.Msg = text
	}
	l.Msg = strings.TrimRight(l.Msg, "\r")
	if len(l.Msg) > maxLineLen {
		l.Msg = l.Msg[:maxLineLen] + " …"
	}
	return l
}

// AgentLogs asks a Pocket Monitoring agent for container logs.
func AgentLogs(ctx context.Context, baseURL, token string, q LogQuery) (*Logs, error) {
	v := url.Values{"tail": {strconv.Itoa(q.Tail)}}
	if q.Container != "" {
		v.Set("container", q.Container)
	}
	if q.Since != "" {
		v.Set("since", q.Since)
	}
	var out Logs
	err := getJSON(ctx, strings.TrimRight(baseURL, "/")+"/agent/v1/logs?"+v.Encode(), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	}, &out)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return nil, errors.New("this agent is too old to serve logs; update it")
	}
	if err != nil {
		return nil, err
	}
	if out.Lines == nil {
		out.Lines = []LogLine{}
	}
	return &out, nil
}

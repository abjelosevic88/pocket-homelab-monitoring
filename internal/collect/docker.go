package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Docker reads containers from the Docker (or Podman) API. Container stats
// are slow to fetch, so they are refreshed on their own loop (Run) and
// Collect only takes the latest list.
type Docker struct {
	client *http.Client
	base   string

	mu      sync.Mutex
	list    []Container
	at      time.Time
	err     error
	missing bool
	prev    map[string]ctrPrev
	inspect map[string]ctrInspect
}

type ctrPrev struct {
	cpu, sys       uint64
	rx, tx, rd, wr uint64
	at             time.Time
}

type ctrInspect struct {
	key      string
	started  int64
	restarts int
}

// NewDocker connects to host ("" = DOCKER_HOST or /var/run/docker.sock).
func NewDocker(host string) *Docker {
	d := &Docker{prev: map[string]ctrPrev{}, inspect: map[string]ctrInspect{}}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		if _, err := os.Stat(path); err != nil {
			d.missing = true
		}
		d.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dl net.Dialer
				return dl.DialContext(ctx, "unix", path)
			},
			MaxIdleConnsPerHost: 16,
		}}
		d.base = "http://docker"
	case strings.HasPrefix(host, "tcp://"):
		d.client = &http.Client{Timeout: 10 * time.Second}
		d.base = "http://" + strings.TrimPrefix(host, "tcp://")
	default:
		d.client = &http.Client{Timeout: 10 * time.Second}
		d.base = strings.TrimRight(host, "/")
	}
	return d
}

// Missing reports that there is no container engine to talk to.
func (d *Docker) Missing() bool { return d == nil || d.missing }

// Latest returns the last container list.
func (d *Docker) Latest() ([]Container, time.Time, error) {
	if d == nil {
		return []Container{}, time.Time{}, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Container, len(d.list))
	copy(out, d.list)
	return out, d.at, d.err
}

// Run refreshes the container list every interval until ctx ends.
func (d *Docker) Run(ctx context.Context, every time.Duration) {
	if d.Missing() {
		return
	}
	for {
		list, err := d.refresh(ctx)
		d.mu.Lock()
		if err == nil {
			d.list, d.at = list, time.Now().UTC()
		}
		d.err = err
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (d *Docker) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return err
	}
	res, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("docker %s: HTTP %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(v)
}

type dockerListItem struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

type dockerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		Rx uint64 `json:"rx_bytes"`
		Tx uint64 `json:"tx_bytes"`
	} `json:"networks"`
	Blkio struct {
		IO []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

func (d *Docker) refresh(ctx context.Context) ([]Container, error) {
	var items []dockerListItem
	if err := d.get(ctx, "/containers/json?all=1", &items); err != nil {
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			return nil, errors.New("no permission to read the Docker socket (add the container to the docker group)")
		}
		return nil, err
	}
	out := make([]Container, len(items))
	now := time.Now()
	type job struct{ i int }
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	next := map[string]ctrPrev{}
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				it := items[j.i]
				c := &out[j.i]
				d.mu.Lock()
				prev, hasPrev := d.prev[it.ID]
				ins, hasIns := d.inspect[it.ID]
				d.mu.Unlock()
				key := it.State
				if !hasIns || ins.key != key {
					var x struct {
						RestartCount int `json:"RestartCount"`
						State        struct {
							StartedAt string `json:"StartedAt"`
						} `json:"State"`
					}
					if d.get(ctx, "/containers/"+it.ID+"/json", &x) == nil {
						ins = ctrInspect{key: key, restarts: x.RestartCount}
						if t, err := time.Parse(time.RFC3339Nano, x.State.StartedAt); err == nil && t.Year() > 2000 {
							ins.started = t.Unix()
						}
						d.mu.Lock()
						d.inspect[it.ID] = ins
						d.mu.Unlock()
					}
				}
				c.Restarts = ins.restarts
				if it.State == "running" {
					c.Started = ins.started
				}
				if it.State != "running" {
					continue
				}
				var st dockerStats
				if err := d.get(ctx, "/containers/"+it.ID+"/stats?stream=false&one-shot=true", &st); err != nil {
					continue
				}
				cur := ctrPrev{cpu: st.CPUStats.CPUUsage.TotalUsage, sys: st.CPUStats.SystemUsage, at: now}
				for _, n := range st.Networks {
					cur.rx += n.Rx
					cur.tx += n.Tx
				}
				for _, io := range st.Blkio.IO {
					switch strings.ToLower(io.Op) {
					case "read":
						cur.rd += io.Value
					case "write":
						cur.wr += io.Value
					}
				}
				mem := st.MemoryStats.Usage
				if v, ok := st.MemoryStats.Stats["inactive_file"]; ok && v < mem {
					mem -= v
				} else if v, ok := st.MemoryStats.Stats["total_inactive_file"]; ok && v < mem {
					mem -= v
				}
				c.MemUsed, c.MemLimit = mem, st.MemoryStats.Limit
				if hasPrev {
					if dSys := sub(cur.sys, prev.sys); dSys > 0 {
						cpus := st.CPUStats.OnlineCPUs
						if cpus == 0 {
							cpus = 1
						}
						c.CPU = round1(float64(sub(cur.cpu, prev.cpu)) / float64(dSys) * float64(cpus) * 100)
					}
					if dt := now.Sub(prev.at).Seconds(); dt > 0 {
						c.RxBps = float64(int64(float64(sub(cur.rx, prev.rx)) / dt))
						c.TxBps = float64(int64(float64(sub(cur.tx, prev.tx)) / dt))
						c.ReadBps = float64(int64(float64(sub(cur.rd, prev.rd)) / dt))
						c.WriteBps = float64(int64(float64(sub(cur.wr, prev.wr)) / dt))
					}
				}
				mu.Lock()
				next[it.ID] = cur
				mu.Unlock()
			}
		}()
	}
	for i, it := range items {
		name := it.ID[:12]
		if len(it.Names) > 0 {
			name = strings.TrimPrefix(it.Names[0], "/")
		}
		c := Container{ID: it.ID, Name: name, Image: it.Image, State: it.State, Status: it.Status, Project: it.Labels["com.docker.compose.project"]}
		switch {
		case strings.Contains(it.Status, "(unhealthy)"):
			c.Health = "unhealthy"
		case strings.Contains(it.Status, "(healthy)"):
			c.Health = "healthy"
		case strings.Contains(it.Status, "(health: starting)"):
			c.Health = "starting"
		}
		var ports []string
		seen := map[string]bool{}
		for _, p := range it.Ports {
			if p.PublicPort == 0 {
				continue
			}
			s := strconv.Itoa(p.PublicPort) + "→" + strconv.Itoa(p.PrivatePort)
			if p.Type != "tcp" {
				s += "/" + p.Type
			}
			if !seen[s] {
				seen[s] = true
				ports = append(ports, s)
			}
		}
		c.Ports = strings.Join(ports, ", ")
		out[i] = c
		jobs <- job{i}
	}
	close(jobs)
	wg.Wait()
	d.mu.Lock()
	d.prev = next
	for id := range d.inspect {
		found := false
		for _, it := range items {
			if it.ID == id {
				found = true
				break
			}
		}
		if !found {
			delete(d.inspect, id)
		}
	}
	d.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ExitCode reads N from a status like "Exited (137) 3 hours ago" (-1 when not exited).
func ExitCode(status string) int {
	if !strings.HasPrefix(status, "Exited (") {
		return -1
	}
	rest := strings.TrimPrefix(status, "Exited (")
	n, err := strconv.Atoi(rest[:strings.IndexByte(rest+")", ')')])
	if err != nil {
		return -1
	}
	return n
}

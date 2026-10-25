package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

// Remote reads jobs from an agent or the jobs helper over HTTP.
type Remote struct {
	URL   string
	Token string
}

var client = &http.Client{Timeout: 45 * time.Second}

// ErrNoJobs means the other side doesn't serve jobs (an older agent, or jobs turned off there).
var ErrNoJobs = errors.New("this agent doesn't report scheduled jobs (update it, or JOBS_SCOPE is off there)")

func (r Remote) do(ctx context.Context, method, path string, q neturl.Values, v any) error {
	u := strings.TrimRight(r.URL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	res, err := client.Do(req)
	if err != nil {
		var ue *neturl.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound && path == "/agent/v1/jobs":
		return ErrNoJobs
	case res.StatusCode == http.StatusUnauthorized:
		return errors.New("the jobs reader refused the token (HTTP 401)")
	case res.StatusCode != http.StatusOK:
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2000))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(res.Body, 128<<20)).Decode(v)
}

// Status reads the jobs and the runs that ended after since (unix ms).
func (r Remote) Status(ctx context.Context, since int64) (*Status, error) {
	var st Status
	q := neturl.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if err := r.do(ctx, http.MethodGet, "/agent/v1/jobs", q, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Log reads a job's journal lines.
func (r Remote) Log(ctx context.Context, unit, invocation string, tail int) ([]LogLine, error) {
	var out struct {
		Lines []LogLine `json:"lines"`
	}
	q := neturl.Values{"unit": {unit}, "tail": {strconv.Itoa(tail)}}
	if invocation != "" {
		q.Set("invocation", invocation)
	}
	if err := r.do(ctx, http.MethodGet, "/agent/v1/jobs/log", q, &out); err != nil {
		return nil, err
	}
	if out.Lines == nil {
		out.Lines = []LogLine{}
	}
	return out.Lines, nil
}

// Act runs, pauses or resumes a job.
func (r Remote) Act(ctx context.Context, unit, action string) (*Job, error) {
	var j Job
	if err := r.do(ctx, http.MethodPost, "/agent/v1/jobs/action", neturl.Values{"unit": {unit}, "action": {action}}, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

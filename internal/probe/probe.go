// Package probe runs one uptime check: an HTTP(S) request (status code and an
// optional keyword), a TCP connect or a DNS lookup, and says how it went.
package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Spec is what to check.
type Spec struct {
	Kind            string // http | tcp | dns
	Target          string
	Port            int
	Method          string
	Keyword         string
	InvertKeyword   bool
	StatusCodes     string // "200-299", "200-299,301", …
	IgnoreTLS       bool
	FollowRedirects bool
	DNSServer       string
	DNSType         string
	Timeout         time.Duration
	UserAgent       string
}

// Result is how it went.
type Result struct {
	OK          bool
	Ms          float64
	Msg         string
	CertExpires time.Time // https: the server certificate's expiry
}

// Run performs the check once.
func Run(ctx context.Context, sp Spec) Result {
	if sp.Timeout <= 0 {
		sp.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, sp.Timeout)
	defer cancel()
	switch sp.Kind {
	case "http":
		return runHTTP(ctx, sp)
	case "tcp":
		return runTCP(ctx, sp)
	case "dns":
		return runDNS(ctx, sp)
	}
	return Result{Msg: "unknown check type " + sp.Kind}
}

// local maps host.docker.internal (what containers call the host) to the
// loopback, since the hub may run on the host network where it doesn't resolve.
func local(host string) string {
	if host == "host.docker.internal" {
		return "127.0.0.1"
	}
	return host
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ---------- http ----------

func runHTTP(ctx context.Context, sp Spec) Result {
	u, err := url.Parse(strings.TrimSpace(sp.Target))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Result{Msg: "not an http(s) URL"}
	}
	if h := local(u.Hostname()); h != u.Hostname() {
		if p := u.Port(); p != "" {
			u.Host = net.JoinHostPort(h, p)
		} else {
			u.Host = h
		}
	}
	tr := &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: sp.IgnoreTLS}, //nolint:gosec // the user's choice per check
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: sp.Timeout,
		ForceAttemptHTTP2:   true,
	}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr}
	if !sp.FollowRedirects {
		cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	} else {
		cl.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("more than 10 redirects")
			}
			return nil
		}
	}
	method := strings.ToUpper(sp.Method)
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return Result{Msg: err.Error()}
	}
	if sp.UserAgent != "" {
		req.Header.Set("User-Agent", sp.UserAgent)
	}
	req.Header.Set("Accept", "*/*")
	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		return Result{Ms: ms(time.Since(start)), Msg: cleanErr(err)}
	}
	defer resp.Body.Close()
	var body []byte
	if sp.Keyword != "" {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	r := Result{Ms: ms(time.Since(start))}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		r.CertExpires = resp.TLS.PeerCertificates[0].NotAfter
	}
	code := fmt.Sprintf("%d - %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	if !codeOK(resp.StatusCode, sp.StatusCodes) {
		r.Msg = code
		return r
	}
	if sp.Keyword != "" {
		found := bytes.Contains(body, []byte(sp.Keyword))
		if found == sp.InvertKeyword {
			if sp.InvertKeyword {
				r.Msg = fmt.Sprintf("%s, but the page contains “%s”", code, sp.Keyword)
			} else {
				r.Msg = fmt.Sprintf("%s, but “%s” isn't on the page", code, sp.Keyword)
			}
			return r
		}
		r.OK, r.Msg = true, code+", keyword "+map[bool]string{true: "absent", false: "found"}[sp.InvertKeyword]
		return r
	}
	r.OK, r.Msg = true, code
	return r
}

// codeOK matches a status code against "200-299,301,4xx"-style ranges.
func codeOK(code int, spec string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = "200-299"
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		switch {
		case part == "":
		case strings.HasSuffix(part, "xx") && len(part) == 3:
			if d, err := strconv.Atoi(part[:1]); err == nil && code/100 == d {
				return true
			}
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			lo, e1 := strconv.Atoi(strings.TrimSpace(a))
			hi, e2 := strconv.Atoi(strings.TrimSpace(b))
			if e1 == nil && e2 == nil && code >= lo && code <= hi {
				return true
			}
		default:
			if n, err := strconv.Atoi(part); err == nil && n == code {
				return true
			}
		}
	}
	return false
}

// ValidCodes reports whether a status code spec parses.
func ValidCodes(spec string) bool {
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		if strings.HasSuffix(part, "xx") && len(part) == 3 {
			if _, err := strconv.Atoi(part[:1]); err != nil {
				return false
			}
			continue
		}
		a, b, rng := strings.Cut(part, "-")
		if _, err := strconv.Atoi(strings.TrimSpace(a)); err != nil {
			return false
		}
		if rng {
			if _, err := strconv.Atoi(strings.TrimSpace(b)); err != nil {
				return false
			}
		}
	}
	return true
}

// cleanErr turns Go's long wrapped errors into what went wrong.
func cleanErr(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timed out"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		msg := oe.Err.Error()
		if strings.Contains(msg, "connection refused") {
			return "connection refused"
		}
		if strings.Contains(msg, "no route to host") {
			return "no route to host"
		}
		return msg
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		if de.IsNotFound {
			return "host not found: " + de.Name
		}
		return "DNS: " + de.Err
	}
	return err.Error()
}

// ---------- tcp ----------

func runTCP(ctx context.Context, sp Spec) Result {
	host := local(strings.TrimSpace(sp.Target))
	if host == "" || sp.Port <= 0 || sp.Port > 65535 {
		return Result{Msg: "needs a host and a port"}
	}
	var d net.Dialer
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(sp.Port)))
	if err != nil {
		return Result{Ms: ms(time.Since(start)), Msg: cleanErr(err)}
	}
	el := time.Since(start)
	c.Close()
	return Result{OK: true, Ms: ms(el), Msg: "port " + strconv.Itoa(sp.Port) + " open"}
}

// ---------- dns ----------

// DNSTypes are the record types a DNS check can ask for.
var DNSTypes = []string{"A", "AAAA", "CNAME", "MX", "NS", "TXT"}

func runDNS(ctx context.Context, sp Spec) Result {
	name := strings.TrimSpace(sp.Target)
	if name == "" {
		return Result{Msg: "needs a name to look up"}
	}
	r := net.DefaultResolver
	if srv := strings.TrimSpace(sp.DNSServer); srv != "" {
		port := sp.Port
		if port <= 0 {
			port = 53
		}
		addr := net.JoinHostPort(local(srv), strconv.Itoa(port))
		r = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
	}
	start := time.Now()
	var out []string
	var err error
	switch strings.ToUpper(sp.DNSType) {
	case "", "A", "AAAA":
		network := "ip4"
		if strings.ToUpper(sp.DNSType) == "AAAA" {
			network = "ip6"
		}
		var ips []net.IP
		ips, err = r.LookupIP(ctx, network, name)
		for _, ip := range ips {
			out = append(out, ip.String())
		}
	case "CNAME":
		var c string
		c, err = r.LookupCNAME(ctx, name)
		if c != "" {
			out = append(out, c)
		}
	case "MX":
		var mx []*net.MX
		mx, err = r.LookupMX(ctx, name)
		for _, m := range mx {
			out = append(out, fmt.Sprintf("%d %s", m.Pref, m.Host))
		}
	case "NS":
		var ns []*net.NS
		ns, err = r.LookupNS(ctx, name)
		for _, n := range ns {
			out = append(out, n.Host)
		}
	case "TXT":
		out, err = r.LookupTXT(ctx, name)
	default:
		return Result{Msg: "unsupported record type " + sp.DNSType}
	}
	el := time.Since(start)
	if err != nil {
		return Result{Ms: ms(el), Msg: cleanErr(err)}
	}
	if len(out) == 0 {
		return Result{Ms: ms(el), Msg: "no " + strings.ToUpper(sp.DNSType) + " records"}
	}
	msg := strings.Join(out, ", ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return Result{OK: true, Ms: ms(el), Msg: msg}
}

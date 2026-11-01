package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCodeOK(t *testing.T) {
	for _, c := range []struct {
		code int
		spec string
		want bool
	}{
		{200, "", true}, {204, "200-299", true}, {301, "200-299", false}, {301, "200-299,301", true},
		{404, "4xx", true}, {500, "4xx", false}, {418, " 400 - 499 ", true},
	} {
		if got := codeOK(c.code, c.spec); got != c.want {
			t.Errorf("codeOK(%d, %q) = %v", c.code, c.spec, got)
		}
	}
	if ValidCodes("2xx,abc") || !ValidCodes("200-299, 301, 4xx") {
		t.Error("ValidCodes")
	}
}

func TestHTTPAndTCP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("all good here"))
	}))
	defer srv.Close()
	ctx := context.Background()
	if r := Run(ctx, Spec{Kind: "http", Target: srv.URL, Keyword: "good", Timeout: 5 * time.Second}); !r.OK {
		t.Errorf("keyword found: %+v", r)
	}
	if r := Run(ctx, Spec{Kind: "http", Target: srv.URL, Keyword: "good", InvertKeyword: true, Timeout: 5 * time.Second}); r.OK {
		t.Errorf("inverted keyword: %+v", r)
	}
	if r := Run(ctx, Spec{Kind: "http", Target: srv.URL + "/moved", FollowRedirects: false, Timeout: 5 * time.Second}); r.OK || !strings.HasPrefix(r.Msg, "302") {
		t.Errorf("redirect not followed: %+v", r)
	}
	if r := Run(ctx, Spec{Kind: "http", Target: srv.URL + "/moved", FollowRedirects: true, Timeout: 5 * time.Second}); !r.OK {
		t.Errorf("redirect followed: %+v", r)
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	p, _ := strconv.Atoi(port)
	if r := Run(ctx, Spec{Kind: "tcp", Target: "127.0.0.1", Port: p, Timeout: 5 * time.Second}); !r.OK {
		t.Errorf("tcp open: %+v", r)
	}
	srv.Close()
	if r := Run(ctx, Spec{Kind: "tcp", Target: "127.0.0.1", Port: p, Timeout: 2 * time.Second}); r.OK || r.Msg != "connection refused" {
		t.Errorf("tcp closed: %+v", r)
	}
}

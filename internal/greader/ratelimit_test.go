package greader

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoginLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	for i := range maxLoginFailures {
		if _, blocked := l.blocked("a"); blocked {
			t.Fatalf("blocked after %d failures", i)
		}
		l.fail("a")
	}
	wait, blocked := l.blocked("a")
	if !blocked || wait != loginFailureWindow {
		t.Fatalf("after %d failures: blocked=%v wait=%v", maxLoginFailures, blocked, wait)
	}
	if _, blocked := l.blocked("b"); blocked {
		t.Error("other client is blocked too")
	}

	now = now.Add(loginFailureWindow)
	if _, blocked := l.blocked("a"); blocked {
		t.Error("still blocked after the window")
	}

	// Failures spread over more than one window do not add up.
	for range maxLoginFailures - 1 {
		l.fail("c")
	}
	now = now.Add(loginFailureWindow)
	l.fail("c")
	if _, blocked := l.blocked("c"); blocked {
		t.Error("failures from an expired window counted")
	}

	// A successful login resets the count.
	for range maxLoginFailures - 1 {
		l.fail("d")
	}
	l.succeed("d")
	l.fail("d")
	if _, blocked := l.blocked("d"); blocked {
		t.Error("failures before a successful login counted")
	}
}

func TestLoginLimiterPrunesExpiredClients(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	for i := range pruneThreshold {
		l.fail(fmt.Sprint(i))
	}
	now = now.Add(loginFailureWindow)
	l.fail("new")
	if len(l.clients) != 1 {
		t.Errorf("%d clients tracked after pruning, want 1", len(l.clients))
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remote     string
		xff        []string
		trustProxy bool
		want       string
	}{
		{name: "remote address", remote: "203.0.113.7:51234", want: "203.0.113.7"},
		{name: "IPv6 remote address", remote: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "header ignored without trust", remote: "10.0.0.2:1", xff: []string{"203.0.113.7"}, want: "10.0.0.2"},
		{name: "proxy header", remote: "10.0.0.2:1", xff: []string{"203.0.113.7"}, trustProxy: true, want: "203.0.113.7"},
		{
			name:   "spoofed entries before the proxy's are ignored",
			remote: "10.0.0.2:1", xff: []string{"1.2.3.4, 203.0.113.7"}, trustProxy: true, want: "203.0.113.7",
		},
		{
			name:   "last of several headers",
			remote: "10.0.0.2:1", xff: []string{"1.2.3.4", "203.0.113.7"}, trustProxy: true, want: "203.0.113.7",
		},
		{name: "invalid header", remote: "10.0.0.2:1", xff: []string{"garbage"}, trustProxy: true, want: "10.0.0.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientIP(r, tt.trustProxy); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientLoginIsRateLimited(t *testing.T) {
	api, _ := newTestServer(t)
	c := &client{t: t, base: api.URL}
	login := func(password string) (int, http.Header) {
		status, header, _ := c.do(http.MethodPost, "/accounts/ClientLogin",
			url.Values{"Email": {"dennis"}, "Passwd": {password}})
		return status, header
	}

	for range maxLoginFailures {
		if status, _ := login("wrong"); status != http.StatusUnauthorized {
			t.Fatalf("wrong password: status %d", status)
		}
	}
	status, header := login("secret")
	if status != http.StatusTooManyRequests || header.Get("Retry-After") == "" {
		t.Fatalf("after %d failures: status %d, Retry-After %q", maxLoginFailures, status, header.Get("Retry-After"))
	}
}

func TestHealth(t *testing.T) {
	api, _ := newTestServer(t)
	c := &client{t: t, base: api.URL}
	if body := c.ok(http.MethodGet, "/healthz", nil); strings.TrimSpace(body) != "ok" {
		t.Errorf("health = %q", body)
	}
}

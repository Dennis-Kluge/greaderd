package greader

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Failed logins allowed per client before it has to wait for the window to end.
const (
	maxLoginFailures   = 5
	loginFailureWindow = 15 * time.Minute

	// pruneThreshold bounds memory: beyond this many tracked clients,
	// expired entries are dropped on the next failure.
	pruneThreshold = 1024
)

// loginLimiter counts failed logins per client. After maxFailures within
// window, the client is blocked until the window has passed.
//
// Locking out a client cannot disrupt syncing: clients keep their auth token
// and only log in again when it stops working.
type loginLimiter struct {
	maxFailures int
	window      time.Duration
	now         func() time.Time

	mu      sync.Mutex
	clients map[string]*loginFailures
}

type loginFailures struct {
	count int
	since time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{
		maxFailures: maxLoginFailures,
		window:      loginFailureWindow,
		now:         time.Now,
		clients:     map[string]*loginFailures{},
	}
}

// blocked reports whether client must wait, and for how long.
func (l *loginLimiter) blocked(client string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.clients[client]
	if !ok || f.count < l.maxFailures {
		return 0, false
	}
	wait := f.since.Add(l.window).Sub(l.now())
	if wait <= 0 {
		delete(l.clients, client)
		return 0, false
	}
	return wait, true
}

// fail records a failed login of client.
func (l *loginLimiter) fail(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.clients) >= pruneThreshold {
		for c, f := range l.clients {
			if now.Sub(f.since) >= l.window {
				delete(l.clients, c)
			}
		}
	}
	f, ok := l.clients[client]
	if !ok || now.Sub(f.since) >= l.window {
		f = &loginFailures{since: now}
		l.clients[client] = f
	}
	f.count++
}

// succeed forgets earlier failures of client.
func (l *loginLimiter) succeed(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, client)
}

// clientIP returns the address of the client that sent r. Behind a reverse
// proxy, every request comes from the proxy; with trustProxy set, the last
// X-Forwarded-For entry is used instead, which is the address the proxy saw.
// Earlier entries are ignored because clients can set them freely.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
				return ip.Unmap().String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

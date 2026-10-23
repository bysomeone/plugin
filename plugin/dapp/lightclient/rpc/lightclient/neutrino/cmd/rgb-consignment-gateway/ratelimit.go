package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter applies a fixed-window request budget per client address.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	seen   map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: window,
		now:    time.Now,
		seen:   make(map[string]*rateWindow),
	}
}

// allow reports whether the client is still within its budget. A non-positive
// limit disables limiting.
func (l *rateLimiter) allow(client string) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	w, ok := l.seen[client]
	if !ok || now.Sub(w.start) >= l.window {
		l.seen[client] = &rateWindow{start: now, count: 1}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

// sweep drops windows that can no longer be consulted, bounding memory use.
func (l *rateLimiter) sweep() {
	if l.limit <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for key, w := range l.seen {
		if now.Sub(w.start) >= l.window {
			delete(l.seen, key)
		}
	}
}

// clientIP returns the address used for rate limiting. X-Forwarded-For and
// X-Real-IP are only honoured when the operator explicitly enables
// --trust-proxy; otherwise those headers are client-controlled.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			if ip := net.ParseIP(xr); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

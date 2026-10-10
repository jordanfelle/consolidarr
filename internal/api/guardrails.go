package api

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// hostOnly lowercases a Host header value and drops any port.
func hostOnly(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.Trim(h, "[]")
}

// ParseAllowedHosts turns CONSOLIDARR_ALLOWED_HOSTS ("a.example.com, 10.0.0.35") into a lookup set.
func ParseAllowedHosts(s string) map[string]bool {
	m := map[string]bool{}
	for _, h := range strings.Split(s, ",") {
		if h = hostOnly(h); h != "" {
			m[h] = true
		}
	}
	return m
}

// browserGuard defends the cookie-less and cookie-authenticated paths against a hostile web page driving a
// logged-in (or login-less) browser:
//
//   - Cross-site request forgery: a state-changing request that carries no API key must be same-origin. Browsers
//     always send Sec-Fetch-Site and, on POST/PUT/DELETE, Origin; either one proving a different origin gets a
//     403. This matters most with CONSOLIDARR_AUTH_DISABLED=true, where there is no cookie to be missing and any
//     page the operator visits could otherwise call the API. Requests that authenticate with an API key
//     header are exempt, because a browser never attaches that header on its own. Clients that send neither
//     header (curl, scripts) are not browsers and are unaffected.
//   - DNS rebinding: when CONSOLIDARR_ALLOWED_HOSTS is set, any request whose Host is not listed is refused, so a
//     rebinding hostname cannot reach the API. Unset, any Host is accepted (the default, since the name varies
//     per deployment).
func (a *API) browserGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(a.AllowedHosts) > 0 && r.URL.Path != "/healthz" && !a.AllowedHosts[hostOnly(r.Host)] {
			fail(w, 421, "host not allowed (see CONSOLIDARR_ALLOWED_HOSTS)")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if apiKey(r) == "" && !a.sameOrigin(r) {
				fail(w, 403, "cross-origin request refused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false // includes the literal "null" origin of sandboxed frames
	}
	h := hostOnly(u.Host)
	return h == hostOnly(r.Host) || h == hostOnly(r.Header.Get("X-Forwarded-Host")) || a.AllowedHosts[h]
}

// loginLimiter slows password guessing: after too many failures for a username, or from one address, further
// attempts are refused until the window passes. In memory on purpose; a restart resets it, which only helps an
// attacker who can already restart the process.
type loginLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

const (
	loginWindow  = 10 * time.Minute
	loginPerUser = 10
	loginPerAddr = 40
)

func (l *loginLimiter) prune(k string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.hits[k] {
		if now.Sub(t) < loginWindow {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.hits, k)
	} else {
		l.hits[k] = keep
	}
	return keep
}

func (l *loginLimiter) blocked(user, addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	return len(l.prune("u:"+user, now)) >= loginPerUser || len(l.prune("a:"+addr, now)) >= loginPerAddr
}

func (l *loginLimiter) fail(user, addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	if len(l.hits) > 10000 { // bound memory against a flood of made-up usernames
		now := time.Now()
		for k := range l.hits {
			l.prune(k, now)
		}
		if len(l.hits) > 10000 {
			l.hits = map[string][]time.Time{}
		}
	}
	now := time.Now()
	l.hits["u:"+user] = append(l.hits["u:"+user], now)
	l.hits["a:"+addr] = append(l.hits["a:"+addr], now)
}

func (l *loginLimiter) ok(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, "u:"+user)
}

func remoteAddr(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// safeGo runs f in a goroutine that logs a panic instead of taking the whole process down.
func safeGo(what string, f func()) {
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic in %s: %v", what, p)
			}
		}()
		f()
	}()
}

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
		if r.URL.Path != "/healthz" && !a.hostAllowed(r.Host) {
			fail(w, 421, "host not allowed (set CONSOLIDARR_ALLOWED_HOSTS to the name you browse to)")
			return
		}
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// A page on another site can make the browser fetch a GET (an <img> pointing at a search endpoint) even though
			// it cannot read the answer; Sonarr and the indexers would still be hit. Browsers label such requests.
			if strings.HasPrefix(r.URL.Path, "/api/") && !publicPage(r.URL.Path) && r.Header.Get("Sec-Fetch-Site") == "cross-site" && apiKey(r) == "" {
				fail(w, 403, "cross-site request refused")
				return
			}
		default:
			if apiKey(r) == "" && !a.sameOrigin(r) {
				fail(w, 403, "cross-origin request refused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed applies the Host allowlist. With an explicit list, only those names pass. Without one, a server
// that has login disabled still refuses ordinary multi-label DNS names, because a rebinding attacker's page can
// only be served from a name they own (always dotted), while people reach a LAN service by IP address,
// localhost or a single-label hostname. Set the allowlist to use a dotted name (for example behind Cloudflare).
func (a *API) hostAllowed(host string) bool {
	h := hostOnly(host)
	if len(a.AllowedHosts) > 0 {
		return a.AllowedHosts[h]
	}
	if !a.AuthDisabled {
		return true // login is required, so a rebound page has no credentials to ride on
	}
	return net.ParseIP(h) != nil || !strings.Contains(h, ".")
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
	// Compare host and port, not just the name: another service on the same machine is a different origin.
	h := strings.ToLower(u.Host)
	return h == strings.ToLower(r.Host) || h == strings.ToLower(r.Header.Get("X-Forwarded-Host")) || a.TrustedOrigins[h] || a.TrustedOrigins[hostOnly(h)]
}

// publicPage: documentation and identity pages that are harmless to open from a link on another site.
func publicPage(p string) bool {
	return p == "/api/docs" || p == "/api/openapi.json" || p == "/api/auth/me"
}

// loginLimiter slows password guessing: after too many failures for a username, or from one address, further
// attempts are refused until the window passes. In memory on purpose; a restart resets it, which only helps an
// attacker who can already restart the process.
type loginLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

const (
	loginWindow      = 10 * time.Minute
	loginPerUserAddr = 10  // wrong guesses for one username from one address
	loginPerUser     = 100 // ...from everywhere: a ceiling against distributed guessing that is far above what one host can use
	loginPerAddr     = 40
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

// reserve checks the budget and counts this attempt in one step, before the password is compared, so a burst
// of parallel guesses cannot all slip past the check. It reports false when the caller must be refused.
func (l *loginLimiter) reserve(user, addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	now := time.Now()
	// The per-address budget decides who is refused first, so a stranger hammering "admin" locks out only themselves.
	if len(l.prune("ua:"+user+"@"+addr, now)) >= loginPerUserAddr || len(l.prune("u:"+user, now)) >= loginPerUser || len(l.prune("a:"+addr, now)) >= loginPerAddr {
		return false
	}
	if len(l.hits) > 10000 { // bound memory against a flood of made-up usernames: drop expired keys only
		for k := range l.hits {
			l.prune(k, now)
		}
		if len(l.hits) > 10000 {
			return true // full of live entries: let this attempt through uncounted rather than evict the admin's
		}
	}
	l.hits["ua:"+user+"@"+addr] = append(l.hits["ua:"+user+"@"+addr], now)
	l.hits["u:"+user] = append(l.hits["u:"+user], now)
	l.hits["a:"+addr] = append(l.hits["a:"+addr], now)
	return true
}

// succeed forgets the user's failures and gives back the address slot this attempt used.
func (l *loginLimiter) succeed(user, addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, "u:"+user)
	delete(l.hits, "ua:"+user+"@"+addr)
	if a := l.hits["a:"+addr]; len(a) > 0 {
		l.hits["a:"+addr] = a[:len(a)-1]
	}
}

// clientAddr is the address a request is attributed to for rate limiting: the real client behind a trusted proxy when
// CONSOLIDARR_ALLOWED_IPS/TRUSTED_PROXIES are configured, otherwise the TCP peer.
func (a *API) clientAddr(r *http.Request) string {
	if a.IPAllow != nil {
		if c, ok := a.IPAllow.ClientIP(r); ok {
			return c.String()
		}
	}
	return remoteAddr(r)
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

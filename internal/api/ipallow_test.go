package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func serve(t *testing.T, ip *IPAllow, remote string, hdr map[string]string, path string) int {
	t.Helper()
	h := ip.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func mustParse(t *testing.T, allow, proxies string) *IPAllow {
	t.Helper()
	ip, err := ParseIPAllow(allow, proxies)
	if err != nil {
		t.Fatal(err)
	}
	return ip
}

func TestUnsetAllowlistIsNoRestriction(t *testing.T) {
	ip := mustParse(t, "", "")
	if ip != nil {
		t.Fatal("empty list must be nil (no restriction)")
	}
	if got := serve(t, ip, "203.0.113.9:1234", nil, "/"); got != 200 {
		t.Fatalf("a nil allowlist must pass everything, got %d", got)
	}
}

func TestAllowlistAdmitsListedAndRefusesOthers(t *testing.T) {
	ip := mustParse(t, "10.0.0.0/24, 192.168.1.5 fd00::/8", "")
	for remote, want := range map[string]int{
		"10.0.0.7:5000":        200,
		"10.0.1.7:5000":        403,
		"192.168.1.5:80":       200,
		"192.168.1.6:80":       403,
		"[fd00::1]:80":         200,
		"[fe80::1%eth0]:80":    403,
		"[::ffff:10.0.0.9]:80": 200, // IPv4-mapped form of a listed address
		"[::ffff:10.0.5.9]:80": 403,
		"203.0.113.9:80":       403,
		"not-an-address":       403, // unreadable peer: refuse
		"127.0.0.1:80":         403, // loopback is not special except for /healthz
		"[::1]:80":             403,
	} {
		if got := serve(t, ip, remote, nil, "/api/settings"); got != want {
			t.Errorf("%s: want %d, got %d", remote, want, got)
		}
	}
}

func TestEveryPathIsGatedButLoopbackHealthz(t *testing.T) {
	ip := mustParse(t, "10.0.0.0/24", "")
	for _, p := range []string{"/", "/index.html", "/api/auth/setup", "/api/auth/login", "/api/openapi.json", "/healthz"} {
		if got := serve(t, ip, "203.0.113.9:1", nil, p); got != 403 {
			t.Errorf("%s from a stranger must be refused, got %d", p, got)
		}
	}
	if got := serve(t, ip, "127.0.0.1:1", nil, "/healthz"); got != 200 {
		t.Errorf("the container healthcheck from loopback must work, got %d", got)
	}
	if got := serve(t, ip, "127.0.0.1:1", nil, "/api/settings"); got != 403 {
		t.Errorf("loopback only gets /healthz, got %d", got)
	}
}

// A forwarded header from a peer that is not a trusted proxy must never get anyone past the list.
func TestForwardedHeaderFromAnUntrustedPeerIsIgnored(t *testing.T) {
	ip := mustParse(t, "10.0.0.0/24", "172.18.0.2")
	if got := serve(t, ip, "203.0.113.9:1", map[string]string{"X-Forwarded-For": "10.0.0.5"}, "/"); got != 403 {
		t.Fatalf("a spoofed X-Forwarded-For from a stranger must not help, got %d", got)
	}
}

func TestTrustedProxyForwardsTheRealClient(t *testing.T) {
	ip := mustParse(t, "10.0.0.0/24", "172.18.0.0/16")
	cases := []struct {
		name string
		xff  string
		want int
	}{
		{"allowed client", "10.0.0.5", 200},
		{"disallowed client", "203.0.113.9", 403},
		// the client prepends whatever it likes; the proxy appends the real peer, so the RIGHT-most untrusted hop counts
		{"client spoofs an allowed address", "10.0.0.5, 203.0.113.9", 403},
		{"allowed client behind a second trusted hop", "10.0.0.5, 172.18.0.9", 200},
		{"garbled hop is refused", "10.0.0.5, nonsense", 403},
		{"no header: the proxy itself is the client, which is not listed", "", 403},
	}
	for _, c := range cases {
		hdr := map[string]string{}
		if c.xff != "" {
			hdr["X-Forwarded-For"] = c.xff
		}
		if got := serve(t, ip, "172.18.0.2:4444", hdr, "/"); got != c.want {
			t.Errorf("%s: want %d, got %d", c.name, c.want, got)
		}
	}
}

func TestBadConfigurationIsRefusedNotIgnored(t *testing.T) {
	for _, tc := range [][2]string{
		{"10.0.0.0/33", ""},
		{"10.0.0.256", ""},
		{"10.0.0.1, banana", ""},
		{"10.0.0.0/24", "172.18.0.0/99"},
		{"", "172.18.0.2"}, // proxies without an allowlist would silently do nothing
	} {
		if ip, err := ParseIPAllow(tc[0], tc[1]); err == nil {
			t.Errorf("%q/%q must be an error, got %+v", tc[0], tc[1], ip)
		}
	}
}

func TestDenialLogIsBoundedAndDeduplicated(t *testing.T) {
	ip := mustParse(t, "10.0.0.0/24", "")
	for i := 0; i < 3000; i++ {
		serve(t, ip, fmt.Sprintf("192.168.%d.%d:1", i/250, i%250+1), nil, "/") // 3000 distinct refused addresses
	}
	ip.mu.Lock()
	n := len(ip.seen)
	ip.mu.Unlock()
	if n == 0 || n > 1000 {
		t.Fatalf("the denial log map must stay within 1000 entries, got %d", n)
	}
	// the same address is only logged once inside the window
	first := time.Now().Add(-time.Minute)
	ip.mu.Lock()
	ip.seen = map[netip.Addr]time.Time{netip.MustParseAddr("192.168.9.9"): first}
	ip.mu.Unlock()
	serve(t, ip, "192.168.9.9:1", nil, "/")
	ip.mu.Lock()
	got := ip.seen[netip.MustParseAddr("192.168.9.9")]
	ip.mu.Unlock()
	if !got.Equal(first) {
		t.Fatal("a repeat denial inside the window must not be logged again")
	}
}

func TestForwardedHopsMayCarryPortsBracketsAndEmptyEntries(t *testing.T) {
	ip := mustParse(t, "10.0.0.0/24", "172.18.0.2")
	for _, xff := range []string{"10.0.0.5:51234", "[::ffff:10.0.0.5]", "10.0.0.5,", "10.0.0.5, , "} {
		if got := serve(t, ip, "172.18.0.2:1", map[string]string{"X-Forwarded-For": xff}, "/"); got != 200 {
			t.Errorf("%q: a hop in a common proxy format must be understood, got %d", xff, got)
		}
	}
	ip6 := mustParse(t, "fd00::/8", "172.18.0.2")
	if got := serve(t, ip6, "172.18.0.2:1", map[string]string{"X-Forwarded-For": "[fd00::5]:443"}, "/"); got != 200 {
		t.Errorf("[v6]:port hop must be understood, got %d", got)
	}
}

func TestFourInSixPrefixesMeanTheirIPv4Range(t *testing.T) {
	ip := mustParse(t, "::ffff:10.0.0.0/120", "")
	if got := serve(t, ip, "10.0.0.5:1", nil, "/"); got != 200 {
		t.Errorf("an IPv4 client must match a 4in6 prefix, got %d", got)
	}
	if got := serve(t, ip, "10.0.1.5:1", nil, "/"); got != 403 {
		t.Errorf("outside the range must be refused, got %d", got)
	}
}

// Behind a trusted proxy the login limiter must tell clients apart, not give them the proxy's single bucket.
func TestLoginLimiterKeysOnTheRealClientBehindATrustedProxy(t *testing.T) {
	a := &API{IPAllow: mustParse(t, "10.0.0.0/24", "172.18.0.2")}
	mk := func(client string) *http.Request {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		r.RemoteAddr = "172.18.0.2:5555"
		r.Header.Set("X-Forwarded-For", client)
		return r
	}
	if a.clientAddr(mk("10.0.0.5")) == a.clientAddr(mk("10.0.0.6")) {
		t.Fatal("two clients behind the same proxy must not share a limiter address")
	}
	if got := a.clientAddr(mk("10.0.0.5")); got != "10.0.0.5" {
		t.Fatalf("want the forwarded client, got %s", got)
	}
}

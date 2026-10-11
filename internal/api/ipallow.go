package api

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// IPAllow is an optional allowlist of client addresses (CONSOLIDARR_ALLOWED_IPS). When it is nil every client is
// accepted, which is the default; when it is set, any other client gets a bare 403 before it can reach the UI or
// the API (including the login and the setup endpoints).
//
// The client is the TCP peer, unless the peer is one of the configured trusted proxies
// (CONSOLIDARR_TRUSTED_PROXIES): then the X-Forwarded-For chain is walked from the right and the first address that
// is not itself a trusted proxy is the client. A header from any other peer is never believed, so it cannot be used
// to talk past the list.
type IPAllow struct {
	allow   []netip.Prefix
	proxies []netip.Prefix

	mu   sync.Mutex
	seen map[netip.Addr]time.Time // when a denial from this address was last logged
}

// ParseIPAllow reads comma- or space-separated addresses and CIDRs ("10.0.0.0/24, 192.168.1.5, fd00::/8") and the
// trusted proxies in the same form. An empty allow list means "no restriction" (nil, nil). A malformed entry is an
// error: the caller refuses to start rather than silently running without the restriction that was asked for.
func ParseIPAllow(allow, proxies string) (*IPAllow, error) {
	a, err := parsePrefixes(allow)
	if err != nil {
		return nil, fmt.Errorf("CONSOLIDARR_ALLOWED_IPS: %w", err)
	}
	p, err := parsePrefixes(proxies)
	if err != nil {
		return nil, fmt.Errorf("CONSOLIDARR_TRUSTED_PROXIES: %w", err)
	}
	if len(a) == 0 {
		if len(p) > 0 {
			return nil, fmt.Errorf("CONSOLIDARR_TRUSTED_PROXIES is set without CONSOLIDARR_ALLOWED_IPS, so it would do nothing")
		}
		return nil, nil
	}
	return &IPAllow{allow: a, proxies: p, seen: map[netip.Addr]time.Time{}}, nil
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		if pfx, err := netip.ParsePrefix(f); err == nil {
			if a := pfx.Addr(); a.Is4In6() && pfx.Bits() >= 96 {
				pfx = netip.PrefixFrom(a.Unmap(), pfx.Bits()-96) // ::ffff:10.0.0.0/120 means 10.0.0.0/24: clients are matched unmapped
			}
			if pfx.Bits() == 0 {
				log.Printf("warning: %q admits every %s address (IPv4 and IPv6 are matched separately)", f, map[bool]string{true: "IPv4", false: "IPv6"}[pfx.Addr().Is4()])
			}
			out = append(out, pfx.Masked())
			continue
		}
		addr, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR", f)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

func contains(list []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP is the address the request is attributed to (see IPAllow). ok is false when the peer address cannot be read.
// ClientIP exposes clientIP so callers such as the login limiter attribute a request to the real client, not to the
// trusted proxy in front of it.
func (ip *IPAllow) ClientIP(r *http.Request) (netip.Addr, bool) { return ip.clientIP(r) }

// clientIP is the address the request is attributed to (see IPAllow). ok is false when the peer address cannot be read
// or a forwarded hop is garbled.
func (ip *IPAllow) clientIP(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	peer = peer.Unmap().WithZone("")
	if !contains(ip.proxies, peer) {
		return peer, true
	}
	// peer is a trusted proxy: believe the forwarded chain, from the right
	var chain []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		chain = append(chain, strings.Split(h, ",")...)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(chain[i])
		if hop == "" {
			continue // an empty hop (a trailing comma) carries no address to believe or to be fooled by
		}
		a, err := parseHop(hop)
		if err != nil {
			return netip.Addr{}, false // a garbled hop in the trusted chain: refuse rather than guess
		}
		a = a.Unmap().WithZone("")
		if !contains(ip.proxies, a) {
			return a, true
		}
	}
	return peer, true // no usable chain: the proxy itself is the client
}

// parseHop reads one X-Forwarded-For entry: a bare address, or the address:port / [v6]:port that some proxies append.
func parseHop(h string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return a, nil
	}
	ap, err := netip.ParseAddrPort(h)
	if err != nil {
		return netip.Addr{}, err
	}
	return ap.Addr(), nil
}

// Wrap refuses clients that are not on the list. /healthz from the loopback is always answered so a container
// healthcheck keeps working whatever the list says (it exposes nothing but "ok").
func (ip *IPAllow) Wrap(next http.Handler) http.Handler {
	if ip == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := ip.clientIP(r)
		if ok && contains(ip.allow, c) {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/healthz" && ok && c.IsLoopback() {
			next.ServeHTTP(w, r)
			return
		}
		ip.logDenied(c, ok)
		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

// logDenied notes a refused address, at most once every ten minutes per address and for at most 1000 addresses at a
// time, so a scan of the port cannot flood the log or memory.
func (ip *IPAllow) logDenied(c netip.Addr, ok bool) {
	ip.mu.Lock()
	defer ip.mu.Unlock()
	if !ok {
		c = netip.Addr{}
	}
	if t, seen := ip.seen[c]; seen && time.Since(t) < 10*time.Minute {
		return
	}
	if len(ip.seen) >= 1000 {
		ip.seen = map[netip.Addr]time.Time{}
	}
	ip.seen[c] = time.Now()
	log.Printf("refused %s: not in CONSOLIDARR_ALLOWED_IPS", map[bool]string{true: c.String(), false: "an unreadable address"}[ok])
}

package sonarr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// Consolidarr calls whatever Sonarr URL an administrator configures, which makes it a request-forgery
// primitive if someone untrusted can reach the UI or API (for example with login disabled on a LAN). Two
// layers limit the damage without getting in the way of normal use (Sonarr on localhost, a LAN address or a
// container name is fine):
//
//   - ValidateURL rejects non-HTTP(S) schemes, embedded credentials and unsafe literal addresses when an
//     instance is saved or tested, so the mistake is reported clearly.
//   - the HTTP client refuses to connect to unsafe addresses at dial time, after DNS resolution, so a host
//     name that resolves (or later re-resolves) to one is blocked too.
//
// "Unsafe" means link-local (which includes the 169.254.169.254 cloud metadata endpoint), unspecified and
// multicast addresses. Loopback and private ranges stay allowed because that is where Sonarr lives.

func unsafeIP(ip net.IP) bool {
	if ip == nil {
		return true // fail closed: anything we cannot parse is not a destination we vouch for
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified()
}

// ErrBlockedAddress is returned when a connection to an unsafe address is refused.
var ErrBlockedAddress = errors.New("blocked address: link-local, unspecified and multicast addresses are not allowed")

// refuseUnsafe is a net.Dialer Control hook: it runs after name resolution, with the concrete IP.
func refuseUnsafe(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	if ip := parseHostIP(host); unsafeIP(ip) {
		return fmt.Errorf("%w (%s)", ErrBlockedAddress, host)
	}
	return nil
}

// parseHostIP parses a literal address, tolerating an IPv6 zone ("fe80::1%eth0"), which net.ParseIP rejects and
// which would otherwise slip past the check. It returns nil if host is not a literal address.
func parseHostIP(host string) net.IP {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	return net.IP(a.WithZone("").Unmap().AsSlice())
}

func newTransport() *http.Transport {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: refuseUnsafe}
	return &http.Transport{
		DialContext:           d.DialContext,
		Proxy:                 nil, // connect directly so the dial-time check sees the real destination
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: ReleaseSearchTimeout, // an interactive season search can take this long; other calls set their own, shorter, deadline
	}
}

// noRedirect makes the client report a redirect instead of following it: a configured Sonarr never needs one,
// and following would let a hostile "Sonarr" bounce the API key to another host.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// ValidateURL checks an instance URL before it is stored or used.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("URL must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return errors.New("URL has no host")
	}
	if u.User != nil {
		return errors.New("URL must not contain credentials; use the API key field")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("URL must not contain a query string or fragment")
	}
	if ip := parseHostIP(u.Hostname()); ip != nil {
		if unsafeIP(ip) {
			return fmt.Errorf("%w (%s)", ErrBlockedAddress, u.Hostname())
		}
		return nil
	}
	// A name that currently resolves to an unsafe address is rejected early. A lookup failure is not an error:
	// Sonarr may simply be down, and the dial-time check still applies to every real connection.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname()); err == nil {
		for _, a := range ips {
			if unsafeIP(a.IP) {
				return fmt.Errorf("%w (%s resolves to %s)", ErrBlockedAddress, u.Hostname(), a.IP)
			}
		}
	}
	return nil
}

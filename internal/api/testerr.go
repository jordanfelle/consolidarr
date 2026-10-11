package api

import (
	"regexp"
	"strings"
)

var upstreamStatus = regexp.MustCompile(`^sonarr \S+ \S+: (\d{3}) `)

// testError turns a failed connection test into a message that says what went wrong without handing back what the
// remote server said: the test can be pointed at any reachable address, so a response body is data from a place the
// caller may not be allowed to read.
func testError(err error) string {
	m := err.Error()
	if g := upstreamStatus.FindStringSubmatch(m); g != nil {
		return "that address answered HTTP " + g[1] + " and does not look like Sonarr (check the URL, base path and API key)"
	}
	if strings.Contains(m, "redirect refused") {
		return "that address redirected; use the final URL"
	}
	if strings.Contains(m, "response larger") {
		return "that address sent an unexpectedly large response"
	}
	if strings.HasPrefix(m, "sonarr ") && strings.Contains(m, "reading response") {
		return "could not read the response"
	}
	if strings.Contains(m, "invalid character") || strings.Contains(m, "cannot unmarshal") || strings.Contains(m, "unexpected end of JSON") {
		return "that address answered, but not with a Sonarr API response"
	}
	return m // network errors (refused, timed out, blocked address) name only the address the caller supplied
}

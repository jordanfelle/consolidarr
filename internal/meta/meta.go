// Package meta looks up alternative episode orderings (DVD, absolute, TMDB episode groups) so a pack numbered in
// another order can be recognised. Both lookups are optional: with no key configured the engine simply has no
// alternatives and only trusts titles Sonarr already knows. The result is evidence to check file titles against,
// never a mapping that is applied on its own.
package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jordanfelle/consolidarr/internal/order"
)

// Lookup returns the alternative orderings known for a series, from every configured source.
type Lookup struct {
	TVDB *TVDB
	TMDB *TMDB

	mu    sync.Mutex
	cache map[cacheKey]cached
}

type cacheKey struct {
	src string
	id  int
}

type cached struct {
	at   time.Time
	ttl  time.Duration
	alts []order.Alt
	err  error
}

const cacheFor = 24 * time.Hour

// Configured reports whether any source has a key.
func (l *Lookup) Configured() bool { return l != nil && (l.TVDB != nil || l.TMDB != nil) }

// Alts is safe to call on a nil Lookup. A source that fails is skipped (its error is returned beside what the
// other sources found) so one outage never blocks a replacement. Each source is cached on its own, failures
// briefly too, so one broken key does not make every cycle re-fetch the healthy source.
func (l *Lookup) Alts(ctx context.Context, tvdbID, tmdbID int) ([]order.Alt, error) {
	if !l.Configured() {
		return nil, nil
	}
	var out []order.Alt
	var errs []string
	if l.TVDB != nil && tvdbID != 0 {
		a, err := l.cached("tvdb", tvdbID, func() ([]order.Alt, error) { return l.TVDB.Alts(ctx, tvdbID) })
		out = append(out, a...)
		if err != nil {
			errs = append(errs, "tvdb: "+err.Error())
		}
	}
	if l.TMDB != nil && tmdbID != 0 {
		a, err := l.cached("tmdb", tmdbID, func() ([]order.Alt, error) { return l.TMDB.Alts(ctx, tmdbID) })
		out = append(out, a...)
		if err != nil {
			errs = append(errs, "tmdb: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

const failureCacheFor = 10 * time.Minute

func (l *Lookup) cached(src string, id int, fetch func() ([]order.Alt, error)) ([]order.Alt, error) {
	key := cacheKey{src, id}
	l.mu.Lock()
	if c, ok := l.cache[key]; ok && time.Since(c.at) < c.ttl {
		l.mu.Unlock()
		return c.alts, c.err
	}
	l.mu.Unlock()
	alts, err := fetch()
	ttl := cacheFor
	if err != nil {
		ttl = failureCacheFor
	}
	l.mu.Lock()
	if l.cache == nil {
		l.cache = map[cacheKey]cached{}
	}
	l.cache[key] = cached{time.Now(), ttl, alts, err}
	l.mu.Unlock()
	return alts, err
}

// HTTPError is a non-200 answer. It carries only the status: the URL may hold an API key.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.Status) }

func isStatus(err error, code int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == code
}

var httpc = &http.Client{Timeout: 30 * time.Second}

// getJSON fetches a URL and decodes the JSON body; the body is capped so a bad endpoint cannot exhaust memory.
func getJSON(ctx context.Context, u string, header map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &HTTPError{Status: resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

// redact keeps an API key that rides in a query string out of logs and job messages.
func redact(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return fmt.Errorf("%s: %v", ue.Op, ue.Err)
	}
	return err
}

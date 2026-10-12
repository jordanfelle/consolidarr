// Package meta looks up alternative episode orderings (DVD, absolute, TMDB episode groups) so a pack numbered in
// another order can be recognised. Both lookups are optional: with no key configured the engine simply has no
// alternatives and only trusts titles Sonarr already knows. The result is evidence to check file titles against,
// never a mapping that is applied on its own.
package meta

import (
	"context"
	"encoding/json"
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
	cache map[[2]int]cached
}

type cached struct {
	at   time.Time
	alts []order.Alt
}

const cacheFor = 24 * time.Hour

// Configured reports whether any source has a key.
func (l *Lookup) Configured() bool { return l != nil && (l.TVDB != nil || l.TMDB != nil) }

// Alts is safe to call on a nil Lookup. A source that fails is skipped (its error is returned beside what the
// other sources found) so one outage never blocks a replacement.
func (l *Lookup) Alts(ctx context.Context, tvdbID, tmdbID int) ([]order.Alt, error) {
	if !l.Configured() {
		return nil, nil
	}
	key := [2]int{tvdbID, tmdbID}
	l.mu.Lock()
	if c, ok := l.cache[key]; ok && time.Since(c.at) < cacheFor {
		l.mu.Unlock()
		return c.alts, nil
	}
	l.mu.Unlock()
	var out []order.Alt
	var errs []string
	if l.TVDB != nil && tvdbID != 0 {
		if a, err := l.TVDB.Alts(ctx, tvdbID); err != nil {
			errs = append(errs, "tvdb: "+err.Error())
		} else {
			out = append(out, a...)
		}
	}
	if l.TMDB != nil && tmdbID != 0 {
		if a, err := l.TMDB.Alts(ctx, tmdbID); err != nil {
			errs = append(errs, "tmdb: "+err.Error())
		} else {
			out = append(out, a...)
		}
	}
	if len(errs) == 0 { // a partial answer is not cached: try the failed source again next time
		l.mu.Lock()
		if l.cache == nil {
			l.cache = map[[2]int]cached{}
		}
		l.cache[key] = cached{time.Now(), out}
		l.mu.Unlock()
		return out, nil
	}
	return out, fmt.Errorf("%s", strings.Join(errs, "; "))
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
		return fmt.Errorf("HTTP %d", resp.StatusCode)
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

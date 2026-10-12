package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jordanfelle/consolidarr/internal/order"
)

// TVDB is the TheTVDB v4 API. Orders are the season types TVDB publishes besides the default (aired) one.
type TVDB struct {
	Key, Pin string
	Base     string // https://api4.thetvdb.com/v4 unless a test overrides it

	mu    sync.Mutex
	token string
	until time.Time
}

var tvdbOrders = []string{"dvd", "absolute", "alternate", "regional"}

func (t *TVDB) base() string {
	if t.Base != "" {
		return t.Base
	}
	return "https://api4.thetvdb.com/v4"
}

func (t *TVDB) login(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Before(t.until) {
		return t.token, nil
	}
	body, _ := json.Marshal(map[string]string{"apikey": t.Key, "pin": t.Pin})
	req, err := http.NewRequestWithContext(ctx, "POST", t.base()+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return "", redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("login: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.Data.Token == "" {
		return "", fmt.Errorf("login: no token in the response")
	}
	t.token, t.until = out.Data.Token, time.Now().Add(20*24*time.Hour) // tokens last a month
	return t.token, nil
}

func (t *TVDB) Alts(ctx context.Context, tvdbID int) ([]order.Alt, error) {
	tok, err := t.login(ctx)
	if err != nil {
		return nil, err
	}
	hdr := map[string]string{"Authorization": "Bearer " + tok}
	var out []order.Alt
	for _, kind := range tvdbOrders {
		alt := order.Alt{Name: "TVDB " + kind}
		for page := 0; page < 20; page++ {
			var r struct {
				Data struct {
					Episodes []struct {
						ID           int    `json:"id"`
						Name         string `json:"name"`
						Aired        string `json:"aired"`
						Number       int    `json:"number"`
						SeasonNumber int    `json:"seasonNumber"`
					} `json:"episodes"`
				} `json:"data"`
				Links struct {
					Next *string `json:"next"`
				} `json:"links"`
			}
			u := fmt.Sprintf("%s/series/%d/episodes/%s?page=%d", t.base(), tvdbID, url.PathEscape(kind), page)
			if err := getJSON(ctx, u, hdr, &r); err != nil {
				if err.Error() == "HTTP 404" { // most series have no such ordering
					break
				}
				return nil, fmt.Errorf("%s: %v", kind, err)
			}
			for _, e := range r.Data.Episodes {
				alt.Eps = append(alt.Eps, order.AltEp{Season: e.SeasonNumber, Number: e.Number, Title: e.Name, TvdbID: e.ID, AirDate: e.Aired})
			}
			if r.Links.Next == nil || *r.Links.Next == "" {
				break
			}
		}
		if len(alt.Eps) > 0 {
			out = append(out, alt)
		}
	}
	return out, nil
}

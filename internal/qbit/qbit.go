// Package qbit is the small slice of qBittorrent's WebUI API that consolidarr needs, reached the way Sonarr reaches it
// on a qui setup: through qui's client proxy (http://qui:7476/proxy/<key>), which needs no login. A plain qBittorrent
// URL with a username and password works too.
package qbit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is returned when the torrent is not in qBittorrent.
var ErrNotFound = errors.New("torrent not found in qBittorrent")

type Client struct {
	Base, User, Pass string
	http             *http.Client
}

func New(base, user, pass string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{Base: strings.TrimRight(base, "/"), User: user, Pass: pass,
		http: &http.Client{Timeout: 3 * time.Minute, Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type Torrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	State       string  `json:"state"`
	Progress    float64 `json:"progress"`
	SavePath    string  `json:"save_path"`
	ContentPath string  `json:"content_path"`
	Size        int64   `json:"size"`
	AddedOn     int64   `json:"added_on"`
}

type File struct {
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		var body io.Reader
		u := c.Base + "/api/v2" + path
		if method == "GET" && len(form) > 0 {
			u += "?" + form.Encode()
		} else if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, u, body)
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden && c.User != "" && attempt == 0 {
			if err := c.login(ctx); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("qBittorrent %s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(b[:min(len(b), 200)])))
		}
		if out != nil {
			return json.Unmarshal(b, out)
		}
		return nil
	}
}

func (c *Client) login(ctx context.Context) error {
	u := c.Base + "/api/v2/auth/login"
	req, _ := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(url.Values{"username": {c.User}, "password": {c.Pass}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.EqualFold(strings.TrimSpace(string(b)), "Ok.") {
		return fmt.Errorf("qBittorrent login failed (%s)", resp.Status)
	}
	return nil
}

// Info returns one torrent, ErrNotFound when qBittorrent does not have it.
func (c *Client) Info(ctx context.Context, hash string) (Torrent, error) {
	var ts []Torrent
	if err := c.do(ctx, "GET", "/torrents/info", url.Values{"hashes": {strings.ToLower(hash)}}, &ts); err != nil {
		return Torrent{}, err
	}
	if len(ts) == 0 {
		return Torrent{}, ErrNotFound
	}
	return ts[0], nil
}

// Files lists the torrent's files (names are relative to its save path).
func (c *Client) Files(ctx context.Context, hash string) ([]File, error) {
	var fs []File
	err := c.do(ctx, "GET", "/torrents/files", url.Values{"hash": {strings.ToLower(hash)}}, &fs)
	return fs, err
}

// EnsureCategory creates the category when qBittorrent does not know it (an existing one is left alone).
func (c *Client) EnsureCategory(ctx context.Context, name string) error {
	var cats map[string]json.RawMessage
	if err := c.do(ctx, "GET", "/torrents/categories", nil, &cats); err != nil {
		return err
	}
	if _, ok := cats[name]; ok {
		return nil
	}
	return c.do(ctx, "POST", "/torrents/createCategory", url.Values{"category": {name}, "savePath": {""}}, nil)
}

// Park moves the torrent to category, which no arr app watches, so none of them imports it.
func (c *Client) Park(ctx context.Context, hash, category string) error {
	return c.do(ctx, "POST", "/torrents/setCategory", url.Values{"hashes": {strings.ToLower(hash)}, "category": {category}}, nil)
}

// HasCategory reports whether qBittorrent already has the category (false on any error). There is deliberately no way
// to delete a torrent here: the operator's rule is that a torrent with data is only ever moved to a seed category.
func (c *Client) HasCategory(ctx context.Context, name string) bool {
	var cats map[string]json.RawMessage
	if err := c.do(ctx, "GET", "/torrents/categories", nil, &cats); err != nil {
		return false
	}
	_, ok := cats[name]
	return ok
}

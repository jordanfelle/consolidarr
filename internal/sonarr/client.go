// Package sonarr is a minimal Sonarr v3/v4 API client (only what consolidarr needs).
package sonarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Base string // e.g. http://sonarr:8989/sonarr
	Key  string
	HTTP *http.Client
}

func New(base, key string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Key: key, HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, timeout time.Duration) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/api/v3"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("sonarr %s %s: %d %s", method, path, resp.StatusCode, truncate(string(data), 200))
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type Quality struct {
	Quality struct {
		ID         int    `json:"id"`
		Name       string `json:"name"`
		Source     string `json:"source"`
		Resolution int    `json:"resolution"`
	} `json:"quality"`
}

// Rank orders qualities: resolution first, then source (remux > bluray > web > tv/dvd).
func (q Quality) Rank() int {
	w := map[string]int{"bluray": 3, "web": 2, "webRip": 2, "television": 1, "dvd": 1}[q.Quality.Source]
	if strings.Contains(strings.ToLower(q.Quality.Name), "remux") {
		w = 4
	}
	return q.Quality.Resolution*10 + w
}

type SeasonStat struct {
	SeasonNumber int `json:"seasonNumber"`
	Statistics   struct {
		EpisodeCount      int `json:"episodeCount"`
		TotalEpisodeCount int `json:"totalEpisodeCount"`
	} `json:"statistics"`
}

type Series struct {
	ID         int          `json:"id"`
	Title      string       `json:"title"`
	SeriesType string       `json:"seriesType"`
	Monitored  bool         `json:"monitored"`
	Path       string       `json:"path"`
	Seasons    []SeasonStat `json:"seasons"`
}

type EpisodeFile struct {
	ID           int     `json:"id"`
	SeriesID     int     `json:"seriesId"`
	SeasonNumber int     `json:"seasonNumber"`
	Size         int64   `json:"size"`
	ReleaseGroup string  `json:"releaseGroup"`
	ReleaseType  string  `json:"releaseType"`
	Path         string  `json:"path"`
	Quality      Quality `json:"quality"`
}

type Episode struct {
	ID            int  `json:"id"`
	SeasonNumber  int  `json:"seasonNumber"`
	EpisodeNumber int  `json:"episodeNumber"`
	EpisodeFileID int  `json:"episodeFileId"`
	Monitored     bool `json:"monitored"`
}

type Release struct {
	GUID       string   `json:"guid"`
	IndexerID  int      `json:"indexerId"`
	Indexer    string   `json:"indexer"`
	Title      string   `json:"title"`
	Size       int64    `json:"size"`
	Seeders    *int     `json:"seeders"`
	FullSeason bool     `json:"fullSeason"`
	SeasonNum  int      `json:"seasonNumber"`
	Quality    Quality  `json:"quality"`
	Rejected   bool     `json:"rejected"`
	Rejections []string `json:"rejections"`
}

type QueueItem struct {
	ID           int     `json:"id"`
	SeriesID     int     `json:"seriesId"`
	SeasonNumber int     `json:"seasonNumber"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	Size         float64 `json:"size"`
	SizeLeft     float64 `json:"sizeleft"`
	DownloadID   string  `json:"downloadId"`
	OutputPath   string  `json:"outputPath"`
}

type ManualImportItem struct {
	Path         string `json:"path"`
	SeasonNumber int    `json:"seasonNumber"`
	Episodes     []struct {
		EpisodeNumber int `json:"episodeNumber"`
	} `json:"episodes"`
	Rejections []struct {
		Reason string `json:"reason"`
	} `json:"rejections"`
}

type Command struct {
	ID      int    `json:"id"`
	Status  string `json:"status"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

type Health struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (c *Client) Status(ctx context.Context) (version string, err error) {
	var s struct {
		Version string `json:"version"`
	}
	err = c.do(ctx, "GET", "/system/status", nil, &s, 20*time.Second)
	return s.Version, err
}

func (c *Client) Series(ctx context.Context) (out []Series, err error) {
	err = c.do(ctx, "GET", "/series", nil, &out, 5*time.Minute)
	return
}

func (c *Client) EpisodeFiles(ctx context.Context, seriesID int) (out []EpisodeFile, err error) {
	err = c.do(ctx, "GET", fmt.Sprintf("/episodefile?seriesId=%d", seriesID), nil, &out, 2*time.Minute)
	return
}

func (c *Client) Episodes(ctx context.Context, seriesID, season int) (out []Episode, err error) {
	err = c.do(ctx, "GET", fmt.Sprintf("/episode?seriesId=%d&seasonNumber=%d", seriesID, season), nil, &out, 2*time.Minute)
	return
}

func (c *Client) Releases(ctx context.Context, seriesID, season int) (out []Release, err error) {
	err = c.do(ctx, "GET", fmt.Sprintf("/release?seriesId=%d&seasonNumber=%d", seriesID, season), nil, &out, 5*time.Minute)
	return
}

func (c *Client) Grab(ctx context.Context, guid string, indexerID int) error {
	return c.do(ctx, "POST", "/release", map[string]any{"guid": guid, "indexerId": indexerID}, nil, 3*time.Minute)
}

func (c *Client) Queue(ctx context.Context, timeout time.Duration) (out []QueueItem, err error) {
	var q struct {
		Records []QueueItem `json:"records"`
	}
	err = c.do(ctx, "GET", "/queue?pageSize=1000", nil, &q, timeout)
	return q.Records, err
}

func (c *Client) Health(ctx context.Context) (out []Health, err error) {
	err = c.do(ctx, "GET", "/health", nil, &out, 20*time.Second)
	return
}

func (c *Client) ManualImport(ctx context.Context, downloadID string, seriesID int) (out []ManualImportItem, err error) {
	p := "/manualimport?downloadId=" + url.QueryEscape(downloadID) + fmt.Sprintf("&seriesId=%d&filterExistingFiles=false", seriesID)
	err = c.do(ctx, "GET", p, nil, &out, 3*time.Minute)
	return
}

func (c *Client) DeleteEpisodeFiles(ctx context.Context, ids []int) error {
	return c.do(ctx, "DELETE", "/episodefile/bulk", map[string]any{"episodeFileIds": ids}, nil, 2*time.Minute)
}

func (c *Client) ImportDownload(ctx context.Context, path, downloadID string) (id int, err error) {
	var cmd Command
	err = c.do(ctx, "POST", "/command", map[string]any{"name": "DownloadedEpisodesScan", "path": path,
		"downloadClientId": downloadID, "importMode": "auto"}, &cmd, 2*time.Minute)
	return cmd.ID, err
}

func (c *Client) Command(ctx context.Context, id int) (out Command, err error) {
	err = c.do(ctx, "GET", fmt.Sprintf("/command/%d", id), nil, &out, 30*time.Second)
	return
}

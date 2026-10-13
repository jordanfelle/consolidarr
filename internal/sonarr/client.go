// Package sonarr is a minimal Sonarr v3/v4 API client (only what consolidarr needs).
package sonarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type Client struct {
	Base string // e.g. http://sonarr:8989/sonarr
	Key  string
	HTTP *http.Client
}

func New(base, key string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Key: key,
		HTTP: &http.Client{Timeout: 5 * time.Minute, Transport: newTransport(), CheckRedirect: noRedirect}}
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponse)+1))
	if err != nil {
		return fmt.Errorf("sonarr %s %s: reading response: %w", method, path, err)
	}
	if len(data) > maxResponse {
		return fmt.Errorf("sonarr %s %s: response larger than %d MB", method, path, maxResponse>>20)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("sonarr %s %s: redirect refused (%d to %s); use the final URL in the instance settings", method, path, resp.StatusCode, truncate(resp.Header.Get("Location"), 100))
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("sonarr %s %s: %d %s", method, path, resp.StatusCode, truncate(string(data), 200))
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// maxResponse bounds how much of a reply is buffered; the largest real payloads (a whole library) are far below it.
var maxResponse = 64 << 20

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
	ID            int    `json:"id"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	EpisodeFileID int    `json:"episodeFileId"`
	Monitored     bool   `json:"monitored"`
	AirDateUTC    string `json:"airDateUtc"`
	AirDate       string `json:"airDate"`
	Title         string `json:"title"`
	TvdbID        int    `json:"tvdbId"`
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
	Indexer        string `json:"indexer"`
	TrackedStatus  string `json:"trackedDownloadStatus"`
	StatusMessages []struct {
		Title    string   `json:"title"`
		Messages []string `json:"messages"`
	} `json:"statusMessages"`
	ID           int     `json:"id"`
	SeriesID     int     `json:"seriesId"`
	SeasonNumber int     `json:"seasonNumber"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	Size         float64 `json:"size"`
	SizeLeft     float64 `json:"sizeleft"`
	DownloadID   string  `json:"downloadId"`
	OutputPath   string  `json:"outputPath"`
	TrackedState string  `json:"trackedDownloadState"`
	Quality      Quality `json:"quality"`
}

type ManualImportItem struct {
	Path   string `json:"path"`
	Series struct {
		ID int `json:"id"`
	} `json:"series"`
	SeasonNumber int `json:"seasonNumber"`
	Episodes     []struct {
		ID            int    `json:"id"`
		SeasonNumber  int    `json:"seasonNumber"`
		EpisodeNumber int    `json:"episodeNumber"`
		Title         string `json:"title"`
	} `json:"episodes"`
	Rejections []struct {
		Reason string `json:"reason"`
	} `json:"rejections"`
	// What a ManualImport command needs to repeat Sonarr's own reading of the file (quality, language, group).
	ReleaseGroup string          `json:"releaseGroup"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	IndexerFlags int             `json:"indexerFlags"`
	ReleaseType  string          `json:"releaseType"`
	DownloadID   string          `json:"downloadId"`
}

// SeriesIDs are the ids an alternative-order lookup needs.
type SeriesIDs struct {
	TvdbID int `json:"tvdbId"`
	TmdbID int `json:"tmdbId"`
}

type Command struct {
	ID      int         `json:"id"`
	Name    string      `json:"name"`
	Status  string      `json:"status"`
	Result  string      `json:"result"`
	Message string      `json:"message"`
	Queued  string      `json:"queued"`
	Started string      `json:"started"`
	Body    CommandBody `json:"body"`
}

// CommandBody is the little we read of a command's body. Sonarr's command list mixes every kind of command, and the
// same field name means different things in different ones ("files" is a list of objects in ManualImport but a list
// of numbers elsewhere), so nothing here may fail the decode of the whole listing: a field that is not the shape we
// expect is simply absent.
type CommandBody struct {
	Path             string   `json:"path"`
	DownloadClientID string   `json:"downloadClientId"`
	Files            []string `json:"files"` // paths of the files a ManualImport command names
}

func (b *CommandBody) UnmarshalJSON(raw []byte) error {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	_ = json.Unmarshal(m["path"], &b.Path)
	_ = json.Unmarshal(m["downloadClientId"], &b.DownloadClientID)
	var files []struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(m["files"], &files) == nil {
		for _, f := range files {
			if f.Path != "" {
				b.Files = append(b.Files, f.Path)
			}
		}
	}
	return nil
}

type Health struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type Indexer struct {
	ID                      int    `json:"id"`
	Name                    string `json:"name"`
	Protocol                string `json:"protocol"`
	EnableInteractiveSearch bool   `json:"enableInteractiveSearch"`
}

// Indexers lists the indexers configured in Sonarr (names match Release.Indexer).
func (c *Client) Indexers(ctx context.Context) (out []Indexer, err error) {
	err = c.do(ctx, "GET", "/indexer", nil, &out, 30*time.Second)
	return
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

// HistoryItem is one entry of a season's history. Only grabs are asked for: a grab records the exact release title
// that was sent to the download client together with the download's id, which is how a queue item whose title was
// rewritten on the way (the client or Sonarr renames it) is tied back to the release we asked for.
type HistoryItem struct {
	EventType   string `json:"eventType"`
	SourceTitle string `json:"sourceTitle"`
	DownloadID  string `json:"downloadId"`
}

func (c *Client) GrabbedHistory(ctx context.Context, seriesID, season int) (out []HistoryItem, err error) {
	err = c.do(ctx, "GET", fmt.Sprintf("/history/series?seriesId=%d&seasonNumber=%d&eventType=1", seriesID, season), nil, &out, 20*time.Second)
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

// Queue returns every item in Sonarr's download queue, following pagination. Reading only the first page would
// make jobs beyond it look "lost" and let the scheduler regrab seasons that are already downloading.
func (c *Client) Queue(ctx context.Context, timeout time.Duration) ([]QueueItem, error) {
	const pageSize = 500
	var all []QueueItem
	for page := 1; page <= 200; page++ {
		var q struct {
			TotalRecords int         `json:"totalRecords"`
			Records      []QueueItem `json:"records"`
		}
		if err := c.do(ctx, "GET", fmt.Sprintf("/queue?page=%d&pageSize=%d", page, pageSize), nil, &q, timeout); err != nil {
			return nil, err
		}
		all = append(all, q.Records...)
		if len(q.Records) < pageSize || (q.TotalRecords > 0 && len(all) >= q.TotalRecords) {
			break
		}
	}
	return all, nil
}

func (c *Client) Health(ctx context.Context) (out []Health, err error) {
	err = c.do(ctx, "GET", "/health", nil, &out, 20*time.Second)
	return
}

func (c *Client) ManualImport(ctx context.Context, downloadID string) (out []ManualImportItem, err error) {
	p := "/manualimport?downloadId=" + url.QueryEscape(downloadID) + "&filterExistingFiles=false"
	err = c.do(ctx, "GET", p, nil, &out, 3*time.Minute)
	return
}

// ManualImportFolder lists the importable files inside a download folder (the queue item's outputPath).
//
// Never pass seriesId here: with it Sonarr ignores the folder and lists that series' existing LIBRARY files,
// so "verifying" would probe files you already have instead of the download.
func (c *Client) ManualImportFolder(ctx context.Context, folder string) (out []ManualImportItem, err error) {
	p := "/manualimport?folder=" + url.QueryEscape(folder) + "&filterExistingFiles=false"
	err = c.do(ctx, "GET", p, nil, &out, 3*time.Minute)
	return
}

// Commands lists Sonarr's recent commands, including queued and running ones. The listing mixes every kind of command,
// so an entry of a kind we never act on that does not decode (a field of an unexpected type) is skipped rather than
// failing the whole list, which would hold every replacement. An unreadable entry of a kind we DO act on fails the call.
func (c *Client) Commands(ctx context.Context) ([]Command, error) {
	var raw []json.RawMessage
	if err := c.do(ctx, "GET", "/command", nil, &raw, 30*time.Second); err != nil {
		return nil, err
	}
	out := make([]Command, 0, len(raw))
	skipped := 0
	for _, r := range raw {
		var cmd Command
		err := json.Unmarshal(r, &cmd)
		if err == nil {
			out = append(out, cmd)
			continue
		}
		// An entry we cannot read is only harmless if it is a kind of command we never act on. The gates that run
		// before old files are deleted look at the import and monitor commands, so an unreadable one of those (or one
		// whose name we cannot even read) must make the whole listing unreadable: fail closed, never "nothing there".
		var head struct {
			Name json.RawMessage `json:"name"`
		}
		var name string
		_ = json.Unmarshal(r, &head)
		if json.Unmarshal(head.Name, &name) != nil || criticalCommands[name] {
			return nil, fmt.Errorf("sonarr: a %q command in the command listing could not be decoded: %w", name, err)
		}
		skipped++
	}
	if len(raw) > 0 && len(out) == 0 {
		return nil, fmt.Errorf("sonarr: none of the %d commands in the command listing could be decoded", len(raw))
	}
	if skipped > 0 {
		logSkippedCommands(skipped, len(raw))
	}
	return out, nil
}

// criticalCommands are the commands the engine's safety checks and restart adoption read.
var criticalCommands = map[string]bool{
	"DownloadedEpisodesScan": true, "ManualImport": true, "ProcessMonitoredDownloads": true, "RefreshMonitoredDownloads": true,
}

var lastSkipLog atomic.Int64

// logSkippedCommands says (at most every ten minutes) that commands were left out of a listing, so a changed Sonarr
// shape is noticed instead of silently swept away.
func logSkippedCommands(skipped, total int) {
	now := time.Now().Unix()
	if last := lastSkipLog.Load(); now-last < 600 || !lastSkipLog.CompareAndSwap(last, now) {
		return
	}
	log.Printf("sonarr: %d of %d commands in the command listing could not be decoded and were skipped (Sonarr changed a shape?)", skipped, total)
}

// RemoveQueue removes items from Sonarr's queue. removeFromClient also deletes the download (and its files in
// the download directory) from the download client; library files are never touched. Sonarr is told not to
// search again on its own: callers decide whether to search.
func (c *Client) RemoveQueue(ctx context.Context, ids []int, removeFromClient, blocklist bool) error {
	p := fmt.Sprintf("/queue/bulk?removeFromClient=%t&blocklist=%t&skipRedownload=true", removeFromClient, blocklist)
	return c.do(ctx, "DELETE", p, map[string]any{"ids": ids}, nil, 2*time.Minute)
}

// SeasonSearch asks Sonarr to search all indexers for a season (async command).
func (c *Client) SeasonSearch(ctx context.Context, seriesID, season int) (id int, err error) {
	var cmd Command
	err = c.do(ctx, "POST", "/command", map[string]any{"name": "SeasonSearch", "seriesId": seriesID, "seasonNumber": season}, &cmd, time.Minute)
	return cmd.ID, err
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

// SeriesIDs reads the external ids of one series.
func (c *Client) SeriesIDs(ctx context.Context, seriesID int) (out SeriesIDs, err error) {
	err = c.do(ctx, "GET", fmt.Sprintf("/series/%d", seriesID), nil, &out, 30*time.Second)
	return
}

// ImportMapped imports pack files onto exactly the episodes given (path -> episode ids), which Sonarr's own
// reading of the file names would not pick. items are Sonarr's own listing of the download, used to carry over
// its quality/language/group reading of each file. downloadId is always the job's own download (a folder listing
// carries none), so Sonarr treats the files as that tracked download and copies or hardlinks them as it would
// itself. A file that is in the mapping but not in the listing is an error.
func (c *Client) ImportMapped(ctx context.Context, seriesID int, downloadID string, mapping map[string][]int, items []ManualImportItem) (id int, err error) {
	byPath := map[string]ManualImportItem{}
	for _, it := range items {
		byPath[it.Path] = it
	}
	var files []map[string]any
	for path, eps := range mapping {
		it, ok := byPath[path]
		if !ok {
			return 0, fmt.Errorf("%s is no longer in the download", path)
		}
		files = append(files, map[string]any{"path": path, "seriesId": seriesID, "episodeIds": eps, "releaseGroup": it.ReleaseGroup,
			"quality": it.Quality, "languages": it.Languages, "indexerFlags": it.IndexerFlags, "releaseType": it.ReleaseType, "downloadId": downloadID})
	}
	var cmd Command
	err = c.do(ctx, "POST", "/command", map[string]any{"name": "ManualImport", "files": files, "importMode": "auto"}, &cmd, 2*time.Minute)
	return cmd.ID, err
}

func (c *Client) Command(ctx context.Context, id int) (out Command, err error) {
	err = c.do(ctx, "GET", fmt.Sprintf("/command/%d", id), nil, &out, 30*time.Second)
	return
}

// Package engine runs scans, the scheduler and the replace-verify-delete job lifecycle for every instance.
package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

type Engine struct {
	St *store.Store

	mu       sync.Mutex
	scanning map[int64]bool
	runNow   chan struct{}
	Healthy  map[int64]string // instance id -> "" (ok) or reason
}

func New(st *store.Store) *Engine {
	return &Engine{St: st, scanning: map[int64]bool{}, runNow: make(chan struct{}, 1), Healthy: map[int64]string{}}
}

func client(i store.Instance) *sonarr.Client { return sonarr.New(i.URL, i.APIKey) }

func (e *Engine) RunNow() {
	select {
	case e.runNow <- struct{}{}:
	default:
	}
}

// HealthOf returns "" when the instance last looked healthy, else the reason.
func (e *Engine) HealthOf(id int64) string { e.mu.Lock(); defer e.mu.Unlock(); return e.Healthy[id] }

// Scanning reports whether a scan of the instance is in progress.
func (e *Engine) Scanning(id int64) bool { e.mu.Lock(); defer e.mu.Unlock(); return e.scanning[id] }

// ScanInstance rebuilds the season table for an instance from Sonarr's API (read-only).
func (e *Engine) ScanInstance(ctx context.Context, inst store.Instance) error {
	e.mu.Lock()
	if e.scanning[inst.ID] {
		e.mu.Unlock()
		return fmt.Errorf("scan already running")
	}
	e.scanning[inst.ID] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.scanning, inst.ID); e.mu.Unlock() }()

	c := client(inst)
	series, err := c.Series(ctx)
	if err != nil {
		return err
	}
	set := e.St.Settings()
	var (
		mu   sync.Mutex
		rows []audit.Season
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 3) // gentle on a live Sonarr
		ferr error
	)
	for _, s := range series {
		wg.Add(1)
		sem <- struct{}{}
		go func(s sonarr.Series) {
			defer wg.Done()
			defer func() { <-sem }()
			files, err := c.EpisodeFiles(ctx, s.ID)
			if err != nil {
				mu.Lock()
				ferr = err
				mu.Unlock()
				return
			}
			r := audit.Build(int(inst.ID), s, files, set.MinFiles)
			mu.Lock()
			rows = append(rows, r...)
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	if ferr != nil {
		return ferr
	}
	if err := e.St.ReplaceSeasons(inst.ID, 0, rows); err != nil {
		return err
	}
	e.St.SetKV(fmt.Sprintf("lastScan:%d", inst.ID), strconv.FormatInt(time.Now().Unix(), 10))
	e.St.Event("info", fmt.Sprintf("scan %s: %d series, %d seasons", inst.Name, len(series), len(rows)))
	return nil
}

func (e *Engine) rescanSeries(ctx context.Context, inst store.Instance, seriesID int) {
	c := client(inst)
	series, err := c.Series(ctx)
	if err != nil {
		return
	}
	for _, s := range series {
		if s.ID != seriesID {
			continue
		}
		files, err := c.EpisodeFiles(ctx, s.ID)
		if err == nil {
			_ = e.St.ReplaceSeasons(inst.ID, seriesID, audit.Build(int(inst.ID), s, files, e.St.Settings().MinFiles))
		}
	}
}

// Candidate is one full-season release with its verdict.
type Candidate struct {
	sonarr.Release
	Blocking []string `json:"blocking"`
	Picked   bool     `json:"picked"`
}

func (e *Engine) Candidates(ctx context.Context, inst store.Instance, s audit.Season) ([]Candidate, error) {
	rels, err := client(inst).Releases(ctx, s.SeriesID, s.Season)
	if err != nil {
		return nil, err
	}
	set := e.St.Settings()
	pick := audit.Pick(rels, s, audit.PickOpts{MaxGB: set.MaxPackGB, LastResort: set.LastResort})
	var out []Candidate
	for _, r := range rels {
		if !r.FullSeason || r.SeasonNum != s.Season {
			continue
		}
		out = append(out, Candidate{Release: r, Blocking: audit.Blocking(r), Picked: pick != nil && pick.GUID == r.GUID})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Picked != out[j].Picked {
			return out[i].Picked
		}
		return len(out[i].Blocking) < len(out[j].Blocking)
	})
	return out, nil
}

// Grab records the season's current files, grabs the release and starts tracking a job.
func (e *Engine) Grab(ctx context.Context, inst store.Instance, s audit.Season, guid string, indexerID int, title string, sizeGB float64, indexer string) (int64, error) {
	c := client(inst)
	files, err := c.EpisodeFiles(ctx, s.SeriesID)
	if err != nil {
		return 0, err
	}
	var old []int
	for _, f := range files {
		if f.SeasonNumber == s.Season {
			old = append(old, f.ID)
		}
	}
	if err := c.Grab(ctx, guid, indexerID); err != nil {
		// a grab that timed out may still have reached the download client
		time.Sleep(30 * time.Second)
		if q, qerr := c.Queue(ctx, 2*time.Minute); qerr != nil || !inQueue(q, s.SeriesID, s.Season) {
			return 0, err
		}
	}
	id, err := e.St.AddJob(store.Job{InstID: inst.ID, SeriesID: s.SeriesID, Season: s.Season, Title: s.Title,
		OldFileIDs: old, Release: title, Indexer: indexer, SizeGB: sizeGB})
	if err == nil {
		e.St.Event("info", fmt.Sprintf("grab %s S%02d on %s <- %s (%.1f GB)", s.Title, s.Season, inst.Name, title, sizeGB))
	}
	return id, err
}

func inQueue(q []sonarr.QueueItem, series, season int) bool {
	for _, x := range q {
		if x.SeriesID == series && x.SeasonNumber == season {
			return true
		}
	}
	return false
}

func skipKey(inst int64, series, season int) string {
	return fmt.Sprintf("%d:%d:%d", inst, series, season)
}

// healthy: Sonarr answers its queue quickly and reports no download-client error.
func healthy(ctx context.Context, c *sonarr.Client) string {
	t := time.Now()
	if _, err := c.Queue(ctx, 20*time.Second); err != nil {
		return "queue unreachable: " + err.Error()
	}
	if time.Since(t) > 15*time.Second {
		return "queue slow"
	}
	h, err := c.Health(ctx)
	if err != nil {
		return "health unreachable"
	}
	for _, x := range h {
		if x.Type == "error" && strings.Contains(strings.ToLower(x.Message), "download client") {
			return x.Message
		}
	}
	return ""
}

// Start runs the scheduler until ctx ends.
func (e *Engine) Start(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-e.runNow:
			last = time.Time{}
		}
		set := e.St.Settings()
		if !set.SchedulerEnabled || time.Since(last) < time.Duration(max(set.CycleMinutes, 1))*time.Minute {
			continue
		}
		last = time.Now()
		insts, err := e.St.Instances()
		if err != nil {
			continue
		}
		for _, inst := range insts {
			if inst.Enabled {
				e.cycle(ctx, inst, set)
			}
		}
	}
}

func (e *Engine) cycle(ctx context.Context, inst store.Instance, set store.Settings) {
	c := client(inst)
	why := healthy(ctx, c)
	e.mu.Lock()
	e.Healthy[inst.ID] = why
	e.mu.Unlock()
	if why != "" {
		e.St.Event("warn", fmt.Sprintf("%s unhealthy (%s); pausing this cycle", inst.Name, why))
		return
	}
	if ls, _ := strconv.ParseInt(e.St.KV(fmt.Sprintf("lastScan:%d", inst.ID)), 10, 64); time.Since(time.Unix(ls, 0)) > time.Duration(set.ScanHours)*time.Hour {
		if err := e.ScanInstance(ctx, inst); err != nil {
			e.St.Event("error", fmt.Sprintf("scan %s: %v", inst.Name, err))
		}
	}
	e.advanceJobs(ctx, inst, c, set)
	e.topUp(ctx, inst, c, set)
}

func (e *Engine) topUp(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings) {
	active, err := e.St.Jobs(true, 1000)
	if err != nil {
		return
	}
	busy := map[string]bool{}
	for _, j := range active {
		busy[skipKey(j.InstID, j.SeriesID, j.Season)] = true
	}
	if len(active) >= set.MaxInflight {
		return
	}
	q := store.SeasonQuery{InstID: inst.ID, FlaggedOnly: true, MinFiles: set.MinFiles, Anime: "hide"}
	if set.IncludeAnime {
		q.Anime = ""
	}
	rows, err := e.St.Seasons(q)
	if err != nil {
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ai, aj := rows[i].SeriesType == "anime", rows[j].SeriesType == "anime"
		if ai != aj {
			return !ai
		}
		return rows[i].Score > rows[j].Score
	})
	queue, _ := c.Queue(ctx, time.Minute)
	for _, r := range rows {
		if len(active) >= set.MaxInflight {
			return
		}
		key := skipKey(inst.ID, r.SeriesID, r.Season)
		if busy[key] || e.St.Skipped(key) || inQueue(queue, r.SeriesID, r.Season) {
			continue
		}
		rels, err := c.Releases(ctx, r.SeriesID, r.Season)
		if err != nil {
			e.St.Event("warn", fmt.Sprintf("search %s S%02d: %v", r.Title, r.Season, err))
			continue
		}
		p := audit.Pick(rels, r, audit.PickOpts{MaxGB: set.MaxPackGB, LastResort: set.LastResort})
		if p == nil {
			e.St.Skip(key, 7*24*time.Hour, "no pack")
			continue
		}
		if set.DryRun {
			e.St.Event("info", fmt.Sprintf("[dry run] would grab %s S%02d <- %s (%.1f GB, %s)", r.Title, r.Season, p.Title, float64(p.Size)/1e9, p.Indexer))
			e.St.Skip(key, 24*time.Hour, "dry run")
			continue
		}
		if _, err := e.Grab(ctx, inst, r, p.GUID, p.IndexerID, p.Title, float64(p.Size)/1e9, p.Indexer); err != nil {
			e.St.Skip(key, 24*time.Hour, "grab failed")
			e.St.Event("error", fmt.Sprintf("grab %s S%02d: %v", r.Title, r.Season, err))
			continue
		}
		active = append(active, store.Job{})
		time.Sleep(20 * time.Second) // let the download client breathe between grabs
	}
}

// ---- job lifecycle ----

func (e *Engine) advanceJobs(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings) {
	jobs, err := e.St.Jobs(true, 1000)
	if err != nil {
		return
	}
	queue, qerr := c.Queue(ctx, time.Minute)
	if qerr != nil {
		return
	}
	for _, j := range jobs {
		if j.InstID != inst.ID {
			continue
		}
		e.advance(ctx, inst, c, set, j, queue)
	}
}

func (e *Engine) advance(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings, j store.Job, queue []sonarr.QueueItem) {
	label := fmt.Sprintf("%s S%02d", j.Title, j.Season)
	if j.Status == "importing" {
		cmd, err := c.Command(ctx, j.ImportCmd)
		if err != nil || (cmd.Status != "completed" && cmd.Status != "failed") {
			return
		}
		files, _ := c.EpisodeFiles(ctx, j.SeriesID)
		n, fresh := 0, true
		old := map[int]bool{}
		for _, id := range j.OldFileIDs {
			old[id] = true
		}
		for _, f := range files {
			if f.SeasonNumber == j.Season {
				n++
				if old[f.ID] {
					fresh = false
				}
			}
		}
		if n > 0 && fresh {
			j.Status, j.Message = "done", fmt.Sprintf("%d files imported", n)
			e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), 30*24*time.Hour, "done")
			e.St.Event("info", fmt.Sprintf("done %s: %d files imported", label, n))
			e.rescanSeries(ctx, inst, j.SeriesID)
		} else {
			j.Status, j.Message = "failed", fmt.Sprintf("import finished with %d files (%s %s)", n, cmd.Status, cmd.Message)
			e.St.Event("error", fmt.Sprintf("%s: %s", label, j.Message))
		}
		_ = e.St.UpdateJob(j)
		return
	}
	var mine []sonarr.QueueItem
	for _, q := range queue {
		if q.SeriesID == j.SeriesID && q.SeasonNumber == j.Season {
			mine = append(mine, q)
		}
	}
	if len(mine) == 0 {
		if time.Since(time.Unix(j.Created, 0)) > 30*time.Minute && j.Status != "ready" {
			j.Status, j.Message = "lost", "no longer in Sonarr's queue; old files untouched"
			e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), 24*time.Hour, "lost")
			e.St.Event("warn", label+": "+j.Message)
			_ = e.St.UpdateJob(j)
		}
		return
	}
	var size, left float64
	done := true
	for _, q := range mine {
		size, left = maxf(size, q.Size), maxf(left, q.SizeLeft)
		if q.Status != "completed" {
			done = false
		}
	}
	if left > 0 || !done {
		j.Status, j.Progress = "downloading", int(100-100*left/maxf(size, 1))
		_ = e.St.UpdateJob(j)
		return
	}
	// finished downloading: verify before touching anything
	items, err := c.ManualImport(ctx, mine[0].DownloadID, j.SeriesID)
	if err != nil {
		return
	}
	packEps := map[int]bool{}
	var bad []string
	unverified := false
	for _, it := range items {
		if it.SeasonNumber != j.Season {
			continue
		}
		for _, ep := range it.Episodes {
			packEps[ep.EpisodeNumber] = true
		}
		ok, why := probeFn(inst, it.Path)
		if why == "unverified" {
			unverified = true
		} else if !ok {
			bad = append(bad, why)
		}
	}
	eps, err := c.Episodes(ctx, j.SeriesID, j.Season)
	if err != nil {
		return
	}
	old := map[int]bool{}
	for _, id := range j.OldFileIDs {
		old[id] = true
	}
	var missing []int
	for _, ep := range eps {
		if old[ep.EpisodeFileID] && !packEps[ep.EpisodeNumber] {
			missing = append(missing, ep.EpisodeNumber)
		}
	}
	switch {
	case len(bad) > 0:
		j.Status, j.Message = "holding", "failed probe: "+strings.Join(bad[:min(len(bad), 2)], "; ")
	case len(items) == 0:
		j.Status, j.Message = "holding", "download client reports no importable files"
	case len(missing) > 0:
		j.Status, j.Message = "holding", fmt.Sprintf("pack is missing episodes %v that you have today", missing)
	case set.DryRun || !set.AutoDelete:
		j.Status, j.Message = "ready", "verified; waiting for manual approval or dry run is on"
	default:
		if err := c.DeleteEpisodeFiles(ctx, j.OldFileIDs); err != nil {
			j.Status, j.Message = "holding", "delete failed: "+err.Error()
			break
		}
		id, err := c.ImportDownload(ctx, mine[0].OutputPath, mine[0].DownloadID)
		if err != nil {
			j.Status, j.Message = "failed", fmt.Sprintf("old files deleted but import command failed: %v", err)
			e.St.Event("error", label+": "+j.Message)
			break
		}
		j.Status, j.ImportCmd, j.Message = "importing", id, "old files deleted, import running"
		if unverified {
			j.Message += " (probe unavailable: files not mounted)"
		}
		e.St.Event("info", fmt.Sprintf("%s: verified, deleted %d old files, import started", label, len(j.OldFileIDs)))
	}
	_ = e.St.UpdateJob(j)
}

// Approve lets the user push a verified "ready" job through delete+import regardless of dry-run.
func (e *Engine) Approve(ctx context.Context, inst store.Instance, j store.Job) error {
	c := client(inst)
	queue, err := c.Queue(ctx, time.Minute)
	if err != nil {
		return err
	}
	for _, q := range queue {
		if q.SeriesID == j.SeriesID && q.SeasonNumber == j.Season {
			if err := c.DeleteEpisodeFiles(ctx, j.OldFileIDs); err != nil {
				return err
			}
			id, err := c.ImportDownload(ctx, q.OutputPath, q.DownloadID)
			if err != nil {
				return err
			}
			j.Status, j.ImportCmd, j.Message = "importing", id, "approved: old files deleted, import running"
			e.St.Event("info", fmt.Sprintf("%s S%02d: approved manually, import started", j.Title, j.Season))
			return e.St.UpdateJob(j)
		}
	}
	return fmt.Errorf("download is no longer in Sonarr's queue")
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// probeFn is swappable so tests can simulate bad downloads without ffprobe.
var probeFn = probe

// probe checks a file with ffprobe through the instance's path mapping.
// why == "unverified" means ffprobe or the file isn't reachable (not an error).
func probe(inst store.Instance, path string) (ok bool, why string) {
	local := path
	if inst.PathFrom != "" && strings.HasPrefix(path, inst.PathFrom) {
		local = inst.PathTo + strings.TrimPrefix(path, inst.PathFrom)
	}
	if _, err := os.Stat(local); err != nil {
		return true, "unverified"
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return true, "unverified"
	}
	var stdout, stderr strings.Builder
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", local)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || strings.Contains(stderr.String(), "encv") {
		return false, fmt.Sprintf("%s: unreadable or DRM-encrypted", baseName(local))
	}
	hasVideo := false
	for _, l := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(l, ",video") && !strings.HasPrefix(l, "unknown,") {
			hasVideo = true
		}
	}
	if !hasVideo {
		return false, baseName(local) + ": no decodable video stream"
	}
	return true, "ok"
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

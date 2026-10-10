// Package engine runs scans, the scheduler and the replace-verify-delete job lifecycle for every instance.
package engine

import (
	"context"
	"encoding/json"
	"errors"
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

// Once is a bounded, one-off run: it works through up to Limit seasons (grab, verify, replace) using the
// same safety rules as the scheduler (dry run, auto-delete), then stops by itself. It needs no schedule.
type Once struct {
	Active   bool  `json:"active"`
	Limit    int   `json:"limit"`
	Started  int   `json:"started"`
	Began    int64 `json:"began"`
	Finished int64 `json:"finished"`
}

func (e *Engine) Once() Once {
	var o Once
	_ = json.Unmarshal([]byte(e.St.KV("once")), &o)
	return o
}

func (e *Engine) saveOnce(o Once) {
	b, _ := json.Marshal(o)
	e.St.SetKV("once", string(b))
}

// StartOnce begins a one-off run of up to limit seasons.
func (e *Engine) StartOnce(limit int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Once().Active {
		return errors.New("a one-off run is already active")
	}
	if e.St.Settings().SchedulerEnabled {
		return errors.New("the scheduler is on and already runs continuously; turn it off to do a bounded one-off run")
	}
	e.saveOnce(Once{Active: true, Limit: limit, Began: time.Now().Unix()})
	e.St.Event("info", fmt.Sprintf("one-off run started: up to %d seasons (dryRun=%v)", limit, e.St.Settings().DryRun))
	select {
	case e.runNow <- struct{}{}:
	default:
	}
	return nil
}

// CancelOnce stops starting new seasons; ones already in flight still finish.
func (e *Engine) CancelOnce() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o := e.Once(); o.Active {
		o.Limit = o.Started
		e.saveOnce(o)
		e.St.Event("info", "one-off run: no new seasons will be started; waiting for in-flight ones")
	}
}

func (e *Engine) bumpOnce() {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.Once()
	o.Started++
	e.saveOnce(o)
}

// running counts jobs still moving on their own (downloading or importing). 'ready' and 'holding' need a
// human, so they do not keep a one-off run alive.
func (e *Engine) running() int {
	js, _ := e.St.Jobs(true, 10000)
	n := 0
	for _, j := range js {
		if j.Status == "downloading" || j.Status == "importing" {
			n++
		}
	}
	return n
}

func (e *Engine) finishOnce() {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.Once()
	if !o.Active {
		return
	}
	o.Active, o.Finished = false, time.Now().Unix()
	e.saveOnce(o)
	e.St.Event("info", fmt.Sprintf("one-off run finished: %d seasons started", o.Started))
}

// shouldCycle: the loop runs cycles while the scheduler is on or a one-off run is active.
func shouldCycle(set store.Settings, o Once) bool { return set.SchedulerEnabled || o.Active }

// grabPause is the breather between grabs so the download client keeps up (tests set it to 0).
var grabPause = 20 * time.Second

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
	_ = e.St.RecordSnapshot(set.MinFiles)
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
			_ = e.St.RecordSnapshot(e.St.Settings().MinFiles) // a finished replacement moves the progress line
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
	_ = e.St.RecordSnapshot(e.St.Settings().MinFiles) // a point on every restart keeps the history line continuous
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
		once := e.Once()
		if set.SchedulerEnabled && once.Active {
			e.finishOnce() // the scheduler subsumes a one-off run; do not leave it dangling at "0 of N"
			once = e.Once()
		}
		if !shouldCycle(set, once) || time.Since(last) < time.Duration(max(set.CycleMinutes, 1))*time.Minute {
			continue
		}
		last = time.Now()
		insts, err := e.St.Instances()
		if err != nil {
			continue
		}
		allDone := true
		for _, inst := range insts {
			if inst.Enabled && !e.cycle(ctx, inst, set) {
				allDone = false
			}
		}
		// A one-off run ends once nothing more will be started and nothing is still downloading/importing.
		if !set.SchedulerEnabled && once.Active && allDone && e.running() == 0 {
			e.finishOnce()
		}
	}
}

// cycle reports whether this instance has nothing further to start (used to end a one-off run).
func (e *Engine) cycle(ctx context.Context, inst store.Instance, set store.Settings) bool {
	c := client(inst)
	why := healthy(ctx, c)
	e.mu.Lock()
	e.Healthy[inst.ID] = why
	e.mu.Unlock()
	if why != "" {
		e.St.Event("warn", fmt.Sprintf("%s unhealthy (%s); pausing this cycle", inst.Name, why))
		return false
	}
	if ls, _ := strconv.ParseInt(e.St.KV(fmt.Sprintf("lastScan:%d", inst.ID)), 10, 64); time.Since(time.Unix(ls, 0)) > time.Duration(set.ScanHours)*time.Hour {
		if err := e.ScanInstance(ctx, inst); err != nil {
			e.St.Event("error", fmt.Sprintf("scan %s: %v", inst.Name, err))
		}
	}
	e.advanceJobs(ctx, inst, c, set)
	return e.topUp(ctx, inst, c, set)
}

// topUp starts new replacements up to the in-flight cap (and, in a one-off run, up to its limit). It
// returns true when there is nothing more to start: the limit was reached or every candidate was tried.
func (e *Engine) topUp(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings) bool {
	oneOff := !set.SchedulerEnabled && e.Once().Active
	limitHit := func() bool { o := e.Once(); return oneOff && o.Started >= o.Limit }
	if limitHit() {
		return true
	}
	active, err := e.St.Jobs(true, 1000)
	if err != nil {
		return false
	}
	busy := map[string]bool{}
	for _, j := range active {
		busy[skipKey(j.InstID, j.SeriesID, j.Season)] = true
	}
	if len(active) >= set.MaxInflight {
		return false
	}
	q := store.SeasonQuery{InstID: inst.ID, FlaggedOnly: true, MinFiles: set.MinFiles, Anime: "hide"}
	if set.IncludeAnime {
		q.Anime = ""
	}
	rows, err := e.St.Seasons(q)
	if err != nil {
		return false
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
		if limitHit() {
			return true
		}
		if len(active) >= set.MaxInflight {
			return false
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
			if oneOff {
				e.bumpOnce()
			}
			continue
		}
		if _, err := e.Grab(ctx, inst, r, p.GUID, p.IndexerID, p.Title, float64(p.Size)/1e9, p.Indexer); err != nil {
			e.St.Skip(key, 24*time.Hour, "grab failed")
			e.St.Event("error", fmt.Sprintf("grab %s S%02d: %v", r.Title, r.Season, err))
			continue
		}
		active = append(active, store.Job{})
		if oneOff {
			e.bumpOnce()
		}
		time.Sleep(grabPause) // let the download client breathe between grabs
	}
	return true
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
	folder := mine[0].OutputPath
	var items []sonarr.ManualImportItem
	var err error
	if folder != "" {
		items, err = c.ManualImportFolder(ctx, folder)
	} else {
		items, err = c.ManualImport(ctx, mine[0].DownloadID)
	}
	if err != nil {
		return
	}
	packEps := map[int]bool{}
	var bad []string
	var outside, used, probed int
	unreachable := ""
	for _, it := range items {
		if it.SeasonNumber != j.Season || (it.Series.ID != 0 && it.Series.ID != j.SeriesID) {
			continue
		}
		// every file must live inside the download folder; library paths mean we are looking at the wrong thing
		if folder != "" && it.Path != folder && !strings.HasPrefix(it.Path, strings.TrimRight(folder, "/")+"/") {
			outside++
			continue
		}
		used++
		for _, ep := range it.Episodes {
			packEps[ep.EpisodeNumber] = true
		}
		ok, why := probeFn(inst, it.Path)
		switch {
		case why == "unverified":
			if unreachable == "" {
				unreachable = it.Path
			}
		case !ok:
			bad = append(bad, why)
		default:
			probed++
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
	var block string // only computed when we are otherwise ready to delete
	switch {
	case len(bad) > 0:
		j.Status, j.Message = "holding", "failed probe: "+strings.Join(bad[:min(len(bad), 2)], "; ")
	case outside > 0:
		j.Status, j.Message = "holding", fmt.Sprintf("%d files are outside the download folder %s; not touching your files", outside, folder)
	case used == 0:
		j.Status, j.Message = "holding", "no importable files found in the download folder"
	case len(missing) > 0:
		j.Status, j.Message = "holding", fmt.Sprintf("pack is missing episodes %v that you have today", missing)
	case unreachable != "" && !set.AllowUnverified:
		j.Status, j.Message = "holding", fmt.Sprintf("cannot verify: ffprobe cannot read %s. Mount the download directory and set the instance path fields, or enable 'Allow unverified' in Settings", unreachable)
	case set.DryRun || !set.AutoDelete:
		j.Status, j.Message = "ready", fmt.Sprintf("verified %d files and episode coverage; waiting for manual approval or dry run is on", probed)
	default:
		if block = e.pipelineBlock(ctx, inst, c, queue, j); block != "" {
			j.Status, j.Message = "holding", "waiting: "+block
			break
		}
		if err := removeOld(ctx, c, j.SeriesID, j.OldFileIDs); err != nil {
			j.Status, j.Message = "holding", "delete failed: "+err.Error()
			break
		}
		id, err := c.ImportDownload(ctx, mine[0].OutputPath, mine[0].DownloadID)
		if err != nil {
			j.Status, j.Message = "failed", fmt.Sprintf("old files deleted but import command failed: %v", err)
			e.St.Event("error", label+": "+j.Message)
			break
		}
		j.Status, j.ImportCmd, j.Message = "importing", id, fmt.Sprintf("old files deleted, import running (%d files probed)", probed)
		if unreachable != "" {
			j.Message += "; some files could not be probed (allowed in Settings)"
		}
		e.St.Event("info", fmt.Sprintf("%s: verified, deleted %d old files, import started", label, len(j.OldFileIDs)))
	}
	_ = e.St.UpdateJob(j)
}

// Thresholds for "is Sonarr able to import right now". Deleting old files is only safe when the replacement
// can actually be imported promptly; a backed-up import pipeline leaves seasons empty in your media server.
const (
	maxQueuedImports = 5                // DownloadedEpisodesScan commands already waiting
	maxImportBacklog = 100              // completed downloads (other than this one) still awaiting import
	maxCommandAge    = 10 * time.Minute // a monitor/import command running longer than this looks stuck
)

// pipelineBlock returns why deleting this job's old files must wait, or "" when it is safe to go ahead:
// another season is mid-import, Sonarr has a command that looks stuck, or its import queue is backed up.
func (e *Engine) pipelineBlock(ctx context.Context, inst store.Instance, c *sonarr.Client, queue []sonarr.QueueItem, j store.Job) string {
	if js, err := e.St.Jobs(true, 10000); err == nil {
		for _, o := range js {
			if o.InstID == inst.ID && o.ID != j.ID && o.Status == "importing" {
				return fmt.Sprintf("%s S%02d is still importing (one season at a time)", o.Title, o.Season)
			}
		}
	}
	cmds, err := c.Commands(ctx)
	if err != nil {
		return "cannot read Sonarr's command queue: " + err.Error()
	}
	queued := 0
	for _, cmd := range cmds {
		switch cmd.Status {
		case "queued":
			if cmd.Name == "DownloadedEpisodesScan" {
				queued++
			}
		case "started":
			switch cmd.Name {
			case "ProcessMonitoredDownloads", "DownloadedEpisodesScan", "RefreshMonitoredDownloads":
				if t, err := time.Parse(time.RFC3339, cmd.Started); err == nil && time.Since(t) > maxCommandAge {
					return fmt.Sprintf("Sonarr's %s has been running for %d minutes", cmd.Name, int(time.Since(t).Minutes()))
				}
			}
		}
	}
	if queued > maxQueuedImports {
		return fmt.Sprintf("Sonarr already has %d imports queued", queued)
	}
	backlog := 0
	for _, q := range queue {
		if q.TrackedState == "importPending" && !(q.SeriesID == j.SeriesID && q.SeasonNumber == j.Season) {
			backlog++
		}
	}
	if backlog > maxImportBacklog {
		return fmt.Sprintf("Sonarr has %d completed downloads waiting to import", backlog)
	}
	return ""
}

// liveOld returns the recorded old file ids that still exist. Sonarr can replace some of them on its own
// (it imports a pack's files as upgrades), and its bulk delete rejects the whole request with a 500
// ("Expected query to return N rows but returned M") if any id is already gone.
func liveOld(ctx context.Context, c *sonarr.Client, seriesID int, old []int) ([]int, error) {
	files, err := c.EpisodeFiles(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	have := map[int]bool{}
	for _, f := range files {
		have[f.ID] = true
	}
	var out []int
	for _, id := range old {
		if have[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// removeOld deletes whichever recorded old files still exist; nothing to delete is not an error.
func removeOld(ctx context.Context, c *sonarr.Client, seriesID int, old []int) error {
	ids, err := liveOld(ctx, c, seriesID, old)
	if err != nil || len(ids) == 0 {
		return err
	}
	return c.DeleteEpisodeFiles(ctx, ids)
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
			if why := e.pipelineBlock(ctx, inst, c, queue, j); why != "" {
				return fmt.Errorf("not now: %s", why)
			}
			if err := removeOld(ctx, c, j.SeriesID, j.OldFileIDs); err != nil {
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

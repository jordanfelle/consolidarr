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

	jobMu  locks // one job is advanced/approved/adopted by one caller at a time
	instMu locks // one season per instance is deleted-and-imported at a time

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
		if j.Status == "reserving" || j.Status == "downloading" || j.Status == "importing" {
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

// grabRetryWait is how long Grab waits after a failed request before checking whether it reached the client anyway.
var grabRetryWait = 30 * time.Second

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
	if e.St.ActiveJob(inst.ID, s.SeriesID, s.Season) { // cheap early exit; the unique index is what makes it airtight
		return 0, store.ErrDuplicate
	}
	// Reserve the season first. The database allows one unfinished job per season, so of two racing callers (a
	// double click, the scheduler and a user) only one gets here, and only that one tells Sonarr to download. The
	// placeholder is "reserving" so the scheduler does not try to bind it to a download that does not exist yet.
	id, err := e.St.AddJob(store.Job{InstID: inst.ID, SeriesID: s.SeriesID, Season: s.Season, Title: s.Title,
		OldFileIDs: old, Release: title, Indexer: indexer, SizeGB: sizeGB, Status: "reserving"})
	if err != nil {
		return 0, err
	}
	if err := c.Grab(ctx, guid, indexerID); err != nil {
		// a grab that timed out may still have reached the download client
		time.Sleep(grabRetryWait)
		if q, qerr := c.Queue(ctx, 2*time.Minute); qerr != nil || !inQueue(q, s.SeriesID, s.Season) {
			_ = e.St.DeleteJob(id) // release the reservation
			return 0, err
		}
	}
	unlock := e.jobMu.lock(id)
	if j, err := e.St.Job(id); err == nil && j.Status == "reserving" { // not if something else already moved it on
		j.Status = "downloading"
		if err := e.St.UpdateJob(j); err != nil {
			e.St.Event("warn", fmt.Sprintf("%s S%02d: grabbed but could not mark it downloading (%v); it will be picked up by its release title", s.Title, s.Season, err))
		}
	}
	unlock()
	e.St.Event("info", fmt.Sprintf("grab %s S%02d on %s <- %s (%.1f GB)", s.Title, s.Season, inst.Name, title, sizeGB))
	return id, nil
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
	queue, qerr := c.Queue(ctx, time.Minute)
	if qerr != nil {
		return false // an unreadable queue is not an empty one; regrabbing seasons already downloading would waste data
	}
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

func (e *Engine) advance(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings, j0 store.Job, queue []sonarr.QueueItem) {
	defer e.jobMu.lock(j0.ID)()
	// Re-read under the lock: the snapshot the caller holds may be minutes old (a cycle works through many jobs)
	// and a concurrent approve/retry may have moved the job on.
	j, err := e.St.Job(j0.ID)
	if err != nil {
		return
	}
	label := fmt.Sprintf("%s S%02d", j.Title, j.Season)
	switch j.Status {
	case "done", "failed", "lost":
		return
	case "reserving": // placeholder while the grab request is in flight; it has no download to bind to yet
		owners := map[string]bool{} // downloads already bound to some job (a stale item from an earlier attempt must not be adopted)
		if all, err := e.St.Jobs(false, 100000); err == nil {
			for _, o := range all {
				if o.ID != j.ID && o.DownloadID != "" {
					owners[strings.ToLower(o.DownloadID)] = true
				}
			}
		}
		for _, q := range queue { // the grab did reach Sonarr but the flip to "downloading" was lost: bind by exact title
			if q.SeriesID == j.SeriesID && q.SeasonNumber == j.Season && j.Release != "" && q.Title == j.Release && !owners[strings.ToLower(q.DownloadID)] {
				j.Status, j.DownloadID = "downloading", q.DownloadID
				_ = e.St.UpdateJob(j)
				return
			}
		}
		if time.Since(time.Unix(j.Created, 0)) > 15*time.Minute {
			j.Status, j.Message = "lost", "the grab never completed; nothing was downloaded or changed"
			e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), time.Hour, "lost")
			_ = e.St.UpdateJob(j)
		}
		return
	case "importing":
		e.finishImport(ctx, inst, c, &j, queue)
		return
	}
	mine := ownedItems(queue, &j)
	if len(mine) == 0 {
		if time.Since(time.Unix(j.Created, 0)) > 30*time.Minute && j.Status != "ready" {
			j.Status = "lost"
			j.Message = "no longer in Sonarr's queue; old files untouched"
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
		prog := int(100 - 100*left/maxf(size, 1))
		switch {
		case j.Status != "downloading" || j.Progress != prog || j.DownloadID == "":
			j.Status, j.Progress = "downloading", prog // an unchanged job is not rewritten, so `updated` means "last progress"
			_ = e.St.UpdateJob(j)
		case time.Since(time.Unix(j.Updated, 0)) > stallAfter:
			j.Status, j.Message = "failed", fmt.Sprintf("the download has not moved for %d hours (%d%%) and is still in Sonarr's queue; your files were not touched. Check the download client, then Retry now or Reject pack", int(stallAfter.Hours()), prog)
			e.St.Event("warn", label+": "+j.Message)
			_ = e.St.UpdateJob(j)
		}
		return
	}
	// finished downloading: verify before touching anything
	v := e.verify(ctx, inst, c, set, &j, mine)
	switch v.State {
	case "retry":
		return
	case "hold":
		j.Status, j.Message = "holding", v.Message
	default:
		switch {
		case set.DryRun || !set.AutoDelete:
			j.Status, j.Message = "ready", fmt.Sprintf("verified %d files and episode coverage; waiting for manual approval or dry run is on", v.Probed)
		default:
			e.replace(ctx, inst, c, &j, v, queue, "automatic")
			return
		}
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

// Approve lets the user push a verified "ready" job through delete+import regardless of dry-run. The job is
// verified again from scratch first: it may have been "ready" for hours, and the download, the files or Sonarr's
// state can have changed since.
func (e *Engine) Approve(ctx context.Context, inst store.Instance, j0 store.Job) error {
	defer e.jobMu.lock(j0.ID)()
	j, err := e.St.Job(j0.ID)
	if err != nil {
		return err
	}
	if j.Status != "ready" {
		return fmt.Errorf("job is %s; only verified 'ready' jobs can be approved", j.Status)
	}
	c := client(inst)
	queue, err := c.Queue(ctx, time.Minute)
	if err != nil {
		return err
	}
	mine := ownedItems(queue, &j)
	if len(mine) == 0 {
		return fmt.Errorf("download is no longer in Sonarr's queue")
	}
	for _, q := range mine {
		if q.SizeLeft > 0 || q.Status != "completed" {
			return fmt.Errorf("the download is not complete")
		}
	}
	v := e.verify(ctx, inst, c, e.St.Settings(), &j, mine)
	switch v.State {
	case "retry":
		return fmt.Errorf("could not re-verify the download just now; try again")
	case "hold":
		j.Status, j.Message = "holding", v.Message
		_ = e.St.UpdateJob(j)
		return fmt.Errorf("not safe to replace: %s", v.Message)
	}
	e.replace(ctx, inst, c, &j, v, queue, "approved manually")
	if j.Status != "importing" {
		return fmt.Errorf("not now: %s", strings.TrimPrefix(j.Message, "waiting: "))
	}
	return nil
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
	local := mapPath(inst, path)
	if _, err := os.Stat(local); err != nil {
		return true, "unverified"
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return true, "unverified"
	}
	var stdout, stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second) // a hung NFS read must not stall every job
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", local)
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

// mapPath translates a path as Sonarr sees it into the path consolidarr sees, respecting directory boundaries
// (/data/tvx is not under /data/tv).
func mapPath(inst store.Instance, path string) string {
	from := strings.TrimRight(inst.PathFrom, "/")
	if from != "" && (path == from || strings.HasPrefix(path, from+"/")) {
		return strings.TrimRight(inst.PathTo, "/") + strings.TrimPrefix(path, from)
	}
	return path
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Package engine runs scans, the scheduler and the replace-verify-delete job lifecycle for every instance.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/meta"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

type Engine struct {
	St   *store.Store
	Meta *meta.Lookup // optional TVDB/TMDB lookups of alternative episode orders; nil = Sonarr's own titles only

	jobMu  locks // one job is advanced/approved/adopted by one caller at a time
	instMu locks // one season per instance is deleted-and-imported at a time

	killpoint func(stage string) // tests only: called at each durable step of a replacement so a test can 'kill -9' there
	draining  atomic.Bool        // set once shutdown begins: no new replacement may start deleting
	inflight  atomic.Int64       // replacements between 'record importing' and 'import started'; a restart waits for them

	mu       sync.Mutex
	origLang map[string]origLang // see originalLanguage
	scanning map[int64]bool
	errSince map[int64]time.Time // job id -> when Sonarr first failed the "did Sonarr import it" check, in a row
	runNow   chan struct{}
	Healthy  map[int64]string // instance id -> "" (ok) or reason
}

func New(st *store.Store) *Engine {
	return &Engine{St: st, scanning: map[int64]bool{}, errSince: map[int64]time.Time{}, runNow: make(chan struct{}, 1), Healthy: map[int64]string{}}
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

// dubRetryAfterReject is how long a season waits before the dub search tries again after its pack was rejected.
var dubRetryAfterReject = 6 * time.Hour

// dubSearchBudget is the default for how many dual-audio searches one cycle makes (the setting dubSearchesPerCycle
// overrides it). On a real library four per cycle pushed Sonarr to 7.8 of 8 GB: its memory falls back within minutes
// once the searching stops, but it does not keep up with a search every 45 seconds; one per cycle does.
// It caps how many dual-audio searches one cycle makes. Each is an interactive search of every indexer
// through Sonarr (a long season can take minutes), and hundreds of seasons may qualify at once; spreading them over
// cycles keeps a live Sonarr and the indexers from being hammered. Tests may raise it.
var dubSearchBudget = 1

// dubSearchTime stops a cycle starting further dub searches once it has spent this long on them, so a run of slow
// searches cannot hold up verifying and importing the jobs already running for long. It is checked before each search
// starts: one slow search can still take up to sonarr.ReleaseSearchTimeout. Tests may lower it.
var dubSearchTime = 10 * time.Minute

// grabPause is the breather between grabs so the download client keeps up (tests set it to 0).
var grabPause = 20 * time.Second

// grabClientAttempts is how many times a grab is sent while Sonarr reports its download client unreachable.
const grabClientAttempts = 3

// clientUnreachable: Sonarr could not connect to the download client, so the request never reached it.
func clientUnreachable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Failed to connect to")
}

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
			defer func() {
				if p := recover(); p != nil {
					mu.Lock()
					ferr = fmt.Errorf("panic scanning %s: %v", s.Title, p)
					mu.Unlock()
				}
			}()
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
	Dual     bool     `json:"dual"` // advertises an English dub
	Kind     string   `json:"kind"` // "dub" when grabbing it would be a dual-audio upgrade, "" for a consolidation
}

// dubMode: this season is being chased for an English dub (the setting is on and it has finished airing without one).
func dubMode(set store.Settings, s audit.Season) bool { return set.DualAudio && audit.DubWanted(s) }

// kindOf is the job kind a grab of rel would be: a dub when the season is chased for one and the release advertises it.
func kindOf(set store.Settings, s audit.Season, rel sonarr.Release) string {
	if dubMode(set, s) && audit.IsDual(rel) {
		return "dub"
	}
	return ""
}

func (e *Engine) Candidates(ctx context.Context, inst store.Instance, s audit.Season) ([]Candidate, error) {
	rels, err := client(inst).Releases(ctx, s.SeriesID, s.Season)
	if err != nil {
		return nil, err
	}
	set := e.St.Settings()
	pick := audit.Pick(rels, s, audit.PickOpts{MaxGB: set.MaxPackGB, LastResort: set.LastResort, Dub: dubMode(set, s)})
	var out []Candidate
	for _, r := range rels {
		if !r.FullSeason || r.SeasonNum != s.Season {
			continue
		}
		blocking := audit.Blocking(r)
		if dubMode(set, s) {
			blocking = audit.BlockingForDub(r)
		}
		out = append(out, Candidate{Release: r, Blocking: blocking, Picked: pick != nil && pick.GUID == r.GUID,
			Dual: audit.IsDual(r), Kind: kindOf(set, s, r)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Picked != out[j].Picked {
			return out[i].Picked
		}
		return len(out[i].Blocking) < len(out[j].Blocking)
	})
	return out, nil
}

// CurrentRank is the best quality among the season's files right now (0 when it has none).
func (e *Engine) CurrentRank(ctx context.Context, inst store.Instance, s audit.Season) (int, error) {
	files, err := client(inst).EpisodeFiles(ctx, s.SeriesID)
	if err != nil {
		return 0, err
	}
	cur := 0
	for _, f := range files {
		if f.SeasonNumber == s.Season {
			if rk := f.Quality.Rank(); rk > cur {
				cur = rk
			}
		}
	}
	return cur, nil
}

// Grab records the season's current files, grabs the release and starts tracking a job.
func (e *Engine) Grab(ctx context.Context, inst store.Instance, s audit.Season, guid string, indexerID int, title string, sizeGB float64, indexer, kind string) (int64, error) {
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
		OldFileIDs: old, Release: title, Indexer: indexer, SizeGB: sizeGB, Status: "reserving", Kind: kind})
	if err != nil {
		return 0, err
	}
	err = c.Grab(ctx, guid, indexerID)
	// Sonarr answers "Failed to connect to <client>" when the download client was unreachable: nothing was sent, so
	// sending again cannot double-grab. A brief outage (qBittorrent's web UI stalls under load) is common; wait it out.
	for attempt := 1; err != nil && clientUnreachable(err) && attempt < grabClientAttempts; attempt++ {
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt) * grabRetryWait):
		}
		err = c.Grab(ctx, guid, indexerID)
	}
	if err != nil {
		// a grab that timed out may still have reached the download client
		time.Sleep(grabRetryWait)
		if q, qerr := c.Queue(ctx, 2*time.Minute); qerr != nil || !inQueue(q, s.SeriesID, s.Season) {
			_ = e.St.DeleteJob(id, false) // release the reservation
			return 0, err
		}
	}
	func() {
		defer e.jobMu.lock(id)()                                           // deferred so a panic cannot leave the job locked forever
		if j, err := e.St.Job(id); err == nil && j.Status == "reserving" { // not if something else already moved it on
			j.Status = "downloading"
			if err := e.St.UpdateJob(j); err != nil {
				e.St.Event("warn", fmt.Sprintf("%s S%02d: grabbed but could not mark it downloading (%v); it will be picked up by its release title", s.Title, s.Season, err))
			}
		}
	}()
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

// logResume records what a (re)start is picking back up. Nothing here needs repair: every state a job can be in
// when the process dies is resumable from the database alone (see replace for the ordering that guarantees it).
func (e *Engine) logResume() {
	js, err := e.St.Jobs(true, 10000)
	if err != nil || len(js) == 0 {
		return
	}
	by := map[string]int{}
	for _, j := range js {
		by[j.Status]++
	}
	e.St.Event("info", fmt.Sprintf("started: resuming %d active jobs %v", len(js), by))
}

// Drain begins shutdown: no replacement may start deleting from here on. Start also calls it when its context ends.
func (e *Engine) Drain() { e.draining.Store(true) }

// WaitIdle blocks until no replacement is between its first database write and the import being started, or the
// timeout passes. A shutdown calls it for a few seconds as a courtesy so a clean stop usually lands between
// replacements. It is not what makes restarts safe: a hard kill, OOM or power loss at any step is recovered from the
// database alone (the job resumes as 'importing'; see TestKillNineAtEveryStepOfAReplacementIsRecovered).
func (e *Engine) WaitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for e.inflight.Load() > 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// Start runs the scheduler until ctx ends.
func (e *Engine) Start(ctx context.Context) {
	go func() { <-ctx.Done(); e.Drain() }()
	e.logResume()
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
			if inst.Enabled && !e.safeCycle(ctx, inst, set) {
				allDone = false
			}
		}
		// A one-off run ends once nothing more will be started and nothing is still downloading/importing.
		if !set.SchedulerEnabled && once.Active && allDone && e.running() == 0 {
			e.finishOnce()
		}
	}
}

// safeCycle keeps a panic in one instance's cycle from killing the process (and every download in flight with
// it): it is logged as an event and the instance is retried next cycle.
func (e *Engine) safeCycle(ctx context.Context, inst store.Instance, set store.Settings) (done bool) {
	defer func() {
		if p := recover(); p != nil {
			e.St.Event("error", fmt.Sprintf("%s: panic in cycle: %v", inst.Name, p))
			done = false
		}
	}()
	return e.cycle(ctx, inst, set)
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
	e.reconcileLost(ctx, inst, c)
	e.advanceJobs(ctx, inst, c, set)
	return e.topUp(ctx, inst, c, set)
}

// dubSearch finds season packs for a season by searching its first episode: Sonarr returns the packs that cover it, and
// the cost no longer grows with the length of the season (see Client.ReleasesForEpisode).
func (e *Engine) dubSearch(ctx context.Context, c *sonarr.Client, r audit.Season) ([]sonarr.Release, error) {
	eps, err := c.Episodes(ctx, r.SeriesID, r.Season)
	if err != nil {
		return nil, err
	}
	first := -1
	for i, ep := range eps {
		if first < 0 || ep.EpisodeNumber < eps[first].EpisodeNumber {
			first = i
		}
	}
	if first < 0 {
		return nil, fmt.Errorf("Sonarr lists no episodes for the season")
	}
	rels, err := c.ReleasesForEpisode(ctx, eps[first].ID)
	if rels == nil && err == nil {
		rels = []sonarr.Release{}
	}
	return rels, err
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
	type cand struct {
		r         audit.Season
		dub, pack bool // chased for an English dub / flagged for consolidation
	}
	var cands []cand
	if set.DualAudio {
		// Finished anime seasons with no English audio come first: a dual-audio pack settles both questions.
		dq, err := e.St.Seasons(store.SeasonQuery{InstID: inst.ID, Anime: "only", MinFiles: set.MinFiles})
		if err != nil {
			return false
		}
		// Smallest seasons first: they are the cheapest to search and download, and the most likely to have a pack.
		sort.SliceStable(dq, func(i, j int) bool { return dq[i].Files < dq[j].Files })
		seen := map[string]bool{}
		for _, r := range dq {
			// A long season makes Sonarr's interactive search return a huge release list; on this host that crashed
			// Sonarr (OutOfMemoryException) for seasons of more than a cour. The cap is a setting.
			if audit.DubWanted(r) && (set.DubMaxFiles <= 0 || r.Files <= set.DubMaxFiles) {
				seen[skipKey(inst.ID, r.SeriesID, r.Season)] = true
				cands = append(cands, cand{r: r, dub: true, pack: r.Flagged && set.IncludeAnime})
			}
		}
		for _, r := range rows {
			if !seen[skipKey(inst.ID, r.SeriesID, r.Season)] {
				cands = append(cands, cand{r: r, pack: true})
			}
		}
	} else {
		for _, r := range rows {
			cands = append(cands, cand{r: r, pack: true})
		}
	}
	queue, qerr := c.Queue(ctx, time.Minute)
	if qerr != nil {
		return false // an unreadable queue is not an empty one; regrabbing seasons already downloading would waste data
	}
	// start grabs (or, in a dry run, only reports) a picked pack. It returns true once the caller should stop.
	start := func(r audit.Season, p *sonarr.Release, kind string, key string) (stop bool) {
		what := ""
		if kind == "dub" {
			what = "dual-audio "
		}
		if set.DryRun {
			e.St.Event("info", fmt.Sprintf("[dry run] would grab %s S%02d <- %s%s (%.1f GB, %s)", r.Title, r.Season, what, p.Title, float64(p.Size)/1e9, p.Indexer))
			e.St.Skip(key, 24*time.Hour, "dry run")
			if oneOff {
				e.bumpOnce()
			}
			return false
		}
		if _, err := e.Grab(ctx, inst, r, p.GUID, p.IndexerID, p.Title, float64(p.Size)/1e9, p.Indexer, kind); err != nil {
			wait := 24 * time.Hour
			if clientUnreachable(err) {
				wait = time.Hour // the download client was down, not the pack: try again soon
			}
			e.St.Skip(key, wait, "grab failed")
			e.St.Event("error", fmt.Sprintf("grab %s S%02d: %v", r.Title, r.Season, err))
			return false
		}
		active = append(active, store.Job{})
		if oneOff {
			e.bumpOnce()
		}
		time.Sleep(grabPause) // let the download client breathe between grabs
		return false
	}
	dubSearches := 0
	searchBudget := dubSearchBudget
	if set.DubSearchesPerCycle > 0 {
		searchBudget = set.DubSearchesPerCycle
	}
	var dubStart time.Time // set when the first dub search starts, so only searching counts against dubSearchTime
	for _, cd := range cands {
		r := cd.r
		if e.draining.Load() {
			return false // shutting down: start nothing new
		}
		if limitHit() {
			return true
		}
		if len(active) >= set.MaxInflight {
			return false
		}
		key := skipKey(inst.ID, r.SeriesID, r.Season)
		if busy[key] || inQueue(queue, r.SeriesID, r.Season) {
			continue
		}
		if (cd.dub || r.Protected > 0) && !ffprobeAvailable() {
			// The English audio of the pack must be proven with ffprobe before anything is deleted; with no ffprobe the
			// download would only sit held and occupy a slot.
			e.warnOften("noffprobe", "ffprobe is not installed: seasons with an English dub to protect, and dub upgrades, are not started")
			continue
		}
		var rels []sonarr.Release
		searched := false
		var dubRels []sonarr.Release
		search := func() bool {
			if searched {
				return rels != nil
			}
			searched = true
			var err error
			if rels, err = c.Releases(ctx, r.SeriesID, r.Season); err != nil {
				e.St.Event("warn", fmt.Sprintf("search %s S%02d: %v", r.Title, r.Season, err))
				rels = nil
				return false
			}
			if rels == nil {
				rels = []sonarr.Release{}
			}
			return true
		}
		// The dub search honours every back-off the consolidation does (done, lost, failed after delete, rejected) as
		// well as its own weekly one.
		if cd.dub && !e.St.Skipped("dub:"+key) && !e.St.Skipped(key) && dubSearches < searchBudget && (dubSearches == 0 || time.Since(dubStart) < dubSearchTime) {
			if dubSearches == 0 {
				dubStart = time.Now()
			}
			dubSearches++
			var derr error
			if dubRels, derr = e.dubSearch(ctx, c, r); derr != nil {
				e.St.Event("warn", fmt.Sprintf("search %s S%02d: %v", r.Title, r.Season, derr))
				// A search that failed (Sonarr or an indexer timing out) must not be retried every cycle.
				e.St.Skip("dub:"+key, 6*time.Hour, "search failed")
				continue
			}
			if p := audit.Pick(dubRels, r, audit.PickOpts{MaxGB: set.MaxPackGB, LastResort: set.LastResort, Dub: true}); p != nil {
				start(r, p, "dub", "dub:"+key)
				continue
			}
			// Packs usually appear only after the season ends and dubs lag the sub, so look again in a week.
			e.St.Skip("dub:"+key, 7*24*time.Hour, "no dual-audio pack")
		}
		if !cd.pack || e.St.Skipped(key) {
			continue
		}
		if !search() {
			continue
		}
		p := audit.Pick(rels, r, audit.PickOpts{MaxGB: set.MaxPackGB, LastResort: set.LastResort})
		if p == nil {
			e.St.Skip(key, 7*24*time.Hour, "no pack")
			continue
		}
		start(r, p, "", key)
	}
	return true
}

// markImportedBySonarr ends the job as done. It returns the database error, if any, so a caller that must not lose the
// job (reconcile) can retry; the rescan is best-effort (the scheduled scan corrects a stale season table anyway).
func (e *Engine) markImportedBySonarr(ctx context.Context, inst store.Instance, j *store.Job, got int) error {
	j.Status, j.Message = "done", fmt.Sprintf("Sonarr imported the pack itself (%d episodes); nothing was deleted by us", got)
	// Skip first: a crash between the two leaves a still-lost job whose season is not regrabbed for 30 days
	// (conservative), never a done job that could be queued again.
	e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), 30*24*time.Hour, "done")
	if err := e.St.UpdateJob(*j); err != nil {
		e.St.Event("error", fmt.Sprintf("could not mark %s S%02d done: %v", j.Title, j.Season, err))
		return err
	}
	e.St.Event("info", fmt.Sprintf("done %s S%02d: %s", j.Title, j.Season, j.Message))
	e.rescanSeries(ctx, inst, j.SeriesID)
	e.RunNow() // Sonarr finished the season itself: let the jobs queued behind it go now
	return nil
}

// reconcileLost re-judges, once per instance, jobs an older build ended as "lost" because the pack left the queue:
// when Sonarr had in fact imported the pack itself they become "done". It never deletes anything and any other job is
// left exactly as it was. The marker is only written after a clean pass, so a Sonarr hiccup retries next cycle.
func (e *Engine) reconcileLost(ctx context.Context, inst store.Instance, c *sonarr.Client) {
	key := fmt.Sprintf("reconciled:vanished-lost:v1:%d", inst.ID)
	if e.St.KV(key) != "" {
		return
	}
	jobs, err := e.St.VanishedLostJobs(inst.ID)
	if err != nil {
		e.St.Event("warn", fmt.Sprintf("%s: could not list lost jobs to re-check: %v (will retry)", inst.Name, err))
		return
	}
	var failed []int64
	for _, j0 := range jobs {
		if ctx.Err() != nil || e.draining.Load() {
			return // shutting down: the marker is not written, so the next start finishes the pass
		}
		if !e.reconcileOne(ctx, inst, c, j0.ID) {
			failed = append(failed, j0.ID) // keep judging the others; one bad series must not block them
		}
	}
	if ctx.Err() != nil {
		return // a shutdown mid-pass is not a failure: no try is burned and the marker stays unset
	}
	// Retries are counted per job, so one permanently broken job cannot use up another job's chances: the pass is
	// finished once every job has either been judged or failed five times.
	var exhausted []int64
	retry := false
	for _, id := range failed {
		tk := fmt.Sprintf("%s:tries:%d", key, id)
		n, _ := strconv.Atoi(e.St.KV(tk))
		e.St.SetKV(tk, strconv.Itoa(n+1))
		if n+1 >= 5 {
			exhausted = append(exhausted, id)
		} else {
			retry = true
		}
	}
	if retry {
		return
	}
	if len(exhausted) > 0 {
		e.St.Event("warn", fmt.Sprintf("%s: gave up re-checking lost jobs %v after 5 tries (Sonarr or the database kept failing); they stay lost. Delete kv key %s and its :tries:<id> keys to re-run them", inst.Name, exhausted, key))
	}
	e.St.SetKV(key, "1")
}

// reconcileOne judges one lost job under its lock, against its current row. It returns false on a Sonarr error.
func (e *Engine) reconcileOne(ctx context.Context, inst store.Instance, c *sonarr.Client, id int64) bool {
	defer e.jobMu.lock(id)()
	if ctx.Err() != nil {
		return false
	}
	j, err := e.St.Job(id)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		return false // a database hiccup is a retry, not "moved on"
	}
	if j.Status != "lost" || !strings.HasPrefix(j.Message, "no longer in Sonarr") {
		return true // retried, adopted or otherwise moved on since the list was read
	}
	newer, nerr := e.St.NewerJobExists(j.InstID, j.SeriesID, j.Season, j.ID)
	if nerr != nil {
		return false // cannot tell whether a later job owns the season: retry rather than skip silently
	}
	if newer {
		return true // a later job has the last word on this season
	}
	got, ok, ierr := e.importedBySonarr(ctx, c, &j)
	if ierr != nil {
		return false
	}
	if ok {
		return e.markImportedBySonarr(ctx, inst, &j, got) == nil
	}
	return true
}

// errorsPersisted records a failed Sonarr check for the job and reports whether it has now been failing for a day.
func (e *Engine) errorsPersisted(id int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	first, ok := e.errSince[id]
	if !ok {
		e.errSince[id] = time.Now()
		return false
	}
	return time.Since(first) >= 24*time.Hour
}

func (e *Engine) clearErr(id int64) {
	e.mu.Lock()
	delete(e.errSince, id)
	e.mu.Unlock()
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
	e.reviveLost(ctx, inst, c, queue)
	for _, j := range jobs {
		if j.InstID != inst.ID {
			continue
		}
		if e.draining.Load() {
			return // shutting down: leave the rest for the next start
		}
		e.advance(ctx, inst, c, set, j, queue)
	}
}

// reviveWindow is how long after a job was marked "lost" its pack may still turn up in Sonarr's queue and be taken
// back. A pack can take longer to download than the 30 minutes a job is given to appear there (a 5 GB pack took
// 95 minutes on a busy client, and Sonarr's own queue view can lag behind it), so "not in the queue yet" is not
// proof the download is gone.
const reviveWindow = 48 * time.Hour

// maxHistoryPerPass bounds the history requests one revive pass makes (one per season at most, and only for a lost
// job whose season has an unmatched download), so a slow or struggling Sonarr cannot stall the cycle.
const maxHistoryPerPass = 10

// reviveRecheck is how long a lost job whose history lookup found no matching grab is left alone before it is looked
// up again. Without it the same first few jobs would spend every pass's request budget and the rest would never be
// asked about.
const reviveRecheck = 20 * time.Minute

// reviveErrBackoff is the same for a lookup that failed: shorter, since nothing was learned, but a request that keeps
// failing for the same first seasons must not eat the budget every pass either.
const reviveErrBackoff = 5 * time.Minute

// reviveLost puts a job that ended "lost" because its download was not in Sonarr's queue back to "downloading"
// when exactly that release (same series, season and title) shows up there afterwards. Nothing was deleted or
// changed when the job was lost, so resuming is the same as if it had never given up: the normal flow verifies the
// pack before any old file is touched. Left alone the finished pack sits in the queue unimported forever (Sonarr
// does not import it over files it counts as just as good), holding disk and a seed slot.
func (e *Engine) reviveLost(ctx context.Context, inst store.Instance, c *sonarr.Client, queue []sonarr.QueueItem) {
	lost, err := e.St.VanishedLostJobs(inst.ID)
	if err != nil || len(lost) == 0 {
		return
	}
	all, err := e.St.Jobs(false, 100000)
	if err != nil {
		return // cannot tell which downloads other jobs already own: do not guess a binding
	}
	owners := map[string]int64{} // download id -> the job that holds it
	for _, o := range all {
		if o.DownloadID != "" {
			owners[strings.ToLower(o.DownloadID)] = o.ID
		}
	}
	type season struct{ series, n int }
	history := map[season]map[string]bool{} // grabbed download ids per release, fetched once per season per pass
	fetches := 0
	for _, j0 := range lost {
		if ctx.Err() != nil || e.draining.Load() {
			return
		}
		if j0.Release == "" || time.Since(time.Unix(j0.Updated, 0)) > reviveWindow {
			continue
		}
		if newer, err := e.St.NewerJobExists(j0.InstID, j0.SeriesID, j0.Season, j0.ID); err != nil || newer {
			continue // a later job has the last word on this season: no point asking Sonarr about this one
		}
		var hit *sonarr.QueueItem
		var cand []*sonarr.QueueItem // this season's downloads that no other job holds
		for i := range queue {
			q := &queue[i]
			if q.SeriesID != j0.SeriesID || q.SeasonNumber != j0.Season || q.DownloadID == "" {
				continue
			}
			if own, taken := owners[strings.ToLower(q.DownloadID)]; taken && own != j0.ID { // its own recorded download is fine
				continue
			}
			cand = append(cand, q)
			if hit == nil && (q.Title == j0.Release || (j0.DownloadID != "" && strings.EqualFold(q.DownloadID, j0.DownloadID))) {
				hit = q // by title, or the download this job recorded before it went lost
			}
		}
		missKey := fmt.Sprintf("revive-miss:%d", j0.ID)
		if hit == nil && len(cand) > 0 {
			ids := make([]string, 0, len(cand))
			for _, q := range cand {
				ids = append(ids, strings.ToLower(q.DownloadID))
			}
			sort.Strings(ids)
			set := strings.Join(ids, ",")
			// "<until>|<candidate set>": left alone until then, unless a download we have not looked at has appeared
			until, seen, _ := strings.Cut(e.St.KV(missKey), "|")
			if u, _ := strconv.ParseInt(until, 10, 64); time.Now().Unix() < u && seen == set {
				continue
			}
			stamp := func(d time.Duration) {
				e.St.SetKV(missKey, strconv.FormatInt(time.Now().Add(d).Unix(), 10)+"|"+set)
			}
			// The queue often shows a different title than the release we grabbed (Sonarr or the client rewrites it), so
			// ask Sonarr which download that exact release became.
			key := season{j0.SeriesID, j0.Season}
			grabs, ok := history[key]
			if !ok {
				if fetches >= maxHistoryPerPass {
					continue // the rest next cycle: a slow Sonarr must not hold up the jobs that are downloading
				}
				fetches++
				list, err := c.GrabbedHistory(ctx, j0.SeriesID, j0.Season)
				if err != nil {
					stamp(reviveErrBackoff)
					continue
				}
				grabs = map[string]bool{}
				for _, g := range list {
					if g.EventType == "grabbed" && g.DownloadID != "" {
						grabs[strings.ToLower(g.DownloadID)+"\x00"+g.SourceTitle] = true
					}
				}
				history[key] = grabs
			}
			for _, q := range cand {
				if grabs[strings.ToLower(q.DownloadID)+"\x00"+j0.Release] {
					hit = q
					break
				}
			}
			if hit == nil {
				stamp(reviveRecheck)
			}
		}
		if hit == nil {
			continue
		}
		if e.reviveOne(inst, j0.ID, hit.DownloadID) {
			e.St.SetKV(missKey, "")
			owners[strings.ToLower(hit.DownloadID)] = j0.ID
		}
	}
}

// reviveOne re-reads the job under its lock and revives it only if it is still the same kind of lost job. The
// season must have no later job at all, and the database also refuses it when another job is active there.
func (e *Engine) reviveOne(inst store.Instance, id int64, downloadID string) bool {
	defer e.jobMu.lock(id)()
	j, err := e.St.Job(id)
	if err != nil || j.Status != "lost" || !strings.HasPrefix(j.Message, "no longer in Sonarr") {
		return false // retried, adopted or otherwise moved on since the list was read
	}
	if newer, err := e.St.NewerJobExists(j.InstID, j.SeriesID, j.Season, j.ID); err != nil || newer {
		return false // a later job has the last word on this season (as in reconcileOne)
	}
	j.Status, j.Message, j.DownloadID, j.Progress = "downloading", "", downloadID, 0
	if err := e.St.UpdateJob(j); err != nil {
		return false
	}
	e.St.Event("info", fmt.Sprintf("%s S%02d: the pack turned up in Sonarr's queue after being marked lost; resuming (old files still untouched)", j.Title, j.Season))
	return true
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
	boundID := j.DownloadID // what the job itself recorded, before ownedItems below may bind a guessed download
	// The error timer counts consecutive failed checks: any pass through advance that is not such a failure resets it.
	// (It is in memory only, so a restart restarts the day; that fails safe by leaving the job alone longer.)
	keepErr := false
	defer func() {
		if !keepErr {
			e.clearErr(j.ID)
		}
	}()
	switch j.Status {
	case "done", "failed", "lost":
		return
	case "reserving": // placeholder while the grab request is in flight; it has no download to bind to yet
		owners := map[string]bool{} // downloads already bound to some job (a stale item from an earlier attempt must not be adopted)
		all, err := e.St.Jobs(false, 100000)
		if err != nil {
			return // cannot tell who owns what: do not guess a binding
		}
		for _, o := range all {
			if o.ID != j.ID && o.DownloadID != "" {
				owners[strings.ToLower(o.DownloadID)] = true
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
		if time.Since(time.Unix(j.Created, 0)) > 30*time.Minute {
			// A verified ("ready") job waiting for approval can also find its pack gone because Sonarr imported it
			// itself (a quality upgrade it accepts on its own); that closes the job. A ready job is never written off as
			// lost: it stays for the operator, as before.
			got, ok, ierr := e.importedBySonarr(ctx, c, &j)
			if ierr != nil && !e.errorsPersisted(j.ID) {
				keepErr = true
				return // Sonarr hiccup: look again next cycle rather than writing a terminal state; a day of errors in a row falls through to lost
			}
			if ok {
				_ = e.markImportedBySonarr(ctx, inst, &j, got) // on a database error the job is still unchanged and is judged again next cycle
				return
			}
			if j.Status == "ready" {
				return
			}
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
		e.clearHold(j.ID) // downloading again (a re-check, a re-grab): a later hold starts a fresh clock
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
		e.St.SetKV(holdKey(j.ID), "") // a check we could not make breaks the run of identical holds
		return
	case "hold":
		j.Status, j.Message = "holding", v.Message
		if e.holdExpired(&j, boundID, v.Kind, set) {
			// A rejected dual-audio pack is blocklisted, so a few hours later the search finds the next best one; a dub
			// has often several candidates (some of them another series' pack that Sonarr's matcher let through).
			skip := 7 * 24 * time.Hour
			if j.Kind == "dub" {
				skip = dubRetryAfterReject
			}
			if err := e.rejectBody(ctx, c, &j, false, skip, fmt.Sprintf("auto-rejected after %dh on hold: %s", set.HoldRejectHours, v.Message)); err != nil {
				e.St.Event("warn", label+": could not auto-reject the held pack: "+err.Error())
				_ = e.St.UpdateJob(j)
			}
			return
		}
	default:
		e.St.SetKV(holdKey(j.ID), "")
		switch {
		case set.DryRun || !set.AutoDelete:
			j.Status, j.Message = "ready", fmt.Sprintf("verified %d files and episode coverage; waiting for manual approval or dry run is on", v.Probed)
			if len(v.Mapping) > 0 {
				j.Message += fmt.Sprintf(". The pack numbers episodes differently from Sonarr; approving imports it with %s", v.Message)
			}
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
	maxImportBacklog = 25               // distinct completed downloads (other than this one) that are genuinely about to import
	maxBacklogFiles  = 300              // ... or that many episode records among them (20 big packs are a backlog too)
	maxCommandAge    = 10 * time.Minute // a monitor/import command running longer than this looks stuck
)

// warnOften records a warning event at most once per half hour per key. A condition that holds every job on an instance
// used to show only as each job's own message; the event log (and anything watching it) now says it once.
func (e *Engine) warnOften(key, msg string) {
	k := "warned:" + key
	if last, _ := strconv.ParseInt(e.St.KV(k), 10, 64); time.Since(time.Unix(last, 0)) < 30*time.Minute {
		return
	}
	e.St.SetKV(k, strconv.FormatInt(time.Now().Unix(), 10))
	e.St.Event("warn", msg)
}

// importBacklog counts the completed downloads Sonarr is really about to import, which is what decides whether
// deleting a season's files now would leave it empty for long. It counts DOWNLOADS, not queue records (a season pack
// is one record per episode: 23 downloads were being reported as "311 waiting"), and leaves out the ones that will
// never drain on their own: this season, packs other consolidarr jobs are holding (they wait for this very gate, so
// counting them is a deadlock), and downloads Sonarr is refusing to import (not an upgrade, already imported, unknown
// series ...), which sit in the queue until a human acts.
func (e *Engine) importBacklog(queue []sonarr.QueueItem, j store.Job) (downloads, files int) {
	ours := map[string]bool{}
	if js, err := e.St.Jobs(true, 10000); err == nil {
		for _, o := range js {
			if o.DownloadID != "" {
				ours[strings.ToLower(o.DownloadID)] = true
			}
		}
	}
	stuck := map[string]bool{} // a download is stuck if any of its records says Sonarr will not import it
	for _, q := range queue {
		for _, sm := range q.StatusMessages {
			for _, m := range sm.Messages { // messages only: the title is a file or release name ("Sample", "Unknown Series ...")
				if neverDrains(m) {
					stuck[strings.ToLower(q.DownloadID)] = true
				}
			}
		}
	}
	pending := map[string]bool{}
	for _, q := range queue {
		id := strings.ToLower(q.DownloadID)
		if q.TrackedState != "importPending" || id == "" || ours[id] || stuck[id] || (q.SeriesID == j.SeriesID && q.SeasonNumber == j.Season) {
			continue
		}
		pending[id] = true
		files++
	}
	return len(pending), files
}

// neverDrains reports whether a Sonarr status message means the download will sit in the queue until a human acts:
// the kinds the Needs-attention page knows, plus the two "nothing here to import" messages Classify leaves as other.
func neverDrains(msg string) bool {
	if Classify(msg) != KindOther {
		return true
	}
	m := strings.ToLower(msg)
	return strings.Contains(m, "not imported or missing from the release") || strings.Contains(m, "no files found are eligible for import")
}

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
		if ctx.Err() == nil { // a shutdown cancelling the call is not an outage worth a half-hour of silence
			e.warnOften("cmdqueue:"+strconv.FormatInt(inst.ID, 10), fmt.Sprintf("%s: replacements are held because Sonarr's command queue cannot be read: %v", inst.Name, err))
		}
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
	if downloads, files := e.importBacklog(queue, j); downloads > maxImportBacklog || files > maxBacklogFiles {
		return fmt.Sprintf("Sonarr has %d downloads (%d files) waiting to import", downloads, files)
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
	if e.draining.Load() {
		return errors.New("consolidarr is restarting; try again in a minute")
	}
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

// clearHold resets a job's hold clock, writing only when there is one to reset.
func (e *Engine) clearHold(id int64) {
	if e.St.KV(holdKey(id)) != "" {
		e.St.SetKV(holdKey(id), "")
	}
}

func holdKey(id int64) string { return fmt.Sprintf("holdSince:%d", id) }

// holdExpired records when a job first held for a reason that cannot fix itself and reports whether it has been held
// for that same reason longer than Settings.HoldRejectHours. Any other outcome resets the clock, so a pack is never
// rejected on the strength of two different or non-consecutive problems. A probe failure waits three times as long:
// an NFS stall looks the same as a genuinely undecodable file.
func (e *Engine) holdExpired(j *store.Job, boundID, kind string, set store.Settings) bool {
	k := holdKey(j.ID)
	if set.HoldRejectHours <= 0 || !rejectable(kind) || boundID == "" {
		e.St.SetKV(k, "")
		return false
	}
	grace := time.Duration(set.HoldRejectHours) * time.Hour
	if kind == holdProbe {
		grace *= 3
	}
	// The clock belongs to one reason on one download: a different reason, or a re-grabbed download, starts afresh.
	scope := kind + "|" + strings.ToLower(boundID)
	var since int64
	if v := e.St.KV(k); strings.LastIndex(v, ":") > 0 { // the timestamp is after the last colon; a download id may contain one
		i := strings.LastIndex(v, ":")
		if v[:i] == scope {
			since, _ = strconv.ParseInt(v[i+1:], 10, 64)
		}
	}
	if since == 0 {
		e.St.SetKV(holdKey(j.ID), scope+":"+strconv.FormatInt(time.Now().Unix(), 10))
		return false
	}
	return time.Since(time.Unix(since, 0)) > grace
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// probeFn is swappable so tests can simulate bad downloads without ffprobe.
var probeFn = probe

// audioFn reads which audio languages a file carries; tests replace it.
var audioFn = probeAudio

// ffprobeAvailable is a variable so tests can run without ffprobe installed.
var ffprobeAvailable = func() bool { _, err := exec.LookPath("ffprobe"); return err == nil }

// probeAudio lists the audio tracks of a file with ffprobe. state is "ok" (langs is the answer, possibly empty),
// or "unverified" when the file or ffprobe is unreachable or unreadable. A track tagged "und" (or untagged) is
// reported as "und": it proves nothing.
func probeAudio(inst store.Instance, path string) (langs []string, state string) {
	local := filepath.Clean(mapPath(inst, path))
	if !filepath.IsAbs(local) {
		return nil, "unverified"
	}
	if _, err := os.Stat(local); err != nil {
		return nil, "unverified"
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return nil, "unverified"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file", "-select_streams", "a",
		"-show_entries", "stream_tags=language,title:stream_disposition=comment,visual_impaired,hearing_impaired", "-of", "json", "-i", local).Output()
	if err != nil {
		return nil, "unverified"
	}
	var doc struct {
		Streams []struct {
			Tags        map[string]string `json:"tags"`
			Disposition map[string]int    `json:"disposition"`
		} `json:"streams"`
	}
	if json.Unmarshal(out, &doc) != nil {
		return nil, "unverified"
	}
	for _, st := range doc.Streams {
		// A commentary or audio-description track is not a dub, however it is tagged.
		if st.Disposition["comment"] == 1 || st.Disposition["visual_impaired"] == 1 || st.Disposition["hearing_impaired"] == 1 || extraTrack.MatchString(st.Tags["title"]) {
			continue
		}
		l := strings.ToLower(strings.TrimSpace(st.Tags["language"]))
		if l == "" || l == "und" {
			// an untagged track titled "English" is English ("Non-English" is not)
			if englishTitle.MatchString(st.Tags["title"]) {
				l = "eng"
			} else {
				l = "und"
			}
		}
		langs = append(langs, l)
	}
	return langs, "ok"
}

var (
	extraTrack   = regexp.MustCompile(`(?i)commentar|descriptive|audio description|\bAD\b|isolated|karaoke|\bsap\b`)
	englishTitle = regexp.MustCompile(`(?i)(^|[^a-z-])english\b`)
)

// probe checks a file with ffprobe through the instance's path mapping.
// why == "unverified" means ffprobe or the file isn't reachable (not an error).
func probe(inst store.Instance, path string) (ok bool, why string) {
	local := filepath.Clean(mapPath(inst, path))
	if !filepath.IsAbs(local) { // never let a name that starts with "-" be read as an ffprobe option
		return true, "unverified"
	}
	if _, err := os.Stat(local); err != nil {
		return true, "unverified"
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return true, "unverified"
	}
	var stdout, stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second) // a hung NFS read must not stall every job
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file", "-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", "-i", local)
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

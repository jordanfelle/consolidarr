package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// Timing for the post-delete phase. Once the old files are gone the season depends on Sonarr importing the
// pack, so these decide when a quiet import is reported instead of waited on forever.
const (
	importSettle   = 45 * time.Minute // command finished but files still missing: allow this long to appear
	importHardStop = 3 * time.Hour    // import command never finished
	stallAfter     = 6 * time.Hour    // a download that has not moved this long is reported, freeing its slot
)

// locks are keyed by id and never removed; the number of jobs and instances is tiny.
type locks struct{ m sync.Map }

func (l *locks) lock(id int64) func() {
	v, _ := l.m.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// verdict is the outcome of verifying a finished download against what the user has today.
type verdict struct {
	State       string // "ok": safe to replace; "hold": a check failed; "retry": transient, leave the job alone
	Message     string
	Items       []sonarr.QueueItem
	DownloadID  string
	OutputPath  string
	Probed      int
	Unreach     string
	Expected    int
	ExpectedEps []int // the episode numbers that must have a new file for the season to count as replaced
	At          time.Time
}

// ownedItems returns the queue items that belong to this job. A season can have unrelated grabs in Sonarr's
// queue at the same time (an extra single-episode download), so match the job's own download rather than
// "anything for this season". The download id is learned from the release title on first sight and kept.
func ownedItems(queue []sonarr.QueueItem, j *store.Job) []sonarr.QueueItem {
	var same []sonarr.QueueItem
	for _, q := range queue {
		if q.SeriesID == j.SeriesID && q.SeasonNumber == j.Season {
			same = append(same, q)
		}
	}
	id := strings.ToLower(j.DownloadID)
	if id == "" {
		for _, q := range same {
			if j.Release != "" && q.Title == j.Release {
				id = strings.ToLower(q.DownloadID)
				break
			}
		}
		if id == "" && len(same) > 0 {
			best := same[0]
			for _, q := range same[1:] { // several downloads for the season: the pack is the biggest one
				if q.Size > best.Size {
					best = q
				}
			}
			id = strings.ToLower(best.DownloadID)
		}
	}
	if id == "" {
		return same
	}
	var mine []sonarr.QueueItem
	for _, q := range same {
		if strings.ToLower(q.DownloadID) == id {
			mine = append(mine, q)
		}
	}
	if j.DownloadID == "" && len(mine) > 0 {
		j.DownloadID = mine[0].DownloadID
	}
	return mine
}

// rejectionOK reports whether every reason Sonarr gives for not importing a file is merely "your current file is
// as good", which is expected because those files are about to be replaced. Anything else (sample, unknown series,
// parse failure) means the file will not import, so it must not count towards covering your episodes.
func rejectionOK(it sonarr.ManualImportItem) bool {
	for _, r := range it.Rejections {
		switch Classify(r.Reason) {
		case KindNotUpgrade, KindDestExists, KindAlreadyDone:
		default:
			return false
		}
	}
	return true
}

// verify checks a finished download. It never changes anything.
func (e *Engine) verify(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings, j *store.Job, mine []sonarr.QueueItem) verdict {
	v := e.verifyNow(ctx, inst, c, set, j, mine)
	v.At = time.Now() // fresh as of when the checks finished, so only a real wait behind another season makes it stale
	return v
}

func (e *Engine) verifyNow(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings, j *store.Job, mine []sonarr.QueueItem) verdict {
	if len(mine) == 0 {
		return verdict{State: "retry"} // nothing to look at: never guess
	}
	v := verdict{Items: mine, DownloadID: mine[0].DownloadID, OutputPath: mine[0].OutputPath, At: time.Now()}
	folder := mine[0].OutputPath
	var items []sonarr.ManualImportItem
	var err error
	if folder != "" {
		items, err = c.ManualImportFolder(ctx, folder)
	} else {
		items, err = c.ManualImport(ctx, mine[0].DownloadID)
	}
	if err != nil {
		return verdict{State: "retry"}
	}
	packEps := map[int]bool{}
	var bad []string
	var outside, used, rejected int
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
		ok, why := probeFn(inst, it.Path)
		switch {
		case why == "unverified":
			if v.Unreach == "" {
				v.Unreach = it.Path
			}
		case !ok:
			bad = append(bad, why)
			continue
		default:
			v.Probed++
		}
		if !rejectionOK(it) {
			rejected++ // it will not import, so it cannot stand in for one of your episodes
			continue
		}
		for _, ep := range it.Episodes {
			packEps[ep.EpisodeNumber] = true
		}
	}
	eps, err := c.Episodes(ctx, j.SeriesID, j.Season)
	if err != nil {
		return verdict{State: "retry"}
	}
	live, err := liveOld(ctx, c, j.SeriesID, j.OldFileIDs)
	if err != nil {
		return verdict{State: "retry"}
	}
	liveSet := map[int]bool{}
	for _, id := range live {
		liveSet[id] = true
	}
	mapped := map[int]bool{}
	oldEps := map[int]bool{}
	for _, ep := range eps {
		if liveSet[ep.EpisodeFileID] {
			mapped[ep.EpisodeFileID] = true
			oldEps[ep.EpisodeNumber] = true
		}
	}
	var missing []int
	for n := range oldEps {
		if !packEps[n] {
			missing = append(missing, n)
		}
	}
	unmapped := len(live) - len(mapped)
	src := oldEps
	if len(src) == 0 {
		src = packEps // nothing of yours is left to replace; the pack just has to import
	}
	for n := range src {
		v.ExpectedEps = append(v.ExpectedEps, n)
	}
	sort.Ints(v.ExpectedEps)
	v.Expected = len(v.ExpectedEps)
	hold := func(msg string) verdict { v.State, v.Message = "hold", msg; return v }
	switch {
	case len(bad) > 0:
		return hold("failed probe: " + strings.Join(bad[:min(len(bad), 2)], "; "))
	case outside > 0:
		return hold(fmt.Sprintf("%d files are outside the download folder %s; not touching your files", outside, folder))
	case used == 0:
		return hold("no importable files found in the download folder")
	case len(eps) == 0:
		return hold("Sonarr returned no episodes for this season, so coverage cannot be checked")
	case unmapped > 0:
		var orphans []int
		for _, id := range live {
			if !mapped[id] {
				orphans = append(orphans, id)
			}
		}
		return hold(fmt.Sprintf("%d of your current files are not linked to any episode in Sonarr (file ids %v), so coverage cannot be confirmed; remove or relink them in Sonarr", unmapped, orphans[:min(len(orphans), 5)]))
	case len(missing) > 0:
		why := fmt.Sprintf("pack is missing episodes %v that you have today", missing)
		if rejected > 0 {
			why += fmt.Sprintf(" (%d pack files would not import: sample, unknown or unparseable)", rejected)
		}
		return hold(why)
	case len(v.ExpectedEps) == 0:
		return hold("could not work out which episodes this pack must supply (none of its files will import); nothing was changed")
	case v.Unreach != "" && !set.AllowUnverified:
		return hold(fmt.Sprintf("cannot verify: ffprobe cannot read %s. Mount the download directory and set the instance path fields, or enable 'Allow unverified' in Settings", v.Unreach))
	}
	v.State = "ok"
	return v
}

// replace deletes the user's old files and starts the import. It is the only place that deletes.
//
// Order matters because everything after the delete depends on Sonarr importing the pack:
//  1. wait for any other season's import on this instance (one at a time), then re-check Sonarr's health;
//  2. record "importing" and the time BEFORE deleting, so a crash or a cut-off request leaves a job that knows
//     the old files may be gone and resumes the import, instead of a "ready" job that looks untouched;
//  3. delete, then start the import; if starting it fails the job stays "importing" and is retried.
//
// It runs on a context detached from the caller: a browser disconnect or proxy timeout must not stop it
// between the delete and the import.
func (e *Engine) replace(ctx context.Context, inst store.Instance, c *sonarr.Client, j *store.Job, v verdict, queue []sonarr.QueueItem, how string) {
	defer e.instMu.lock(inst.ID)() // wait our turn first: time spent waiting must not eat the deadline below
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	label := fmt.Sprintf("%s S%02d", j.Title, j.Season)
	if time.Since(v.At) > 30*time.Second { // we may have waited behind another season; what was true then may not be now
		fresh := e.verify(ctx, inst, c, e.St.Settings(), j, v.Items)
		if fresh.State != "ok" {
			j.Status = "holding"
			j.Message = fresh.Message
			if fresh.State == "retry" {
				j.Message = "waiting: could not re-verify just before deleting; will try again"
			}
			_ = e.St.UpdateJob(*j)
			return
		}
		v = fresh
	}
	if block := e.pipelineBlock(ctx, inst, c, queue, *j); block != "" {
		j.Status, j.Message = "holding", "waiting: "+block
		_ = e.St.UpdateJob(*j)
		return
	}
	ids, err := liveOld(ctx, c, j.SeriesID, j.OldFileIDs)
	if err != nil {
		j.Status, j.Message = "holding", "could not read your current files: "+err.Error()
		_ = e.St.UpdateJob(*j)
		return
	}
	now := time.Now().Unix()
	j.Status, j.Deleted, j.ImportAt, j.DownloadID, j.Expected, j.ExpectedEps, j.ImportCmd = "importing", now, now, v.DownloadID, v.Expected, v.ExpectedEps, 0
	j.Message = fmt.Sprintf("deleting %d old files, then importing", len(ids))
	_ = e.St.UpdateJob(*j) // before the delete
	if len(ids) > 0 {
		if err := c.DeleteEpisodeFiles(ctx, ids); err != nil {
			if left, lerr := liveOld(ctx, c, j.SeriesID, ids); lerr == nil && len(left) == len(ids) {
				j.Status, j.Deleted, j.Message = "holding", 0, "delete failed, nothing was removed: "+err.Error()
				_ = e.St.UpdateJob(*j)
				return
			}
			// some files may be gone: keep the job as "importing" so the import still happens
			e.St.Event("error", fmt.Sprintf("%s: delete reported an error after possibly removing files: %v", label, err))
		}
	}
	id, err := c.ImportDownload(ctx, v.OutputPath, v.DownloadID)
	if err != nil {
		j.Message = fmt.Sprintf("old files deleted; the import has not started yet (%v). It will be retried", err)
		_ = e.St.UpdateJob(*j)
		e.St.Event("error", label+": "+j.Message)
		return
	}
	j.ImportCmd = id
	j.Message = fmt.Sprintf("old files deleted, import running (%d files probed)", v.Probed)
	if v.Unreach != "" {
		j.Message += "; some files could not be probed (allowed in Settings)"
	}
	_ = e.St.UpdateJob(*j)
	e.St.Event("info", fmt.Sprintf("%s: verified, deleted %d old files, import started (%s)", label, len(ids), how))
}

// finishImport follows a job whose old files are (or may be) gone until every expected episode has a new file.
// "Done" is judged by episode numbers, not file counts: a multi-episode file covers several episodes, and files
// Sonarr imported on its own before our delete must not be mistaken for the result of this import.
func (e *Engine) finishImport(ctx context.Context, inst store.Instance, c *sonarr.Client, j *store.Job, queue []sonarr.QueueItem) {
	label := fmt.Sprintf("%s S%02d", j.Title, j.Season)
	since := j.ImportAt
	if since == 0 {
		since = j.Deleted
	}
	if since == 0 {
		since = j.Updated
	}
	age := time.Since(time.Unix(since, 0))
	eps, err := c.Episodes(ctx, j.SeriesID, j.Season)
	if err != nil {
		return
	}
	old := map[int]bool{}
	for _, id := range j.OldFileIDs {
		old[id] = true
	}
	covered := map[int]bool{}
	for _, ep := range eps {
		if ep.EpisodeFileID != 0 && !old[ep.EpisodeFileID] {
			covered[ep.EpisodeNumber] = true
		}
	}
	want := j.ExpectedEps
	if len(want) == 0 {
		// A job recorded before episodes were tracked. Counting files would let episodes Sonarr imported earlier
		// satisfy it, so be strict instead: every episode of the season must have a new file. A season with
		// unaired episodes then ends up "failed" with a message pointing at Adopt, which is safe.
		for _, ep := range eps {
			want = append(want, ep.EpisodeNumber)
		}
	}
	got := 0
	for _, n := range want {
		if covered[n] {
			got++
		}
	}
	fail := func(msg string) {
		j.Status, j.Message = "failed", msg
		_ = e.St.UpdateJob(*j)
		e.St.Event("error", label+": "+msg)
	}
	if got >= len(want) {
		j.Status, j.Message = "done", fmt.Sprintf("%d episodes imported", got)
		_ = e.St.UpdateJob(*j)
		e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), 30*24*time.Hour, "done")
		e.St.Event("info", fmt.Sprintf("done %s: %d episodes imported", label, got))
		e.rescanSeries(ctx, inst, j.SeriesID)
		return
	}
	if j.ImportCmd == 0 { // the delete happened but the import was never started (or its start failed)
		mine := ownedItems(queue, j)
		if len(mine) == 0 {
			if age > 15*time.Minute {
				fail(fmt.Sprintf("the pack is no longer in Sonarr's queue and only %d of %d episodes have files. Use Needs attention to adopt or re-grab it", got, len(want)))
			}
			return
		}
		id, err := c.ImportDownload(ctx, mine[0].OutputPath, mine[0].DownloadID)
		if err != nil {
			j.Message = fmt.Sprintf("old files deleted; the import has not started yet (%v). It will be retried", err)
			_ = e.St.UpdateJob(*j)
			return
		}
		j.ImportCmd, j.ImportAt, j.Message = id, time.Now().Unix(), "import started"
		_ = e.St.UpdateJob(*j)
		return
	}
	cmd, cerr := c.Command(ctx, j.ImportCmd)
	finished := cerr != nil || cmd.Status == "completed" || cmd.Status == "failed" // an unknown command was pruned: judge by the files
	switch {
	case finished && age > importSettle:
		fail(fmt.Sprintf("the import finished but only %d of %d episodes have files. Your old files were already replaced; use Needs attention to retry or adopt the pack", got, len(want)))
	case !finished && age > importHardStop:
		fail(fmt.Sprintf("the import has not finished after %d hours; %d of %d episodes have files", int(age.Hours()), got, len(want)))
	}
}

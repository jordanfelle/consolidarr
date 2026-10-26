package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jordanfelle/consolidarr/internal/order"
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
	Kind        string           // why a hold happened (the hold* constants); only some kinds can never fix themselves
	Mapping     map[string][]int // pack file -> episode ids, when the pack's order differs from Sonarr's (see order)
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

// listPack asks Sonarr what it would import from a finished download. The download's own listing comes first because
// Sonarr can tell which series a tracked download belongs to; a bare folder listing cannot, so a release whose folder
// name it does not recognise comes back as "Unknown Series" and looks like it has nothing importable (a pack Sonarr
// itself would import was held for hours for exactly that). The folder is the fallback when the download has no id
// or its listing identifies nothing. Neither is filtered by series: that would list the library, not the download.
func listPack(ctx context.Context, c *sonarr.Client, folder, downloadID string) ([]sonarr.ManualImportItem, error) {
	if downloadID != "" {
		items, err := c.ManualImport(ctx, downloadID)
		if err != nil && folder == "" {
			return nil, err
		}
		// An id listing that fails falls back to the folder listing, which is what verification always used before;
		// the folder cannot show anything the download does not contain, and the insideFolder checks still apply.
		if err == nil {
			for _, it := range items {
				// Any identified series ends the search, even another one: Sonarr imports a tracked download as the series
				// that listing reports, so a folder parse that disagrees must never override it (that would verify one
				// series and import another). A listing for the wrong series simply holds.
				if it.Series.ID != 0 {
					return items, nil
				}
			}
			if folder == "" {
				return items, nil
			}
		}
	}
	if folder != "" {
		return c.ManualImportFolder(ctx, folder)
	}
	return nil, fmt.Errorf("the download has neither an id nor a folder to list")
}

// verify checks a finished download. It never changes anything.
var extrasDir = regexp.MustCompile(`(?i)^(samples?|extras?|featurettes?|bonus|behind[ ._-]the[ ._-]scenes|trailers?|specials?|subs?|proof|scenes|deleted[ ._-]scenes)$`)

// inExtras reports whether the file sits under a sample/extras-style folder inside the download folder.
func inExtras(path, folder string) bool {
	if folder == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(folder), filepath.Dir(path))
	if err != nil || rel == "." {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if extrasDir.MatchString(part) {
			return true
		}
	}
	return false
}

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
	items, err := listPack(ctx, c, folder, mine[0].DownloadID)
	if err != nil {
		return verdict{State: "retry"}
	}
	packEps := map[int]bool{}
	var bad []string
	var outside, used, rejected int
	var packFiles []order.File // every file of this series in the download, whatever season Sonarr put it in
	byPath := map[string]sonarr.ManualImportItem{}
	packByPath := map[string][]int{} // importable file -> the episode numbers Sonarr would file it under
	for _, it := range items {
		// Only files Sonarr itself would consider importing and that sit in the release proper: a sample, an unknown
		// series, an unparseable file or anything under an extras folder is never a stand-in for one of your episodes.
		if (it.Series.ID == 0 || it.Series.ID == j.SeriesID) && (folder == "" || insideFolder(it.Path, folder)) && rejectionOK(it) && !inExtras(it.Path, folder) {
			var ids []int
			for _, ep := range it.Episodes {
				ids = append(ids, ep.ID)
			}
			// Files named for another season (a multi-season pack) are not this season's business: a title that
			// recurs in two seasons must not look like a clash.
			if f := order.NewFile(it.Path, ids); (f.HasSxE && f.Season == j.Season) || (!f.HasSxE && it.SeasonNumber == j.Season) {
				packFiles = append(packFiles, f)
				byPath[it.Path] = it
			}
		}
		if it.SeasonNumber != j.Season || (it.Series.ID != 0 && it.Series.ID != j.SeriesID) {
			continue
		}
		// every file must live inside the download folder; library paths mean we are looking at the wrong thing
		if folder != "" && !insideFolder(it.Path, folder) {
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
			packByPath[it.Path] = append(packByPath[it.Path], ep.EpisodeNumber)
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
	protected, err := e.protectedEps(ctx, c, j, eps, liveSet)
	if err != nil {
		return verdict{State: "retry"}
	}
	src := oldEps
	if len(src) == 0 {
		src = packEps // nothing of yours is left to replace; the pack just has to import
	}
	for n := range src {
		v.ExpectedEps = append(v.ExpectedEps, n)
	}
	sort.Ints(v.ExpectedEps)
	v.Expected = len(v.ExpectedEps)
	hold := func(kind, msg string) verdict { v.State, v.Kind, v.Message = "hold", kind, msg; return v }
	// Does the pack's numbering disagree with Sonarr's? A file whose name carries an episode's title but that Sonarr
	// mapped elsewhere means a different ordering (aired/DVD/absolute, or a wrong scene mapping): importing it the
	// normal way would file episodes under the wrong names, even when the counts happen to add up.
	ordEps := orderEps(eps)
	conflicts := order.Conflicts(packFiles, ordEps)
	var remapWhy string
	var remapUnsure bool // a TVDB/TMDB lookup failed: the verdict may be wrong, so it must not lead to a rejection
	// Only a title that contradicts Sonarr's mapping starts a remap. A pack that merely lacks an episode does not:
	// searching it for "any file that could be that episode" is how the wrong episode gets placed.
	if len(conflicts) > 0 && set.RemapOrders && len(eps) > 0 && unmapped == 0 && outside == 0 {
		need := map[int]bool{}
		for _, ep := range eps {
			if liveSet[ep.EpisodeFileID] {
				need[ep.ID] = true
			}
		}
		plan, why, lookupFailed := e.planRemap(ctx, c, j, packFiles, ordEps, need)
		if why == "" {
			rv := e.remapVerdict(inst, set, v, plan, byPath)
			if rv.State == "ok" {
				if h := audioGate(inst, rv, mappedEpisodes(plan.Files, eps), requiredEnglish(protected, rv.ExpectedEps, isDub(j.Kind)), isDub(j.Kind)); h != nil {
					return *h
				}
			}
			return rv
		}
		remapWhy, remapUnsure = why, lookupFailed
	}
	orderHold := func() verdict {
		msg := "the pack's episode numbering disagrees with Sonarr's: " + strings.Join(conflicts[:min(len(conflicts), 2)], "; ")
		if remapWhy != "" {
			msg += ". It could not be matched by title either (" + remapWhy + ")"
		}
		if remapUnsure {
			return hold(holdOther, msg)
		}
		return hold(holdOrder, msg)
	}
	switch {
	case len(bad) > 0:
		return hold(holdProbe, "failed probe: "+strings.Join(bad[:min(len(bad), 2)], "; "))
	case outside > 0:
		return hold(holdOther, fmt.Sprintf("%d files are outside the download folder %s; not touching your files", outside, folder))
	case used == 0 && len(conflicts) > 0:
		return orderHold()
	case used == 0:
		return hold(holdEmpty, "no importable files found in the download folder")
	case len(eps) == 0:
		return hold(holdOther, "Sonarr returned no episodes for this season, so coverage cannot be checked")
	case unmapped > 0:
		var orphans []int
		for _, id := range live {
			if !mapped[id] {
				orphans = append(orphans, id)
			}
		}
		return hold(holdOther, fmt.Sprintf("%d of your current files are not linked to any episode in Sonarr (file ids %v), so coverage cannot be confirmed; remove or relink them in Sonarr", unmapped, orphans[:min(len(orphans), 5)]))
	case len(conflicts) > 0:
		return orderHold()
	case len(missing) > 0:
		why := fmt.Sprintf("pack is missing episodes %v that you have today", missing)
		if rejected > 0 {
			why += fmt.Sprintf(" (%d pack files would not import: sample, unknown or unparseable)", rejected)
		}
		if remapWhy != "" {
			why += "; matching by title did not help (" + remapWhy + ")"
		}
		if remapUnsure {
			return hold(holdOther, why)
		}
		return hold(holdMissing, why)
	case len(v.ExpectedEps) == 0:
		return hold(holdOther, "could not work out which episodes this pack must supply (none of its files will import); nothing was changed")
	case v.Unreach != "" && !set.AllowUnverified:
		return hold(holdOther, fmt.Sprintf("cannot verify: ffprobe cannot read %s. Mount the download directory and set the instance path fields, or enable 'Allow unverified' in Settings", v.Unreach))
	}
	if h := audioGate(inst, v, packByPath, requiredEnglish(protected, v.ExpectedEps, isDub(j.Kind)), isDub(j.Kind)); h != nil {
		return *h
	}
	v.State = "ok"
	return v
}

// requiredEnglish: the episodes whose new file must have English audio. Protected ones always; for a dub job, every
// episode it replaces.
func requiredEnglish(protected map[int]bool, expected []int, dub bool) map[int]bool {
	out := map[int]bool{}
	for n := range protected {
		out[n] = true
	}
	if dub {
		for _, n := range expected {
			out[n] = true
		}
	}
	return out
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
	if j.Kind != kindReview && time.Since(v.At) > 30*time.Second { // (a relabelled pack is not in Sonarr's queue: verify() cannot judge it) we may have waited behind another season; what was true then may not be now
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
	// A mapped import names the episodes itself and deletes nothing: Sonarr swaps each episode's file as it imports,
	// so an import that fails leaves your files exactly as they were.
	mapped := len(v.Mapping) > 0
	var ids []int
	if !mapped {
		var err error
		if ids, err = liveOld(ctx, c, j.SeriesID, j.OldFileIDs); err != nil {
			j.Status, j.Message = "holding", "could not read your current files: "+err.Error()
			_ = e.St.UpdateJob(*j)
			return
		}
	}
	e.inflight.Add(1) // counted before the draining check so a shutdown's WaitIdle cannot miss us
	defer e.inflight.Add(-1)
	if e.draining.Load() {
		return // shutting down: nothing has been recorded or deleted, the job is exactly as it was
	}
	e.kill("before-record")
	prev := *j
	now := time.Now().Unix()
	j.Status, j.Deleted, j.ImportAt, j.DownloadID, j.Expected, j.ExpectedEps, j.ImportCmd, j.Mapping = "importing", now, now, v.DownloadID, v.Expected, v.ExpectedEps, 0, v.Mapping
	j.Message = fmt.Sprintf("deleting %d old files, then importing", len(ids))
	if mapped {
		j.Message = fmt.Sprintf("importing %d files onto the episodes their titles identify", len(v.Mapping))
	}
	if err := e.St.UpdateJob(*j); err != nil { // the write-ahead record: without it a crash after the delete loses the job
		*j = prev
		e.St.Event("error", fmt.Sprintf("%s: could not record the replacement, so nothing was deleted: %v", label, err))
		return
	}
	e.kill("after-record")
	if len(ids) > 0 {
		if err := c.DeleteEpisodeFiles(ctx, ids); err != nil {
			if left, lerr := liveOld(ctx, c, j.SeriesID, ids); lerr == nil && len(left) == len(ids) {
				// If this write fails the job stays 'importing' with the delete recorded; finishImport then retries the
				// (idempotent) delete before it imports, so nothing is lost either way.
				j.Status, j.Deleted, j.Message = "holding", 0, "delete failed, nothing was removed: "+err.Error()
				_ = e.St.UpdateJob(*j)
				return
			}
			// some files may be gone: keep the job as "importing" so the import still happens
			e.St.Event("error", fmt.Sprintf("%s: delete reported an error after possibly removing files: %v", label, err))
		}
	}
	e.kill("after-delete")
	id, err := startImport(ctx, c, j, v.OutputPath, v.DownloadID)
	e.kill("after-import-start")
	if err != nil {
		j.Message = fmt.Sprintf("old files deleted; the import has not started yet (%v). It will be retried", err)
		if mapped {
			j.Message = fmt.Sprintf("the import has not started yet (%v). It will be retried; your files are untouched", err)
		}
		_ = e.St.UpdateJob(*j)
		e.St.Event("error", label+": "+j.Message)
		return
	}
	j.ImportCmd = id
	j.Message = fmt.Sprintf("old files deleted, import running (%d files probed)", v.Probed)
	if mapped {
		j.Message = fmt.Sprintf("import running with %s (%d files probed); your files are swapped one episode at a time", v.Message, v.Probed)
	}
	if v.Unreach != "" {
		j.Message += "; some files could not be probed (allowed in Settings)"
	}
	_ = e.St.UpdateJob(*j)
	e.St.Event("info", fmt.Sprintf("%s: verified, deleted %d old files, import started (%s)", label, len(ids), how))
}

// findImportCommand finds the DownloadedEpisodesScan this job's current import attempt started, so a restart that lost
// the command id does not start a second scan. It matches by folder or download client id and only counts a command
// that is still queued/running, or that was queued after this attempt began (importAt): an older command of the same
// folder (an earlier attempt, a manual scan) must never be mistaken for the current one, or a re-ask would be swallowed.
// findImportCommandFor picks the right search for the job: a mapped job's import is a ManualImport command.
func findImportCommandFor(cmds []sonarr.Command, j *store.Job, path, downloadID string) int {
	if len(j.Mapping) > 0 {
		return findMappedImportCommand(cmds, j.Mapping, j.ImportAt)
	}
	return findImportCommand(cmds, path, downloadID, j.ImportAt)
}

func findImportCommand(cmds []sonarr.Command, path, downloadID string, importAt int64) int {
	for i := len(cmds) - 1; i >= 0; i-- {
		c := cmds[i]
		if c.Name != "DownloadedEpisodesScan" || c.ID == 0 {
			continue
		}
		if !(path != "" && c.Body.Path == path) && !(downloadID != "" && strings.EqualFold(c.Body.DownloadClientID, downloadID)) {
			continue
		}
		switch strings.ToLower(c.Status) {
		case "queued", "started":
			return c.ID
		}
		if t, err := time.Parse(time.RFC3339Nano, c.Queued); err == nil && importAt > 0 && t.Unix() >= importAt-5 {
			return c.ID
		}
	}
	return 0
}

// insideFolder reports whether path is the folder or lies under it, after cleaning both, so "/dl/x/../../etc/y" is not
// "inside" /dl/x. Relative paths are never inside anything.
func insideFolder(path, folder string) bool {
	if !filepath.IsAbs(path) || !filepath.IsAbs(folder) {
		return false
	}
	p, f := filepath.Clean(path), filepath.Clean(folder)
	return p == f || strings.HasPrefix(p, strings.TrimRight(f, "/")+"/")
}

// kill is a test hook: a process death at this step. It is nil in production.
func (e *Engine) kill(stage string) {
	if e.killpoint != nil {
		e.killpoint(stage)
	}
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
	want, _, got := coverage(j, eps)
	fail := func(msg string) {
		j.Status, j.Message = "failed", msg
		e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), 7*24*time.Hour, "failed after delete") // never auto-regrab over a half-replaced season; first, so a crash between the two cannot leave a failed job unprotected
		_ = e.St.UpdateJob(*j)
		e.St.Event("error", label+": "+msg)
	}
	if len(want) > 0 && got >= len(want) { // with nothing known to wait for, only the age checks below can end the job
		j.Status, j.Message = "done", fmt.Sprintf("%d episodes imported", got)
		e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), 30*24*time.Hour, "done")
		_ = e.St.UpdateJob(*j)
		if j.Kind == kindReview {
			e.afterReviewImport(ctx, *j) // the parked torrent goes to its seed category and keeps seeding
		}
		e.St.Event("info", fmt.Sprintf("done %s: %d episodes imported", label, got))
		e.rescanSeries(ctx, inst, j.SeriesID)
		e.RunNow() // a season finished: the jobs waiting their turn behind it should not wait out the rest of the cycle
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
		if e.draining.Load() {
			return
		}
		// The process may have died after recording the delete but before (or while) Sonarr carried it out. Deleting is
		// idempotent and only ever targets files that are still there, so finish it now: an import against files that are
		// still present is rejected as "not an upgrade" and the job would end up failed.
		unlockInst := e.instMu.lock(inst.ID) // one season at a time, same as a replacement
		defer unlockInst()
		if left, lerr := liveOld(ctx, c, j.SeriesID, j.OldFileIDs); lerr != nil {
			return
		} else if len(left) > 0 && len(j.Mapping) == 0 { // a mapped import deletes nothing itself
			if derr := c.DeleteEpisodeFiles(ctx, left); derr != nil {
				j.Message = fmt.Sprintf("finishing the interrupted delete failed (%v). It will be retried", derr)
				_ = e.St.UpdateJob(*j)
				return
			}
			e.St.Event("info", fmt.Sprintf("%s: finished a delete that was interrupted by a restart (%d files)", label, len(left)))
		}
		// The process may also have died after Sonarr accepted the import but before we saved its id: adopt that command
		// instead of starting a second scan of the same download.
		if cmds, cerr := c.Commands(ctx); cerr != nil {
			return
		} else if id := findImportCommandFor(cmds, j, mine[0].OutputPath, mine[0].DownloadID); id != 0 {
			j.ImportCmd, j.Message = id, "import already started (found after a restart)"
			_ = e.St.UpdateJob(*j)
			return
		}
		id, err := startImport(ctx, c, j, mine[0].OutputPath, mine[0].DownloadID)
		e.kill("fi-after-import-start")
		if err != nil {
			if len(j.Mapping) > 0 {
				if age > mappedImportGiveUp {
					fail(fmt.Sprintf("the mapped import could not be started for %d minutes (%v); your files were not deleted. Retry now to verify the pack again", int(age.Minutes()), err))
					return
				}
				j.Message = fmt.Sprintf("the import has not started yet (%v). It will be retried; your files are untouched", err)
			} else {
				j.Message = fmt.Sprintf("old files deleted; the import has not started yet (%v). It will be retried", err)
			}
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
		// A host or Sonarr that was down can lose the command (or fail it) while the pack is still queued: ask again a
		// couple of times before handing the job to a human.
		if mine := ownedItems(queue, j); len(mine) > 0 && !e.draining.Load() {
			rk := fmt.Sprintf("reimport:%d", j.ID)
			if n, _ := strconv.Atoi(e.St.KV(rk)); n < 2 {
				e.St.SetKV(rk, strconv.Itoa(n+1))
				j.ImportCmd, j.ImportAt, j.Message = 0, time.Now().Unix(), fmt.Sprintf("the import finished with %d of %d episodes; asking Sonarr again (%d of 2)", got, len(want), n+1)
				_ = e.St.UpdateJob(*j)
				e.St.Event("warn", label+": "+j.Message)
				return
			}
		}
		if len(j.Mapping) > 0 {
			fail(fmt.Sprintf("the mapped import finished but only %d of %d episodes have a new file. Episodes Sonarr did not replace still have your old file; use Needs attention to retry or adopt the pack", got, len(want)))
			return
		}
		fail(fmt.Sprintf("the import finished but only %d of %d episodes have files. Your old files were already replaced; use Needs attention to retry or adopt the pack", got, len(want)))
	case !finished && age > importHardStop:
		fail(fmt.Sprintf("the import has not finished after %d hours; %d of %d episodes have files", int(age.Hours()), got, len(want)))
	}
}

// coverage works out which episodes the job is waiting for, which of them already have a file that is not one of
// the old files, and how many that is.
func coverage(j *store.Job, eps []sonarr.Episode) (want []int, covered map[int]bool, got int) {
	old := map[int]bool{}
	for _, id := range j.OldFileIDs {
		old[id] = true
	}
	covered = map[int]bool{}
	for _, ep := range eps {
		if ep.EpisodeFileID != 0 && !old[ep.EpisodeFileID] {
			covered[ep.EpisodeNumber] = true
		}
	}
	want = j.ExpectedEps
	if len(want) == 0 {
		// A job recorded before episodes were tracked. Counting files would let episodes Sonarr imported earlier
		// satisfy it, so be strict instead: every episode of the season must have a new file. A season with
		// unaired episodes then ends up "failed" with a message pointing at Adopt, which is safe.
		for _, ep := range eps {
			if aired(ep) {
				want = append(want, ep.EpisodeNumber)
			}
		}
	}
	for _, n := range want {
		if covered[n] {
			got++
		}
	}
	return want, covered, got
}

var trailingTag = regexp.MustCompile(`\[[^\]]*\]$`)

// groupMatches reports whether a Sonarr file's release group is the group at the end of the grabbed release title
// (a group that is only the tail of a longer hyphenated group still matches; the worst case is a "done" with nothing deleted)
// ("...H.264-D-Z0N3", "...-NTb[rartv]"). Quality tokens that follow a hyphen ("WEB-DL") are never a group.
func groupMatches(title, group string) bool {
	g := strings.ToLower(strings.TrimSpace(group))
	switch g {
	case "", "dl", "rip", "hd", "hdtv", "dtv", "ray", "r", "res", "hdr":
		return false
	}
	t := strings.ToLower(strings.TrimSpace(title))
	// Fansub-style names lead with the group: "[GetItTwisted] Show S01 [BD 1080p]...".
	if strings.HasPrefix(t, "["+g+"]") {
		return true
	}
	for trailingTag.MatchString(t) {
		t = strings.TrimSpace(trailingTag.ReplaceAllString(t, ""))
	}
	return strings.HasSuffix(t, "-"+g)
}

// importedBySonarr reports whether Sonarr already replaced the whole season with this pack itself (its own upgrade
// import of a finished pack, which can beat our verification on a small pack). Nothing is deleted on this path: it
// only lets a job that would be called "lost" end as "done". To avoid crediting unrelated upgrades it requires that
// the job recorded old files, that every wanted episode has a file that is not one of them, and that each of those
// files carries the release group of the grabbed pack. A Sonarr error is returned so the caller retries next cycle.
func (e *Engine) importedBySonarr(ctx context.Context, c *sonarr.Client, j *store.Job) (int, bool, error) {
	if j.Release == "" || len(j.OldFileIDs) == 0 {
		return 0, false, nil
	}
	eps, err := c.Episodes(ctx, j.SeriesID, j.Season)
	if err != nil {
		return 0, false, err
	}
	files, err := c.EpisodeFiles(ctx, j.SeriesID)
	if err != nil {
		return 0, false, err
	}
	group := map[int]string{} // file id -> Sonarr's release group
	for _, f := range files {
		group[f.ID] = f.ReleaseGroup
	}
	want, _, _ := coverage(j, eps)
	if len(want) == 0 {
		return 0, false, nil
	}
	old := map[int]bool{}
	for _, id := range j.OldFileIDs {
		old[id] = true
	}
	byEp := map[int]int{}
	for _, ep := range eps {
		byEp[ep.EpisodeNumber] = ep.EpisodeFileID
	}
	for _, n := range want {
		id := byEp[n]
		if id == 0 || old[id] || !groupMatches(j.Release, group[id]) {
			return 0, false, nil
		}
	}
	return len(want), true, nil
}

// aired reports whether an episode has already aired (specials, season 0, are not waited for).
func aired(ep sonarr.Episode) bool {
	if ep.SeasonNumber == 0 {
		return false
	}
	t, err := time.Parse(time.RFC3339, ep.AirDateUTC)
	return err != nil || !t.After(time.Now()) // an unknown air date counts as aired: fail towards waiting, not skipping
}

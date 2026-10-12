package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// Reason kinds for downloads that Sonarr will not import on its own.
const (
	KindNotUpgrade    = "not_upgrade"
	KindAlreadyDone   = "already_imported"
	KindIDMatch       = "id_match"
	KindNotInRelease  = "not_in_release"
	KindDestExists    = "destination_exists"
	KindSample        = "sample"
	KindUnknownSeries = "unknown_series"
	KindOther         = "other"
)

// Classify turns one of Sonarr's import warnings into a kind. Order matters: the specific cases first.
func Classify(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "not an upgrade") || strings.Contains(m, "not a custom format upgrade"):
		return KindNotUpgrade
	case strings.Contains(m, "already imported"):
		return KindAlreadyDone
	case strings.Contains(m, "destination already exists"):
		return KindDestExists
	case strings.Contains(m, "matched to series by id") || strings.Contains(m, "automatic import is not possible"):
		return KindIDMatch
	case strings.Contains(m, "was not found in the grabbed release"):
		return KindNotInRelease
	case strings.Contains(m, "unknown series"):
		return KindUnknownSeries
	case m == "sample" || strings.Contains(m, "sample"):
		return KindSample
	}
	return KindOther
}

// Hint is the plain-language suggestion shown next to a reason.
var Hint = map[string]string{
	KindNotUpgrade:    "Your existing files are as good as this pack, so Sonarr won't replace them. Adopt it to swap them for the pack, or remove the pack.",
	KindAlreadyDone:   "These episodes were already imported (often from another download). Safe to remove the pack.",
	KindDestExists:    "A file with the same name already exists. Safe to remove the pack, or adopt it to replace the existing files.",
	KindIDMatch:       "Sonarr matched the release by id and won't import it automatically. Use Retry after fixing the match in Sonarr, or remove the pack.",
	KindNotInRelease:  "Some expected episodes aren't in this release.",
	KindUnknownSeries: "Sonarr can't tell which series this is.",
	KindSample:        "Sample files can be ignored.",
	KindOther:         "See Sonarr for details.",
}

type Reason struct {
	Kind    string `json:"kind"`
	Count   int    `json:"count"`
	Example string `json:"example"`
	Hint    string `json:"hint"`
}

// Attention is one season whose downloaded pack is stuck in Sonarr's queue and not owned by an active job.
type Attention struct {
	InstID     int64    `json:"instId"`
	Instance   string   `json:"instance"`
	SeriesID   int      `json:"seriesId"`
	Series     string   `json:"series"`
	Season     int      `json:"season"`
	Pack       string   `json:"pack"`
	Indexer    string   `json:"indexer"`
	SizeGB     float64  `json:"sizeGB"`
	Items      int      `json:"items"`
	QueueIDs   []int    `json:"queueIds"`
	DownloadID string   `json:"downloadId"`
	OutputPath string   `json:"outputPath"`
	PackRank   int      `json:"packRank"` // quality rank of the downloaded pack (0 = unknown)
	State      string   `json:"state"`
	Reasons    []Reason `json:"reasons"`
	HaveFiles  int      `json:"haveFiles"`
	Aired      int      `json:"aired"`
}

func stuck(q sonarr.QueueItem) bool {
	switch q.TrackedState {
	case "importBlocked", "importFailed":
		return true
	case "importPending":
		return q.TrackedStatus == "warning" || q.TrackedStatus == "error"
	}
	return false
}

// attentionFor groups an instance's stuck queue items by series+season, skipping seasons an active job owns.
func (e *Engine) attentionFor(ctx context.Context, inst store.Instance, owned map[string]bool) ([]Attention, error) {
	c := client(inst)
	queue, err := c.Queue(ctx, time.Minute)
	if err != nil {
		return nil, err
	}
	type key struct{ s, n int }
	groups := map[key]*Attention{}
	counts := map[key]map[string]*Reason{}
	for _, q := range queue {
		if q.SeasonNumber == 0 || !stuck(q) || owned[skipKey(inst.ID, q.SeriesID, q.SeasonNumber)] {
			continue
		}
		k := key{q.SeriesID, q.SeasonNumber}
		g := groups[k]
		if g == nil {
			g = &Attention{InstID: inst.ID, Instance: inst.Name, SeriesID: q.SeriesID, Season: q.SeasonNumber, Pack: q.Title,
				Indexer: q.Indexer, DownloadID: q.DownloadID, OutputPath: q.OutputPath, State: q.TrackedState}
			groups[k] = g
			counts[k] = map[string]*Reason{}
		}
		g.Items++
		if rk := q.Quality.Rank(); rk > g.PackRank {
			g.PackRank = rk
		}
		g.QueueIDs = append(g.QueueIDs, q.ID)
		g.SizeGB = maxf(g.SizeGB, q.Size/1e9)
		seen := map[string]bool{} // count each file once per reason, however many warnings repeat it
		for _, sm := range q.StatusMessages {
			for _, m := range sm.Messages {
				kd := Classify(m)
				r := counts[k][kd]
				if r == nil {
					r = &Reason{Kind: kd, Example: m, Hint: Hint[kd]}
					counts[k][kd] = r
				}
				if !seen[kd] {
					seen[kd] = true
					r.Count++
				}
			}
		}
		if q.TrackedState == "importBlocked" {
			g.State = "importBlocked"
		}
	}
	if len(groups) == 0 {
		return nil, nil
	}
	titles := map[int]string{}
	if series, err := c.Series(ctx); err == nil {
		for _, s := range series {
			titles[s.ID] = s.Title
		}
	}
	var out []Attention
	for k, g := range groups {
		g.Series = titles[k.s]
		if g.Series == "" {
			g.Series = fmt.Sprintf("series %d", k.s)
		}
		for _, r := range counts[k] {
			g.Reasons = append(g.Reasons, *r)
		}
		sort.Slice(g.Reasons, func(i, j int) bool { return g.Reasons[i].Count > g.Reasons[j].Count })
		if files, err := c.EpisodeFiles(ctx, k.s); err == nil {
			for _, f := range files {
				if f.SeasonNumber == k.n {
					g.HaveFiles++
				}
			}
		}
		if eps, err := c.Episodes(ctx, k.s, k.n); err == nil {
			for _, ep := range eps {
				_ = ep
				g.Aired++
			}
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Series < out[j].Series || (out[i].Series == out[j].Series && out[i].Season < out[j].Season)
	})
	return out, nil
}

// Attention lists stuck packs across every enabled instance. An unreachable instance is reported, not fatal.
func (e *Engine) Attention(ctx context.Context) ([]Attention, []string) {
	insts, _ := e.St.Instances()
	owned := e.ownedSeasons()
	var out []Attention
	errs := []string{}
	for _, inst := range insts {
		if !inst.Enabled {
			continue
		}
		g, err := e.attentionFor(ctx, inst, owned)
		if err != nil {
			errs = append(errs, inst.Name+": "+err.Error())
			continue
		}
		out = append(out, g...)
	}
	return out, errs
}

// ownedSeasons are seasons a job is responsible for: unfinished ones, and failed ones whose old files were already
// deleted (their pack is then the only copy, so it must never be offered for removal).
func (e *Engine) ownedSeasons() map[string]bool {
	owned := map[string]bool{}
	if js, err := e.St.Jobs(false, 100000); err == nil {
		for _, j := range js {
			if j.Status == "reserving" || j.Status == "downloading" || j.Status == "ready" || j.Status == "holding" || j.Status == "importing" || (j.Status == "failed" && j.Deleted > 0) {
				owned[skipKey(j.InstID, j.SeriesID, j.Season)] = true
			}
		}
	}
	return owned
}

func (e *Engine) findAttention(ctx context.Context, inst store.Instance, seriesID, season int) (*Attention, error) {
	gs, err := e.attentionFor(ctx, inst, e.ownedSeasons())
	if err != nil {
		return nil, err
	}
	for i := range gs {
		if gs[i].SeriesID == seriesID && gs[i].Season == season {
			return &gs[i], nil
		}
	}
	return nil, store.ErrNotFound
}

// ErrDowngrade is returned by Adopt when the stuck pack is lower quality than the files it would replace.
var ErrDowngrade = errors.New("needs force: the pack is lower quality than the files it would replace")

// Adopt turns a stuck download into a normal job: it records the season's current files as the ones to replace
// and runs one verification pass right away. From there it follows every rule a grabbed job does (probe,
// coverage, dry run / manual approval, the import-health gate), so nothing is deleted without those checks.
func (e *Engine) Adopt(ctx context.Context, inst store.Instance, seriesID, season int, force bool) (store.Job, error) {
	g, err := e.findAttention(ctx, inst, seriesID, season)
	if err != nil {
		return store.Job{}, err
	}
	files, err := client(inst).EpisodeFiles(ctx, seriesID)
	if err != nil {
		return store.Job{}, err
	}
	var old []int
	cur := 0
	for _, f := range files {
		if f.SeasonNumber == season {
			old = append(old, f.ID)
			if rk := f.Quality.Rank(); rk > cur {
				cur = rk
			}
		}
	}
	if !force && g.PackRank > 0 && g.PackRank < cur {
		return store.Job{}, ErrDowngrade
	}
	id, err := e.St.AddJob(store.Job{InstID: inst.ID, SeriesID: seriesID, Season: season, Title: g.Series, OldFileIDs: old,
		Release: g.Pack, Indexer: g.Indexer, SizeGB: g.SizeGB, DownloadID: g.DownloadID})
	if err != nil {
		return store.Job{}, err // store.ErrDuplicate when the season already has an active job
	}
	e.St.Event("info", fmt.Sprintf("adopted the stuck download for %s S%02d (%d current files to replace)", g.Series, season, len(old)))
	return e.AdvanceJob(ctx, id)
}

// AdvanceJob re-evaluates one job immediately (Adopt, Retry now) and returns its new state.
func (e *Engine) AdvanceJob(ctx context.Context, id int64) (store.Job, error) {
	job, err := e.St.Job(id)
	if err != nil {
		return store.Job{}, err
	}
	if job.Status == "done" || job.Status == "reserving" {
		return job, fmt.Errorf("job is %s", job.Status)
	}
	inst, err := e.St.Instance(job.InstID)
	if err != nil {
		return job, err
	}
	c := client(inst)
	queue, err := c.Queue(ctx, time.Minute)
	if err != nil {
		return job, err
	}
	var flipErr error
	if job.Status == "lost" || job.Status == "failed" { // give it another go
		func() {
			defer e.jobMu.lock(id)() // deferred so a panic cannot leave the job locked forever
			if cur, err := e.St.Job(id); err == nil && (cur.Status == "lost" || cur.Status == "failed") {
				if cur.Deleted > 0 && len(cur.Mapping) > 0 {
					// A mapped import deleted nothing and its mapping is only as good as the files it named: verify the
					// pack again from scratch instead of resuming on a stale mapping.
					// Deleted stays: some episodes may already have been swapped, so the job stays protected from rejection.
					cur.Status, cur.Mapping, cur.ImportCmd, cur.ImportAt = "downloading", nil, 0, 0
				} else if cur.Deleted > 0 {
					cur.Status, cur.ImportCmd, cur.ImportAt = "importing", 0, time.Now().Unix() // the old files are gone: resume the import, never start over
					e.St.SetKV(fmt.Sprintf("reimport:%d", id), "0")                             // a revived job gets its automatic re-asks back
				} else {
					cur.Status = "downloading"
				}
				cur.Message = ""
				flipErr = e.St.UpdateJob(cur) // fails if a newer active job already owns the season
			}
		}()
	}
	if flipErr != nil {
		return job, fmt.Errorf("could not restart this job: %w", flipErr)
	}
	e.advance(ctx, inst, c, e.St.Settings(), job, queue)
	return e.St.Job(id)
}

// RetryImport asks Sonarr to import a stuck download again.
func (e *Engine) RetryImport(ctx context.Context, inst store.Instance, seriesID, season int) (int, error) {
	g, err := e.findAttention(ctx, inst, seriesID, season)
	if err != nil {
		return 0, err
	}
	id, err := client(inst).ImportDownload(ctx, g.OutputPath, g.DownloadID)
	if err == nil {
		e.St.Event("info", fmt.Sprintf("retrying the import of %s S%02d", g.Series, season))
	}
	return id, err
}

type RemoveOpts struct {
	Blocklist   bool `json:"blocklist"`   // never grab this release again
	DeleteFiles bool `json:"deleteFiles"` // also delete the download from the client (library files are untouched)
	Research    bool `json:"research"`    // then search the season again
}

// RemovePack takes a stuck pack out of Sonarr's queue.
func (e *Engine) RemovePack(ctx context.Context, inst store.Instance, seriesID, season int, o RemoveOpts) (removed int, searched bool, err error) {
	g, err := e.findAttention(ctx, inst, seriesID, season)
	if err != nil {
		return 0, false, err
	}
	c := client(inst)
	if err := c.RemoveQueue(ctx, g.QueueIDs, o.DeleteFiles, o.Blocklist); err != nil {
		return 0, false, err
	}
	e.St.Event("info", fmt.Sprintf("removed %d queue items for %s S%02d (blocklist=%v deleteFiles=%v)", len(g.QueueIDs), g.Series, season, o.Blocklist, o.DeleteFiles))
	if o.Research {
		if _, err := c.SeasonSearch(ctx, seriesID, season); err != nil {
			return len(g.QueueIDs), false, err
		}
		e.St.Event("info", fmt.Sprintf("searching %s S%02d again", g.Series, season))
		searched = true
	}
	return len(g.QueueIDs), searched, nil
}

// RejectJob throws away a job's bad pack: remove it from Sonarr's queue and the client, blocklist the
// release, optionally search again, and mark the job failed. It refuses once the old files have been deleted,
// because then the pack is the only copy of the season.
func (e *Engine) RejectJob(ctx context.Context, jobID int64, research bool) (store.Job, error) {
	defer e.jobMu.lock(jobID)()
	j, err := e.St.Job(jobID)
	if err != nil {
		return store.Job{}, err
	}
	if j.Status == "importing" || j.Status == "done" || j.Status == "reserving" {
		return j, fmt.Errorf("job is %s; nothing to reject", j.Status)
	}
	inst, err := e.St.Instance(j.InstID)
	if err != nil {
		return j, err
	}
	msg := "pack rejected and blocklisted; your files were not touched"
	if research {
		msg += "; searching for another"
	}
	return j, e.rejectBody(ctx, client(inst), &j, research, time.Hour, msg) // an hour lets a re-search settle
}

// rejectBody discards a job's pack (removed from the queue and the download client, release blocklisted) and ends the
// job as failed with msg. The caller holds the job's lock. The season is skipped for skip so the same pack is not
// grabbed straight back. It refuses when old files were already deleted: then the pack is the only copy.
func (e *Engine) rejectBody(ctx context.Context, c *sonarr.Client, j *store.Job, research bool, skip time.Duration, msg string) error {
	if j.Status == "importing" || j.Status == "done" || j.Status == "reserving" {
		return fmt.Errorf("job is %s; nothing to reject", j.Status)
	}
	if j.Deleted > 0 && len(j.Mapping) > 0 {
		return fmt.Errorf("an import onto your episodes was already started, so this pack may be the only copy of some of them; use Retry or Adopt instead of rejecting it")
	}
	if j.Deleted > 0 {
		return fmt.Errorf("your old files were already deleted, so this pack is the only copy of the season; use Retry or Adopt instead of rejecting it")
	}
	queue, err := c.Queue(ctx, time.Minute)
	if err != nil {
		return err
	}
	var ids []int
	for _, q := range ownedItems(queue, j) { // this job's own download, not every grab for the season
		ids = append(ids, q.ID)
	}
	if len(ids) > 0 {
		if err := c.RemoveQueue(ctx, ids, true, true); err != nil {
			return err
		}
	}
	if research {
		if _, err := c.SeasonSearch(ctx, j.SeriesID, j.Season); err != nil {
			return err
		}
	}
	j.Status, j.Message = "failed", msg
	_ = e.St.UpdateJob(*j)
	e.St.SetKV(holdKey(j.ID), "")
	e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), skip, "rejected")
	e.St.Event("info", fmt.Sprintf("rejected the pack for %s S%02d (research=%v): %s", j.Title, j.Season, research, msg))
	return nil
}

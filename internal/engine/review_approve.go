package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jordanfelle/consolidarr/internal/order"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// ReviewFile is one file of a parked pack and the episode Approve would import it as.
type ReviewFile struct {
	Path    string `json:"path"`
	Episode int    `json:"episode"`
	Title   string `json:"title"`
	Why     string `json:"why"` // how the episode was identified
}

// ReviewPlan is what Approve would do, shown to the human first.
type ReviewPlan struct {
	OK      bool         `json:"ok"`
	Reason  string       `json:"reason,omitempty"` // when not OK: why consolidarr cannot map it by itself
	Folder  string       `json:"folder"`
	Files   []ReviewFile `json:"files"`
	Skipped []string     `json:"skipped,omitempty"` // videos that are not episodes (openings, trailers, ...)
}

var nonEpisode = regexp.MustCompile(`(?i)(^|[^a-z0-9])(nc[ ._-]?op|nc[ ._-]?ed|creditless|trailer|pv|cm|menu|sample|promo|teaser|opening|ending)([^a-z0-9]|$)`)

func reviewReady(j store.Job) error {
	switch {
	case j.Kind != kindReview:
		return errors.New("this is not a relabelled pack")
	case j.Status != "holding":
		return fmt.Errorf("the pack is %s; only a finished pack awaiting review can be approved", j.Status)
	case j.DownloadID == "":
		return errors.New("the pack is not bound to a download yet")
	}
	return nil
}

// PlanReview works out, without changing anything, how a parked pack would be imported onto the season: every file
// is placed on an episode only by hard evidence in its name (S0N where N is this season, or an episode title of this
// season), every episode of the season needs exactly one file, and every placed file must probe and carry English audio.
// Anything less and the human imports it by hand in Sonarr.
func (e *Engine) PlanReview(ctx context.Context, inst store.Instance, j store.Job) (ReviewPlan, map[string][]int, verdict, error) {
	var plan ReviewPlan
	v := verdict{Items: nil, At: time.Now()}
	if e.Qbit == nil {
		return plan, nil, v, errors.New("CONSOLIDARR_QBIT_URL is not set")
	}
	if err := reviewReady(j); err != nil {
		return plan, nil, v, err
	}
	tr, err := e.Qbit.Info(ctx, j.DownloadID)
	if err != nil {
		return plan, nil, v, err
	}
	if tr.Progress < 1 {
		return plan, nil, v, errors.New("the pack has not finished downloading")
	}
	if tr.AddedOn > 0 && time.Unix(tr.AddedOn, 0).Before(time.Unix(j.Created, 0).Add(-addedSlack)) {
		return plan, nil, v, errors.New("this torrent was in qBittorrent before consolidarr grabbed it; import it by hand")
	}
	plan.Folder = tr.ContentPath
	fail := func(format string, a ...any) (ReviewPlan, map[string][]int, verdict, error) {
		plan.Reason = fmt.Sprintf(format, a...)
		return plan, nil, v, nil
	}
	c := client(inst)
	items, err := c.ManualImportFolder(ctx, tr.ContentPath)
	if err != nil {
		return plan, nil, v, err
	}
	eps, err := c.Episodes(ctx, j.SeriesID, j.Season)
	if err != nil {
		return plan, nil, v, err
	}
	if len(eps) == 0 {
		return fail("Sonarr lists no episodes for this season")
	}
	target := orderEps(eps)
	byNumber := map[int]sonarr.Episode{}
	for _, ep := range eps {
		byNumber[ep.EpisodeNumber] = ep
	}
	mapping := map[string][]int{}
	packByPath := map[string][]int{}
	covered := map[int]string{}
	for _, it := range items {
		if !insideFolder(it.Path, tr.ContentPath) || inExtras(it.Path, tr.ContentPath) || !videoExt[strings.ToLower(filepath.Ext(it.Path))] {
			continue
		}
		if nonEpisode.MatchString(filepath.Base(it.Path)) { // an opening, a trailer, a menu: never an episode
			plan.Skipped = append(plan.Skipped, filepath.Base(it.Path))
			continue
		}
		f := order.NewFile(it.Path, nil)
		var ep sonarr.Episode
		var how string
		switch {
		case f.HasSxE && f.Season == j.Season && !f.Multi:
			var ok bool
			if ep, ok = byNumber[f.Number]; !ok {
				return fail("%s names episode %d, which Sonarr's season %d does not have", filepath.Base(it.Path), f.Number, j.Season)
			}
			how = fmt.Sprintf("named S%02dE%02d", j.Season, f.Number)
		case f.HasSxE:
			return fail("%s is numbered season %d, not %d; consolidarr will not guess (import it by hand)", filepath.Base(it.Path), f.Season, j.Season)
		default:
			t, ok := order.TitleAnywhere(it.Path, target)
			if !ok {
				return fail("%s carries no season number and no episode title of this season, so it cannot be placed", filepath.Base(it.Path))
			}
			ep = byNumber[t.Number]
			how = "named \"" + t.Title + "\""
		}
		if prev, dup := covered[ep.EpisodeNumber]; dup {
			return fail("two files claim episode %d: %s and %s", ep.EpisodeNumber, prev, filepath.Base(it.Path))
		}
		covered[ep.EpisodeNumber] = filepath.Base(it.Path)
		mapping[it.Path] = []int{ep.ID}
		packByPath[it.Path] = []int{ep.EpisodeNumber}
		plan.Files = append(plan.Files, ReviewFile{Path: it.Path, Episode: ep.EpisodeNumber, Title: ep.Title, Why: how})
	}
	var missing []int
	for _, ep := range eps {
		if _, ok := covered[ep.EpisodeNumber]; !ok {
			missing = append(missing, ep.EpisodeNumber)
		}
	}
	if len(missing) > 0 {
		return fail("the pack has no file for episode(s) %v of this season", missing)
	}
	sort.Slice(plan.Files, func(a, b int) bool { return plan.Files[a].Episode < plan.Files[b].Episode })
	bad := ""
	for p := range mapping {
		if ok, why := probeFn(inst, p); !ok && why != "unverified" {
			bad = filepath.Base(p) + ": " + why
			break
		}
	}
	if bad != "" {
		return fail("a file failed to probe (%s)", bad)
	}
	var nums []int
	required := map[int]bool{}
	for _, ep := range eps {
		nums = append(nums, ep.EpisodeNumber)
		required[ep.EpisodeNumber] = true
	}
	sort.Ints(nums)
	v = verdict{State: "ok", Mapping: mapping, OutputPath: tr.ContentPath, Probed: len(mapping), Expected: len(nums), ExpectedEps: nums,
		Message: fmt.Sprintf("%d files onto the episodes their names identify", len(mapping)), At: time.Now()}
	if h := audioGate(inst, v, packByPath, required, true); h != nil {
		return fail("%s", h.Message)
	}
	plan.OK = true
	return plan, mapping, v, nil
}

// ApproveReview imports a parked pack onto its season after a human approved the plan. The import names the episodes
// itself (Sonarr swaps each episode's file as it imports; nothing is deleted first) and COPIES the files, so the
// torrent keeps seeding; when it has finished the torrent is moved to its seed category. Never deletes a torrent.
func (e *Engine) ApproveReview(ctx context.Context, inst store.Instance, j0 store.Job) error {
	defer e.jobMu.lock(j0.ID)()
	if e.draining.Load() {
		return errors.New("consolidarr is restarting; try again in a minute")
	}
	j, err := e.St.Job(j0.ID)
	if err != nil {
		return err
	}
	if e.St.Settings().DryRun {
		return errors.New("dry run is on in Settings: nothing is imported")
	}
	plan, _, v, err := e.PlanReview(ctx, inst, j)
	if err != nil {
		return err
	}
	if !plan.OK {
		return errors.New("consolidarr cannot map this pack by itself: " + plan.Reason + ". Import it by hand in Sonarr (Manual Import)")
	}
	c := client(inst)
	queue, err := c.Queue(ctx, time.Minute)
	if err != nil {
		return err
	}
	e.St.SetKV(reviewHashKey(j.ID), j.DownloadID) // replace() clears the job's download id (the pack is not in Sonarr's queue); the torrent is moved when the import is done
	v.DownloadID = ""                             // Sonarr does not track this download: the files are listed from their folder
	e.replace(ctx, inst, c, &j, v, queue, "approved by hand")
	return nil
}

func reviewHashKey(id int64) string { return fmt.Sprintf("reviewhash:%d", id) }

// afterReviewImport moves the torrent of an imported relabelled pack to its seed category.
func (e *Engine) afterReviewImport(ctx context.Context, j store.Job) {
	hash := e.St.KV(reviewHashKey(j.ID))
	if hash == "" || e.Qbit == nil {
		return
	}
	tr, err := e.Qbit.Info(ctx, hash)
	if err != nil {
		e.St.Event("warn", fmt.Sprintf("%s S%02d: imported, but could not move its torrent to the seed category: %v", j.Title, j.Season, err))
		return
	}
	j.DownloadID = hash // replace() cleared it: the pack was never in Sonarr's queue
	where, err := e.shelve(ctx, j, tr.Category)
	if err != nil {
		e.St.Event("warn", fmt.Sprintf("%s S%02d: imported, but could not move its torrent to the seed category: %v", j.Title, j.Season, err))
		return
	}
	e.St.Event("info", fmt.Sprintf("%s S%02d: the imported pack's torrent was kept (%s)", j.Title, j.Season, where))
}

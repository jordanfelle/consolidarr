package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	Path        string `json:"path"`
	Episode     int    `json:"episode"`
	Title       string `json:"title"`
	Why         string `json:"why"`         // how the episode was identified
	PackQuality string `json:"packQuality"` // what Sonarr reads the pack file as
	Replaces    string `json:"replaces"`    // the library file this import overwrites ("" when the episode has none yet)
}

// ReviewPlan is what Approve would do, shown to the human first.
type ReviewPlan struct {
	OK      bool         `json:"ok"`
	Reason  string       `json:"reason,omitempty"` // when not OK: why consolidarr cannot map it by itself
	Folder  string       `json:"folder"`
	Files   []ReviewFile `json:"files"`
	Skipped []string     `json:"skipped,omitempty"` // videos that are not episodes (openings, trailers, ...)
	Token   string       `json:"token,omitempty"`   // identity of this exact mapping: Approve only runs the plan the human saw
}

var nonEpisode = regexp.MustCompile(`(?i)(^|[^a-z0-9])(nc[ ._-]?op|nc[ ._-]?ed|creditless|trailer|pv|cm|menu|sample|promo|teaser|preview|opening|ending)([^a-z0-9]|$)`)

// ReviewStateError is a refusal caused by the pack or job (not an outage): the API answers 409 for it, 502 for others.
type ReviewStateError struct{ msg string }

func (e *ReviewStateError) Error() string { return e.msg }

func stateErr(format string, a ...any) error { return &ReviewStateError{fmt.Sprintf(format, a...)} }

func reviewReady(j store.Job) error {
	switch {
	case j.Kind != kindReview:
		return stateErr("this is not a relabelled pack")
	case j.Status != "holding":
		return stateErr("the pack is %s; only a finished pack awaiting review can be approved", j.Status)
	case j.DownloadID == "":
		return stateErr("the pack is not bound to a download yet")
	}
	return nil
}

func qualityName(raw json.RawMessage) string {
	var q sonarr.Quality
	if json.Unmarshal(raw, &q) != nil {
		return ""
	}
	return q.Quality.Name
}

func planToken(mapping map[string][]int) string {
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%v\n", k, mapping[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// PlanReview works out, without changing anything, how a parked pack would be imported onto the season: every file
// is placed on an episode only by hard evidence in its name (S0N where N is this season, or an episode title of this
// season), every episode of the season needs exactly one file, and every placed file must probe, carry English audio and
// have a quality Sonarr can read. Anything less and the human imports it by hand in Sonarr.
func (e *Engine) PlanReview(ctx context.Context, inst store.Instance, j store.Job) (ReviewPlan, map[string][]int, verdict, error) {
	var plan ReviewPlan
	v := verdict{Items: nil, At: time.Now()}
	if e.Qbit == nil {
		return plan, nil, v, stateErr("CONSOLIDARR_QBIT_URL is not set")
	}
	if err := reviewReady(j); err != nil {
		return plan, nil, v, err
	}
	tr, err := e.Qbit.Info(ctx, j.DownloadID)
	if err != nil {
		return plan, nil, v, err
	}
	if tr.Progress < 1 {
		return plan, nil, v, stateErr("the pack has not finished downloading")
	}
	if tr.AddedOn > 0 && time.Unix(tr.AddedOn, 0).Before(time.Unix(j.Created, 0).Add(-addedSlack)) {
		return plan, nil, v, stateErr("this torrent was in qBittorrent before consolidarr grabbed it; import it by hand")
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
	files, err := c.EpisodeFiles(ctx, j.SeriesID)
	if err != nil {
		return plan, nil, v, err
	}
	libFile := map[int]sonarr.EpisodeFile{}
	for _, f := range files {
		libFile[f.ID] = f
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
		qn := qualityName(it.Quality)
		if qn == "" || strings.EqualFold(qn, "unknown") {
			return fail("Sonarr cannot read the quality of %s (it lists it as Unknown), so importing it could mark your episode Unknown; import it by hand", filepath.Base(it.Path))
		}
		replaces := ""
		if lf, ok := libFile[ep.EpisodeFileID]; ok && ep.EpisodeFileID != 0 {
			replaces = fmt.Sprintf("%s (%s, %.1f GB)", filepath.Base(lf.Path), lf.Quality.Quality.Name, float64(lf.Size)/1e9)
		}
		covered[ep.EpisodeNumber] = filepath.Base(it.Path)
		mapping[it.Path] = []int{ep.ID}
		packByPath[it.Path] = []int{ep.EpisodeNumber}
		plan.Files = append(plan.Files, ReviewFile{Path: it.Path, Episode: ep.EpisodeNumber, Title: ep.Title, Why: how, PackQuality: qn, Replaces: replaces})
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
	for p := range mapping {
		if ok, why := probeFn(inst, p); !ok && why != "unverified" {
			return fail("a file failed to probe (%s: %s)", filepath.Base(p), why)
		}
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
	plan.Token = planToken(mapping)
	return plan, mapping, v, nil
}

// ApproveReview imports a parked pack onto its season after a human approved the plan with this token. The import names
// the episodes itself and COPIES the files, so the torrent keeps seeding. Sonarr then swaps each episode's file; with no
// recycle bin the replaced library file is gone for good, which is why the plan lists every file it will overwrite. When
// the import is done the torrent is moved to its seed category. Never deletes a torrent.
func (e *Engine) ApproveReview(ctx context.Context, inst store.Instance, j0 store.Job, token string) error {
	defer e.jobMu.lock(j0.ID)()
	if e.draining.Load() {
		return stateErr("consolidarr is restarting; try again in a minute")
	}
	j, err := e.St.Job(j0.ID)
	if err != nil {
		return err
	}
	if e.St.Settings().DryRun {
		return stateErr("dry run is on in Settings: nothing is imported")
	}
	plan, _, v, err := e.PlanReview(ctx, inst, j)
	if err != nil {
		return err
	}
	if !plan.OK {
		return stateErr("consolidarr cannot map this pack by itself: %s. Import it by hand in Sonarr (Manual Import)", plan.Reason)
	}
	if token == "" || token != plan.Token {
		return stateErr("the pack or the season changed since you looked at it; review it again")
	}
	c := client(inst)
	queue, err := c.Queue(ctx, time.Minute)
	if err != nil {
		return err
	}
	// "Done" is judged by episodes that now have a file which is not one of the old files: the old files are whatever the
	// season has today, not what it had when the pack was grabbed days ago.
	files, err := c.EpisodeFiles(ctx, j.SeriesID)
	if err != nil {
		return err
	}
	j.OldFileIDs = nil
	for _, f := range files {
		if f.SeasonNumber == j.Season {
			j.OldFileIDs = append(j.OldFileIDs, f.ID)
		}
	}
	if err := e.St.SetOldFiles(j.ID, j.OldFileIDs); err != nil {
		return err
	}
	e.St.SetKV(reviewHashKey(j.ID), j.DownloadID) // replace() clears the job's download id (the pack is not in Sonarr's queue); the torrent is moved when the import is done
	v.DownloadID = ""                             // Sonarr does not track this download: the files are listed from their folder
	e.replace(ctx, inst, c, &j, v, queue, "approved by hand")
	// replace() can return without starting (another import in progress, Sonarr's import backlog, a restart): say so.
	after, err := e.St.Job(j.ID)
	if err != nil {
		return err
	}
	if after.Status != "importing" {
		msg := after.Message
		if msg == "" {
			msg = "the import did not start"
		}
		return stateErr("the import did not start: %s. Nothing was changed; approve again in a minute", msg)
	}
	if after.ImportCmd == 0 { // Sonarr refused the import command: nothing was imported, so put the job back for another try
		e.revertReviewImport(&after, "Sonarr did not accept the import and nothing was changed")
		return stateErr("Sonarr did not accept the import; nothing was changed. Try again")
	}
	return nil
}

func reviewHashKey(id int64) string { return fmt.Sprintf("reviewhash:%d", id) }

// revertReviewImport puts a relabelled-pack job back to "awaiting review" when its import did not start or finished
// only partly. A pack that is not in Sonarr's queue cannot be re-imported by the queue-based retry, so the human decides:
// Review again and approve. Nothing was deleted by consolidarr.
func (e *Engine) revertReviewImport(j *store.Job, why string) {
	j.Status, j.Deleted, j.ImportAt, j.ImportCmd, j.Mapping = "holding", 0, 0, 0, nil
	j.DownloadID = e.St.KV(reviewHashKey(j.ID))
	j.Message = "awaiting your review: " + why + ". Review it again and approve to retry"
	_ = e.St.UpdateJob(*j)
	e.St.Event("warn", fmt.Sprintf("%s S%02d: %s", j.Title, j.Season, j.Message))
}

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

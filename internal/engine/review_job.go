package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jordanfelle/consolidarr/internal/qbit"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// DefaultReviewCategory is the qBittorrent category a relabelled pack is moved to. No arr app watches it, so none of
// them imports the download; only a human decides what happens next.
const DefaultReviewCategory = "consolidarr-review"

const (
	reviewBindWait   = 2 * time.Hour    // how long a grab may take to show up in Sonarr's queue (and be told apart from others)
	reviewGoneChecks = 3                // consecutive "qBittorrent does not have it" answers before the job is closed
	addedSlack       = 30 * time.Second // a torrent added longer than this before the grab was not added by the grab
)

func reviewKey(id int64) string     { return fmt.Sprintf("reviewcheck:%d", id) }
func reviewGoneKey(id int64) string { return fmt.Sprintf("reviewgone:%d", id) }

// advanceReview drives a relabelled-pack job through qBittorrent (reached via qui) instead of Sonarr's queue, which the
// pack leaves the moment it is parked:
//  1. bind the job to its download (exact release title, never another job's);
//  2. move the torrent out of every arr's category so nothing imports it;
//  3. as soon as the torrent's file list exists, check the names against the season (reviewCheck): a clear mismatch
//     (Tenchi's "2nd Season" grabbed for season 4) sets that download aside (moved to the seed category and stopped, never deleted);
//  4. when it has finished, stop and wait for a human.
//
// It never touches your library. It never deletes a torrent, and only moves or stops one this grab added.
func (e *Engine) advanceReview(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings, j store.Job, queue []sonarr.QueueItem) {
	label := fmt.Sprintf("%s S%02d", j.Title, j.Season)
	finish := func(status, msg string, backOff time.Duration) {
		j.Status, j.Message = status, msg
		if backOff > 0 {
			e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), backOff, "reviewed")
			e.St.Skip("dub:"+skipKey(j.InstID, j.SeriesID, j.Season), backOff, "reviewed")
		}
		e.St.Event("info", label+": "+msg)
		_ = e.St.UpdateJob(j)
	}
	if j.DownloadID == "" {
		id := e.bindReview(&j, queue)
		if id == "" {
			if time.Since(time.Unix(j.Created, 0)) > reviewBindWait {
				e.St.Event("warn", label+": could not tell which Sonarr download is this grab (none new, or several). If a pack for this season is in Sonarr's queue, check it before it imports: it was NOT parked")
				finish("failed", "could not bind to its download in Sonarr's queue (none appeared, or several were new); nothing was parked or changed by consolidarr. Check Sonarr's queue for this season", 24*time.Hour)
			}
			return
		}
		j.DownloadID = id
		if err := e.St.UpdateJob(j); err != nil {
			return
		}
	}
	tr, err := e.Qbit.Info(ctx, j.DownloadID)
	if errors.Is(err, qbit.ErrNotFound) {
		// One empty answer can be a proxy or qBittorrent restart: close the job only when it is repeatedly gone.
		n, _ := strconv.Atoi(e.St.KV(reviewGoneKey(j.ID)))
		e.St.SetKV(reviewGoneKey(j.ID), strconv.Itoa(n+1))
		if n+1 >= reviewGoneChecks {
			finish("done", "the torrent is no longer in qBittorrent (removed by hand, or Sonarr imported it first); nothing was deleted by us", reviewBackOff)
		}
		return
	}
	if err != nil {
		return // qui or qBittorrent hiccup: look again next cycle
	}
	e.St.SetKV(reviewGoneKey(j.ID), "")
	if tr.AddedOn > 0 && time.Unix(tr.AddedOn, 0).Before(time.Unix(j.Created, 0).Add(-addedSlack)) {
		// Sonarr re-used a torrent that was already there (an earlier copy, a cross-seed). It is not ours to move or remove.
		finish("failed", "this release was already in qBittorrent before consolidarr grabbed it; left exactly as it was", reviewBackOff)
		return
	}
	// A wrong pack is only ever set aside: moved to the seed category and, if it is still downloading, stopped. Never deleted.
	setAside := func(current, detail string) {
		dest, err := e.shelve(ctx, j, current)
		if err != nil {
			e.warnOften(fmt.Sprintf("review-shelve:%d", j.ID), label+": could not set the wrong pack aside in qBittorrent: "+err.Error())
			return
		}
		if tr.Progress < 1 {
			if err := e.Qbit.Stop(ctx, j.DownloadID); err == nil {
				dest += ", download stopped"
			} else {
				dest += ", but it could not be stopped and keeps downloading: pause it in qBittorrent"
				e.St.Event("warn", label+": could not stop the wrong pack: "+err.Error())
			}
		}
		finish("failed", "set aside: this pack is not this season ("+detail+"). The torrent and its data were kept ("+dest+"); your files were not touched", reviewBackOff)
	}
	if v, d := splitVerdict(e.St.KV(reviewKey(j.ID))); v == reviewMismatch {
		setAside(tr.Category, d) // a retry of a set-aside job: do not park it again
		return
	}
	cat := set.ReviewCategory
	if cat == "" {
		cat = DefaultReviewCategory
	}
	if tr.Category != cat {
		if err := e.Qbit.EnsureCategory(ctx, cat); err != nil {
			e.warnOften(fmt.Sprintf("review-cat:%d", j.ID), label+": could not prepare the qBittorrent category "+cat+": "+err.Error())
			return
		}
		if e.St.KV(reviewCatKey(j.ID)) == "" {
			e.St.SetKV(reviewCatKey(j.ID), tr.Category) // where it came from, so a rejected pack goes to the matching seed category
		}
		if err := e.Qbit.Park(ctx, j.DownloadID, cat); err != nil {
			e.warnOften(fmt.Sprintf("review-park:%d", j.ID), label+": could not move the torrent out of the arr's category: "+err.Error())
			return
		}
		e.St.Event("info", fmt.Sprintf("%s: moved the torrent from %q to %q so no arr app imports it", label, tr.Category, cat))
	}
	verdict, detail := splitVerdict(e.St.KV(reviewKey(j.ID)))
	if verdict == "" {
		files, err := e.Qbit.Files(ctx, j.DownloadID)
		if err != nil {
			return
		}
		if len(files) > 0 { // the file list is empty until qBittorrent has the metadata
			names := make([]string, len(files))
			for i, f := range files {
				names[i] = f.Name
			}
			target, err := c.Episodes(ctx, j.SeriesID, j.Season)
			if err != nil {
				return
			}
			all, err := c.SeriesEpisodes(ctx, j.SeriesID)
			if err != nil {
				return
			}
			verdict, detail = reviewCheck(names, orderEps(target), orderEps(all), j.Season)
			e.St.SetKV(reviewKey(j.ID), verdict+"|"+detail)
		}
	}
	if verdict == reviewMismatch {
		setAside(cat, detail)
		return
	}
	if tr.Progress < 1 {
		prog := int(tr.Progress * 100)
		switch {
		case j.Status != "downloading" || j.Progress != prog:
			j.Status, j.Progress = "downloading", prog // an unchanged job is not rewritten, so `updated` means "last progress"
			_ = e.St.UpdateJob(j)
		case time.Since(time.Unix(j.Updated, 0)) > stallAfter:
			finish("failed", fmt.Sprintf("the download has not moved for %d hours (%d%%); your files were not touched. The torrent is still in qBittorrent (%s): Reject pack removes it", int(stallAfter.Hours()), prog, cat), reviewBackOff)
		}
		return
	}
	how := "its file names could not settle which season it is (" + detail + ")"
	if verdict == reviewMatch {
		how = "its file names say it is this season (" + detail + ")"
	}
	msg := fmt.Sprintf("awaiting your review: %s. Nothing has been imported or deleted. Check it, then import it in Sonarr (Manual Import onto this season) or reject it here. Folder: %s", how, tr.ContentPath)
	if j.Status != "holding" || j.Message != msg {
		j.Status, j.Message = "holding", msg
		e.St.Event("warn", label+": "+msg)
		_ = e.St.UpdateJob(j)
	}
}

// bindReview finds the Sonarr queue item of this job's grab: by its exact release title, else the single download of
// this series and season added after the job was created, and never one an active job already owns. It never falls
// back to "the biggest download of that season": an unrelated grab must not be adopted (and then possibly thrown away).
func (e *Engine) bindReview(j *store.Job, queue []sonarr.QueueItem) string {
	all, err := e.St.Jobs(true, 100000) // active only: a finished job's hash must not block a later grab of the same release
	if err != nil || j.Release == "" {
		return ""
	}
	owners := map[string]bool{}
	for _, o := range all {
		if o.ID != j.ID && o.DownloadID != "" {
			owners[strings.ToLower(o.DownloadID)] = true
		}
	}
	// Sonarr names a queue item by the torrent's own name, which is rarely the release title the indexer showed
	// ("...Ryo-Ohki S01 [Blu-ray]..." vs "...Ryo-Ohki OVA 1 v2 [BD]..."), so an exact title is a bonus, not a
	// requirement: otherwise the one download of this series and season that appeared after the grab was made.
	var fresh []string
	for _, q := range queue {
		if q.SeriesID != j.SeriesID || q.SeasonNumber != j.Season || q.DownloadID == "" || owners[strings.ToLower(q.DownloadID)] {
			continue
		}
		if q.Title == j.Release {
			return q.DownloadID
		}
		if t, err := time.Parse(time.RFC3339, q.Added); err == nil && t.After(time.Unix(j.Created, 0).Add(-addedSlack)) {
			seen := false
			for _, id := range fresh {
				seen = seen || strings.EqualFold(id, q.DownloadID)
			}
			if !seen {
				fresh = append(fresh, q.DownloadID)
			}
		}
	}
	if len(fresh) == 1 {
		return fresh[0]
	}
	return "" // none yet, or several: never guess which one is ours
}

// RejectReview sets a relabelled pack aside on a human's say-so: the torrent is moved to the matching seed category and
// kept (a torrent with data is never deleted), the job ends failed and the season is left alone for a week.
// Sonarr's queue is not touched: the pack is no longer in it, and guessing at a "same season" item could remove another
// download.
func (e *Engine) RejectReview(ctx context.Context, j store.Job) (store.Job, error) {
	if e.Qbit == nil {
		return j, errors.New("CONSOLIDARR_QBIT_URL is not set, so consolidarr cannot reach this torrent; remove it in qBittorrent by hand")
	}
	if j.DownloadID == "" {
		// The grab has not shown up in Sonarr's queue yet. Ending the job now would strand it: the torrent would land
		// later with nobody to park or remove it. Wait for it to bind (or for the job to give up by itself).
		return j, errors.New("the download has not appeared in Sonarr's queue yet, so there is nothing to remove; try again in a minute")
	}
	shelved := ""
	{
		tr, err := e.Qbit.Info(ctx, j.DownloadID)
		switch {
		case errors.Is(err, qbit.ErrNotFound):
		case err != nil:
			return j, err
		case tr.AddedOn > 0 && time.Unix(tr.AddedOn, 0).Before(time.Unix(j.Created, 0).Add(-addedSlack)):
			// not ours: leave the torrent alone and only end the job
		default:
			dest, err := e.shelve(ctx, j, tr.Category)
			if err != nil {
				return j, err
			}
			if tr.Progress < 1 { // a rejected pack must not go on downloading the rest of itself
				if stopErr := e.Qbit.Stop(ctx, j.DownloadID); stopErr == nil {
					dest += ", download stopped"
				} else {
					dest += ", but it could not be stopped and keeps downloading: pause it in qBittorrent"
					e.St.Event("warn", fmt.Sprintf("%s S%02d: could not stop the rejected pack: %v", j.Title, j.Season, stopErr))
				}
			}
			shelved = " The torrent and its data were kept (" + dest + ")."
		}
	}
	j.Status, j.Message = "failed", "pack rejected."+shelved+" Your files were not touched"
	e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), reviewBackOff, "rejected")
	e.St.Skip("dub:"+skipKey(j.InstID, j.SeriesID, j.Season), reviewBackOff, "rejected")
	e.St.SetKV(holdKey(j.ID), "")
	e.St.Event("info", fmt.Sprintf("rejected the relabelled pack for %s S%02d", j.Title, j.Season))
	return j, e.St.UpdateJob(j)
}

func reviewCatKey(id int64) string { return fmt.Sprintf("reviewcat:%d", id) }

// keepDownload is what happens to a download when its Sonarr queue item is removed (a reject, "remove pack"): it is
// never deleted. With qBittorrent access it is moved to the seed category matching its own (btn -> seed-btn), so Sonarr
// does not re-track it; without access it stays exactly where it is. It returns where it is, for the event log, and
// whether the torrent is now OUT of Sonarr's reach (in a seed category). Only then may the release be blocklisted:
// Sonarr treats a blocklisted download as failed and, with the download client's "Remove Failed Downloads" on (it is on
// for this qBittorrent client), removes it from the client itself.
func (e *Engine) keepDownload(ctx context.Context, hash string) (where string, safe bool) {
	if hash == "" || e.Qbit == nil {
		return "left in the download client, as no qBittorrent access is set", false
	}
	tr, err := e.Qbit.Info(ctx, hash)
	if err != nil {
		return "left in the download client (" + err.Error() + ")", false
	}
	if strings.HasPrefix(tr.Category, "seed-") {
		return "already in " + tr.Category, true
	}
	if tr.Category == "" {
		return "has no category", false
	}
	dest := "seed-" + tr.Category
	if !e.Qbit.HasCategory(ctx, dest) {
		return "left in category " + tr.Category + " (no " + dest + " category)", false
	}
	if err := e.Qbit.Park(ctx, hash, dest); err != nil {
		return "left in category " + tr.Category + " (" + err.Error() + ")", false
	}
	return "moved to " + dest, true
}

// shelve sets a wrong or rejected pack aside. A torrent with data is NEVER deleted (operator rule): it goes to the seed
// category matching the one it came from (btn -> seed-btn) and stays there. If that category does not exist, it is left
// in the review category instead. It returns where the torrent now is.
func (e *Engine) shelve(ctx context.Context, j store.Job, currentCat string) (string, error) {
	orig := e.St.KV(reviewCatKey(j.ID))
	if orig == "" {
		orig = currentCat
	}
	dest := orig
	if !strings.HasPrefix(dest, "seed-") {
		dest = "seed-" + dest
	}
	if orig == "" || !e.Qbit.HasCategory(ctx, dest) {
		return "left in category " + currentCat, nil
	}
	if err := e.Qbit.Park(ctx, j.DownloadID, dest); err != nil {
		return "", err
	}
	return "moved to " + dest, nil
}

func splitVerdict(s string) (verdict, detail string) {
	v, d, _ := strings.Cut(s, "|")
	return v, d
}

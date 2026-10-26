package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jordanfelle/consolidarr/internal/qbit"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// DefaultReviewCategory is the qBittorrent category a relabelled pack is moved to. No arr app watches it, so none of
// them imports the download; only a human decides what happens next.
const DefaultReviewCategory = "consolidarr-review"

func reviewKey(id int64) string { return fmt.Sprintf("reviewcheck:%d", id) }

// advanceReview drives a relabelled-pack job through qBittorrent (reached via qui) instead of Sonarr's queue, which the
// pack leaves the moment it is parked:
//  1. move the torrent out of every arr's category so nothing imports it;
//  2. as soon as the torrent's file list exists, check the names against the season (reviewCheck): a clear mismatch
//     (Tenchi's "2nd Season" grabbed for season 4) throws the download away at once;
//  3. when it has finished, stop and wait for a human.
//
// Nothing here imports or deletes your library files.
func (e *Engine) advanceReview(ctx context.Context, inst store.Instance, c *sonarr.Client, set store.Settings, j store.Job, queue []sonarr.QueueItem) {
	label := fmt.Sprintf("%s S%02d", j.Title, j.Season)
	if j.DownloadID == "" {
		mine := ownedItems(queue, &j)
		if len(mine) == 0 {
			return // not in Sonarr's queue yet; the reservation logic above handles one that never arrives
		}
		j.DownloadID = mine[0].DownloadID
		if err := e.St.UpdateJob(j); err != nil {
			return
		}
	}
	finish := func(status, msg string, backOff time.Duration) {
		j.Status, j.Message = status, msg
		if backOff > 0 {
			e.St.Skip(skipKey(j.InstID, j.SeriesID, j.Season), backOff, "reviewed")
			e.St.Skip("dub:"+skipKey(j.InstID, j.SeriesID, j.Season), backOff, "reviewed")
		}
		e.St.Event("info", label+": "+msg)
		_ = e.St.UpdateJob(j)
	}
	tr, err := e.Qbit.Info(ctx, j.DownloadID)
	if errors.Is(err, qbit.ErrNotFound) {
		finish("done", "the torrent is no longer in qBittorrent (removed by hand); nothing was deleted by us", reviewBackOff)
		return
	}
	if err != nil {
		return // qui or qBittorrent hiccup: look again next cycle
	}
	cat := set.ReviewCategory
	if cat == "" {
		cat = DefaultReviewCategory
	}
	if tr.Category != cat {
		if err := e.Qbit.EnsureCategory(ctx, cat); err != nil {
			e.St.Event("warn", label+": could not prepare the qBittorrent category "+cat+": "+err.Error())
			return
		}
		if err := e.Qbit.Park(ctx, j.DownloadID, cat); err != nil {
			e.St.Event("warn", label+": could not move the torrent out of the arr's category: "+err.Error())
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
		if err := e.Qbit.Remove(ctx, j.DownloadID, true); err != nil {
			e.St.Event("warn", label+": could not remove the wrong pack from qBittorrent: "+err.Error())
			return
		}
		finish("failed", "discarded: this pack is not this season ("+detail+"); your files were not touched", reviewBackOff)
		return
	}
	if tr.Progress < 1 {
		if prog := int(tr.Progress * 100); j.Status != "downloading" || j.Progress != prog {
			j.Status, j.Progress = "downloading", prog
			_ = e.St.UpdateJob(j)
		}
		return
	}
	how := "its file names could not settle which season it is"
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

func splitVerdict(s string) (verdict, detail string) {
	v, d, _ := strings.Cut(s, "|")
	return v, d
}

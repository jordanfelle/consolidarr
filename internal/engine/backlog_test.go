package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

type sm = struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

// A season pack is one queue record per episode. 23 downloads were reported as "311 waiting to import" and held every
// replacement behind a gate that could never open: most of those records were packs Sonarr refuses to import
// ("not an upgrade", "already imported") and packs consolidarr itself was holding.
func TestBacklogCountsDownloadsNotRecordsAndIgnoresWhatNeverDrains(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	var q []sonarr.QueueItem
	rec := func(dl string, n int, msgs ...string) {
		for i := 0; i < n; i++ {
			r := sonarr.QueueItem{SeriesID: 50, SeasonNumber: 1, DownloadID: dl, Status: "completed", TrackedState: "importPending"}
			if len(msgs) > 0 {
				r.StatusMessages = []sm{{Title: "x", Messages: msgs}}
			}
			q = append(q, r)
		}
	}
	for i := 0; i < 8; i++ { // 8 genuinely pending packs of 30 episodes each = 240 records
		rec(fmt.Sprintf("real%d", i), 30)
	}
	rec("empty", 12, "One or more episodes expected in this release were not imported or missing from the release")
	for i := 0; i < 11; i++ { // packs Sonarr refuses to import
		rec(fmt.Sprintf("stuck%d", i), 30, "Not a Custom Format upgrade for existing episode file(s)")
	}
	rec("done", 20, "Episode file already imported at 10/10/2026")
	// another consolidarr job's pack, waiting for this very gate
	other, err := v.st.AddJob(store.Job{InstID: 1, SeriesID: 60, Season: 2, Title: "Other", OldFileIDs: []int{1}, Release: "r", DownloadID: "OURS"})
	if err != nil {
		t.Fatal(err)
	}
	_ = other
	rec("ours", 12)
	q[len(q)-1].DownloadID = "OURS"
	for i := len(q) - 12; i < len(q); i++ {
		q[i].DownloadID = "OURS"
	}
	if got, files := v.e.importBacklog(q, j); got != 8 || files != 240 {
		t.Fatalf("want 8 real pending downloads / 240 files, got %d / %d", got, files)
	}
	if blocked := v.e.pipelineBlock(t.Context(), v.inst, v.c, q, j); blocked != "" {
		t.Fatalf("8 pending downloads must not block (limit %d): %q", maxImportBacklog, blocked)
	}
}

func TestBacklogStillBlocksWhenSonarrReallyIsBackedUp(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	var q []sonarr.QueueItem
	for i := 0; i < maxImportBacklog+5; i++ {
		q = append(q, sonarr.QueueItem{SeriesID: 50, SeasonNumber: 1, DownloadID: fmt.Sprintf("p%d", i), Status: "completed", TrackedState: "importPending"})
	}
	if blocked := v.e.pipelineBlock(t.Context(), v.inst, v.c, q, j); blocked == "" {
		t.Fatal("a genuinely backed-up import queue must still hold the delete")
	}
}

// Twenty big packs are under the download limit but are still a backlog: the file count catches them.
func TestBacklogAlsoCountsFilesSoBigPacksStillHold(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	var q []sonarr.QueueItem
	for i := 0; i < 20; i++ {
		for e := 0; e < 22; e++ {
			q = append(q, sonarr.QueueItem{SeriesID: 50, SeasonNumber: 1, DownloadID: fmt.Sprintf("pack%d", i), Status: "completed", TrackedState: "importPending"})
		}
	}
	if blocked := v.e.pipelineBlock(t.Context(), v.inst, v.c, q, j); blocked == "" {
		t.Fatal("440 pending episode files must hold the delete even though that is only 20 downloads")
	}
}

// A file or release name in a status title ("Sample", "Unknown Series ...") must not mark a draining download stuck.
func TestBacklogIgnoresStatusTitles(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	q := []sonarr.QueueItem{{SeriesID: 50, SeasonNumber: 1, DownloadID: "free", Status: "completed", TrackedState: "importPending",
		StatusMessages: []sm{{Title: "Free.Samples.S01E01.mkv", Messages: []string{"Waiting to import"}}}}}
	if got, _ := v.e.importBacklog(q, j); got != 1 {
		t.Fatalf("a draining download whose file name says 'Samples' still counts, got %d", got)
	}
}

// An unreadable command queue holds every replacement on the instance; the event log must say so (once), not only
// each job's own message.
func TestUnreadableCommandQueueIsLoggedOnceAsAnEvent(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	v.fake.srv.Close() // every call now fails
	for i := 0; i < 3; i++ {
		if msg := v.e.pipelineBlock(t.Context(), v.inst, v.c, nil, j); msg == "" {
			t.Fatal("must be blocked while Sonarr is unreachable")
		}
	}
	evs, _ := v.st.Events(50)
	n := 0
	for _, e := range evs {
		if e.Level == "warn" && strings.Contains(e.Msg, "command queue cannot be read") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one warn event, got %d", n)
	}
}

// A season that just finished must wake the scheduler: the jobs waiting behind it ("one season at a time") used to sit
// out the rest of the 3-minute cycle with a message about a season that was already done.
func TestFinishingASeasonWakesTheScheduler(t *testing.T) {
	v := setup(t, nil)
	j := v.job(t, time.Time{})
	j.Status, j.ImportCmd = "importing", 42
	_ = v.st.UpdateJob(j)
	v.fake.seasonEps = map[int]int{1: 99, 2: 98}
	select {
	case <-v.e.runNow:
		t.Fatal("nothing should be pending before the season finishes")
	default:
	}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "done" {
		t.Fatalf("want done, got %s %q", got.Status, got.Message)
	}
	select {
	case <-v.e.runNow:
	default:
		t.Fatal("finishing a season must request an immediate cycle")
	}
}

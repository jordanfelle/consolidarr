package engine

import (
	"fmt"
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
	for i := 0; i < 8; i++ { // 8 genuinely pending packs of 40 episodes each = 320 records
		rec(fmt.Sprintf("real%d", i), 40)
	}
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
	if got := v.e.importBacklog(q, j); got != 8 {
		t.Fatalf("want 8 real pending downloads, got %d", got)
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

package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// A finished job's hash must not stop a later grab of the same release from binding (then it is never parked and
// Sonarr imports it as the wrong season).
func TestAFinishedJobsHashDoesNotBlockALaterGrabOfTheSameRelease(t *testing.T) {
	q := newFakeQbit(t, "btn", 0.1, "p/01.mkv", "p/02.mkv", "p/03.mkv")
	v := setup(t, []int{1, 2, 3})
	v.e.Qbit = q.client()
	old, err := v.st.AddJob(store.Job{InstID: 1, SeriesID: 7, Season: 2, Title: "Other", Status: "failed", Kind: kindReview, DownloadID: "d"})
	if err != nil {
		t.Fatal(err)
	}
	_ = old
	j := v.job(t, time.Now().Add(-time.Minute))
	_, _ = v.st.Exec(`update jobs set status='downloading', kind=?, release=?, download_id='' where id=?`, kindReview, "The Pack", j.ID)
	j, _ = v.st.Job(j.ID)
	mine := sonarr.QueueItem{SeriesID: 1, SeasonNumber: 3, Title: "The Pack", DownloadID: "d"}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{mine})
	if got := v.status(t, j.ID); got.DownloadID != "d" {
		t.Fatalf("a finished job's hash blocked the bind: %q", got.DownloadID)
	}
}

// Rejecting before the grab has appeared must not strand it (it would land later with nobody to park or remove it).
func TestRejectingAnUnboundReviewJobIsRefused(t *testing.T) {
	q := newFakeQbit(t, "btn", 0)
	v := setup(t, []int{1, 2, 3})
	v.e.Qbit = q.client()
	j := v.job(t, time.Now().Add(-time.Minute))
	_, _ = v.st.Exec(`update jobs set status='downloading', kind=?, download_id='' where id=?`, kindReview, j.ID)
	_, _ = v.st.SaveInstance(storeInstance(v))
	if _, err := v.e.RejectJob(context.Background(), j.ID, false); err == nil {
		t.Fatal("rejecting a job that has no download yet must be refused")
	}
	if got := v.status(t, j.ID); got.Status == "failed" {
		t.Fatal("the job must stay active so the grab is still parked when it lands")
	}
}

func TestAStalledReviewDownloadBacksTheSeasonOffAndSaysHowToClearIt(t *testing.T) {
	q := newFakeQbit(t, "consolidarr-review", 0.4, "p/01.mkv", "p/02.mkv", "p/03.mkv")
	v, j := reviewEnv(t, q)
	_, _ = v.st.Exec(`update jobs set progress=40, updated=? where id=?`, time.Now().Add(-stallAfter-time.Hour).Unix(), j.ID)
	j, _ = v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	got := v.status(t, j.ID)
	if got.Status != "failed" || !strings.Contains(got.Message, "Reject pack removes it") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
	if !v.st.Skipped(skipKey(1, 1, 3)) || !v.st.Skipped("dub:"+skipKey(1, 1, 3)) {
		t.Fatal("a stalled pack must not be re-picked at once (it would collide with the leftover torrent)")
	}
}

// Scene numbering is not TVDB's: files named S02 inside a pack that is TVDB season 3 must not be deleted.
func TestSceneS02NamingForSeason3IsNotGroundsToDelete(t *testing.T) {
	q := newFakeQbit(t, "btn", 0.2, "Show.S02/Show.S02E01.mkv.!qB", "Show.S02/Show.S02E02.mkv.!qB", "Show.S02/Show.S02E03.mkv.!qB")
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if strings.Contains(q.log(), "remove") {
		t.Fatalf("removed on numbering alone: %q", q.log())
	}
}

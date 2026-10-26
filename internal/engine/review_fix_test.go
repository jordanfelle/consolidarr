package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

func storeInstance(v *env) store.Instance {
	return store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true}
}

// Found by the hostile review: a correct S01-named pack must never be thrown away for season 3.
func TestAnS01NamedPackIsKeptForAHumanNotDeleted(t *testing.T) {
	q := newFakeQbit(t, "btn", 1, "Show.S01/Show.S01E01.mkv", "Show.S01/Show.S01E02.mkv", "Show.S01/Show.S01E03.mkv")
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if strings.Contains(q.log(), "remove") {
		t.Fatalf("an S01-named relabelled pack was removed: %q", q.log())
	}
	if got := v.status(t, j.ID); got.Status != "holding" || !strings.Contains(got.Message, "could not settle") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
}

// A release Sonarr re-used because it was already in qBittorrent (an earlier copy, a cross-seed) is not ours to move.
func TestATorrentThatWasThereBeforeTheGrabIsLeftAlone(t *testing.T) {
	q := newFakeQbit(t, "btn", 1, "Pack/Show S01 - 01 - Mother and Children.mkv", "Pack/Show S01 - 02 - A Quiet Morning.mkv", "Pack/Show S01 - 03 - The Long Walk Home.mkv")
	q.torrent["added_on"] = time.Now().Add(-30 * 24 * time.Hour).Unix()
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if q.log() != "" {
		t.Fatalf("a pre-existing torrent must not be moved or removed: %q", q.log())
	}
	if got := v.status(t, j.ID); got.Status != "failed" || !strings.Contains(got.Message, "already in qBittorrent") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
}

// Rejecting a parked pack must take it out of qBittorrent, not just mark the job failed.
func TestRejectingAParkedPackRemovesTheTorrent(t *testing.T) {
	q := newFakeQbit(t, "consolidarr-review", 1, "p/01.mkv", "p/02.mkv", "p/03.mkv")
	v, j := reviewEnv(t, q)
	_, _ = v.st.SaveInstance(storeInstance(v))
	v.st.SetKV(reviewCatKey(j.ID), "btn") // where it was parked from
	if _, err := v.e.RejectJob(context.Background(), j.ID, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q.log(), "park:seed-btn") || strings.Contains(q.log(), "remove") {
		t.Fatalf("qBittorrent changes: %q", q.log())
	}
	if got := v.status(t, j.ID); got.Status != "failed" || len(v.fake.removed) != 0 {
		t.Fatalf("got %s; Sonarr's queue must not be touched (removed=%v)", got.Status, v.fake.removed)
	}
	if !v.st.Skipped(skipKey(1, 1, 3)) {
		t.Fatal("season must be left alone afterwards")
	}
}

// Kilo: a rejected pack that is still downloading must be stopped too, not just moved.
func TestRejectingAPackStillDownloadingStopsIt(t *testing.T) {
	q := newFakeQbit(t, "consolidarr-review", 0.4, "p/01.mkv", "p/02.mkv", "p/03.mkv")
	v, j := reviewEnv(t, q)
	_, _ = v.st.SaveInstance(storeInstance(v))
	v.st.SetKV(reviewCatKey(j.ID), "btn")
	if _, err := v.e.RejectJob(context.Background(), j.ID, false); err != nil {
		t.Fatal(err)
	}
	if l := q.log(); !strings.Contains(l, "park:seed-btn") || !strings.Contains(l, "stop") || strings.Contains(l, "remove") {
		t.Fatalf("qBittorrent changes: %q", l)
	}
}

func TestRejectingAPackThatWasNotAddedByTheGrabOnlyEndsTheJob(t *testing.T) {
	q := newFakeQbit(t, "consolidarr-review", 1, "p/01.mkv")
	q.torrent["added_on"] = time.Now().Add(-30 * 24 * time.Hour).Unix()
	v, j := reviewEnv(t, q)
	_, _ = v.st.SaveInstance(storeInstance(v))
	if _, err := v.e.RejectJob(context.Background(), j.ID, false); err != nil {
		t.Fatal(err)
	}
	if q.log() != "" {
		t.Fatalf("someone else's torrent was touched: %q", q.log())
	}
}

// The job binds to its download by exact release title only, and never to a download another job owns.
func TestAReviewJobBindsOnlyToItsOwnDownloadByExactTitle(t *testing.T) {
	q := newFakeQbit(t, "btn", 0.1, "p/01.mkv", "p/02.mkv", "p/03.mkv")
	v := setup(t, []int{1, 2, 3})
	v.e.Qbit = q.client()
	j := v.job(t, time.Now().Add(-time.Minute))
	_, _ = v.st.Exec(`update jobs set status='downloading', kind=?, release=?, download_id='' where id=?`, kindReview, "The Pack", j.ID)
	j, _ = v.st.Job(j.ID)
	other := sonarr.QueueItem{SeriesID: 1, SeasonNumber: 3, Title: "Some Other Grab", DownloadID: "x", Size: 99}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{other})
	if got := v.status(t, j.ID); got.DownloadID != "" {
		t.Fatalf("bound to an unrelated download: %q", got.DownloadID)
	}
	mine := sonarr.QueueItem{SeriesID: 1, SeasonNumber: 3, Title: "The Pack", DownloadID: "d"}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{other, mine})
	if got := v.status(t, j.ID); got.DownloadID != "d" {
		t.Fatalf("download id = %q", got.DownloadID)
	}
}

func TestAGrabThatNeverShowsUpIsGivenUpOn(t *testing.T) {
	q := newFakeQbit(t, "btn", 0)
	v := setup(t, []int{1, 2, 3})
	v.e.Qbit = q.client()
	j := v.job(t, time.Now().Add(-2*time.Hour))
	_, _ = v.st.Exec(`update jobs set status='downloading', kind=?, release=?, download_id='' where id=?`, kindReview, "The Pack", j.ID)
	j, _ = v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "failed" {
		t.Fatalf("got %s %q: a job that never bound would hold a review slot for ever", got.Status, got.Message)
	}
}

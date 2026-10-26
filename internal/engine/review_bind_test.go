package engine

import (
	"context"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

func bindEnv(t *testing.T) (*env, int64) {
	q := newFakeQbit(t, "btn", 0.1, "p/01.mkv", "p/02.mkv", "p/03.mkv")
	v := setup(t, []int{1, 2, 3})
	v.e.Qbit = q.client()
	j := v.job(t, time.Now().Add(-time.Minute))
	// Prowlarr's release title, which is NOT what Sonarr shows in its queue
	_, _ = v.st.Exec(`update jobs set status='downloading', kind=?, release=?, download_id='' where id=?`, kindReview, "[Grp] Show S01 [Blu-ray][1080p]", j.ID)
	return v, j.ID
}

func queued(title, id string, added time.Time) sonarr.QueueItem {
	return sonarr.QueueItem{SeriesID: 1, SeasonNumber: 3, Title: title, DownloadID: id, Added: added.UTC().Format(time.RFC3339)}
}

// Live bug (#86): Sonarr titles a queue item by the torrent name, so an exact release-title match never happens.
func TestAReviewJobBindsToTheOneDownloadAddedAfterItsGrabWhateverItsTitle(t *testing.T) {
	v, id := bindEnv(t)
	j, _ := v.st.Job(id)
	item := queued("[Grp] Show OVA 1 v2 [BD][Hi10][1080p]", "d", time.Now())
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{item})
	if got := v.status(t, id); got.DownloadID != "d" {
		t.Fatalf("not bound: %q", got.DownloadID)
	}
}

func TestAReviewJobNeverGuessesBetweenTwoFreshDownloads(t *testing.T) {
	v, id := bindEnv(t)
	j, _ := v.st.Job(id)
	a, b := queued("one", "a", time.Now()), queued("two", "b", time.Now())
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{a, b})
	if got := v.status(t, id); got.DownloadID != "" {
		t.Fatalf("guessed %q between two candidates", got.DownloadID)
	}
}

func TestAReviewJobIgnoresADownloadQueuedBeforeItsGrab(t *testing.T) {
	v, id := bindEnv(t)
	j, _ := v.st.Job(id)
	old := queued("an older grab of the same season", "o", time.Now().Add(-6*time.Hour))
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{old})
	if got := v.status(t, id); got.DownloadID != "" {
		t.Fatalf("adopted an older download: %q", got.DownloadID)
	}
}

func TestBindingPrefersTheExactTitleAndSkipsOwnedHashesAndOtherSeasons(t *testing.T) {
	v, id := bindEnv(t)
	j, _ := v.st.Job(id)
	other := sonarr.QueueItem{SeriesID: 1, SeasonNumber: 2, Title: "a different season", DownloadID: "s2", Added: time.Now().UTC().Format(time.RFC3339)}
	a := queued("someone else's grab", "a", time.Now())
	exact := queued(j.Release, "x", time.Now())
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{other, a, exact})
	if got := v.status(t, id); got.DownloadID != "x" {
		t.Fatalf("exact title must win over other fresh downloads: %q", got.DownloadID)
	}
}

func TestAHashAnotherActiveJobOwnsIsNotBoundTwice(t *testing.T) {
	v, id := bindEnv(t)
	j, _ := v.st.Job(id)
	_, _ = v.st.AddJob(storeJobOwning("d"))
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{queued("the pack", "d", time.Now())})
	if got := v.status(t, id); got.DownloadID != "" {
		t.Fatalf("bound a download another active job owns: %q", got.DownloadID)
	}
}

func TestSeveralRecordsOfOnePackAreOneDownload(t *testing.T) {
	v, id := bindEnv(t)
	j, _ := v.st.Job(id)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{queued("p", "d", time.Now()), queued("p", "D", time.Now())})
	if got := v.status(t, id); got.DownloadID == "" {
		t.Fatal("a season pack has one queue record per episode; they are one download")
	}
}

func TestAnUnparseableAddedTimeNeverBinds(t *testing.T) {
	v, id := bindEnv(t)
	j, _ := v.st.Job(id)
	bad := sonarr.QueueItem{SeriesID: 1, SeasonNumber: 3, Title: "p", DownloadID: "d", Added: "yesterday-ish"}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{bad})
	if got := v.status(t, id); got.DownloadID != "" {
		t.Fatalf("bound on a time it could not read: %q", got.DownloadID)
	}
}

func TestAnOldFreshPairStillBindsTheOneFreshOne(t *testing.T) {
	v, id := bindEnv(t)
	j, _ := v.st.Job(id)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{queued("old", "o", time.Now().Add(-5*time.Hour)), queued("new", "n", time.Now())})
	if got := v.status(t, id); got.DownloadID != "n" {
		t.Fatalf("got %q: the old one must be ignored, the new one bound", got.DownloadID)
	}
}

func storeJobOwning(hash string) store.Job {
	return store.Job{InstID: 1, SeriesID: 1, Season: 4, Title: "Show", Status: "downloading", Kind: kindReview, DownloadID: hash}
}

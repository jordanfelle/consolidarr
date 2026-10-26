package engine

import (
	"context"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
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

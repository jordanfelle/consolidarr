package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The operator's rule, repo-wide: nothing consolidarr does may delete a torrent that has data.

// A wrong pack that is still downloading is moved to the seed category AND stopped, so it does not download the rest.
func TestAWrongPackStillDownloadingIsStoppedAndKeptInSeedCategory(t *testing.T) {
	q := newFakeQbit(t, "btn", 0.3, manyFiles(12, 3)...)
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	l := q.log()
	if !strings.Contains(l, "park:seed-btn") || !strings.Contains(l, "stop") || strings.Contains(l, "remove") {
		t.Fatalf("qBittorrent changes: %q", l)
	}
	if got := v.status(t, j.ID); !strings.Contains(got.Message, "download stopped") {
		t.Fatalf("message %q", got.Message)
	}
}

// Retry on a set-aside job must not pull the torrent back into the review category.
func TestRetryingASetAsideJobDoesNotParkItAgain(t *testing.T) {
	q := newFakeQbit(t, "btn", 1, manyFiles(12, 3)...)
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	before := q.log()
	_, _ = v.st.Exec(`update jobs set status='downloading' where id=?`, j.ID)
	j, _ = v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	after := strings.TrimPrefix(q.log(), before)
	if strings.Contains(after, "park:consolidarr-review") {
		t.Fatalf("a retry parked the set-aside torrent again: %q", after)
	}
	q.mu.Lock()
	cat := q.torrent["category"]
	q.mu.Unlock()
	if cat != "seed-btn" {
		t.Fatalf("category = %v", cat)
	}
}

// Rejecting an ordinary job (not a relabelled pack) takes it out of Sonarr's queue without removing it from the client,
// and moves the torrent to its seed category.
func TestRejectingAnOrdinaryJobKeepsTheTorrentInItsSeedCategory(t *testing.T) {
	q := newFakeQbit(t, "btn", 1, "p/01.mkv")
	v := setup(t, []int{1, 2, 3})
	v.e.Qbit = q.client()
	j := v.job(t, time.Now().Add(-time.Hour))
	_, _ = v.st.Exec(`update jobs set status='holding', download_id='d', release='r' where id=?`, j.ID)
	_, _ = v.st.SaveInstance(storeInstance(v))
	v.fake.queueItems = []map[string]any{{"id": 7, "seriesId": 1, "seasonNumber": 3, "downloadId": "d", "status": "completed", "title": "r", "outputPath": "/dl/pack"}}
	if _, err := v.e.RejectJob(context.Background(), j.ID, false); err != nil {
		t.Fatal(err)
	}
	if len(v.fake.removed) != 1 || !strings.Contains(v.fake.removed[0].Query, "removeFromClient=false") {
		t.Fatalf("Sonarr removal = %+v", v.fake.removed)
	}
	if l := q.log(); !strings.Contains(l, "park:seed-btn") || strings.Contains(l, "remove") {
		t.Fatalf("qBittorrent changes: %q", l)
	}
}

// Sonarr treats a blocklisted download as failed and (Remove Failed Downloads is on for this qBittorrent client) may
// remove it from the client itself. So the release is blocklisted only after the torrent has been moved out of reach.
func TestTheReleaseIsOnlyBlocklistedOnceTheTorrentIsOutOfSonarrsReach(t *testing.T) {
	for _, c := range []struct {
		name      string
		seedCat   bool
		wantBlock string
	}{{"seed category exists", true, "blocklist=true"}, {"no seed category", false, "blocklist=false"}} {
		q := newFakeQbit(t, "btn", 1, "p/01.mkv")
		if !c.seedCat {
			delete(q.cats, "seed-btn")
		}
		v := setup(t, []int{1, 2, 3})
		v.e.Qbit = q.client()
		j := v.job(t, time.Now().Add(-time.Hour))
		_, _ = v.st.Exec(`update jobs set status='holding', download_id='d', release='r' where id=?`, j.ID)
		_, _ = v.st.SaveInstance(storeInstance(v))
		v.fake.queueItems = []map[string]any{{"id": 7, "seriesId": 1, "seasonNumber": 3, "downloadId": "d", "status": "completed", "title": "r", "outputPath": "/dl/pack"}}
		if _, err := v.e.RejectJob(context.Background(), j.ID, false); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(v.fake.removed) != 1 || !strings.Contains(v.fake.removed[0].Query, c.wantBlock) || !strings.Contains(v.fake.removed[0].Query, "removeFromClient=false") {
			t.Fatalf("%s: removal = %+v", c.name, v.fake.removed)
		}
	}
}

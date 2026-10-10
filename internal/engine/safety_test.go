package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

func readyJob(t *testing.T, v *env) store.Job {
	t.Helper()
	j := v.job(t, time.Time{})
	j.Status = "ready"
	_ = v.st.UpdateJob(j)
	return v.status(t, j.ID)
}

func completed() []sonarr.QueueItem { return done(100, 0, "completed") }

func TestApproveVerifiesAgainBeforeDeleting(t *testing.T) {
	v := attentionEnv(t, map[string]any{"seriesId": 1, "seasonNumber": 3, "status": "completed", "downloadId": "d", "outputPath": "/dl/pack", "size": 100.0, "sizeleft": 0.0, "title": "r"})
	j := readyJob(t, v)
	probeFn = func(store.Instance, string) (bool, string) { return false, "e.mkv: unreadable or DRM-encrypted" } // went bad since "ready"
	err := v.e.Approve(context.Background(), v.inst, j)
	if err == nil || len(v.fake.deleted) != 0 {
		t.Fatalf("approve must re-verify and refuse a pack that no longer passes: err=%v deleted=%v", err, v.fake.deleted)
	}
	if got := v.status(t, j.ID); got.Status != "holding" || got.Deleted != 0 {
		t.Fatalf("got %s deleted=%d", got.Status, got.Deleted)
	}
}

func TestApproveRefusesAnIncompleteDownloadAndOnlyReadyJobs(t *testing.T) {
	v := attentionEnv(t, map[string]any{"seriesId": 1, "seasonNumber": 3, "status": "downloading", "downloadId": "d", "outputPath": "/dl/pack", "size": 100.0, "sizeleft": 40.0, "title": "r"})
	if err := v.e.Approve(context.Background(), v.inst, readyJob(t, v)); err == nil || len(v.fake.deleted) != 0 {
		t.Fatalf("an incomplete download must not be approved: %v", err)
	}
	w := attentionEnv(t, map[string]any{"seriesId": 1, "seasonNumber": 3, "status": "completed", "downloadId": "d", "outputPath": "/dl/pack", "size": 100.0, "sizeleft": 0.0, "title": "r"})
	j := w.job(t, time.Time{}) // still "downloading" in our records
	if err := w.e.Approve(context.Background(), w.inst, j); err == nil || len(w.fake.deleted) != 0 {
		t.Fatalf("only ready jobs can be approved: %v", err)
	}
}

func TestDeleteAndImportSurviveACancelledRequest(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := readyJob(t, v)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the browser went away / the proxy timed out
	v.e.replace(ctx, v.inst, v.c, &j, verdict{State: "ok", DownloadID: "d", OutputPath: "/dl/pack", Expected: 3, ExpectedEps: []int{1, 2, 3}, Items: completed(), At: time.Now()}, completed(), "test")
	if len(v.fake.deleted) != 1 || v.fake.imports != 1 || v.status(t, j.ID).Status != "importing" {
		t.Fatalf("once started, delete+import must finish: deleted=%v imports=%d status=%s", v.fake.deleted, v.fake.imports, v.status(t, j.ID).Status)
	}
}

func TestTheDeleteIsRecordedBeforeItHappens(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := readyJob(t, v)
	var atDelete store.Job
	v.fake.onDelete = func() { atDelete, _ = v.st.Job(j.ID) }
	v.e.replace(context.Background(), v.inst, v.c, &j, verdict{State: "ok", DownloadID: "d", OutputPath: "/dl/pack", Expected: 3, ExpectedEps: []int{1, 2, 3}, Items: completed(), At: time.Now()}, completed(), "test")
	if atDelete.Status != "importing" || atDelete.Deleted == 0 || atDelete.Expected != 3 || atDelete.DownloadID != "d" {
		t.Fatalf("when the delete request arrived the job must already say so: %+v", atDelete)
	}
}

func TestAnImportThatFailsToStartAfterTheDeleteIsRetriedNotAbandoned(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.failImport = 1
	j := readyJob(t, v)
	v.e.replace(context.Background(), v.inst, v.c, &j, verdict{State: "ok", DownloadID: "d", OutputPath: "/dl/pack", Expected: 3, ExpectedEps: []int{1, 2, 3}, Items: completed(), At: time.Now()}, completed(), "test")
	got := v.status(t, j.ID)
	if got.Status != "importing" || got.ImportCmd != 0 || got.Deleted == 0 || len(v.fake.deleted) != 1 {
		t.Fatalf("a failed import start after the delete must stay importing for a retry, got %+v", got)
	}
	v.fake.existing = map[int]bool{} // the old files really are gone
	q := []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Status: "completed", DownloadID: "d", OutputPath: "/dl/pack", Title: got.Release}}
	v.e.advance(context.Background(), v.inst, v.c, live(), got, q)
	if after := v.status(t, j.ID); after.ImportCmd != 42 || v.fake.imports != 1 {
		t.Fatalf("the next cycle must start the import: %+v imports=%d", after, v.fake.imports)
	}
}

func TestAPartialImportIsNeverMarkedDone(t *testing.T) {
	v := setup(t, nil)
	j := v.job(t, time.Time{})
	j.Status, j.ImportCmd, j.Expected, j.ExpectedEps, j.Deleted = "importing", 42, 3, []int{1, 2, 3}, time.Now().Unix()
	j.ImportAt = j.Deleted
	_ = v.st.UpdateJob(j)
	v.fake.seasonEps = map[int]int{1: 99, 2: 12, 3: 13} // 1 of 3 episodes has a new file
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "importing" {
		t.Fatalf("1 of 3 episodes is not done, and not yet failed either: %s", got.Status)
	}
	j = v.status(t, j.ID)
	j.Deleted = time.Now().Add(-time.Hour).Unix()
	j.ImportAt = j.Deleted
	_ = v.st.UpdateJob(j)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	got := v.status(t, j.ID)
	if got.Status != "failed" || got.Message == "" || got.Deleted == 0 {
		t.Fatalf("after the settle period a partial import is reported as failed (and remembers the delete): %+v", got)
	}
	v.fake.seasonEps = map[int]int{1: 99, 2: 98, 3: 97}
	_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
	again, err := v.e.AdvanceJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "done" {
		t.Fatalf("once every episode has a file the retried job is done, got %s %q", again.Status, again.Message)
	}
}

func TestCoverageFailsClosed(t *testing.T) {
	cases := map[string]func(*env){
		"no episodes returned": func(v *env) { v.fake.noEpisodes = true },
		"a pack file Sonarr will not import stands in for an episode": func(v *env) { v.fake.rejections = map[int][]string{3: {"Sample"}} },
	}
	for name, mutate := range cases {
		v := setup(t, []int{1, 2, 3})
		mutate(v)
		j := v.job(t, time.Time{})
		v.e.advance(context.Background(), v.inst, v.c, live(), j, completed())
		if got := v.status(t, j.ID); got.Status != "holding" || len(v.fake.deleted) != 0 {
			t.Errorf("%s: must hold and delete nothing, got %s %q deleted=%v", name, got.Status, got.Message, v.fake.deleted)
		}
	}
	v := setup(t, []int{1, 2, 3}) // "not an upgrade" is the expected reason: those files replace ours
	v.fake.rejections = map[int][]string{1: {"Not an upgrade for existing episode file(s)."}, 2: {"Not an upgrade for existing episode file(s)."}, 3: {"Not an upgrade for existing episode file(s)."}}
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, completed())
	if got := v.status(t, j.ID); got.Status != "importing" {
		t.Fatalf("'not an upgrade' rejections must not block the replacement, got %s %q", got.Status, got.Message)
	}
}

func TestRejectRefusesOnceTheOldFilesAreGone(t *testing.T) {
	v := attentionEnv(t, stuckItem(10, 1, 3, "x"))
	j := v.job(t, time.Time{})
	j.Status, j.Deleted = "failed", time.Now().Unix()
	_ = v.st.UpdateJob(j)
	if _, err := v.e.RejectJob(context.Background(), j.ID, true); err == nil || len(v.fake.removed) != 0 {
		t.Fatalf("rejecting would delete the only copy: err=%v removed=%v", err, v.fake.removed)
	}
	if got, _ := v.e.Attention(context.Background()); len(got) != 0 {
		t.Fatalf("a failed job that already deleted files owns its season: its pack must not be offered for removal, got %+v", got)
	}
}

func TestRejectOnlyRemovesTheJobsOwnDownload(t *testing.T) {
	v := attentionEnv(t,
		map[string]any{"id": 10, "seriesId": 1, "seasonNumber": 3, "title": "the.pack", "downloadId": "AAA", "status": "completed"},
		map[string]any{"id": 11, "seriesId": 1, "seasonNumber": 3, "title": "an.unrelated.episode", "downloadId": "BBB", "status": "completed"})
	j := v.job(t, time.Time{})
	j.Status, j.Release = "holding", "the.pack"
	_ = v.st.UpdateJob(j)
	_, _ = v.st.Exec(`update jobs set release=? where id=?`, "the.pack", j.ID)
	if _, err := v.e.RejectJob(context.Background(), j.ID, false); err != nil {
		t.Fatal(err)
	}
	if len(v.fake.removed) != 1 || len(v.fake.removed[0].IDs) != 1 || v.fake.removed[0].IDs[0] != 10 {
		t.Fatalf("only the job's own download (id 10) may be removed, got %+v", v.fake.removed)
	}
}

func TestConcurrentCallersDeleteOnce(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.e.advance(context.Background(), v.inst, v.c, live(), j, completed())
		}()
	}
	wg.Wait()
	if len(v.fake.deleted) != 1 || v.fake.imports != 1 {
		t.Fatalf("six simultaneous callers must delete and import exactly once: deleted=%v imports=%d", v.fake.deleted, v.fake.imports)
	}
}

func TestASeasonCanHaveOnlyOneActiveJob(t *testing.T) {
	v := setup(t, nil)
	v.job(t, time.Time{})
	if _, err := v.st.AddJob(store.Job{InstID: 1, SeriesID: 1, Season: 3, Title: "dup"}); err != store.ErrDuplicate {
		t.Fatalf("a second active job for the same season must be refused, got %v", err)
	}
	if _, err := v.st.AddJob(store.Job{InstID: 1, SeriesID: 1, Season: 4, Title: "other season"}); err != nil {
		t.Fatalf("other seasons are unaffected: %v", err)
	}
}

func TestAStalledDownloadIsReportedAndFreesItsSlot(t *testing.T) {
	v := setup(t, nil)
	j := v.job(t, time.Time{})
	j.DownloadID, j.Progress = "d", 40
	_ = v.st.UpdateJob(j)
	_, _ = v.st.Exec(`update jobs set updated=? where id=?`, time.Now().Add(-7*time.Hour).Unix(), j.ID)
	q := []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Size: 100, SizeLeft: 60, Status: "downloading", DownloadID: "d"}}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, q)
	if got := v.status(t, j.ID); got.Status != "failed" || got.Deleted != 0 {
		t.Fatalf("a download with no progress for hours is reported, files untouched: %+v", got)
	}
	if active, _ := v.st.Jobs(true, 10); len(active) != 0 {
		t.Fatal("a stalled job must stop occupying an in-flight slot")
	}
}

func TestPathMappingRespectsDirectoryBoundaries(t *testing.T) {
	inst := store.Instance{PathFrom: "/data/tv", PathTo: "/mnt/tv"}
	for in, want := range map[string]string{
		"/data/tv/Show/e.mkv": "/mnt/tv/Show/e.mkv",
		"/data/tvx/Show.mkv":  "/data/tvx/Show.mkv", // a sibling directory is not under /data/tv
		"/data/tv":            "/mnt/tv",
		"/other/e.mkv":        "/other/e.mkv",
	} {
		if got := mapPath(inst, in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestACurrentFileThatMapsToNoEpisodeBlocksTheReplacement(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.existing[14] = true // a file of yours that Sonarr has not linked to any episode
	id, _ := v.st.AddJob(store.Job{InstID: 1, SeriesID: 1, Season: 3, Title: "Show", OldFileIDs: []int{11, 12, 13, 14}})
	j, _ := v.st.Job(id)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, completed())
	if got := v.status(t, id); got.Status != "holding" || len(v.fake.deleted) != 0 {
		t.Fatalf("an unmapped file means coverage cannot be confirmed: %s %q deleted=%v", got.Status, got.Message, v.fake.deleted)
	}
}

func TestMultiEpisodeFilesCountEpisodesNotFiles(t *testing.T) {
	v := setup(t, nil)
	j := v.job(t, time.Time{})
	j.Status, j.ImportCmd, j.ExpectedEps, j.Expected, j.Deleted = "importing", 42, []int{1, 2, 3}, 3, time.Now().Unix()
	j.ImportAt = j.Deleted
	_ = v.st.UpdateJob(j)
	v.fake.seasonEps = map[int]int{1: 99, 2: 99, 3: 98} // ONE file (99) holds episodes 1 and 2: two files, three episodes
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "done" {
		t.Fatalf("a successful import with a multi-episode file must be done, got %s %q", got.Status, got.Message)
	}
}

func TestFilesSonarrImportedBeforeTheDeleteAreNotTheResult(t *testing.T) {
	v := setup(t, nil)
	j := v.job(t, time.Time{})
	// Sonarr had already upgraded episodes 1 and 2 on its own (files 90, 91); only episode 3 was ours to replace.
	j.Status, j.ImportCmd, j.ExpectedEps, j.Expected, j.Deleted = "importing", 42, []int{3}, 1, time.Now().Unix()
	j.ImportAt = j.Deleted
	_ = v.st.UpdateJob(j)
	v.fake.seasonEps = map[int]int{1: 90, 2: 91, 3: 0} // 1 and 2 have (pre-existing) new files; 3 has nothing yet
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "importing" {
		t.Fatalf("episode 3 is still missing: the job is not done just because other episodes have files, got %s", got.Status)
	}
	v.fake.seasonEps[3] = 99
	v.e.advance(context.Background(), v.inst, v.c, live(), v.status(t, j.ID), nil)
	if got := v.status(t, j.ID); got.Status != "done" {
		t.Fatalf("once episode 3 has a file it is done, got %s", got.Status)
	}
}

func TestARetryGetsAFreshSettleWindow(t *testing.T) {
	// The pack is no longer in Sonarr's queue, so the retry cannot restart the import. It must still wait the normal
	// grace period from NOW, not fail instantly because the original delete was hours ago.
	v := attentionEnv(t)
	j := v.job(t, time.Time{})
	hoursAgo := time.Now().Add(-5 * time.Hour).Unix()
	j.Status, j.Deleted, j.ImportAt, j.ExpectedEps, j.Expected = "failed", hoursAgo, hoursAgo, []int{1, 2, 3}, 3
	_ = v.st.UpdateJob(j)
	v.fake.seasonEps = map[int]int{1: 11, 2: 12, 3: 13} // still no new files
	got, err := v.e.AdvanceJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "importing" || got.ImportAt < time.Now().Add(-time.Minute).Unix() {
		t.Fatalf("a retried job must restart its clock and get a fresh grace period instead of failing again at once: %+v", got)
	}
}

func TestRacingGrabsReserveTheSeasonSoOnlyOneDownloads(t *testing.T) {
	grabPause = 0
	v := setup(t, nil)
	_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
	inst := store.Instance{ID: 1, Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true}
	s := audit.Season{InstID: 1, SeriesID: 1, Season: 3, Title: "Show"}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.e.Grab(context.Background(), inst, s, "g", 1, "Show.S03", 10, "BTN")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
		} else if err != store.ErrDuplicate {
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok != 1 || v.fake.grabs != 1 {
		t.Fatalf("eight racing grabs must produce one job and one download: ok=%d downloads=%d", ok, v.fake.grabs)
	}
}

func TestAStaleVerdictIsVerifiedAgainBeforeTheDelete(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := readyJob(t, v)
	probeFn = func(store.Instance, string) (bool, string) { return false, "e.mkv: unreadable or DRM-encrypted" }
	stale := verdict{State: "ok", DownloadID: "d", OutputPath: "/dl/pack", ExpectedEps: []int{1, 2, 3}, Expected: 3,
		Items: completed(), At: time.Now().Add(-5 * time.Minute)} // verified minutes ago, while we waited behind another season
	v.e.replace(context.Background(), v.inst, v.c, &j, stale, completed(), "test")
	if got := v.status(t, j.ID); got.Status != "holding" || len(v.fake.deleted) != 0 {
		t.Fatalf("a verdict that is no longer true must not delete: %s %q deleted=%v", got.Status, got.Message, v.fake.deleted)
	}
}

func TestWithSeveralDownloadsForASeasonTheJobBindsToTheBiggest(t *testing.T) {
	q := []sonarr.QueueItem{
		{SeriesID: 1, SeasonNumber: 3, Title: "an.episode", DownloadID: "SMALL", Size: 1},
		{SeriesID: 1, SeasonNumber: 3, Title: "the.pack", DownloadID: "BIG", Size: 50},
		{SeriesID: 1, SeasonNumber: 3, Title: "the.pack", DownloadID: "BIG", Size: 50},
	}
	j := &store.Job{SeriesID: 1, Season: 3, Release: "no title matches"}
	mine := ownedItems(q, j)
	if len(mine) != 2 || j.DownloadID != "BIG" {
		t.Fatalf("with no title match the job must bind to the pack (the biggest download), got %d items id=%q", len(mine), j.DownloadID)
	}
}

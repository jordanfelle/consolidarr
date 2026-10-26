package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/jordanfelle/consolidarr/internal/store"
)

// approveEnv: a finished relabelled pack parked in consolidarr-review for a 3-episode season 3.
func approveEnv(t *testing.T, names ...string) (*env, *fakeQbit, store.Job) {
	t.Helper()
	q := newFakeQbit(t, "consolidarr-review", 1, names...)
	v, j := reviewEnv(t, q)
	v.fake.episodes = []map[string]any{
		{"id": 601, "seasonNumber": 3, "episodeNumber": 1, "title": "Down Bad"},
		{"id": 602, "seasonNumber": 3, "episodeNumber": 2, "title": "Calculating"},
		{"id": 603, "seasonNumber": 3, "episodeNumber": 3, "title": "Correctness"}}
	var items []map[string]any
	for _, n := range names {
		items = append(items, map[string]any{"path": "/dl/pack/" + n, "series": map[string]int{"id": 1}, "rejections": []map[string]string{},
			"quality": map[string]any{"quality": map[string]any{"id": 7, "name": "Bluray-1080p Remux", "source": "bluray", "resolution": 1080}}})
	}
	v.fake.folderItems = items
	_, _ = v.st.SaveInstance(storeInstance(v))
	_ = v.st.SaveSettings(live())
	_, _ = v.st.Exec(`update jobs set status='holding' where id=?`, j.ID)
	j, _ = v.st.Job(j.ID)
	oldProbe, oldAudio := probeFn, audioFn
	probeFn = func(store.Instance, string) (bool, string) { return true, "" }
	audioFn = func(store.Instance, string) ([]string, string) { return []string{"jpn", "eng"}, "ok" }
	t.Cleanup(func() { probeFn, audioFn = oldProbe, oldAudio })
	q.mu.Lock()
	q.torrent["content_path"] = "/dl/pack"
	q.mu.Unlock()
	return v, q, j
}

func tokenOf(t *testing.T, v *env, j store.Job) string {
	t.Helper()
	plan, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j)
	if err != nil || !plan.OK {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	return plan.Token
}

func TestAnS03NamedPackIsPlannedOnItsEpisodesAndApprovedByCopy(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv", "Show.S03E04.NCOP.mkv")
	plan, mapping, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j)
	if err != nil || !plan.OK || len(plan.Files) != 3 || len(mapping) != 3 {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	if mapping["/dl/pack/Show.S03E02.mkv"][0] != 602 {
		t.Fatalf("mapping %v", mapping)
	}
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, plan.Token); err != nil {
		t.Fatal(err)
	}
	if len(v.fake.mappedImports) != 1 || len(v.fake.mappedImports[0]) != 3 {
		t.Fatalf("imports = %v", v.fake.mappedImports)
	}
	if v.fake.importMode != "copy" {
		t.Fatalf("importMode = %q: files Sonarr does not track must be copied, never moved out from under the torrent", v.fake.importMode)
	}
	if len(v.fake.deleted) != 0 {
		t.Fatalf("a mapped import deletes nothing itself: %v", v.fake.deleted)
	}
	if got := v.status(t, j.ID); got.Status != "importing" {
		t.Fatalf("status %s", got.Status)
	}
}

func TestEpisodeTitlesPlaceFilesThatCarryNoNumber(t *testing.T) {
	v, _, j := approveEnv(t, "[G] Show - 01 - Down Bad [1080p].mkv", "[G] Show - 02 - Calculating [1080p].mkv", "[G] Show - 03 - Correctness [1080p].mkv")
	plan, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j)
	if err != nil || !plan.OK || plan.Files[0].Title != "Down Bad" {
		t.Fatalf("plan %+v err %v", plan, err)
	}
}

func TestAPackNumberedS01ForALaterSeasonIsNeverMappedByGuessing(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S01E01.mkv", "Show.S01E02.mkv", "Show.S01E03.mkv")
	plan, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j)
	if err != nil || plan.OK || !strings.Contains(plan.Reason, "will not guess") {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, "x"); err == nil || len(v.fake.mappedImports) != 0 {
		t.Fatalf("approved an unmappable pack: err=%v imports=%v", err, v.fake.mappedImports)
	}
}

func TestAPackMissingAnEpisodeOrWithDuplicatesIsNotApproved(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv")
	if plan, _, _, _ := v.e.PlanReview(context.Background(), v.liveInst(), j); plan.OK || !strings.Contains(plan.Reason, "no file for episode") {
		t.Fatalf("plan %+v", plan)
	}
	v2, _, j2 := approveEnv(t, "Show.S03E01.mkv", "Show.S03E01.v2.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	if plan, _, _, _ := v2.e.PlanReview(context.Background(), v2.liveInst(), j2); plan.OK || !strings.Contains(plan.Reason, "two files claim") {
		t.Fatalf("plan %+v", plan)
	}
}

func TestAPackWithoutEnglishAudioIsNotApproved(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	audioFn = func(store.Instance, string) ([]string, string) { return []string{"jpn"}, "ok" }
	plan, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j)
	if err != nil || plan.OK {
		t.Fatalf("plan %+v err %v: a dub pack with no English track must not be approved", plan, err)
	}
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, "x"); err == nil || len(v.fake.mappedImports) != 0 {
		t.Fatalf("imported a pack with no English audio: err=%v", err)
	}
}

func TestApproveRefusesUnlessThePackIsFinishedAwaitingReviewAndNotDryRun(t *testing.T) {
	v, q, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	q.mu.Lock()
	q.torrent["progress"] = 0.5
	q.mu.Unlock()
	if _, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j); err == nil {
		t.Fatal("an unfinished pack was planned")
	}
	q.mu.Lock()
	q.torrent["progress"] = 1.0
	q.mu.Unlock()
	_, _ = v.st.Exec(`update jobs set status='downloading' where id=?`, j.ID)
	j2, _ := v.st.Job(j.ID)
	if _, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j2); err == nil {
		t.Fatal("a pack not awaiting review was planned")
	}
	_, _ = v.st.Exec(`update jobs set status='holding' where id=?`, j.ID)
	set := store.DefaultSettings() // DryRun on
	if err := v.st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, "x"); err == nil || !strings.Contains(err.Error(), "dry run") {
		t.Fatalf("dry run did not block: %v", err)
	}
}

// When the import is done the parked torrent goes to its seed category: kept, seeding, never deleted.
func TestAfterTheImportTheTorrentIsMovedToItsSeedCategory(t *testing.T) {
	v, q, j := approveEnv(t, "Show.S03E01.mkv")
	v.st.SetKV(reviewHashKey(j.ID), "d")
	v.st.SetKV(reviewCatKey(j.ID), "btn") // where it was parked from
	j.DownloadID = ""                     // as replace() leaves the job
	v.e.afterReviewImport(context.Background(), j)
	q.mu.Lock()
	cat := q.torrent["category"]
	q.mu.Unlock()
	if cat != "seed-btn" {
		t.Fatalf("category = %v", cat)
	}
	if strings.Contains(q.log(), "remove") {
		t.Fatalf("a torrent was removed: %q", q.log())
	}
}

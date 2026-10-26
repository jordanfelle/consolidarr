package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// The human is shown exactly what an approval overwrites: Sonarr has no recycle bin here, so a replaced file is gone.
func TestThePlanNamesEveryLibraryFileItWillReplace(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	v.fake.episodes[0]["episodeFileId"] = 11
	v.fake.filesAfter = []sonarr.EpisodeFile{{ID: 11, SeasonNumber: 3, Path: "/lib/Show - S03E01 - Down Bad [WEBDL-1080p].mkv", Size: 300e6}}
	plan, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j)
	if err != nil || !plan.OK {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	if !strings.Contains(plan.Files[0].Replaces, "Down Bad [WEBDL-1080p]") || plan.Files[1].Replaces != "" || plan.Files[0].PackQuality == "" {
		t.Fatalf("files = %+v", plan.Files)
	}
}

// Approve runs only the plan the human saw.
func TestApproveNeedsTheTokenOfThePlanThatWasReviewed(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	for _, tok := range []string{"", "not-the-token"} {
		err := v.e.ApproveReview(context.Background(), v.liveInst(), j, tok)
		var se *ReviewStateError
		if err == nil || !errors.As(err, &se) || len(v.fake.mappedImports) != 0 {
			t.Fatalf("token %q: err=%v imports=%v", tok, err, v.fake.mappedImports)
		}
	}
}

// A pack Sonarr cannot read the quality of would turn the episodes Unknown.
func TestAPackWhoseQualitySonarrCannotReadIsNotApproved(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	for _, it := range v.fake.folderItems {
		it["quality"] = map[string]any{"quality": map[string]any{"id": 0, "name": "Unknown"}}
	}
	plan, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j)
	if err != nil || plan.OK || !strings.Contains(plan.Reason, "Unknown") {
		t.Fatalf("plan %+v err %v", plan, err)
	}
}

// If Sonarr refuses the import command nothing was imported: the job goes back to review instead of getting stuck
// 'importing' and later failing with a claim about Sonarr's queue.
func TestARefusedImportPutsTheJobBackForAnotherTry(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	token := tokenOf(t, v, j)
	v.fake.failImport = 1
	err := v.e.ApproveReview(context.Background(), v.liveInst(), j, token)
	if err == nil {
		t.Fatal("approve reported success though Sonarr refused the import")
	}
	got := v.status(t, j.ID)
	if got.Status != "holding" || len(got.Mapping) != 0 || got.Deleted != 0 || !strings.Contains(got.Message, "awaiting your review") {
		t.Fatalf("got %s %q mapping=%v deleted=%d", got.Status, got.Message, got.Mapping, got.Deleted)
	}
	if len(v.fake.deleted) != 0 {
		t.Fatalf("library files were deleted: %v", v.fake.deleted)
	}
}

// A partial import (some episodes swapped, the rest still on the old file) goes back to the human, not to a queue retry.
func TestAPartialImportGoesBackToReview(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	token := tokenOf(t, v, j)
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, token); err != nil {
		t.Fatal(err)
	}
	// the import command finished long ago but no episode has a new file (the fake still lists only the old ones)
	_, _ = v.st.Exec(`update jobs set import_at=? where id=?`, time.Now().Add(-3*time.Hour).Unix(), j.ID)
	j2, _ := v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j2, nil)
	got := v.status(t, j.ID)
	if got.Status != "holding" || !strings.Contains(got.Message, "awaiting your review") || !strings.Contains(got.Message, "still have your old file") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
	if got.DownloadID == "" {
		t.Fatal("the job must be bound to its torrent again so it can be reviewed")
	}
}

func TestOldFilesAreTheSeasonsFilesAtApprovalTime(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	_, _ = v.st.Exec(`update jobs set old_file_ids='[1,2,3]' where id=?`, j.ID) // recorded days ago at grab time
	j, _ = v.st.Job(j.ID)
	token := tokenOf(t, v, j)
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, token); err != nil {
		t.Fatal(err)
	}
	got := v.status(t, j.ID)
	for _, id := range got.OldFileIDs {
		if id == 1 || id == 2 || id == 3 {
			t.Fatalf("stale old file ids kept: %v", got.OldFileIDs)
		}
	}
	_ = store.Job{}
}

// After a restart a relabelled job can be 'importing' with no import command (the start was lost). The pack is not in
// Sonarr's queue, so the queue-based retry would give up with a wrong claim; the job goes back to review instead.
func TestARestartedReviewImportWithNoCommandGoesBackToReview(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	v.st.SetKV(reviewHashKey(j.ID), j.DownloadID)
	_, _ = v.st.Exec(`update jobs set status='importing', import_cmd=0, deleted=?, import_at=?, download_id='' where id=?`, time.Now().Unix(), time.Now().Unix(), j.ID)
	j2, _ := v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j2, nil)
	got := v.status(t, j.ID)
	if got.Status != "holding" || got.Deleted != 0 || got.DownloadID == "" || !strings.Contains(got.Message, "awaiting your review") {
		t.Fatalf("got %s %q deleted=%d dl=%q", got.Status, got.Message, got.Deleted, got.DownloadID)
	}
}

func libFiles(ids ...int) []sonarr.EpisodeFile {
	var out []sonarr.EpisodeFile
	for _, id := range ids {
		out = append(out, sonarr.EpisodeFile{ID: id, SeasonNumber: 3, Path: "/lib/Show - S03E0x [WEBDL-1080p].mkv", Size: 300e6})
	}
	return out
}

// The token covers what the human was shown, including which library file each episode overwrites.
func TestTheTokenChangesWhenALibraryFileChangesAfterTheReview(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	v.fake.episodes[0]["episodeFileId"] = 11
	v.fake.filesAfter = libFiles(11)
	token := tokenOf(t, v, j)
	v.fake.episodes[0]["episodeFileId"] = 12 // Sonarr upgraded the episode meanwhile: a different file would be destroyed
	v.fake.filesAfter = libFiles(12)
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, token); err == nil || len(v.fake.mappedImports) != 0 {
		t.Fatalf("an approval of a stale review went through: err=%v imports=%v", err, v.fake.mappedImports)
	}
}

func TestOneLibraryFileCoveringTwoEpisodesIsFlaggedAndAMissingOneIsNotCalledNothing(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	v.fake.episodes[0]["episodeFileId"] = 11
	v.fake.episodes[1]["episodeFileId"] = 11 // a two-episode library file
	v.fake.episodes[2]["episodeFileId"] = 99 // an id Sonarr does not list
	v.fake.filesAfter = libFiles(11)
	plan, _, _, err := v.e.PlanReview(context.Background(), v.liveInst(), j)
	if err != nil || !plan.OK {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	if !strings.Contains(plan.Files[0].Replaces, "ONE FILE FOR") || !strings.Contains(plan.Files[1].Replaces, "S03E01") {
		t.Fatalf("shared file not flagged: %+v", plan.Files)
	}
	if !strings.Contains(plan.Files[2].Replaces, "did not list") {
		t.Fatalf("a missing library file must not read as 'nothing': %q", plan.Files[2].Replaces)
	}
}

// 1 of 3 episodes imported: back to review, with the number, not a failure about Sonarr's queue.
func TestAnImportThatSwappedOnlySomeEpisodesGoesBackToReviewWithTheCount(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	for i, id := range []int{11, 12, 13} {
		v.fake.episodes[i]["episodeFileId"] = id
	}
	v.fake.filesAfter = libFiles(11, 12, 13)
	token := tokenOf(t, v, j)
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, token); err != nil {
		t.Fatal(err)
	}
	v.fake.episodes[0]["episodeFileId"] = 99 // Sonarr swapped episode 1 only
	_, _ = v.st.Exec(`update jobs set import_at=? where id=?`, time.Now().Add(-3*time.Hour).Unix(), j.ID)
	j2, _ := v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j2, nil)
	got := v.status(t, j.ID)
	if got.Status != "holding" || !strings.Contains(got.Message, "1 of 3") || !strings.Contains(got.Message, "already imported keep the new file") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
}

// A second approve must not start another import of the same files while Sonarr is still running one.
func TestAnImportStillRunningBlocksASecondApproval(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	token := tokenOf(t, v, j)
	v.fake.cmds = []map[string]any{{"id": 77, "name": "ManualImport", "status": "started", "queued": time.Now().UTC().Format(time.RFC3339Nano),
		"body": map[string]any{"files": []map[string]any{{"path": "/dl/pack/Show.S03E01.mkv"}}}}}
	err := v.e.ApproveReview(context.Background(), v.liveInst(), j, token)
	if err == nil || !strings.Contains(err.Error(), "still importing") || len(v.fake.mappedImports) != 0 {
		t.Fatalf("err=%v imports=%v", err, v.fake.mappedImports)
	}
}

// A lost import id with the command still running in Sonarr is adopted, not reverted (and not imported twice).
func TestARunningImportWhoseIdWasLostIsAdoptedNotReverted(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	v.st.SetKV(reviewHashKey(j.ID), j.DownloadID)
	_, _ = v.st.Exec(`update jobs set status='importing', import_cmd=0, deleted=?, import_at=?, download_id='', mapping=? where id=?`,
		time.Now().Unix(), time.Now().Unix(), `{"/dl/pack/Show.S03E01.mkv":[601]}`, j.ID)
	v.fake.cmds = []map[string]any{{"id": 77, "name": "ManualImport", "status": "started", "queued": time.Now().UTC().Format(time.RFC3339Nano),
		"body": map[string]any{"files": []map[string]any{{"path": "/dl/pack/Show.S03E01.mkv"}}}}}
	j2, _ := v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j2, nil)
	got := v.status(t, j.ID)
	if got.Status != "importing" || got.ImportCmd != 77 {
		t.Fatalf("got %s cmd=%d %q: a running import must be adopted", got.Status, got.ImportCmd, got.Message)
	}
}

// A command still 'started' after the hard stop puts the job back to review (it does not wait for ever).
func TestAnImportStillRunningAfterTheHardStopGoesBackToReview(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	token := tokenOf(t, v, j)
	if err := v.e.ApproveReview(context.Background(), v.liveInst(), j, token); err != nil {
		t.Fatal(err)
	}
	v.fake.cmdStatus = "started"
	_, _ = v.st.Exec(`update jobs set import_at=? where id=?`, time.Now().Add(-importHardStop-time.Hour).Unix(), j.ID)
	j2, _ := v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j2, nil)
	got := v.status(t, j.ID)
	if got.Status != "holding" || !strings.Contains(got.Message, "has not finished after") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
}

// The reason the import did not start is not lost.
func TestARefusedImportKeepsItsReasonInTheError(t *testing.T) {
	v, _, j := approveEnv(t, "Show.S03E01.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv")
	token := tokenOf(t, v, j)
	v.fake.failImport = 1
	err := v.e.ApproveReview(context.Background(), v.liveInst(), j, token)
	if err == nil || !strings.Contains(err.Error(), "import refused") {
		t.Fatalf("err = %v", err)
	}
}

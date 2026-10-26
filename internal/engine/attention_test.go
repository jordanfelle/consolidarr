package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/store"
)

func TestClassifyKnownSonarrWarnings(t *testing.T) {
	for msg, want := range map[string]string{
		"Not an upgrade for existing episode file(s). Existing quality: HDTV-1080p. New Quality SDTV.":                       KindNotUpgrade,
		"Not a Custom Format upgrade for existing episode file(s). New: [x] (0)":                                             KindNotUpgrade,
		"episode file already imported at 10/10/2026 09:49:39":                                                               KindAlreadyDone,
		"failed to import episode, destination already exists.":                                                              KindDestExists,
		"Found matching series via grab history, but release was matched to series by ID. Automatic import is not possible.": KindIDMatch,
		"Episode 5x05 was not found in the grabbed release: Spidey.S05E03":                                                   KindNotInRelease,
		"Sample":              KindSample,
		"Unknown Series":      KindUnknownSeries,
		"something brand new": KindOther,
	} {
		if got := Classify(msg); got != want {
			t.Errorf("%q: got %s want %s", msg, got, want)
		}
	}
}

func stuckItem(id int, series, season int, msgs ...string) map[string]any {
	return map[string]any{"id": id, "seriesId": series, "seasonNumber": season, "title": "Pack.S01.1080p-GRP", "status": "completed",
		"trackedDownloadState": "importPending", "trackedDownloadStatus": "warning", "size": 20e9, "downloadId": "dl", "outputPath": "/dl/pack",
		"indexer": "BTN", "statusMessages": []map[string]any{{"title": "x", "messages": msgs}}}
}

func attentionEnv(t *testing.T, items ...map[string]any) *env {
	t.Helper()
	v := setup(t, []int{1, 2, 3})
	v.fake.queueItems = items
	v.fake.series = []map[string]any{{"id": 1, "title": "Show One"}, {"id": 2, "title": "Show Two"}}
	_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
	v.inst = store.Instance{ID: 1, Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true}
	v.c = client(v.inst)
	return v
}

func TestAttentionGroupsStuckPacksAndExplainsThem(t *testing.T) {
	v := attentionEnv(t,
		stuckItem(10, 1, 3, "Not an upgrade for existing episode file(s)."),
		stuckItem(11, 1, 3, "Not an upgrade for existing episode file(s).", "Sample"),
		stuckItem(12, 2, 1, "episode file already imported at 10/10/2026 09:49:39"),
		map[string]any{"id": 13, "seriesId": 2, "seasonNumber": 4, "title": "Healthy", "status": "completed", "trackedDownloadState": "importPending", "trackedDownloadStatus": "ok"},
		map[string]any{"id": 14, "seriesId": 2, "seasonNumber": 5, "title": "Still downloading", "status": "downloading", "trackedDownloadState": "downloading", "trackedDownloadStatus": "ok"},
	)
	got, errs := v.e.Attention(context.Background())
	if len(errs) != 0 || len(got) != 2 {
		t.Fatalf("want 2 stuck seasons (healthy and downloading ones excluded), got %d errs=%v", len(got), errs)
	}
	a := got[0]
	if a.Series != "Show One" || a.Season != 3 || a.Items != 2 || len(a.QueueIDs) != 2 || a.SizeGB != 20 {
		t.Fatalf("unexpected group %+v", a)
	}
	if a.Reasons[0].Kind != KindNotUpgrade || a.Reasons[0].Count != 2 || a.Reasons[0].Hint == "" {
		t.Fatalf("reasons must be classified, counted and hinted: %+v", a.Reasons)
	}
}

func TestAttentionSkipsSeasonsAnActiveJobOwns(t *testing.T) {
	v := attentionEnv(t, stuckItem(10, 1, 3, "Not an upgrade for existing episode file(s)."))
	v.job(t, time.Time{}) // active job for series 1 season 3
	if got, _ := v.e.Attention(context.Background()); len(got) != 0 {
		t.Fatalf("a season with an active job is not an orphan, got %+v", got)
	}
}

func TestAdoptCreatesAJobAndVerifiesWithoutDeletingUnderDryRun(t *testing.T) {
	v := attentionEnv(t, stuckItem(10, 1, 3, "Not an upgrade for existing episode file(s)."))
	j, err := v.e.Adopt(context.Background(), v.inst, 1, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "ready" || len(v.fake.deleted) != 0 {
		t.Fatalf("with dry run on, an adopted pack is verified and waits: got %s %q deleted=%v", j.Status, j.Message, v.fake.deleted)
	}
	if len(j.OldFileIDs) != 3 {
		t.Fatalf("the season's current files are what gets replaced, got %v", j.OldFileIDs)
	}
}

func TestAdoptStillRefusesToDeleteWhenVerificationFails(t *testing.T) {
	v := attentionEnv(t, stuckItem(10, 1, 3, "Not an upgrade for existing episode file(s)."))
	probeFn = func(store.Instance, string) (bool, string) { return false, "e.mkv: unreadable or DRM-encrypted" }
	_ = v.st.SaveSettings(live())
	j, err := v.e.Adopt(context.Background(), v.inst, 1, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "holding" || len(v.fake.deleted) != 0 {
		t.Fatalf("a bad pack must hold even when adopted, got %s %q deleted=%v", j.Status, j.Message, v.fake.deleted)
	}
}

func TestRemovePackFlagsAndResearch(t *testing.T) {
	v := attentionEnv(t, stuckItem(10, 1, 3, "Not an upgrade"), stuckItem(11, 1, 3, "Not an upgrade"))
	n, searched, err := v.e.RemovePack(context.Background(), v.inst, 1, 3, RemoveOpts{Blocklist: true, DeleteFiles: true, Research: true})
	if err != nil || n != 2 || !searched {
		t.Fatalf("n=%d searched=%v err=%v", n, searched, err)
	}
	r := v.fake.removed[0]
	if len(r.IDs) != 2 || !strings.Contains(r.Query, "removeFromClient=false") || !strings.Contains(r.Query, "blocklist=true") || !strings.Contains(r.Query, "skipRedownload=true") {
		t.Fatalf("wrong removal %+v", r)
	}
	if len(v.fake.searches) != 1 || v.fake.searches[0] != [2]int{1, 3} {
		t.Fatalf("want one season search for series 1 season 3, got %v", v.fake.searches)
	}
	// keeping the files: no client deletion, no search
	v2 := attentionEnv(t, stuckItem(10, 1, 3, "Not an upgrade"))
	_, searched, _ = v2.e.RemovePack(context.Background(), v2.inst, 1, 3, RemoveOpts{})
	if searched || len(v2.fake.searches) != 0 || !strings.Contains(v2.fake.removed[0].Query, "removeFromClient=false") {
		t.Fatalf("a plain removal must keep client files and not search: %+v %v", v2.fake.removed, v2.fake.searches)
	}
	if _, _, err := v.e.RemovePack(context.Background(), v.inst, 9, 9, RemoveOpts{}); err != store.ErrNotFound {
		t.Fatalf("an unknown season must be not-found, got %v", err)
	}
}

func TestRejectJobBlocklistsSearchesAndNeverTouchesLibraryFiles(t *testing.T) {
	v := attentionEnv(t, stuckItem(10, 1, 3, "x"))
	j := v.job(t, time.Time{})
	j.Status = "holding"
	_ = v.st.UpdateJob(j)
	got, err := v.e.RejectJob(context.Background(), j.ID, true)
	if err != nil || got.Status != "failed" || !strings.Contains(got.Message, "your files were not touched") {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if len(v.fake.deleted) != 0 {
		t.Fatalf("rejecting a pack must never delete library files, deleted=%v", v.fake.deleted)
	}
	if len(v.fake.removed) != 1 || !strings.Contains(v.fake.removed[0].Query, "blocklist=true") || len(v.fake.searches) != 1 {
		t.Fatalf("want a blocklisting removal and a search, got %+v %v", v.fake.removed, v.fake.searches)
	}
	finished := v.job(t, time.Time{})
	finished.Status = "done"
	_ = v.st.UpdateJob(finished)
	if _, err := v.e.RejectJob(context.Background(), finished.ID, false); err == nil {
		t.Fatal("a finished job cannot be rejected")
	}
}

func TestRetryJobReevaluatesImmediately(t *testing.T) {
	v := attentionEnv(t, map[string]any{"seriesId": 1, "seasonNumber": 3, "status": "completed", "downloadId": "d", "outputPath": "/dl/pack", "size": 100.0, "sizeleft": 0.0})
	j := v.job(t, time.Time{})
	j.Status, j.Message = "holding", "waiting: Sonarr already has 8 imports queued"
	_ = v.st.UpdateJob(j)
	got, err := v.e.AdvanceJob(context.Background(), j.ID)
	if err != nil || got.Status != "ready" {
		t.Fatalf("once healthy a retried job moves on (dry run -> ready), got %s %q err=%v", got.Status, got.Message, err)
	}
}

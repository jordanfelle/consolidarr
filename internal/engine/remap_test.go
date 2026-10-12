package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/store"
)

// shiftedPack models Sealab 2021: the pack is in aired order (E01 Alpha, E02 Beta, E03 Gamma) but Sonarr's scene
// mapping puts every file three places later, so the third lands in season 4.
func shiftedPack(v *env) {
	ep := func(id, season, n, file int, title string) map[string]any {
		return map[string]any{"id": id, "seasonNumber": season, "episodeNumber": n, "episodeFileId": file, "title": title, "airDateUtc": airDate(false)}
	}
	v.fake.episodes = []map[string]any{ep(21, 3, 1, 11, "Alpha"), ep(22, 3, 2, 12, "Beta"), ep(23, 3, 3, 13, "Gamma")}
	item := func(file string, season int, mapped ...int) map[string]any {
		var eps []map[string]any
		for _, id := range mapped {
			eps = append(eps, map[string]any{"id": id, "episodeNumber": id - 20})
		}
		return map[string]any{"path": "/dl/pack/" + file, "seasonNumber": season, "series": map[string]int{"id": 1}, "episodes": eps,
			"rejections":   []map[string]string{{"reason": "Not a Custom Format upgrade for existing episode file(s)."}},
			"releaseGroup": "grp", "quality": map[string]any{"quality": map[string]any{"id": 3}}, "languages": []map[string]any{{"id": 1}}, "downloadId": "d"}
	}
	v.fake.items = []map[string]any{
		item("Show.S03E01.Alpha.WEB.mkv", 3, 22),
		item("Show.S03E02.Beta.WEB.mkv", 3, 23),
		item("Show.S03E03.Gamma.WEB.mkv", 4, 24),
	}
}

func TestShiftedPackIsImportedByTitleWithoutDeleting(t *testing.T) {
	v := setup(t, nil)
	shiftedPack(v)
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	got := v.status(t, j.ID)
	if got.Status != "importing" || len(got.Mapping) != 3 {
		t.Fatalf("want importing with a 3-file mapping, got %s %q %v", got.Status, got.Message, got.Mapping)
	}
	if len(v.fake.deleted) != 0 {
		t.Fatalf("a mapped import must delete nothing itself, deleted %v", v.fake.deleted)
	}
	if len(v.fake.mappedImports) != 1 || len(v.fake.mappedImports[0]) != 3 {
		t.Fatalf("want one ManualImport of 3 files, got %v", v.fake.mappedImports)
	}
	for _, f := range v.fake.mappedImports[0] {
		want := map[string]float64{"/dl/pack/Show.S03E01.Alpha.WEB.mkv": 21, "/dl/pack/Show.S03E02.Beta.WEB.mkv": 22, "/dl/pack/Show.S03E03.Gamma.WEB.mkv": 23}[f["path"].(string)]
		if ids := f["episodeIds"].([]any); len(ids) != 1 || ids[0].(float64) != want {
			t.Fatalf("%v mapped to %v, want [%v]", f["path"], ids, want)
		}
		if f["releaseGroup"] != "grp" || f["downloadId"] != "d" {
			t.Fatalf("Sonarr's own reading of the file must be carried over: %v", f)
		}
	}
	if fmt.Sprint(got.ExpectedEps) != "[1 2 3]" {
		t.Fatalf("expected episodes %v", got.ExpectedEps)
	}
}

func TestShiftedPackIsHeldWhenRemappingIsOff(t *testing.T) {
	v := setup(t, nil)
	shiftedPack(v)
	j := v.job(t, time.Time{})
	set := live()
	set.RemapOrders = false
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	got := v.status(t, j.ID)
	if got.Status != "holding" || len(v.fake.deleted) != 0 || v.fake.imports != 0 {
		t.Fatalf("want a hold with nothing touched, got %s %q", got.Status, got.Message)
	}
}

func TestMismappedPackWithMatchingCountsIsStillCaught(t *testing.T) {
	// Every file lands in the season and every episode is "covered", just on the wrong episodes: counts alone would pass.
	v := setup(t, nil)
	shiftedPack(v)
	v.fake.items = []map[string]any{
		v.fake.items[0], v.fake.items[1], v.fake.items[2],
	}
	v.fake.items[2]["seasonNumber"], v.fake.items[2]["episodes"] = 3, []map[string]any{{"id": 21, "episodeNumber": 1}}
	v.fake.items[0]["episodes"] = []map[string]any{{"id": 22, "episodeNumber": 2}}
	v.fake.items[1]["episodes"] = []map[string]any{{"id": 23, "episodeNumber": 3}}
	j := v.job(t, time.Time{})
	set := live()
	set.RemapOrders = false
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "holding" || len(v.fake.deleted) != 0 {
		t.Fatalf("a rotated mapping must not be imported the normal way: %s %q", got.Status, got.Message)
	}
}

func heldMissing(t *testing.T) (*env, store.Job) {
	v := setup(t, []int{1, 2}) // episode 3 is on disk but not in the pack
	v.fake.queueItems = []map[string]any{{"id": 5, "seriesId": 1, "seasonNumber": 3, "downloadId": "d", "status": "completed", "size": 100, "sizeleft": 0, "outputPath": "/dl/pack"}}
	return v, v.job(t, time.Time{})
}

func TestStaleDeterministicHoldIsRejectedAndSeasonSkipped(t *testing.T) {
	v, j := heldMissing(t)
	set := live()
	set.HoldRejectHours = 3
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed")) // binds the download
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed")) // starts the clock
	if got := v.status(t, j.ID); got.Status != "holding" || len(v.fake.removed) != 0 || v.st.KV(holdKey(j.ID)) == "" {
		t.Fatalf("the first sighting only starts the clock: %s", got.Status)
	}
	v.st.SetKV(holdKey(j.ID), holdScope(j, holdMissing)+":"+strconv.FormatInt(time.Now().Add(-4*time.Hour).Unix(), 10))
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	got := v.status(t, j.ID)
	if got.Status != "failed" || len(v.fake.removed) != 1 || len(v.fake.deleted) != 0 || v.fake.imports != 0 {
		t.Fatalf("want failed with the pack removed and nothing else touched: %s %q removed=%v", got.Status, got.Message, v.fake.removed)
	}
	if !v.st.Skipped(skipKey(1, 1, 3)) {
		t.Fatal("the season must be skipped so the same pack is not grabbed straight back")
	}
}

func TestHoldIsNotRejectedBeforeItsGraceOrWhenDisabled(t *testing.T) {
	v, j := heldMissing(t)
	set := live()
	set.HoldRejectHours = 3
	v.st.SetKV(holdKey(j.ID), holdScope(j, holdMissing)+":"+strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10))
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	if v.status(t, j.ID).Status != "holding" {
		t.Fatal("inside the grace period")
	}
	set.HoldRejectHours = 0
	v.st.SetKV(holdKey(j.ID), holdScope(j, holdMissing)+":"+strconv.FormatInt(time.Now().Add(-90*time.Hour).Unix(), 10))
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	if v.status(t, j.ID).Status != "holding" || len(v.fake.removed) != 0 {
		t.Fatal("0 means never")
	}
}

func TestWaitingHoldsAreNeverAutoRejected(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.items = nil
	j := v.job(t, time.Time{})
	set := live()
	set.HoldRejectHours = 1
	v.st.SetKV(holdKey(j.ID), holdScope(j, holdMissing)+":"+strconv.FormatInt(time.Now().Add(-50*time.Hour).Unix(), 10))
	v.fake.itemPath = "/elsewhere/x.mkv" // outside the download folder: a hold that is not a "this pack is wrong" kind
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "holding" || len(v.fake.removed) != 0 {
		t.Fatalf("an unclassified hold must wait for a human: %s", got.Status)
	}
}

func TestHoldClockRestartsWhenTheReasonChanges(t *testing.T) {
	v, j := heldMissing(t)
	set := live()
	set.HoldRejectHours = 3
	// An old clock for a different reason (a probe problem, since fixed) must not count towards this one.
	v.st.SetKV(holdKey(j.ID), holdScope(j, holdProbe)+":"+strconv.FormatInt(time.Now().Add(-90*time.Hour).Unix(), 10))
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	if v.status(t, j.ID).Status != "holding" || len(v.fake.removed) != 0 {
		t.Fatal("a different reason starts a fresh clock")
	}
}

func TestSampleAndExtrasFilesAreNeverStandInsForAnEpisode(t *testing.T) {
	v := setup(t, nil)
	shiftedPack(v)
	// The real "Gamma" is missing from the pack; only a sample and an extra carry that title.
	v.fake.items = v.fake.items[:2]
	v.fake.items = append(v.fake.items,
		map[string]any{"path": "/dl/pack/Sample/show.s03e03.gamma-sample.mkv", "seasonNumber": 3, "series": map[string]int{"id": 1}, "episodes": []map[string]any{{"id": 24, "episodeNumber": 4}}, "rejections": []map[string]string{{"reason": "Sample"}}},
		map[string]any{"path": "/dl/pack/Extras/Gamma.mkv", "seasonNumber": 3, "series": map[string]int{"id": 1}, "episodes": []map[string]any{}, "rejections": []map[string]string{}})
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "holding" || len(v.fake.mappedImports) != 0 || len(v.fake.deleted) != 0 {
		t.Fatalf("a sample or extra must never be imported as an episode: %s %q", got.Status, got.Message)
	}
}

func TestRetryClearsTheHoldClock(t *testing.T) {
	v, j := heldMissing(t)
	set := live()
	set.HoldRejectHours = 3
	v.st.SetKV(holdKey(j.ID), holdScope(j, holdMissing)+":"+strconv.FormatInt(time.Now().Add(-90*time.Hour).Unix(), 10))
	v.fake.failFiles = true // makes the verification unable to decide ("retry")
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	if v.st.KV(holdKey(j.ID)) != "" {
		t.Fatal("a check that could not be made must restart the run of identical holds")
	}
}

func TestAPackThatMerelyLacksAnEpisodeIsNotSearchedForStandIns(t *testing.T) {
	v := setup(t, nil)
	v.fake.episodes = []map[string]any{
		{"id": 21, "seasonNumber": 3, "episodeNumber": 1, "episodeFileId": 11, "title": "Revenge", "airDateUtc": airDate(false)},
		{"id": 28, "seasonNumber": 3, "episodeNumber": 8, "episodeFileId": 0, "title": "Sweet Revenge (1)", "airDateUtc": airDate(false)},
	}
	v.fake.items = []map[string]any{{"path": "/dl/pack/Show.S03E08.Sweet.Revenge.720p.WEB.mkv", "seasonNumber": 3, "series": map[string]int{"id": 1},
		"episodes": []map[string]any{{"id": 28, "episodeNumber": 8}}, "rejections": []map[string]string{}}}
	id, err := v.st.AddJob(store.Job{InstID: 1, SeriesID: 1, Season: 3, Title: "Show", OldFileIDs: []int{11}, Release: "r"})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := v.st.Job(id)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, id); got.Status != "holding" || len(v.fake.mappedImports) != 0 || len(v.fake.deleted) != 0 {
		t.Fatalf("episode 8's file must never replace episode 1: %s %q", got.Status, got.Message)
	}
}

// holdScope is the clock scope once the job is bound to the queue's download "d" (the first pass binds it).
func holdScope(_ store.Job, kind string) string { return kind + "|" + strings.ToLower("d") }

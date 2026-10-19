package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

func epFile(id int, audio string) sonarr.EpisodeFile {
	f := sonarr.EpisodeFile{ID: id, SeasonNumber: 3}
	f.MediaInfo = &struct {
		AudioLanguages string `json:"audioLanguages"`
	}{audio}
	return f
}

// packOf scripts a finished download of three files, one per episode, each with the audio languages given.
func packOf(v *env, t *testing.T, audio map[string][]string, state string) (calls *int) {
	t.Helper()
	var items []map[string]any
	for n := 1; n <= 3; n++ {
		items = append(items, map[string]any{"path": fmt.Sprintf("/dl/pack/Show.S03E%02d.mkv", n), "seasonNumber": 3, "series": map[string]int{"id": 1},
			"episodes": []map[string]int{{"episodeNumber": n}}, "rejections": []map[string]string{}})
	}
	v.fake.items = items
	n := 0
	audioFn = func(_ store.Instance, path string) ([]string, string) {
		n++
		return audio[path[strings.LastIndex(path, "/")+1:]], state
	}
	t.Cleanup(func() { audioFn = probeAudio })
	return &n
}

var allEnglish = map[string][]string{"Show.S03E01.mkv": {"jpn", "eng"}, "Show.S03E02.mkv": {"jpn", "eng"}, "Show.S03E03.mkv": {"jpn", "eng"}}
var subOnly = map[string][]string{"Show.S03E01.mkv": {"jpn"}, "Show.S03E02.mkv": {"jpn"}, "Show.S03E03.mkv": {"jpn"}}

func runAdvance(v *env, j store.Job, set store.Settings) store.Job {
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	js, _ := v.st.Jobs(false, 100)
	for _, x := range js {
		if x.ID == j.ID {
			return x
		}
	}
	return store.Job{}
}

func oldFiles(v *env, audio string) {
	v.fake.filesAfter = []sonarr.EpisodeFile{epFile(11, audio), epFile(12, audio), epFile(13, audio)}
}

func TestADubIsNeverReplacedBySubtitlesOnly(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.origLang = "Japanese"
	oldFiles(v, "eng/jpn") // dual audio today
	packOf(v, t, subOnly, "ok")
	got := runAdvance(v, v.job(t, time.Time{}), live())
	if got.Status != "holding" || !strings.Contains(got.Message, "English audio") {
		t.Fatalf("a sub-only pack must be held for lacking English audio, got %s %q", got.Status, got.Message)
	}
	if len(v.fake.deleted) != 0 || v.fake.imports != 0 {
		t.Fatal("nothing may be deleted or imported")
	}
}

func TestADualPackMayReplaceADub(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.origLang = "Japanese"
	oldFiles(v, "eng/jpn")
	packOf(v, t, allEnglish, "ok")
	if got := runAdvance(v, v.job(t, time.Time{}), live()); got.Status != "importing" {
		t.Fatalf("want importing, got %s %q", got.Status, got.Message)
	}
}

func TestADubOnlyFileOfAForeignShowIsProtectedToo(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.origLang = "Japanese"
	oldFiles(v, "eng") // a single English track on a Japanese show
	packOf(v, t, subOnly, "ok")
	if got := runAdvance(v, v.job(t, time.Time{}), live()); got.Status != "holding" {
		t.Fatalf("want holding, got %s %q", got.Status, got.Message)
	}
}

func TestEnglishAudioOfAnEnglishShowNeedsNoProof(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.origLang = "English"
	oldFiles(v, "eng")
	calls := packOf(v, t, subOnly, "unverified")
	if got := runAdvance(v, v.job(t, time.Time{}), live()); got.Status != "importing" || *calls != 0 {
		t.Fatalf("ordinary English TV must replace as before without any audio probe: %s %q probes=%d", got.Status, got.Message, *calls)
	}
}

func TestSubOnlySeasonConsolidatesAsBefore(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.origLang = "Japanese"
	oldFiles(v, "jpn")
	calls := packOf(v, t, subOnly, "unverified")
	if got := runAdvance(v, v.job(t, time.Time{}), live()); got.Status != "importing" || *calls != 0 {
		t.Fatalf("a season with no dub to protect is unaffected: %s %q probes=%d", got.Status, got.Message, *calls)
	}
}

func TestTheAudioProofCannotBeSkippedWithAllowUnverified(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.origLang = "Japanese"
	oldFiles(v, "eng/jpn")
	packOf(v, t, allEnglish, "unverified")
	set := live()
	set.AllowUnverified = true
	got := runAdvance(v, v.job(t, time.Time{}), set)
	if got.Status != "holding" || len(v.fake.deleted) != 0 {
		t.Fatalf("an unreadable pack must not pass the English check: %s %q", got.Status, got.Message)
	}
}

func TestADubJobMustAddEnglishToEveryEpisode(t *testing.T) {
	for name, tc := range map[string]struct {
		audio map[string][]string
		want  string
	}{
		"complete dub":           {allEnglish, "importing"},
		"one episode undubbed":   {map[string][]string{"Show.S03E01.mkv": {"jpn", "eng"}, "Show.S03E02.mkv": {"jpn"}, "Show.S03E03.mkv": {"jpn", "eng"}}, "holding"},
		"untagged audio":         {map[string][]string{"Show.S03E01.mkv": {"und", "und"}, "Show.S03E02.mkv": {"und"}, "Show.S03E03.mkv": {"und"}}, "holding"},
		"a lying dual-audio tag": {subOnly, "holding"},
	} {
		t.Run(name, func(t *testing.T) {
			v := setup(t, []int{1, 2, 3})
			v.fake.origLang = "Japanese"
			oldFiles(v, "jpn") // sub-only today
			packOf(v, t, tc.audio, "ok")
			j := v.job(t, time.Time{})
			_, _ = v.st.Exec(`update jobs set kind='dub' where id=?`, j.ID)
			j.Kind = "dub"
			got := runAdvance(v, j, live())
			if got.Status != tc.want {
				t.Fatalf("want %s, got %s %q", tc.want, got.Status, got.Message)
			}
			if tc.want == "holding" && (len(v.fake.deleted) != 0 || v.fake.imports != 0) {
				t.Fatal("a pack that is not a dub must not delete or import anything")
			}
		})
	}
}

func TestADubJobPackWithoutAnEnglishTrackIsRejectedLikeAnyWrongPack(t *testing.T) {
	if !rejectable(holdAudio) {
		t.Fatal("a pack that cannot fix itself must not occupy an in-flight slot forever")
	}
}

func dualRelease(guid, title string, langs ...string) map[string]any {
	var ls []map[string]string
	for _, l := range langs {
		ls = append(ls, map[string]string{"name": l})
	}
	return map[string]any{"guid": guid, "indexerId": 1, "indexer": "BTN", "title": title, "size": 20e9, "seeders": 10, "fullSeason": true,
		"seasonNumber": 3, "rejected": false, "rejections": []string{}, "languages": ls,
		"quality": map[string]any{"quality": map[string]any{"id": 3, "name": "WEBDL-1080p", "source": "web", "resolution": 1080}}}
}

func seedDubSeason(t *testing.T, v *env, protected int) {
	t.Helper()
	rows := []audit.Season{{InstID: 1, SeriesID: 1, Season: 3, Title: "Show", SeriesType: "anime", Files: 12, Aired: 12, Pack: 12,
		NoEnglish: 12, Complete: true, Protected: protected, Qualities: []string{"WEBDL-1080p"}, Sources: []string{"web"}, CurRank: 10802}}
	if err := v.st.ReplaceSeasons(1, 0, rows); err != nil {
		t.Fatal(err)
	}
	_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
	v.st.SetKV("lastScan:1", fmt.Sprint(time.Now().Unix()))
}

func cycleOnce(v *env, set store.Settings) {
	v.e.cycle(context.Background(), store.Instance{ID: 1, Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true}, set)
}

func TestFinishedSubOnlyAnimeGetsADualAudioPackNotTheSubOne(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{
		dualRelease("sub", "Show S03 1080p WEB-DL-GRP", "Japanese"),
		dualRelease("dual", "Show S03 1080p WEB-DL Dual Audio-GRP", "Japanese"),
	}
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled, set.DualAudio, set.IncludeAnime = true, true, false
	cycleOnce(v, set)
	jobs, _ := v.st.Jobs(true, 10)
	if len(jobs) != 1 || jobs[0].Kind != "dub" || v.fake.grabs != 1 || !strings.Contains(jobs[0].Release, "Dual Audio") {
		t.Fatalf("want one dub job for the dual pack, got %+v grabs=%d", jobs, v.fake.grabs)
	}
}

func TestNoDualPackMeansNoGrabAndASevenDayWait(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{dualRelease("sub", "Show S03 1080p WEB-DL-GRP", "Japanese")}
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled, set.DualAudio = true, true
	cycleOnce(v, set)
	if v.fake.grabs != 0 || !v.st.Skipped("dub:"+skipKey(1, 1, 3)) {
		t.Fatalf("a sub-only pack must not be grabbed for a dub and the season must wait: grabs=%d", v.fake.grabs)
	}
}

func TestDualAudioIsOffByDefaultAndSkipsUnfinishedSeasons(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{dualRelease("dual", "Show S03 1080p WEB-DL Dual Audio-GRP")}
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled = true
	cycleOnce(v, set)
	if v.fake.grabs != 0 {
		t.Fatal("off by default")
	}
	rows, _ := v.st.Seasons(store.SeasonQuery{InstID: 1})
	rows[0].Complete = false
	_ = v.st.ReplaceSeasons(1, 0, rows)
	set.DualAudio = true
	cycleOnce(v, set)
	if v.fake.grabs != 0 {
		t.Fatal("a season still airing is not chased: its pack does not exist yet")
	}
}

func TestSeasonWithADubOnlyTakesDualPacksEvenOnTheNormalPath(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{dualRelease("sub", "Show S03 1080p WEB-DL-GRP", "Japanese")}
	rows := []audit.Season{{InstID: 1, SeriesID: 1, Season: 3, Title: "Show", SeriesType: "anime", Files: 8, Aired: 8, Single: 8, English: 8, Protected: 8,
		Groups: []string{"a", "b"}, Qualities: []string{"WEBDL-1080p"}, Sources: []string{"web"}, CurRank: 10802}}
	_ = v.st.ReplaceSeasons(1, 0, rows)
	_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
	v.st.SetKV("lastScan:1", fmt.Sprint(time.Now().Unix()))
	set := live()
	set.SchedulerEnabled, set.IncludeAnime = true, true
	cycleOnce(v, set)
	if v.fake.grabs != 0 {
		t.Fatal("a flagged season made of dual-audio files must not be consolidated into a sub-only pack")
	}
}

func TestTwoFilesForOneEpisodeBothNeedEnglish(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.origLang = "Japanese"
	oldFiles(v, "eng/jpn")
	var items []map[string]any
	for _, name := range []string{"Show.S03E01.mkv", "Show.S03E01.v2.mkv", "Show.S03E02.mkv", "Show.S03E03.mkv"} {
		n := int(name[len("Show.S03E0")] - '0')
		items = append(items, map[string]any{"path": "/dl/pack/" + name, "seasonNumber": 3, "series": map[string]int{"id": 1},
			"episodes": []map[string]int{{"episodeNumber": n}}, "rejections": []map[string]string{}})
	}
	v.fake.items = items
	audioFn = func(_ store.Instance, path string) ([]string, string) {
		if strings.HasSuffix(path, "E01.v2.mkv") {
			return []string{"jpn"}, "ok" // the second copy of episode 1 has no dub
		}
		return []string{"jpn", "eng"}, "ok"
	}
	t.Cleanup(func() { audioFn = probeAudio })
	if got := runAdvance(v, v.job(t, time.Time{}), live()); got.Status != "holding" || len(v.fake.deleted) != 0 {
		t.Fatalf("Sonarr may import either copy, so both must have English: %s %q", got.Status, got.Message)
	}
}

func TestCommentaryAndDescriptionTracksAreNotADub(t *testing.T) {
	for title, extra := range map[string]bool{"English Commentary": true, "Commentary (English)": true, "English Audio Description": true,
		"Director's commentary": true, "SDH AD": true, "English": false, "": false, "English 5.1": false} {
		if extraTrack.MatchString(title) != extra {
			t.Errorf("extraTrack(%q) = %v", title, !extra)
		}
	}
	for title, want := range map[string]bool{"English": true, "English 2.0": true, "Japanese / English": true, "Non-English": false, "": false, "Japanese": false} {
		if englishTitle.MatchString(title) != want {
			t.Errorf("englishTitle(%q) = %v", title, !want)
		}
	}
}

func TestOriginalLanguageIsFetchedOncePerHour(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.origLang = "Japanese"
	for i := 0; i < 3; i++ {
		if l, err := v.e.originalLanguage(context.Background(), v.c, 1); err != nil || l != "Japanese" {
			t.Fatalf("%q %v", l, err)
		}
	}
	v.fake.origLang = "English"
	if l, _ := v.e.originalLanguage(context.Background(), v.c, 1); l != "Japanese" {
		t.Fatal("the answer must be cached")
	}
}

func TestDubSearchHonoursTheConsolidationBackOffs(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{dualRelease("dual", "Show S03 1080p WEB-DL Dual Audio-GRP")}
	seedDubSeason(t, v, 0)
	v.st.Skip(skipKey(1, 1, 3), time.Hour, "done") // e.g. a finished job, a lost one, a half-replaced season
	set := live()
	set.SchedulerEnabled, set.DualAudio = true, true
	cycleOnce(v, set)
	if v.fake.grabs != 0 {
		t.Fatal("a season skipped for any reason must not be re-grabbed through the dub path")
	}
}

func TestNoFfprobeMeansNoDubDownloadsThatCouldNeverBeProven(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	ffprobeAvailable = func() bool { return false }
	v.fake.releases = []map[string]any{dualRelease("dual", "Show S03 1080p WEB-DL Dual Audio-GRP")}
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled, set.DualAudio = true, true
	cycleOnce(v, set)
	if v.fake.grabs != 0 {
		t.Fatal("without ffprobe the pack could only be held")
	}
}

func TestDubSearchesAreSpreadOverCyclesSmallestSeasonFirst(t *testing.T) {
	grabPause = 0
	old := dubSearchBudget
	dubSearchBudget = 2
	t.Cleanup(func() { dubSearchBudget = old })
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{dualRelease("sub", "Show S03 1080p WEB-DL-GRP", "Japanese")} // nothing to grab
	var rows []audit.Season
	for i, files := range []int{40, 12, 24, 8} {
		rows = append(rows, audit.Season{InstID: 1, SeriesID: i + 1, Season: 3, Title: fmt.Sprintf("Show %d", i+1), SeriesType: "anime", Files: files,
			Aired: files, NoEnglish: files, Complete: true, Qualities: []string{"WEBDL-1080p"}, CurRank: 10802})
	}
	_ = v.st.ReplaceSeasons(1, 0, rows)
	_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
	v.st.SetKV("lastScan:1", fmt.Sprint(time.Now().Unix()))
	set := live()
	set.SchedulerEnabled, set.DualAudio = true, true
	cycleOnce(v, set)
	searched := func(series int) bool { return v.st.Skipped("dub:" + skipKey(1, series, 3)) }
	if !searched(4) || !searched(2) || searched(3) || searched(1) {
		t.Fatalf("only the two smallest seasons (8 and 12 files) may be searched this cycle: 8=%v 12=%v 24=%v 40=%v", searched(4), searched(2), searched(3), searched(1))
	}
}

func TestAFailedDubSearchBacksOffInsteadOfRetryingEveryCycle(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.failReleases = true
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled, set.DualAudio = true, true
	cycleOnce(v, set)
	if !v.st.Skipped("dub:" + skipKey(1, 1, 3)) {
		t.Fatal("a search that errored must back off")
	}
}

package engine

import (
	"context"
	"fmt"
	"strconv"
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

// Seen live on the first dub job: with auto-delete off the job sat "ready" while Sonarr, which accepts a Remux
// dual-audio pack as a quality upgrade on its own, imported all of it and cleared the download.
func TestAReadyJobWhoseDownloadSonarrImportedItselfIsDoneNotStuck(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Now().Add(-time.Hour))
	_, _ = v.st.Exec(`update jobs set status='ready', release=? where id=?`, "Show S03 1080p WEB x265-NOXXUS", j.ID)
	j, _ = v.st.Job(j.ID)
	v.fake.seasonEps = map[int]int{1: 91, 2: 92, 3: 93}
	v.fake.filesAfter = []sonarr.EpisodeFile{{ID: 91, ReleaseGroup: "NOXXUS"}, {ID: 92, ReleaseGroup: "NOXXUS"}, {ID: 93, ReleaseGroup: "NOXXUS"}}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "done" || len(v.fake.deleted) != 0 {
		t.Fatalf("got %s %q deleted=%v", got.Status, got.Message, v.fake.deleted)
	}
}

func TestAReadyJobWhoseDownloadVanishedAndWasNotImportedStaysReady(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Now().Add(-time.Hour))
	_, _ = v.st.Exec(`update jobs set status='ready' where id=?`, j.ID)
	j, _ = v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "ready" {
		t.Fatalf("a verified job must never be written off as lost: %s %q", got.Status, got.Message)
	}
}

func TestGroupMatchesSceneAndFansubTitles(t *testing.T) {
	for _, c := range []struct {
		title, group string
		want         bool
	}{
		{"Show.S03.1080p.WEB-DL.H.264-NTb", "NTb", true},
		{"[GetItTwisted] PSYCHO-PASS 3 S03 [Blu-ray][MKV][h264][1080p Remux][FLAC 2.0][Dual Audio][Softsubs (GetItTwisted)]", "GetItTwisted", true},
		{"[SubsPlease] Link Click S3 - 03 (1080p) [103F97D0]", "SubsPlease", true},
		{"[GetItTwisted] PSYCHO-PASS 3 S03 [Blu-ray]", "Mysteria", false}, // another group's file is someone else's upgrade
		{"Show.S03.1080p.WEB-DL", "", false},
		{"[Mysteria] Show S03 [GetItTwisted]", "GetItTwisted", false}, // a tag that merely mentions the group
		{"[NTbExtra] Some Show S03", "NTb", false},                    // a longer group name that merely starts with ours
		{"[NTb Extra] Some Show S03", "NTb", false},                   // likewise with a space
	} {
		if got := groupMatches(c.title, c.group); got != c.want {
			t.Errorf("groupMatches(%q, %q) = %v, want %v", c.title, c.group, got, c.want)
		}
	}
}

func TestLongSeasonsAreNotSearchedForADub(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{dualRelease("sub", "Show S03 1080p WEB-DL-GRP", "Japanese")}
	var rows []audit.Season
	for i, files := range []int{12, 13, 14, 136} {
		rows = append(rows, audit.Season{InstID: 1, SeriesID: i + 1, Season: 3, Title: fmt.Sprintf("Show %d", i+1), SeriesType: "anime", Files: files,
			Aired: files, NoEnglish: files, Complete: true, Qualities: []string{"WEBDL-1080p"}, CurRank: 10802})
	}
	_ = v.st.ReplaceSeasons(1, 0, rows)
	_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
	v.st.SetKV("lastScan:1", fmt.Sprint(time.Now().Unix()))
	set := live()
	set.SchedulerEnabled, set.DualAudio, set.DubMaxFiles = true, true, 13
	cycleOnce(v, set)
	searched := func(series int) bool { return v.st.Skipped("dub:" + skipKey(1, series, 3)) }
	if !searched(1) || !searched(2) || searched(3) || searched(4) {
		t.Fatalf("only seasons of 13 files or fewer may be searched: 12=%v 13=%v 14=%v 136=%v", searched(1), searched(2), searched(3), searched(4))
	}
}

func TestSlowDubSearchesStopTheCycleEarly(t *testing.T) {
	for name, tc := range map[string]struct {
		budget time.Duration
		want   int32
	}{"spent budget stops after the first search": {time.Nanosecond, 1}, "ample budget searches all four": {time.Hour, 4}} {
		t.Run(name, func(t *testing.T) {
			grabPause = 0
			oldT, oldB := dubSearchTime, dubSearchBudget
			dubSearchTime, dubSearchBudget = tc.budget, 10
			t.Cleanup(func() { dubSearchTime, dubSearchBudget = oldT, oldB })
			v := setup(t, []int{1, 2, 3})
			v.fake.releases = []map[string]any{dualRelease("sub", "Show S03 1080p WEB-DL-GRP", "Japanese")}
			var rows []audit.Season
			for i := 1; i <= 4; i++ {
				rows = append(rows, audit.Season{InstID: 1, SeriesID: i, Season: 3, Title: fmt.Sprintf("Show %d", i), SeriesType: "anime", Files: 10 + i,
					Aired: 10 + i, NoEnglish: 10 + i, Complete: true, Qualities: []string{"WEBDL-1080p"}, CurRank: 10802})
			}
			_ = v.st.ReplaceSeasons(1, 0, rows)
			_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
			v.st.SetKV("lastScan:1", fmt.Sprint(time.Now().Unix()))
			set := live()
			set.SchedulerEnabled, set.DualAudio, set.DubMaxFiles = true, true, 26
			cycleOnce(v, set)
			if got := v.fake.releaseCalls.Load(); got != tc.want {
				t.Fatalf("release searches served = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTheHTTPClientAllowsTheLongestSearch(t *testing.T) {
	if sonarr.New("http://x", "k").HTTP.Timeout < sonarr.ReleaseSearchTimeout {
		t.Fatal("the client-wide timeout would cut a long search short whatever deadline the call asks for")
	}
}

// The cost of a whole-season search grows with the season for anime (Sonarr asks the indexers about every episode:
// minutes and gigabytes on a real library); the first episode's search returns the covering packs in under a minute.
func TestTheDubSearchAsksForTheFirstEpisodeNotTheWholeSeason(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{dualRelease("sub", "Show S03 1080p WEB-DL-GRP", "Japanese")}
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled, set.DualAudio = true, true
	cycleOnce(v, set)
	q, _ := v.fake.releaseQuery.Load().(string)
	if v.fake.releaseCalls.Load() != 1 || q != "episodeId=501" {
		t.Fatalf("want one search for the season's first episode (episodeId=501), got %d searches, last query %q", v.fake.releaseCalls.Load(), q)
	}
}

func TestADubPackFoundThroughTheEpisodeSearchIsGrabbed(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.releases = []map[string]any{dualRelease("dual", "Show S03 1080p WEB-DL Dual Audio-GRP")}
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled, set.DualAudio = true, true
	cycleOnce(v, set)
	if jobs, _ := v.st.Jobs(true, 10); len(jobs) != 1 || jobs[0].Kind != "dub" {
		t.Fatalf("want one dub job, got %+v", jobs)
	}
}

func (v *env) liveInst() store.Instance {
	return store.Instance{ID: 1, Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true}
}

func grabSeason() audit.Season {
	return audit.Season{InstID: 1, SeriesID: 1, Season: 3, Title: "Show"}
}

func TestAGrabIsResentWhileTheDownloadClientIsUnreachable(t *testing.T) {
	grabRetryWait = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.connectFails = 2 // two stalls, then qBittorrent answers
	id, err := v.e.Grab(context.Background(), v.liveInst(), grabSeason(), "g", 1, "Show.S03", 10, "BTN", "")
	if err != nil || id == 0 || v.fake.postAttempts.Load() != 3 || v.fake.grabs != 1 {
		t.Fatalf("id=%d err=%v attempts=%d grabs=%d", id, err, v.fake.postAttempts.Load(), v.fake.grabs)
	}
}

func TestAGrabGivesUpWhenTheClientStaysDownAndReleasesTheSeason(t *testing.T) {
	grabRetryWait = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.connectFails = 100
	if _, err := v.e.Grab(context.Background(), v.liveInst(), grabSeason(), "g", 1, "Show.S03", 10, "BTN", ""); err == nil {
		t.Fatal("want an error")
	}
	if n := v.fake.postAttempts.Load(); n != grabClientAttempts {
		t.Fatalf("want %d attempts, got %d", grabClientAttempts, n)
	}
	if jobs, _ := v.st.Jobs(true, 10); len(jobs) != 0 {
		t.Fatal("the reservation must be released")
	}
}

func TestOtherGrabErrorsAreNotResent(t *testing.T) {
	grabRetryWait = 0
	v := setup(t, []int{1, 2, 3})
	v.fake.failGrab = true // a plain 500: the request may have reached the client, so never send it twice
	_, _ = v.e.Grab(context.Background(), v.liveInst(), grabSeason(), "g", 1, "Show.S03", 10, "BTN", "")
	if n := v.fake.postAttempts.Load(); n != 1 {
		t.Fatalf("a grab that may have reached the client must be sent once, got %d", n)
	}
}

func TestAClientOutageBacksTheSeasonOffForAnHourNotADay(t *testing.T) {
	grabPause, grabRetryWait = 0, 0
	v := setup(t, []int{1, 2, 3})
	v.fake.connectFails = 100
	v.fake.releases = []map[string]any{dualRelease("dual", "Show S03 1080p WEB-DL Dual Audio-GRP")}
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled, set.DualAudio = true, true
	cycleOnce(v, set)
	key := "dub:" + skipKey(1, 1, 3)
	var until int64
	_ = v.st.QueryRow(`select until from skips where key=?`, key).Scan(&until)
	if left := time.Until(time.Unix(until, 0)); left > 2*time.Hour || left < 30*time.Minute {
		t.Fatalf("back-off should be about an hour, is %v", left)
	}
}

func TestARejectedDubPackIsRetriedWithinHoursNotAWeek(t *testing.T) {
	v, j := heldMissing(t)
	_, _ = v.st.Exec(`update jobs set kind='dub' where id=?`, j.ID)
	j.Kind = "dub"
	set := live()
	set.HoldRejectHours = 3
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	v.st.SetKV(holdKey(j.ID), holdScope("d", holdMissing)+":"+strconv.FormatInt(time.Now().Add(-4*time.Hour).Unix(), 10))
	v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "failed" {
		t.Fatalf("want the held pack rejected: %s %q", got.Status, got.Message)
	}
	for _, key := range []string{skipKey(1, 1, 3), "dub:" + skipKey(1, 1, 3)} {
		var until int64
		_ = v.st.QueryRow(`select until from skips where key=?`, key).Scan(&until)
		if left := time.Until(time.Unix(until, 0)); left < 5*time.Hour || left > 7*time.Hour {
			t.Fatalf("%s: a rejected dub should be retried in about 6 hours, back-off is %v", key, left)
		}
	}
}

package audit

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
)

func q(name, source string, res int) sonarr.Quality {
	var x sonarr.Quality
	x.Quality.Name, x.Quality.Source, x.Quality.Resolution = name, source, res
	return x
}

func rel(guid, indexer string, gb float64, seeds int, ql sonarr.Quality, rej ...string) sonarr.Release {
	return sonarr.Release{GUID: guid, Indexer: indexer, Size: int64(gb * 1e9), Seeders: &seeds, FullSeason: true, SeasonNum: 3, Quality: ql, Rejections: rej}
}

func TestIsFlagged(t *testing.T) {
	cases := []struct {
		name string
		s    Season
		want bool
	}{
		{"no packs, complete", Season{Files: 8, Aired: 8}, true},
		{"has a pack file", Season{Files: 8, Aired: 8, Pack: 1}, false},
		{"too few files", Season{Files: 2, Aired: 2}, false},
		{"season only half on disk", Season{Files: 4, Aired: 10}, false},
		{"80% on disk is enough", Season{Files: 8, Aired: 10}, true},
	}
	for _, c := range cases {
		if got := IsFlagged(c.s, 3); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestPickIgnoresCutoffRejectionOnly(t *testing.T) {
	web := q("WEBDL-1080p", "web", 1080)
	s := Season{Season: 3, CurRank: web.Rank()}
	rels := []sonarr.Release{
		rel("blocked", "A", 30, 100, web, "Release is blocklisted"),
		rel("cutoff", "A", 30, 50, web, "Existing file meets cutoff: 1080p"),
	}
	p := Pick(rels, s, PickOpts{MaxGB: 60})
	if p == nil || p.GUID != "cutoff" {
		t.Fatalf("want cutoff-only release picked, got %+v", p)
	}
}

func TestPickLastResortIndexerAndBounds(t *testing.T) {
	web := q("WEBDL-1080p", "web", 1080)
	hd := q("Bluray-1080p", "bluray", 1080)
	s := Season{Season: 3, CurRank: web.Rank()}
	rels := []sonarr.Release{
		rel("tl-best", "TorrentLeech (Prowlarr)", 20, 500, hd),
		rel("btn", "BroadcastheNet", 20, 5, web),
		rel("huge", "BroadcastheNet", 90, 900, web),
		rel("worse", "BroadcastheNet", 5, 900, q("WEBDL-720p", "web", 720)),
	}
	p := Pick(rels, s, PickOpts{MaxGB: 60, LastResort: []string{"torrentleech"}})
	if p == nil || p.GUID != "btn" {
		t.Fatalf("want btn (non-last-resort, within size, not a downgrade), got %+v", p)
	}
	p = Pick(rels[:1], s, PickOpts{MaxGB: 60, LastResort: []string{"torrentleech"}})
	if p == nil || p.GUID != "tl-best" {
		t.Fatalf("last-resort indexer must still be used when it's the only option, got %+v", p)
	}
	if Pick(rels[2:], s, PickOpts{MaxGB: 60}) != nil {
		t.Fatal("oversized and downgraded packs must not be picked")
	}
}

func TestBuildCountsGroupsCaseInsensitively(t *testing.T) {
	f := func(id int, g, rt string) sonarr.EpisodeFile {
		return sonarr.EpisodeFile{ID: id, SeasonNumber: 1, ReleaseGroup: g, ReleaseType: rt, Size: 1e9, Quality: q("WEBDL-1080p", "web", 1080)}
	}
	s := sonarr.Series{ID: 1, Title: "X", Seasons: []sonarr.SeasonStat{{SeasonNumber: 1}}}
	s.Seasons[0].Statistics.EpisodeCount = 3
	rows := Build(1, s, []sonarr.EpisodeFile{f(1, "NTb", "singleEpisode"), f(2, "ntb", "singleEpisode"), f(3, "FLUX", "unknown")}, 3)
	if len(rows) != 1 || len(rows[0].Groups) != 2 || !rows[0].Flagged || rows[0].Single != 2 || rows[0].Unknown != 1 {
		t.Fatalf("unexpected row %+v", rows)
	}
}

func TestZeroMaxGBMeansNoSizeLimit(t *testing.T) {
	web := q("WEBDL-1080p", "web", 1080)
	s := Season{Season: 3, CurRank: web.Rank()}
	rels := []sonarr.Release{rel("huge", "BTN", 400, 10, web)}
	if Pick(rels, s, PickOpts{MaxGB: 60}) != nil {
		t.Fatal("a 400 GB pack must be rejected under a 60 GB cap")
	}
	if p := Pick(rels, s, PickOpts{MaxGB: 0}); p == nil || p.GUID != "huge" {
		t.Fatalf("MaxGB 0 must mean no limit, got %+v", p)
	}
}

func TestParseAudio(t *testing.T) {
	for in, want := range map[string]Audio{
		"eng/jpn": {Known: true, English: true, Other: true, Tracks: 2},
		"jpn/eng": {Known: true, English: true, Other: true, Tracks: 2},
		"eng":     {Known: true, English: true, Tracks: 1},
		"English": {Known: true, English: true, Tracks: 1},
		"jpn":     {Known: true, Other: true, Tracks: 1},
		"zho":     {Known: true, Other: true, Tracks: 1},
		"":        {},
		"und":     {Undetermined: true, Tracks: 1},
		// two tracks, one untagged and none English: maybe an unlabelled dub, so neither "sub only" nor "has English"
		"jpn/und": {Other: true, Undetermined: true, Tracks: 2},
	} {
		if got := ParseAudio(in); got != want {
			t.Errorf("ParseAudio(%q) = %+v, want %+v", in, got, want)
		}
	}
	if !ParseAudio("jpn/und").MaybeDub() || ParseAudio("und").MaybeDub() || ParseAudio("jpn").MaybeDub() {
		t.Error("only a multi-track file with an untagged track may be an unlabelled dub")
	}
}

func TestProtectedDependsOnWhoMadeTheShow(t *testing.T) {
	for _, c := range []struct {
		audio, orig string
		want        bool
	}{
		{"eng/jpn", "Japanese", true}, {"eng", "Japanese", true}, {"jpn/und", "Japanese", true},
		{"jpn", "Japanese", false}, {"und", "Japanese", false}, {"", "Japanese", false},
		{"eng", "English", false}, {"eng/spa", "English", false}, // English is the original: any pack has it
		{"eng/jpn", "", true}, {"eng", "", false}, // origin unknown: only a visibly dual file is protected
	} {
		if got := Protected(ParseAudio(c.audio), c.orig); got != c.want {
			t.Errorf("Protected(%q, %q) = %v, want %v", c.audio, c.orig, got, c.want)
		}
	}
}

func TestIsDualReadsTheReleaseNameNotSonarrsLanguageLabel(t *testing.T) {
	yes := []string{
		"[Dae-P9] LINK CLICK S01 [Blu-ray][1080p][FLAC 2.0][Dual Audio][Softsubs (Dae-P9)]",
		"Show.S01.1080p.BluRay.Dual-Audio.x265-GRP", "Show S01 1080p DUAL AUDIO", "Show.S02.Multi-Audio.1080p", "Show S01 English Dub 1080p",
		"Show S01 ENG.DUB 1080p", "Show.S01.English.Dubbed.720p", "Show S01 [Dual] 1080p", "Show.S01.1080p.Dual.Lang.x264",
	}
	no := []string{"Show.S01.1080p.WEB-DL-GRP", "Show S01 Multi-Subs 1080p", "Show.S01.MULTi.1080p", "Individual.S01.1080p", "Show S01 Dualshock", "Show S01 Redub"}
	for _, n := range yes {
		if !IsDual(sonarr.Release{Title: n}) {
			t.Errorf("%q should advertise a dub", n)
		}
	}
	for _, n := range no {
		if IsDual(sonarr.Release{Title: n}) {
			t.Errorf("%q should not advertise a dub", n)
		}
	}
}

func dubRel(title string, res int, source string) sonarr.Release {
	r := sonarr.Release{Title: title, FullSeason: true, SeasonNum: 1, Size: 10e9}
	r.Quality.Quality.Resolution, r.Quality.Quality.Source, r.Quality.Quality.Name = res, source, "q"
	return r
}

func TestDubPickAcceptsOnlyDualPacksAtNoLowerResolutionButAnySource(t *testing.T) {
	s := Season{Season: 1, CurRank: 10803} // a 1080p Blu-ray sub today
	rels := []sonarr.Release{dubRel("S01 1080p BD", 1080, "bluray"), dubRel("S01 720p Dual Audio", 720, "web"), dubRel("S01 1080p WEB Dual Audio", 1080, "web")}
	p := Pick(rels, s, PickOpts{Dub: true})
	if p == nil || p.Title != "S01 1080p WEB Dual Audio" {
		t.Fatalf("got %+v: a 1080p web dub may replace a Blu-ray sub, a 720p one or a sub-only one may not", p)
	}
	if Pick([]sonarr.Release{dubRel("S01 1080p BD", 1080, "bluray")}, s, PickOpts{Dub: true}) != nil {
		t.Fatal("a sub-only pack is never a dub")
	}
}

func TestAConsolidationNeverPicksASubPackForASeasonWithADubToProtect(t *testing.T) {
	rels := []sonarr.Release{dubRel("S01 1080p BD", 1080, "bluray"), dubRel("S01 1080p BD Dual Audio", 1080, "bluray")}
	if p := Pick(rels, Season{Season: 1, CurRank: 10802, Protected: 3}, PickOpts{}); p == nil || !IsDual(*p) {
		t.Fatalf("got %+v", p)
	}
	if p := Pick(rels, Season{Season: 1, CurRank: 10802}, PickOpts{}); p == nil || p.Title != "S01 1080p BD" {
		t.Fatalf("with nothing to protect the ordinary best-quality rule stands: %+v", p)
	}
}

func TestBuildCountsAudioAndCompleteness(t *testing.T) {
	f := func(id int, audio string) sonarr.EpisodeFile {
		x := sonarr.EpisodeFile{ID: id, SeasonNumber: 1}
		x.MediaInfo = &struct {
			AudioLanguages string `json:"audioLanguages"`
		}{audio}
		return x
	}
	mk := func(orig string, ep, total int) sonarr.Series {
		s := sonarr.Series{ID: 1, SeriesType: "anime", Seasons: []sonarr.SeasonStat{{SeasonNumber: 1}}}
		s.OriginalLanguage.Name = orig
		s.Seasons[0].Statistics.EpisodeCount, s.Seasons[0].Statistics.TotalEpisodeCount = ep, total
		return s
	}
	files := []sonarr.EpisodeFile{f(1, "eng/jpn"), f(2, "jpn"), f(3, "eng"), f(4, ""), f(5, "jpn"), f(6, "chi")}
	r := Build(1, mk("Japanese", 6, 6), files, 3)[0]
	if r.English != 2 || r.NoEnglish != 3 || r.AudioUnk != 1 || r.Protected != 2 || !r.Complete {
		t.Fatalf("%+v", r)
	}
	if !DubWanted(r) {
		t.Fatal("a finished anime season with sub-only files is wanted")
	}
	if DubWanted(Season{SeriesType: "anime", Files: 43, NoEnglish: 1, English: 42, Complete: true}) {
		t.Fatal("one stray sub-only file in a dubbed season must not trigger a wholesale replacement")
	}
	if !DubWanted(Season{SeriesType: "anime", Files: 26, NoEnglish: 21, English: 5, Complete: true}) {
		t.Fatal("a mostly-sub season with a few dubbed episodes is wanted")
	}
	if r2 := Build(1, mk("Japanese", 6, 24), files, 3)[0]; r2.Complete || DubWanted(r2) {
		t.Fatal("a season with unaired episodes is not chased")
	}
	if r3 := Build(1, mk("English", 6, 6), files, 3)[0]; r3.Protected != 0 {
		t.Fatalf("nothing in an English-original show has a dub to protect: %+v", r3)
	}
}

// A real episode-search response (Higurashi S01, trimmed): the genuine packs carry only "Full season pack" and
// upgrade-preference rejections, which used to make every one of them unusable for the dub pass.
func realDualPacks(t *testing.T) []sonarr.Release {
	b, err := os.ReadFile("../sonarr/testdata/release_episode_search_dual.json")
	if err != nil {
		t.Fatal(err)
	}
	var rs []sonarr.Release
	if err := json.Unmarshal(b, &rs); err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestTheDubPassIgnoresUpgradeAndEpisodeSearchRejections(t *testing.T) {
	rs := realDualPacks(t)
	s := Season{Season: 1, CurRank: 10801} // 24 HDTV-1080p files today
	for i := range rs {
		rs[i].FullSeason, rs[i].SeasonNum = true, 1
	}
	if Pick(rs, s, PickOpts{}) != nil {
		t.Fatal("the consolidation rule is unchanged: 'Full season pack' still blocks")
	}
	p := Pick(rs, s, PickOpts{Dub: true})
	if p == nil {
		t.Fatal("the dub pass found no pack in a response full of dual-audio ones")
	}
	for _, m := range BlockingForDub(*p) {
		t.Errorf("picked a pack with a blocking rejection %q", m)
	}
	if !IsDual(*p) || p.Quality.Quality.Resolution < 1080 {
		t.Errorf("picked %q (%s)", p.Title, p.Quality.Quality.Name)
	}
}

func TestTheDubPassStillHonoursTheRejectionsThatMatter(t *testing.T) {
	rs := realDualPacks(t)
	for i := range rs {
		rs[i].FullSeason, rs[i].SeasonNum = true, 1
	}
	for _, x := range rs {
		blocked := BlockingForDub(x)
		switch {
		case strings.Contains(x.Title, "[BYT]"):
			if len(blocked) == 0 {
				t.Errorf("a blocklisted release must stay blocked: %v", x.Rejections)
			}
		case strings.Contains(x.Title, "SOTSU") || strings.Contains(x.Title, "Sotsu"):
			if len(blocked) == 0 {
				t.Errorf("a pack for another season ('Episode wasn't requested') must stay blocked: %q", x.Title)
			}
		case len(x.Rejections) == 1 && x.Rejections[0] == "Unknown Series":
			if len(blocked) == 0 {
				t.Errorf("an unknown series must stay blocked: %q", x.Title)
			}
		case strings.Contains(strings.Join(x.Rejections, "|"), "smaller than minimum"):
			if len(blocked) == 0 {
				t.Errorf("a pack under the size limit must stay blocked: %q", x.Title)
			}
		}
	}
}

// AnimeBytes numbers every cour "S01", so the dub of season 3 arrives as a season 1 pack. Fields as Sonarr returned
// them for Oshi no Ko (torrent 1233944 and an older real S1 pack).
func relabelRel(guid, indexer string, published string, rej ...string) sonarr.Release {
	r := rel(guid, indexer, 19.9, 65, q("Bluray-1080p", "bluray", 1080), rej...)
	r.SeasonNum, r.Title, r.PublishDate = 1, "[YURASUKA] Oshi no Ko S01 [Blu-ray][1080p][Opus 2.0][Dual Audio]", published
	return r
}

func TestARelabelledAnimeBytesPackStandsInForALaterSeasonOnlyWhenPublishedAfterItAired(t *testing.T) {
	aired := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	s := Season{Season: 3, CurRank: 10802}
	notReq := "Episode wasn't requested: 1x1-11"
	fresh := relabelRel("s3", "AnimeBytes (Prowlarr)", "2026-07-25T05:24:37Z", notReq, "Full season pack")
	old := relabelRel("s1", "AnimeBytes (Prowlarr)", "2025-11-15T07:40:27Z", notReq, "Full season pack")
	o := PickOpts{Dub: true, Relabelled: true, Since: aired}
	if p := Pick([]sonarr.Release{old, fresh}, s, o); p == nil || p.GUID != "s3" {
		t.Fatalf("want the pack published after the season aired, got %+v", p)
	}
	if p := Pick([]sonarr.Release{old}, s, o); p != nil {
		t.Fatalf("a pack published before the season aired is the real season 1: %+v", p)
	}
	for name, mod := range map[string]func(*PickOpts, *sonarr.Release, *Season){
		"off by default":    func(o *PickOpts, _ *sonarr.Release, _ *Season) { o.Relabelled = false },
		"not the dub pass":  func(o *PickOpts, _ *sonarr.Release, _ *Season) { o.Dub = false },
		"another indexer":   func(_ *PickOpts, r *sonarr.Release, _ *Season) { r.Indexer = "Nyaa.si" },
		"not numbered S01":  func(_ *PickOpts, r *sonarr.Release, _ *Season) { r.SeasonNum = 2 },
		"season 1 itself":   func(_ *PickOpts, _ *sonarr.Release, s *Season) { s.Season = 1 },
		"no air date known": func(o *PickOpts, _ *sonarr.Release, _ *Season) { o.Since = time.Time{} },
		"not dual audio":    func(_ *PickOpts, r *sonarr.Release, _ *Season) { r.Title = "[Group] Oshi no Ko S01 [1080p]" },
		"other rejection": func(_ *PickOpts, r *sonarr.Release, _ *Season) {
			r.Rejections = append(r.Rejections, "Release is blocklisted")
		},
		"size below minimum": func(_ *PickOpts, r *sonarr.Release, _ *Season) {
			r.Rejections = append(r.Rejections, "15.9 GB is smaller than minimum allowed 16.0 GB")
		},
		"unpublished/no date": func(_ *PickOpts, r *sonarr.Release, _ *Season) { r.PublishDate = "" },
	} {
		oo, rr, ss := o, fresh, s
		mod(&oo, &rr, &ss)
		if p := Pick([]sonarr.Release{rr}, ss, oo); p != nil {
			t.Errorf("%s: still picked %q", name, p.Title)
		}
	}
}

// Live (#88): a 1 TB "Complete [Extras]" whole-series torrent was picked for one cour.
func TestARelabelledPackThatSaysItHoldsMoreThanOneSeasonIsNeverPicked(t *testing.T) {
	aired := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	s := Season{Season: 8, CurRank: 10802}
	o := PickOpts{Dub: true, Relabelled: true, Since: aired}
	for _, title := range []string{
		"[JySzE] Naruto Shippuden [Dual Audio] [Complete] [Extras] [x265]",
		"[Grp] Show Complete Collection Dual Audio",
		"[Grp] Show S01-S08 Dual Audio",
		"[Grp] Show Season 1-8 Dual Audio",
		"[Grp] Show Batch Dual Audio",
	} {
		r := relabelRel("x", "AnimeBytes (Prowlarr)", "2026-07-25T05:24:37Z")
		r.Title = title
		if p := Pick([]sonarr.Release{r}, s, o); p != nil {
			t.Errorf("picked %q", title)
		}
	}
	ok := relabelRel("ok", "AnimeBytes (Prowlarr)", "2026-07-25T05:24:37Z") // "[YURASUKA] Oshi no Ko S01 [Blu-ray]..."
	if p := Pick([]sonarr.Release{ok}, s, o); p == nil {
		t.Fatal("an ordinary single-cour pack must still be picked")
	}
}

func TestMultiSeasonTitlePatterns(t *testing.T) {
	yes := []string{"Show S01-S08 Dual", "Show S01-08 Dual", "Show S01+S02", "Show Seasons 1+2", "Show Seasons 1&2", "Show S01–S03",
		"Show Seasons 1, 2", "Show BD-BOX", "Show Box Set", "Show Vol.1-4", "Show Complete Series", "Show Batch",
		"Show S01.S02.1080p", "Show S01S02", "Show S01 to S03", "Show Seasons 1 thru 3", "Show Extras"}
	no := []string{"[YURASUKA] Oshi no Ko S01 [Blu-ray][1080p][Opus 2.0][Dual Audio]", "Show S01E01-E02", "Show S01E01-S01E13 Dual", "Show S03 1080p BluRay x265-GRP", "Show 2nd Season [BD]",
		"Show S01 - 1080 BluRay", "Show S01 - 10 Dual", "Show S01 + 1080", "Show Season 3 - 2024", "Show - Extra Edition S01", "Show S03-1080p x265"}
	for _, s := range yes {
		if !multiSeasonTitle.MatchString(s) {
			t.Errorf("should be multi-season: %q", s)
		}
	}
	for _, s := range no {
		if multiSeasonTitle.MatchString(s) {
			t.Errorf("should NOT be multi-season: %q", s)
		}
	}
}

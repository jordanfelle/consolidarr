package audit

import (
	"testing"

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

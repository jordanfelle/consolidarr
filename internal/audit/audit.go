// Package audit finds seasons assembled from single-episode grabs and picks replacement packs.
package audit

import (
	"sort"
	"strings"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
)

type Season struct {
	InstID     int      `json:"instId"`
	SeriesID   int      `json:"seriesId"`
	Season     int      `json:"season"`
	Title      string   `json:"title"`
	SeriesType string   `json:"seriesType"`
	Monitored  bool     `json:"monitored"`
	Files      int      `json:"files"`
	Aired      int      `json:"aired"`
	Pack       int      `json:"pack"`
	Single     int      `json:"single"`
	Multi      int      `json:"multi"`
	Unknown    int      `json:"unknown"`
	Groups     []string `json:"groups"`
	Qualities  []string `json:"qualities"`
	Sources    []string `json:"sources"`
	GB         float64  `json:"gb"`
	CurRank    int      `json:"curRank"`
	Path       string   `json:"path"`
	Score      float64  `json:"score"`
	Flagged    bool     `json:"flagged"`
}

func uniq(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build summarises one series' episode files into per-season rows.
func Build(instID int, s sonarr.Series, files []sonarr.EpisodeFile, minFiles int) []Season {
	stat := map[int]int{}
	for _, x := range s.Seasons {
		stat[x.SeasonNumber] = x.Statistics.EpisodeCount
	}
	by := map[int][]sonarr.EpisodeFile{}
	for _, f := range files {
		by[f.SeasonNumber] = append(by[f.SeasonNumber], f)
	}
	var out []Season
	for n, fs := range by {
		if n == 0 || len(fs) < 2 {
			continue
		}
		r := Season{InstID: instID, SeriesID: s.ID, Season: n, Title: s.Title, SeriesType: s.SeriesType,
			Monitored: s.Monitored, Files: len(fs), Aired: stat[n], Path: s.Path}
		groups, quals, srcs := map[string]bool{}, map[string]bool{}, map[string]bool{}
		var bytes int64
		for _, f := range fs {
			switch strings.ToLower(f.ReleaseType) {
			case "seasonpack":
				r.Pack++
			case "singleepisode":
				r.Single++
			case "multiepisode":
				r.Multi++
			default:
				r.Unknown++
			}
			g := strings.ToLower(f.ReleaseGroup)
			if g == "" {
				g = "?"
			}
			groups[g] = true
			quals[f.Quality.Quality.Name] = true
			srcs[f.Quality.Quality.Source] = true
			if rk := f.Quality.Rank(); rk > r.CurRank {
				r.CurRank = rk
			}
			bytes += f.Size
		}
		r.Groups, r.Qualities, r.Sources = uniq(groups), uniq(quals), uniq(srcs)
		r.GB = float64(bytes) / 1e9
		r.Score = float64(len(r.Groups)*3+len(r.Qualities)*2+len(r.Sources)*2) + float64(r.Files)*0.2
		r.Flagged = IsFlagged(r, minFiles)
		out = append(out, r)
	}
	return out
}

// IsFlagged: no pack-sourced files, enough files, and nearly the whole aired season on disk.
func IsFlagged(r Season, minFiles int) bool {
	aired := r.Aired
	if aired < 1 {
		aired = 1
	}
	return r.Pack == 0 && r.Files >= minFiles && float64(r.Files) >= float64(aired)*0.8
}

type PickOpts struct {
	MaxGB      float64  // 0 = no size limit
	LastResort []string // indexer name substrings used only when nothing else qualifies
}

// Pick chooses a pack deterministically. Sonarr's own rejections are honoured except
// "Existing file meets cutoff", which is expected because the old files get replaced.
func Pick(rels []sonarr.Release, s Season, o PickOpts) *sonarr.Release {
	var c []sonarr.Release
	for _, x := range rels {
		if !x.FullSeason || x.SeasonNum != s.Season || (o.MaxGB > 0 && float64(x.Size)/1e9 > o.MaxGB) || x.Quality.Rank() < s.CurRank {
			continue
		}
		if len(Blocking(x)) > 0 {
			continue
		}
		c = append(c, x)
	}
	last := func(x sonarr.Release) bool {
		for _, l := range o.LastResort {
			if l != "" && strings.Contains(strings.ToLower(x.Indexer), strings.ToLower(l)) {
				return true
			}
		}
		return false
	}
	seeds := func(x sonarr.Release) int {
		if x.Seeders == nil {
			return 0
		}
		return *x.Seeders
	}
	sort.SliceStable(c, func(i, j int) bool {
		if last(c[i]) != last(c[j]) {
			return !last(c[i])
		}
		if c[i].Quality.Rank() != c[j].Quality.Rank() {
			return c[i].Quality.Rank() > c[j].Quality.Rank()
		}
		return seeds(c[i]) > seeds(c[j])
	})
	if len(c) == 0 {
		return nil
	}
	return &c[0]
}

// Blocking returns the rejections that actually matter.
func Blocking(x sonarr.Release) []string {
	var b []string
	for _, m := range x.Rejections {
		if !strings.HasPrefix(m, "Existing file meets cutoff") {
			b = append(b, m)
		}
	}
	return b
}

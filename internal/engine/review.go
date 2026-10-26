package engine

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jordanfelle/consolidarr/internal/order"
)

// Review verdicts for a relabelled pack, judged from its file names alone.
const (
	reviewMatch    = "match"    // the files say they are this season
	reviewMismatch = "mismatch" // the files say they are another season, or the pack cannot cover this one
	reviewUnknown  = "unknown"  // nothing in the names settles it: a human has to look
)

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".ts": true}

// reviewCheck decides whether a pack's files are the season the job is for. target is that season's episodes in
// Sonarr, all every episode of the series (to name the season a pack really is). A mismatch is certain enough to
// throw the download away; "unknown" always goes to a human.
func reviewCheck(names []string, target, all []order.Ep, season int) (verdict, detail string) {
	var videos []string
	for _, n := range names {
		n = strings.TrimSuffix(n, ".!qB") // qBittorrent's suffix for an unfinished file
		if videoExt[strings.ToLower(filepath.Ext(n))] && !inExtras(n, "") && !underExtras(n) {
			videos = append(videos, n)
		}
	}
	if len(videos) == 0 {
		return reviewUnknown, "no video files in the torrent"
	}
	// Only positive evidence from the names may condemn a pack (the caller throws it away on a mismatch). A short file
	// count proves nothing: Sonarr's list can include recaps, specials and merged episodes.
	short := len(videos) < len(target)
	var here, elsewhere int
	other := map[int]int{}
	for _, n := range videos {
		f := order.NewFile(n, nil)
		switch {
		case f.HasSxE && f.Season > 0:
			if f.Season == season {
				here++
			}
			// Any other SxxEyy is neutral too: scene numbering is not TVDB's (an S02 pack can be TVDB season 3), and
			// "S01" is what the indexer calls every cour. Only an episode title that belongs to another season counts
			// against a pack, because that cannot be a numbering convention.
		default:
			if _, ok := order.TitleAnywhere(n, target); ok {
				here++
			} else if ep, ok := order.TitleAnywhere(n, all); ok && ep.Season != season {
				elsewhere++
				other[ep.Season]++
			}
		}
	}
	switch {
	case here*5 >= len(videos)*4 && !short:
		return reviewMatch, fmt.Sprintf("%d of %d files name season %d or one of its episode titles", here, len(videos), season)
	case elsewhere > here && elsewhere*5 >= len(videos)*4:
		best := 0
		for s, n := range other {
			if n > other[best] || best == 0 {
				best = s
			}
		}
		return reviewMismatch, fmt.Sprintf("%d of %d files are season %d, not season %d", elsewhere, len(videos), best, season)
	}
	if short {
		return reviewUnknown, fmt.Sprintf("the pack has %d video files but season %d has %d episodes", len(videos), season, len(target))
	}
	return reviewUnknown, fmt.Sprintf("only %d of %d file names identify the season", here, len(videos))
}

// underExtras reports whether any folder in the torrent's relative path is a sample/extras folder.
func underExtras(name string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Dir(name)), "/")
	for _, p := range parts {
		if extrasDir.MatchString(p) {
			return true
		}
	}
	return false
}

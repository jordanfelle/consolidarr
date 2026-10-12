// Package order works out which episode each file of a season pack really is when Sonarr's own numbering
// disagrees with the pack's (aired vs DVD vs absolute order, XEM scene mappings, files named by production code).
//
// The evidence is always the episode title: a file is only placed on an episode when the title in its file name
// is that episode's title. Numbers alone are never trusted, because a different ordering is exactly the case where
// the numbers lie. Anything ambiguous is left unplaced, and a pack is only usable when every episode the user has
// today is placed exactly once.
package order

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Ep is one of Sonarr's episodes for the season being replaced.
type Ep struct {
	ID      int
	Season  int
	Number  int
	Title   string
	TvdbID  int
	AirDate string // YYYY-MM-DD, may be empty
}

// File is one video file of the pack.
type File struct {
	Path      string
	Season    int   // from SxxExx in the name; 0 when there is none
	Number    int   // from SxxExx in the name
	SonarrEps []int // episode ids Sonarr itself mapped the file to
	rest      []string
}

var sxe = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]?E(\d{1,3})`)

// maxTitleOffset is how many words may precede the title in a file name ("2e66a-Take My Siblings Please").
const maxTitleOffset = 2

// NewFile parses a pack file's name.
func NewFile(path string, sonarrEps []int) File {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	f := File{Path: path, SonarrEps: sonarrEps}
	if m := sxe.FindStringSubmatchIndex(base); m != nil {
		fmt.Sscanf(base[m[2]:m[3]], "%d", &f.Season)
		fmt.Sscanf(base[m[4]:m[5]], "%d", &f.Number)
		base = base[m[1]:]
	}
	f.rest = words(base)
	return f
}

// words lower-cases and splits into letter/digit runs; apostrophes vanish ("Let 'Em" == "Let.Em"), "&" is "and".
func words(s string) []string {
	s = strings.ReplaceAll(strings.ToLower(s), "&", " and ")
	s = strings.NewReplacer("'", "", "’", "", "`", "").Replace(s)
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

var placeholder = regexp.MustCompile(`^(episode \d+|tba|tbd|untitled|unknown)$`)

// usableTitle returns the title as words, or nil when it carries no evidence (empty, "TBA", "Episode 4").
func usableTitle(t string) []string {
	w := words(t)
	if len(w) == 0 || placeholder.MatchString(strings.Join(w, " ")) {
		return nil
	}
	return w
}

// titleAt reports whether the title appears in the file's words, starting no later than maxTitleOffset.
func titleAt(rest, title []string) bool {
	for off := 0; off <= maxTitleOffset && off+len(title) <= len(rest); off++ {
		ok := true
		for i, w := range title {
			if rest[off+i] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// TitleOf returns the single episode whose title is in the file name (the longest title wins: "Pilot Part 2" beats
// "Pilot"). ok is false when no title matches or the best match is shared by several episodes.
func TitleOf(f File, eps []Ep) (ep Ep, ok bool) {
	best, n := 0, 0
	for _, e := range eps {
		t := usableTitle(e.Title)
		if t == nil || !titleAt(f.rest, t) {
			continue
		}
		switch {
		case len(t) > best:
			best, n, ep = len(t), 1, e
		case len(t) == best:
			n++
		}
	}
	return ep, n == 1
}

// Conflicts lists the files whose title says one episode while Sonarr mapped them to others (never to that one).
func Conflicts(files []File, eps []Ep) []string {
	var out []string
	for _, f := range files {
		ep, ok := TitleOf(f, eps)
		if !ok || len(f.SonarrEps) == 0 {
			continue
		}
		same := false
		for _, id := range f.SonarrEps {
			same = same || id == ep.ID
		}
		if !same {
			out = append(out, fmt.Sprintf("%s is %q (S%02dE%02d) but Sonarr maps it to episode id %v", filepath.Base(f.Path), ep.Title, ep.Season, ep.Number, f.SonarrEps))
		}
	}
	return out
}

// AltEp is an episode as another ordering (a TVDB DVD/absolute order, a TMDB episode group) numbers it.
type AltEp struct {
	Season, Number int
	Title          string
	TvdbID         int
	AirDate        string
}

// Alt is one alternative ordering of the series.
type Alt struct {
	Name string
	Eps  []AltEp
}

// Plan is a complete, evidence-backed placement of pack files onto the season's episodes.
type Plan struct {
	Files map[string][]int // pack file path -> Sonarr episode ids
	Eps   []int            // episode numbers (in the season) that now have a file
	Basis string           // how it was decided
	Spare []string         // pack files left alone (no usable title match, not needed)
}

// Resolve places the pack's files on the season's episodes. need is the set of episode ids the user has files for
// today; every one of them must be placed exactly once. It tries Sonarr's own titles first, then each alternative
// ordering. The returned reason explains a failure.
func Resolve(files []File, eps []Ep, need map[int]bool, alts []Alt) (Plan, string) {
	if len(need) == 0 {
		return Plan{}, "none of your episodes in this season has a file, so there is nothing to place"
	}
	p, why := place(files, eps, need, func(f File) (Ep, bool) { return TitleOf(f, eps) })
	if why == "" {
		p.Basis = "episode titles in the file names match Sonarr's titles"
		return p, ""
	}
	reasons := []string{"by Sonarr's titles: " + why}
	for _, a := range alts {
		a := a
		p, w := place(files, eps, need, func(f File) (Ep, bool) { return viaAlt(f, a, eps) })
		if w == "" {
			p.Basis = "the pack follows the " + a.Name + " ordering and its titles confirm it"
			return p, ""
		}
		reasons = append(reasons, a.Name+": "+w)
	}
	return Plan{}, strings.Join(reasons, "; ")
}

func place(files []File, eps []Ep, need map[int]bool, pick func(File) (Ep, bool)) (Plan, string) {
	p := Plan{Files: map[string][]int{}}
	owner := map[int]string{}
	var dup []string
	numbers := map[int]bool{}
	for _, f := range files {
		ep, ok := pick(f)
		if !ok {
			p.Spare = append(p.Spare, f.Path)
			continue
		}
		if prev, taken := owner[ep.ID]; taken {
			dup = append(dup, fmt.Sprintf("%s and %s are both %q", filepath.Base(prev), filepath.Base(f.Path), ep.Title))
			continue
		}
		owner[ep.ID] = f.Path
		p.Files[f.Path] = []int{ep.ID}
		numbers[ep.Number] = true
	}
	if len(dup) > 0 {
		return Plan{}, "ambiguous: " + strings.Join(dup, "; ")
	}
	var missing []string
	for _, e := range eps {
		if need[e.ID] && owner[e.ID] == "" {
			missing = append(missing, fmt.Sprintf("S%02dE%02d %q", e.Season, e.Number, e.Title))
		}
	}
	if len(missing) > 0 {
		return Plan{}, fmt.Sprintf("no file in the pack is identified as %s", strings.Join(missing, ", "))
	}
	for n := range numbers {
		p.Eps = append(p.Eps, n)
	}
	sort.Ints(p.Eps)
	return p, ""
}

// viaAlt finds the episode an ordering says this file is (by the number in its name) and only accepts it when the
// file's title is that episode's title in the ordering, then maps it to Sonarr's episode by TVDB id, or by air date
// plus title when the ordering carries no TVDB ids.
func viaAlt(f File, a Alt, eps []Ep) (Ep, bool) {
	if f.Number == 0 {
		return Ep{}, false
	}
	for _, x := range a.Eps {
		if x.Season != f.Season || x.Number != f.Number {
			continue
		}
		t := usableTitle(x.Title)
		if t == nil || !titleAt(f.rest, t) {
			return Ep{}, false
		}
		var hit []Ep
		for _, e := range eps {
			switch {
			case x.TvdbID != 0 && e.TvdbID == x.TvdbID:
				hit = append(hit, e)
			case x.TvdbID == 0 && x.AirDate != "" && e.AirDate == x.AirDate && strings.Join(words(e.Title), " ") == strings.Join(t, " "):
				hit = append(hit, e)
			}
		}
		return firstIfOne(hit)
	}
	return Ep{}, false
}

func firstIfOne(e []Ep) (Ep, bool) {
	if len(e) != 1 {
		return Ep{}, false
	}
	return e[0], true
}

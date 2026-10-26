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
	HasSxE    bool  // the name carries an SxxExx (so Season 0 means specials, not "unknown")
	Multi     bool  // a multi-episode file ("S01E01E02"): it cannot stand in for a single episode
	rest      []string
}

var sxe = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]?E(\d{1,3})`)

// multiEp matches the extra episodes of "S01E01E02", "S01E01-E02" and "S01E01-02".
var multiEp = regexp.MustCompile(`(?i)^(?:[ ._-]?(?:E|-E|-)\d{1,3})+`)

// maxTitleOffset is how many words may precede the title in a file name ("2e66a-Take My Siblings Please").
const maxTitleOffset = 2

// NewFile parses a pack file's name.
func NewFile(path string, sonarrEps []int) File {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	f := File{Path: path, SonarrEps: sonarrEps}
	if m := sxe.FindStringSubmatchIndex(base); m != nil {
		f.HasSxE = true
		fmt.Sscanf(base[m[2]:m[3]], "%d", &f.Season)
		fmt.Sscanf(base[m[4]:m[5]], "%d", &f.Number)
		base = base[m[1]:]
		f.Multi = multiEp.MatchString(base)
	}
	f.Multi = f.Multi || len(sonarrEps) > 1
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

// boundary words end a title: the quality, source, codec, audio and streaming-service tags that follow it in a
// release name. A title only matches when the next word is one of these (or the name ends), so "Home" is not the
// start of "Home Is Where the Heart Is", and a title made only of such words ("Web") is no evidence at all.
var boundary = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`web webdl webrip hdtv pdtv dsr dvdrip dvd bluray bdrip brrip remux hdrip hddvd uhd hdr hdr10 dv sdr
		x264 x265 h264 h265 hevc avc av1 xvid divx vp9 aac ac3 eac3 ddp dd dts dtshd truehd flac opus atmos mp3 mp4 mkv avi
		proper repack nf amzn hmax hbomax dsnp atvp pcok pmtp hulu crav crunchyroll funi sdtv dl bdmv`) {
		m[w] = true
	}
	return m
}()

var resolution = regexp.MustCompile(`^\d{3,4}[pi]$|^[248]k$`)

func isBoundary(w string) bool {
	return boundary[w] || resolution.MatchString(w) || (len(w) == 4 && w[0] == '2' && (w[1] == '0' || w[1] == '1') && isDigits(w))
}

func hasDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// titleAt reports whether the title appears in the file's words, starting no later than maxTitleOffset.
func titleAt(rest, title []string) bool {
	allTags := true
	for _, w := range title {
		allTags = allTags && isBoundary(w)
	}
	if allTags {
		return false
	}
	for off := 0; off <= maxTitleOffset && off+len(title) <= len(rest); off++ {
		ok := true
		for i := 0; i < off && ok; i++ {
			// What precedes the title must be a production/episode code ("2e66a", "01"), not words: otherwise the
			// tail of a longer title ("Sweet Revenge") would pass for a shorter one ("Revenge").
			ok = hasDigit(rest[i]) && !isBoundary(rest[i])
		}
		for i, w := range title {
			if rest[off+i] != w {
				ok = false
				break
			}
		}
		if end := off + len(title); ok && end < len(rest) && !isBoundary(rest[end]) {
			ok = false
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
	if f.Multi {
		return Ep{}, false
	}
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
		if !need[ep.ID] { // only episodes you have a file for are replaced; anything else is left for Sonarr's own rules
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
			m := fmt.Sprintf("S%02dE%02d", e.Season, e.Number)
			if e.Title != "" {
				m += fmt.Sprintf(" %q", e.Title)
			}
			missing = append(missing, m)
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
	if f.Number == 0 || f.Multi {
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

// TitleAnywhere is TitleOf for names that do not follow SxxExx: the title may sit anywhere in the file name
// ("[Group] Show 2nd Season - 01 - Hello Baby! (REMUX ...)"). It is for telling which season a pack is, never for
// placing files, so it is stricter about what counts: a one-word title shorter than six letters carries no evidence.
func TitleAnywhere(path string, eps []Ep) (ep Ep, ok bool) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	rest := words(base)
	best, n := 0, 0
	for _, e := range eps {
		t := usableTitle(e.Title)
		if t == nil || (len(t) == 1 && len(t[0]) < 6) {
			continue
		}
		hit := false
		for off := 0; off+len(t) <= len(rest) && !hit; off++ {
			match := true
			for i, w := range t {
				if rest[off+i] != w {
					match = false
					break
				}
			}
			if end := off + len(t); match && end < len(rest) && !isBoundary(rest[end]) {
				match = false
			}
			hit = match
		}
		if !hit {
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

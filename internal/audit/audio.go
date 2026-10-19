package audit

import (
	"regexp"
	"strings"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
)

// Audio describes which languages a file's audio carries, as far as anyone has proved.
type Audio struct {
	Known   bool // false when nothing reads the file's audio (no MediaInfo, an untagged track): never assume either way
	English bool
	Other   bool // a track in some other language sits beside / instead of English
	// Undetermined: some track is untagged ("und"). With two or more tracks and no English, the file may well be dual
	// audio whose dub nobody labelled, so it is treated as unknown rather than as "no English".
	Undetermined bool
	Tracks       int
}

// ParseAudio reads Sonarr's MediaInfo.AudioLanguages ("eng/jpn", "chi", or full names).
func ParseAudio(langs string) Audio {
	var a Audio
	for _, l := range strings.FieldsFunc(strings.ToLower(langs), func(r rune) bool { return r == '/' || r == ',' || r == ';' || r == '+' }) {
		l = strings.TrimSpace(l)
		switch l {
		case "":
			continue
		case "eng", "en", "english":
			a.Known, a.English = true, true
		case "und", "unk", "zxx", "mis", "mul":
			a.Undetermined = true // it proves nothing either way
		default:
			a.Known, a.Other = true, true
		}
		a.Tracks++
	}
	if !a.English && a.Undetermined && a.Tracks >= 2 {
		a.Known = false // possibly an unlabelled dub: neither "has English" nor "sub only"
	}
	return a
}

// MaybeDub: no track says English, but two or more tracks include an untagged one.
func (a Audio) MaybeDub() bool { return !a.English && a.Undetermined && a.Tracks >= 2 }

// Dual: an English track that sits beside another language, i.e. a dual-audio file. A single English track on a
// foreign show is a dub-only file; whether that is worth protecting depends on the show's original language.
func (a Audio) Dual() bool { return a.English && a.Other }

// dualTitle matches release names that advertise more than one audio language. It is only a way to choose which
// packs are worth downloading: whether a pack really has an English track is settled by ffprobe before anything is
// deleted. Plain "Multi" is deliberately absent, it usually means subtitles.
var dualTitle = regexp.MustCompile(`(?i)(^|[^a-z0-9])(dual[ ._-]?(audio|lang(uage)?s?)?|multi[ ._-]?(audio|lang(uage)?s?)|(eng(lish)?|en)[ ._-]?dub(bed)?|dubbed)([^a-z0-9]|$)`)

// IsDual reports whether a release advertises an English dub: by its name, or by the languages Sonarr read from it.
func IsDual(x sonarr.Release) bool {
	if dualTitle.MatchString(x.Title) {
		return true
	}
	for _, l := range x.Languages {
		if strings.EqualFold(l.Name, "english") && len(x.Languages) > 1 {
			return true
		}
	}
	return false
}

// DubWanted: an anime season that has finished airing and is mostly without English audio (at least half its files).
// A season that is nearly all dubbed with one stray sub-only file (a special, a missing dub) is not worth replacing
// wholesale. Files whose audio nobody has read are not counted as missing a dub.
func DubWanted(r Season) bool {
	return r.SeriesType == "anime" && r.Files > 0 && r.NoEnglish > 0 && r.NoEnglish*2 >= r.Files && r.Complete
}

// Protected reports whether a file's English audio must survive a replacement: the show was not made in English (so
// an English track is a dub, alone or beside the original), or the original language is unknown and the file is dual
// audio. A file of an English-original show has nothing to lose: any pack of it has English. A possibly-unlabelled dub
// (two or more tracks, one untagged, none English) of a foreign show is protected too. Files Sonarr could not read at
// all are not: nothing says there is anything to keep.
func Protected(f Audio, originalLanguage string) bool {
	if !f.English && !f.MaybeDub() {
		return false
	}
	switch {
	case strings.EqualFold(originalLanguage, "english"):
		return false
	case originalLanguage == "":
		return f.Dual()
	}
	return true
}

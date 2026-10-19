package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// The English-audio gate. Replacing files is only ever allowed to keep or add English audio, never remove it:
//   - an episode whose current file is dual audio (or a dub of a foreign show) must come back from the pack with an
//     English track, so a consolidation can never swap a dub for a sub;
//   - every episode a "dub" job replaces must come back with one, or the job did not do what it exists for.
//
// The proof is ffprobe on the pack's own files. Release names only choose what to download, and Sonarr's language
// label reports a "Dual Audio" pack as one language, so neither is evidence. Unlike the decodability probe, an
// unreachable file does not pass under "Allow unverified": nothing may be deleted on a guess about audio.

// protectedEps are the episode numbers whose live old file has English audio that must survive.
func (e *Engine) protectedEps(ctx context.Context, c *sonarr.Client, j *store.Job, eps []sonarr.Episode, liveSet map[int]bool) (map[int]bool, error) {
	out := map[int]bool{}
	files, err := c.EpisodeFiles(ctx, j.SeriesID)
	if err != nil {
		return nil, err
	}
	byID := map[int]sonarr.EpisodeFile{}
	for _, f := range files {
		byID[f.ID] = f
	}
	any := false
	for _, ep := range eps {
		a := audit.ParseAudio(byID[ep.EpisodeFileID].AudioLanguages())
		if liveSet[ep.EpisodeFileID] && (a.English || a.MaybeDub()) {
			any = true
		}
	}
	if !any {
		return out, nil // nothing that could be a dub: skip the extra lookup
	}
	orig, err := e.originalLanguage(ctx, c, j.SeriesID)
	if err != nil {
		return nil, err
	}
	for _, ep := range eps {
		if liveSet[ep.EpisodeFileID] && audit.Protected(audit.ParseAudio(byID[ep.EpisodeFileID].AudioLanguages()), orig) {
			out[ep.EpisodeNumber] = true
		}
	}
	return out, nil
}

// audioGate checks that every required episode is carried by a pack file proven to hold an English track. files maps
// each pack file to the episode numbers it will be imported as. It returns nil when the gate passes.
func audioGate(inst store.Instance, v verdict, files map[string][]int, required map[int]bool, dub bool) *verdict {
	if len(required) == 0 {
		return nil
	}
	hold := func(kind, msg string) *verdict { v.State, v.Kind, v.Message = "hold", kind, msg; return &v }
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	have, lack := map[int]bool{}, map[int]bool{}
	var unreach string
	for _, p := range paths {
		need := false
		for _, n := range files[p] {
			need = need || required[n]
		}
		if !need {
			continue
		}
		langs, state := audioFn(inst, p)
		if state != "ok" {
			if unreach == "" {
				unreach = p
			}
			continue
		}
		eng := false
		for _, l := range langs {
			eng = eng || audit.ParseAudio(l).English
		}
		// Every importable file for an episode must have English: if the pack holds two for one episode, Sonarr may
		// import either, and the old dub is already gone by then.
		for _, n := range files[p] {
			if eng {
				have[n] = true
			} else {
				lack[n] = true
			}
		}
	}
	if unreach != "" {
		return hold(holdOther, fmt.Sprintf("cannot confirm the English audio: ffprobe cannot read %s. This check ignores 'Allow unverified' because the pack must prove it keeps the English dub; mount the download directory and set the instance path fields", unreach))
	}
	var lacking []int
	for n := range required {
		if !have[n] || lack[n] {
			lacking = append(lacking, n)
		}
	}
	if len(lacking) == 0 {
		return nil
	}
	sort.Ints(lacking)
	why := fmt.Sprintf("the pack has no English audio track on episodes %v", lacking)
	if dub {
		why += ", so it is not the dub it was grabbed for"
	} else {
		why += ", which have English audio today; replacing them would turn a dub into subtitles only"
	}
	return hold(holdAudio, why+". Nothing was changed")
}

// mappedEpisodes turns a remap plan (file -> episode ids) into file -> episode numbers.
func mappedEpisodes(plan map[string][]int, eps []sonarr.Episode) map[string][]int {
	num := map[int]int{}
	for _, ep := range eps {
		num[ep.ID] = ep.EpisodeNumber
	}
	out := map[string][]int{}
	for p, ids := range plan {
		for _, id := range ids {
			out[p] = append(out[p], num[id])
		}
	}
	return out
}

// originalLanguage is the language a show was made in, cached for an hour: it is needed on every verify of a season
// with English audio, and a Sonarr that answers it slowly or not at all must not stall ordinary English TV.
func (e *Engine) originalLanguage(ctx context.Context, c *sonarr.Client, seriesID int) (string, error) {
	key := fmt.Sprintf("%s#%d", c.Base, seriesID)
	e.mu.Lock()
	if hit, ok := e.origLang[key]; ok && time.Since(hit.at) < time.Hour {
		e.mu.Unlock()
		return hit.name, nil
	}
	e.mu.Unlock()
	ids, err := c.SeriesIDs(ctx, seriesID)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	if e.origLang == nil {
		e.origLang = map[string]origLang{}
	}
	e.origLang[key] = origLang{ids.OriginalLanguage.Name, time.Now()}
	e.mu.Unlock()
	return ids.OriginalLanguage.Name, nil
}

type origLang struct {
	name string
	at   time.Time
}

package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jordanfelle/consolidarr/internal/order"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// Why a verification held. A hold whose reason cannot fix itself (the pack is the wrong one, not merely not ready)
// is rejected after Settings.HoldRejectHours so it stops occupying one of the in-flight slots; the others wait.
const (
	holdMissing = "missing" // the pack lacks episodes you have
	holdEmpty   = "empty"   // nothing in the download will import
	holdProbe   = "probe"   // a file is undecodable or DRM-protected
	holdOrder   = "order"   // the pack numbers episodes differently from Sonarr and titles could not settle it
	holdOther   = ""        // anything that may resolve itself or needs a human: never auto-rejected
)

func rejectable(kind string) bool {
	switch kind {
	case holdMissing, holdEmpty, holdProbe, holdOrder:
		return true
	}
	return false
}

func orderEps(eps []sonarr.Episode) []order.Ep {
	out := make([]order.Ep, 0, len(eps))
	for _, e := range eps {
		out = append(out, order.Ep{ID: e.ID, Season: e.SeasonNumber, Number: e.EpisodeNumber, Title: e.Title, TvdbID: e.TvdbID, AirDate: e.AirDate})
	}
	return out
}

// planRemap tries to place every pack file by title: first against Sonarr's own titles, then, if TVDB/TMDB keys are
// configured, against each alternative ordering those sources publish.
func (e *Engine) planRemap(ctx context.Context, c *sonarr.Client, j *store.Job, files []order.File, eps []order.Ep, need map[int]bool) (order.Plan, string) {
	if p, why := order.Resolve(files, eps, need, nil); why == "" {
		return p, ""
	}
	if !e.Meta.Configured() {
		_, why := order.Resolve(files, eps, need, nil)
		return order.Plan{}, why
	}
	ids, err := c.SeriesIDs(ctx, j.SeriesID)
	if err != nil {
		return order.Plan{}, "could not read the series ids from Sonarr: " + err.Error()
	}
	alts, aerr := e.Meta.Alts(ctx, ids.TvdbID, ids.TmdbID)
	p, why := order.Resolve(files, eps, need, alts)
	if why != "" && aerr != nil {
		why += " (lookup problem: " + aerr.Error() + ")"
	}
	return p, why
}

// remapVerdict turns a plan into a verdict. Every planned file is probed, since the ordinary loop only looked at the
// files Sonarr itself placed in this season.
func (e *Engine) remapVerdict(inst store.Instance, set store.Settings, v verdict, p order.Plan, byPath map[string]sonarr.ManualImportItem) verdict {
	paths := make([]string, 0, len(p.Files))
	for path := range p.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var bad []string
	v.Probed, v.Unreach = 0, ""
	for _, path := range paths {
		if _, ok := byPath[path]; !ok {
			return verdict{State: "retry"}
		}
		ok, why := probeFn(inst, path)
		switch {
		case why == "unverified":
			if v.Unreach == "" {
				v.Unreach = path
			}
		case !ok:
			bad = append(bad, why)
		default:
			v.Probed++
		}
	}
	hold := func(kind, msg string) verdict { v.State, v.Kind, v.Message = "hold", kind, msg; return v }
	switch {
	case len(bad) > 0:
		return hold(holdProbe, "failed probe: "+strings.Join(bad[:min(len(bad), 2)], "; "))
	case v.Unreach != "" && !set.AllowUnverified:
		return hold(holdOther, fmt.Sprintf("cannot verify: ffprobe cannot read %s. Mount the download directory and set the instance path fields, or enable 'Allow unverified' in Settings", v.Unreach))
	}
	v.Mapping, v.ExpectedEps, v.Expected = p.Files, p.Eps, len(p.Eps)
	v.Message = "an explicit episode mapping (" + p.Basis + ")"
	v.State = "ok"
	return v
}

// startImport asks Sonarr to import the download. A job with a mapping names the episodes itself; anything else
// lets Sonarr read the file names.
func startImport(ctx context.Context, c *sonarr.Client, j *store.Job, path, downloadID string) (int, error) {
	if len(j.Mapping) == 0 {
		return c.ImportDownload(ctx, path, downloadID)
	}
	var items []sonarr.ManualImportItem
	var err error
	if downloadID != "" { // the same listing Sonarr's own import would use, so the paths are the ones it knows
		items, err = c.ManualImport(ctx, downloadID)
	} else {
		items, err = c.ManualImportFolder(ctx, path)
	}
	if err != nil {
		return 0, err
	}
	return c.ImportMapped(ctx, j.SeriesID, downloadID, j.Mapping, items)
}

// findMappedImportCommand is findImportCommand for a mapped job: a ManualImport command that names one of the
// mapped files and is still queued/running, or was queued after this attempt began.
func findMappedImportCommand(cmds []sonarr.Command, mapping map[string][]int, importAt int64) int {
	for i := len(cmds) - 1; i >= 0; i-- {
		c := cmds[i]
		if c.Name != "ManualImport" || c.ID == 0 {
			continue
		}
		hit := false
		for _, f := range c.Body.Files {
			_, in := mapping[f.Path]
			hit = hit || in
		}
		if !hit {
			continue
		}
		switch strings.ToLower(c.Status) {
		case "queued", "started":
			return c.ID
		}
		if t, err := time.Parse(time.RFC3339Nano, c.Queued); err == nil && importAt > 0 && t.Unix() >= importAt-5 {
			return c.ID
		}
	}
	return 0
}

// mappedImportGiveUp is how long a mapped import may fail to even start (its files moved, Sonarr refuses it) before
// the job ends as failed. Nothing was deleted, so ending it is safe; waiting forever would hold up every other season.
const mappedImportGiveUp = 30 * time.Minute

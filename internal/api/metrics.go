package api

import (
	"net/http"
	"time"

	"github.com/jordanfelle/consolidarr/internal/store"
)

var ranges = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour}

// metrics feeds the dashboard: consolidation progress over time, replacement throughput, the shape of what is
// left, and the worst offenders. `range` is 24h, 7d (default), 30d, 90d or all.
func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	var since int64
	if rng != "all" {
		d, ok := ranges[rng]
		if !ok {
			fail(w, 422, "range must be one of: 24h, 7d, 30d, 90d, all")
			return
		}
		since = time.Now().Add(-d).Unix()
	}
	set := a.St.Settings()
	snaps, err := a.St.Snapshots(since, 400)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	days, err := a.St.PerDay(since)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	buckets, top, err := a.St.Mess(set.MinFiles, set.IncludeAnime, 10)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	cur, err := a.St.Measure(set.MinFiles)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	totals := struct {
		Done     int     `json:"done"`
		Failed   int     `json:"failed"`
		GB       float64 `json:"gb"`
		Inflight int     `json:"inflight"`
	}{}
	if js, err := a.St.Jobs(false, 100000); err == nil {
		for _, j := range js {
			switch j.Status {
			case "done":
				totals.Done++
				totals.GB += j.SizeGB
			case "failed":
				totals.Failed++
			case "downloading", "ready", "holding", "importing":
				totals.Inflight++
			}
		}
	}
	if snaps == nil {
		snaps = []store.Snapshot{}
	}
	ok(w, map[string]any{"range": rng, "since": since, "current": cur, "snapshots": snaps, "perDay": days,
		"groupBuckets": buckets, "top": nonNil(top), "totals": totals})
}

func nonNil(o []store.Offender) []store.Offender {
	if o == nil {
		return []store.Offender{}
	}
	return o
}

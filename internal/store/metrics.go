package store

import (
	"sort"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
)

// Snapshot is the library's consolidation state at one moment, recorded after every scan and every finished
// replacement so the dashboard can show progress over time.
type Snapshot struct {
	TS           int64   `json:"ts"`
	FlaggedTV    int     `json:"flaggedTV"`
	FlaggedAnime int     `json:"flaggedAnime"`
	Seasons      int     `json:"seasons"`
	Files        int     `json:"files"`
	PackFiles    int     `json:"packFiles"`
	SingleFiles  int     `json:"singleFiles"`
	MultiFiles   int     `json:"multiFiles"`
	UnknownFiles int     `json:"unknownFiles"`
	GB           float64 `json:"gb"`
}

// Measure computes a snapshot from the stored seasons.
func (s *Store) Measure(minFiles int) (Snapshot, error) {
	rows, err := s.Seasons(SeasonQuery{MinFiles: max(minFiles, 1)})
	if err != nil {
		return Snapshot{}, err
	}
	sn := Snapshot{TS: time.Now().Unix(), Seasons: len(rows)}
	for _, r := range rows {
		sn.Files += r.Files
		sn.PackFiles += r.Pack
		sn.SingleFiles += r.Single
		sn.MultiFiles += r.Multi
		sn.UnknownFiles += r.Unknown
		sn.GB += r.GB
		if r.Flagged {
			if r.SeriesType == "anime" {
				sn.FlaggedAnime++
			} else {
				sn.FlaggedTV++
			}
		}
	}
	return sn, nil
}

// RecordSnapshot stores a snapshot unless nothing changed since the last one (so idle periods do not pile up
// identical rows), and trims history older than a year.
func (s *Store) RecordSnapshot(minFiles int) error {
	sn, err := s.Measure(minFiles)
	if err != nil {
		return err
	}
	var last Snapshot
	err = s.QueryRow(`select ts,flagged_tv,flagged_anime,seasons,files,pack_files,single_files,multi_files,unknown_files,gb from snapshots order by ts desc limit 1`).
		Scan(&last.TS, &last.FlaggedTV, &last.FlaggedAnime, &last.Seasons, &last.Files, &last.PackFiles, &last.SingleFiles, &last.MultiFiles, &last.UnknownFiles, &last.GB)
	if err == nil {
		same := last.FlaggedTV == sn.FlaggedTV && last.FlaggedAnime == sn.FlaggedAnime && last.Seasons == sn.Seasons &&
			last.Files == sn.Files && last.PackFiles == sn.PackFiles && last.SingleFiles == sn.SingleFiles
		if same && sn.TS-last.TS < 6*3600 { // an unchanged library is still sampled every few hours so the line stays continuous
			return nil
		}
	}
	if _, err := s.Exec(`insert into snapshots(ts,flagged_tv,flagged_anime,seasons,files,pack_files,single_files,multi_files,unknown_files,gb) values(?,?,?,?,?,?,?,?,?,?)`,
		sn.TS, sn.FlaggedTV, sn.FlaggedAnime, sn.Seasons, sn.Files, sn.PackFiles, sn.SingleFiles, sn.MultiFiles, sn.UnknownFiles, sn.GB); err != nil {
		return err
	}
	_, _ = s.Exec(`delete from snapshots where ts < ?`, time.Now().Add(-365*24*time.Hour).Unix())
	return nil
}

// Snapshots returns history since `since` (unix seconds, 0 = all), thinned to at most max points by keeping the
// last snapshot in each time bucket, so a long range stays cheap to draw and the newest point is always present.
func (s *Store) Snapshots(since int64, max int) ([]Snapshot, error) {
	rows, err := s.Query(`select ts,flagged_tv,flagged_anime,seasons,files,pack_files,single_files,multi_files,unknown_files,gb from snapshots where ts >= ? order by ts`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Snapshot
	for rows.Next() {
		var x Snapshot
		if err := rows.Scan(&x.TS, &x.FlaggedTV, &x.FlaggedAnime, &x.Seasons, &x.Files, &x.PackFiles, &x.SingleFiles, &x.MultiFiles, &x.UnknownFiles, &x.GB); err != nil {
			return nil, err
		}
		all = append(all, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if max <= 0 || len(all) <= max {
		return append([]Snapshot{}, all...), nil
	}
	span := all[len(all)-1].TS - all[0].TS + 1
	bucket := span/int64(max) + 1
	out := make([]Snapshot, 0, max)
	for i, x := range all {
		if i == len(all)-1 || (all[i+1].TS-all[0].TS)/bucket != (x.TS-all[0].TS)/bucket {
			out = append(out, x)
		}
	}
	return out, nil
}

// DayStat is one calendar day of replacement outcomes.
type DayStat struct {
	Day    string  `json:"day"` // YYYY-MM-DD in the server's time zone
	Done   int     `json:"done"`
	Failed int     `json:"failed"`
	GB     float64 `json:"gb"` // size of the packs that finished that day
}

// PerDay aggregates finished jobs (done or failed) by the day they finished, oldest first, with empty days
// filled in so a gap in activity is visible as a gap, not hidden.
func (s *Store) PerDay(since int64) ([]DayStat, error) {
	js, err := s.Jobs(false, 100000)
	if err != nil {
		return nil, err
	}
	by := map[string]*DayStat{}
	var first, last time.Time
	for _, j := range js {
		if (j.Status != "done" && j.Status != "failed") || j.Updated < since {
			continue
		}
		t := time.Unix(j.Updated, 0)
		d := t.Format("2006-01-02")
		st := by[d]
		if st == nil {
			st = &DayStat{Day: d}
			by[d] = st
		}
		if j.Status == "done" {
			st.Done++
			st.GB += j.SizeGB
		} else {
			st.Failed++
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
		if t.After(last) {
			last = t
		}
	}
	if len(by) == 0 {
		return []DayStat{}, nil
	}
	end := time.Now()
	if last.After(end) {
		end = last
	}
	var out []DayStat
	for d := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, first.Location()); !d.After(end); d = d.AddDate(0, 0, 1) {
		k := d.Format("2006-01-02")
		if st, ok := by[k]; ok {
			out = append(out, *st)
		} else {
			out = append(out, DayStat{Day: k})
		}
	}
	return out, nil
}

// Bucket is one bar of a histogram.
type Bucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Offender is one of the messiest flagged seasons.
type Offender struct {
	InstID   int     `json:"instId"`
	SeriesID int     `json:"seriesId"`
	Title    string  `json:"title"`
	Season   int     `json:"season"`
	Files    int     `json:"files"`
	Groups   int     `json:"groups"`
	GB       float64 `json:"gb"`
}

// Mess summarises the flagged seasons: how many release groups they are spread across, and the worst of them.
func (s *Store) Mess(minFiles int, includeAnime bool, top int) ([]Bucket, []Offender, error) {
	q := SeasonQuery{FlaggedOnly: true, MinFiles: max(minFiles, 1), Anime: "hide"}
	if includeAnime {
		q.Anime = ""
	}
	rows, err := s.Seasons(q)
	if err != nil {
		return nil, nil, err
	}
	edges := []struct {
		label  string
		lo, hi int
	}{{"1–2 groups", 1, 2}, {"3–4", 3, 4}, {"5–6", 5, 6}, {"7+", 7, 1 << 30}}
	buckets := make([]Bucket, len(edges))
	for i, e := range edges {
		buckets[i].Label = e.label
	}
	for _, r := range rows {
		g := len(r.Groups)
		for i, e := range edges {
			if g >= e.lo && g <= e.hi {
				buckets[i].Count++
				break
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Score > rows[j].Score })
	var offenders []Offender
	for _, r := range rows[:min(top, len(rows))] {
		offenders = append(offenders, offenderOf(r))
	}
	return buckets, offenders, nil
}

func offenderOf(r audit.Season) Offender {
	return Offender{InstID: r.InstID, SeriesID: r.SeriesID, Title: r.Title, Season: r.Season, Files: r.Files, Groups: len(r.Groups), GB: r.GB}
}

package store

import (
	"encoding/json"
	"strings"

	"github.com/jordanfelle/consolidarr/internal/audit"
)

// ReplaceSeasons swaps stored season rows for one instance (seriesID 0) or one series.
func (s *Store) ReplaceSeasons(instID int64, seriesID int, rows []audit.Season) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if seriesID == 0 {
		_, err = tx.Exec(s.rebind(`delete from seasons where inst_id=?`), instID)
	} else {
		_, err = tx.Exec(s.rebind(`delete from seasons where inst_id=? and series_id=?`), instID, seriesID)
	}
	if err != nil {
		return err
	}
	for _, r := range rows {
		g, _ := json.Marshal(r.Groups)
		q, _ := json.Marshal(r.Qualities)
		sr, _ := json.Marshal(r.Sources)
		if _, err := tx.Exec(s.rebind(`insert into seasons(inst_id,series_id,season,title,series_type,monitored,files,aired,pack,single,multi,unknown,groups,qualities,sources,gb,cur_rank,path)
			values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
			instID, r.SeriesID, r.Season, r.Title, r.SeriesType, b2i(r.Monitored), r.Files, r.Aired, r.Pack, r.Single, r.Multi, r.Unknown,
			string(g), string(q), string(sr), r.GB, r.CurRank, r.Path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type SeasonQuery struct {
	InstID      int64
	FlaggedOnly bool
	Anime       string // "", "only", "hide"
	Search      string
	MinFiles    int
}

func (s *Store) Seasons(q SeasonQuery) ([]audit.Season, error) {
	rows, err := s.Query(`select inst_id,series_id,season,title,series_type,monitored,files,aired,pack,single,multi,unknown,groups,qualities,sources,gb,cur_rank,path from seasons`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []audit.Season{}
	for rows.Next() {
		var r audit.Season
		var mon int
		var g, ql, sr string
		if err := rows.Scan(&r.InstID, &r.SeriesID, &r.Season, &r.Title, &r.SeriesType, &mon, &r.Files, &r.Aired, &r.Pack, &r.Single, &r.Multi,
			&r.Unknown, &g, &ql, &sr, &r.GB, &r.CurRank, &r.Path); err != nil {
			return nil, err
		}
		r.Monitored = mon == 1
		_ = json.Unmarshal([]byte(g), &r.Groups)
		_ = json.Unmarshal([]byte(ql), &r.Qualities)
		_ = json.Unmarshal([]byte(sr), &r.Sources)
		r.Score = float64(len(r.Groups)*3+len(r.Qualities)*2+len(r.Sources)*2) + float64(r.Files)*0.2
		r.Flagged = audit.IsFlagged(r, max(q.MinFiles, 1))
		switch {
		case q.InstID != 0 && int64(r.InstID) != q.InstID:
			continue
		case q.FlaggedOnly && !r.Flagged:
			continue
		case q.Anime == "only" && r.SeriesType != "anime", q.Anime == "hide" && r.SeriesType == "anime":
			continue
		case q.Search != "" && !strings.Contains(strings.ToLower(r.Title), strings.ToLower(q.Search)):
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

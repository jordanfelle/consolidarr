package meta

import (
	"context"
	"fmt"
	"net/url"

	"github.com/jordanfelle/consolidarr/internal/order"
)

// TMDB is The Movie Database v3 API; its alternative orderings are episode groups (DVD, absolute, production...).
type TMDB struct {
	Key  string
	Base string // https://api.themoviedb.org/3 unless a test overrides it
}

func (t *TMDB) base() string {
	if t.Base != "" {
		return t.Base
	}
	return "https://api.themoviedb.org/3"
}

// maxGroups bounds the follow-up requests for a series with a great many groups.
const maxGroups = 12

func (t *TMDB) Alts(ctx context.Context, tmdbID int) ([]order.Alt, error) {
	var list struct {
		Results []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type int    `json:"type"`
		} `json:"results"`
	}
	if err := getJSON(ctx, fmt.Sprintf("%s/tv/%d/episode_groups?api_key=%s", t.base(), tmdbID, url.QueryEscape(t.Key)), nil, &list); err != nil {
		return nil, err
	}
	var out []order.Alt
	for i, g := range list.Results {
		if i >= maxGroups {
			break
		}
		var d struct {
			Groups []struct {
				Order    int `json:"order"`
				Episodes []struct {
					Name    string `json:"name"`
					AirDate string `json:"air_date"`
					Order   int    `json:"order"`
				} `json:"episodes"`
			} `json:"groups"`
		}
		if err := getJSON(ctx, fmt.Sprintf("%s/tv/episode_group/%s?api_key=%s", t.base(), url.PathEscape(g.ID), url.QueryEscape(t.Key)), nil, &d); err != nil {
			return nil, fmt.Errorf("group %q: %v", g.Name, err)
		}
		alt := order.Alt{Name: "TMDB " + g.Name}
		for _, grp := range d.Groups { // a group is one "season" of the ordering; its episodes are numbered from order 0
			for _, e := range grp.Episodes {
				alt.Eps = append(alt.Eps, order.AltEp{Season: grp.Order, Number: e.Order + 1, Title: e.Name, AirDate: e.AirDate})
			}
		}
		if len(alt.Eps) > 0 {
			out = append(out, alt)
		}
	}
	return out, nil
}

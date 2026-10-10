package store

import (
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
)

func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRebindPostgres(t *testing.T) {
	s := &Store{PG: true}
	if got := s.rebind("select ? , ?, ?"); got != "select $1 , $2, $3" {
		t.Fatalf("got %q", got)
	}
	if got := (&Store{}).rebind("select ?"); got != "select ?" {
		t.Fatalf("sqlite must be untouched, got %q", got)
	}
}

func TestSettingsDefaultsAndRoundTrip(t *testing.T) {
	st := open(t)
	if s := st.Settings(); !s.DryRun || s.MaxInflight != 4 {
		t.Fatalf("unsafe defaults: %+v", s)
	}
	s := st.Settings()
	s.DryRun, s.MaxInflight = false, 9
	if err := st.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	if got := st.Settings(); got.DryRun || got.MaxInflight != 9 {
		t.Fatalf("not persisted: %+v", got)
	}
}

func TestInstancesKeepKeyOnUpdateAndMultiple(t *testing.T) {
	st := open(t)
	a, err := st.SaveInstance(Instance{Name: "A", URL: "http://a", APIKey: "ka", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.SaveInstance(Instance{Name: "B", URL: "http://b", APIKey: "kb", Enabled: true})
	if a == b {
		t.Fatal("ids must differ")
	}
	if _, err := st.SaveInstance(Instance{ID: a, Name: "A2", URL: "http://a2", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Instance(a)
	if got.Name != "A2" || got.APIKey != "ka" {
		t.Fatalf("api key must survive an update without one: %+v", got)
	}
}

func TestSeasonsPerInstanceAndJobs(t *testing.T) {
	st := open(t)
	rows := func(inst int, title string) []audit.Season {
		return []audit.Season{{InstID: inst, SeriesID: 1, Season: 1, Title: title, SeriesType: "standard", Files: 8, Aired: 8,
			Groups: []string{"a", "b"}, Qualities: []string{"WEBDL-1080p"}, Sources: []string{"web"}}}
	}
	if err := st.ReplaceSeasons(1, 0, rows(1, "One")); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSeasons(2, 0, rows(2, "Two")); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSeasons(1, 0, rows(1, "One again")); err != nil { // must not touch instance 2
		t.Fatal(err)
	}
	all, _ := st.Seasons(SeasonQuery{FlaggedOnly: true, MinFiles: 3})
	if len(all) != 2 {
		t.Fatalf("want 2 seasons across instances, got %d", len(all))
	}
	id, err := st.AddJob(Job{InstID: 1, SeriesID: 1, Season: 1, Title: "One", OldFileIDs: []int{4, 5}})
	if err != nil {
		t.Fatal(err)
	}
	js, _ := st.Jobs(true, 10)
	if len(js) != 1 || js[0].ID != id || len(js[0].OldFileIDs) != 2 || js[0].Status != "downloading" {
		t.Fatalf("job roundtrip: %+v", js)
	}
	st.Skip("k", time.Hour, "x")
	if !st.Skipped("k") || st.Skipped("other") {
		t.Fatal("skip window")
	}
}

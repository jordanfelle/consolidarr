package store

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
)

// open uses SQLite, or Postgres when CONSOLIDARR_TEST_DATABASE_URL is set (CI runs both).
func open(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("CONSOLIDARR_TEST_DATABASE_URL")
	if url != "" {
		db, err := sql.Open("pgx", url)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`drop schema public cascade; create schema public`); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	st, err := Open(t.TempDir(), url)
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

func TestSnapshotsAreRecordedThinnedAndDeduplicated(t *testing.T) {
	st := open(t)
	rows := func(flagged int) []audit.Season {
		var out []audit.Season
		for i := 0; i < flagged; i++ {
			out = append(out, audit.Season{InstID: 1, SeriesID: i + 1, Season: 1, Title: "S", SeriesType: "standard", Files: 8, Aired: 8,
				Single: 8, Groups: []string{"a", "b"}, Qualities: []string{"q"}, Sources: []string{"web"}, GB: 1})
		}
		return out
	}
	_ = st.ReplaceSeasons(1, 0, rows(3))
	if err := st.RecordSnapshot(3); err != nil {
		t.Fatal(err)
	}
	_ = st.RecordSnapshot(3) // unchanged and recent: not stored again
	got, _ := st.Snapshots(0, 400)
	if len(got) != 1 || got[0].FlaggedTV != 3 || got[0].SingleFiles != 24 || got[0].PackFiles != 0 {
		t.Fatalf("one snapshot of the current state expected, got %+v", got)
	}
	_ = st.ReplaceSeasons(1, 0, rows(2)) // a replacement finished: progress
	_ = st.RecordSnapshot(3)
	if got, _ = st.Snapshots(0, 400); len(got) != 2 || got[1].FlaggedTV != 2 {
		t.Fatalf("a changed library must add a point: %+v", got)
	}
	// thinning keeps the newest point and never exceeds the cap
	for i := 0; i < 50; i++ {
		_, _ = st.Exec(`insert into snapshots(ts,flagged_tv,flagged_anime,seasons,files,pack_files,single_files,multi_files,unknown_files,gb) values(?,?,?,?,?,?,?,?,?,?)`,
			time.Now().Unix()-int64(1000+i*60), i, 0, 0, 0, 0, 0, 0, 0, 0)
	}
	thin, _ := st.Snapshots(0, 10)
	if len(thin) > 10 || thin[len(thin)-1].FlaggedTV != 2 {
		t.Fatalf("thinning must respect the cap and keep the latest point, got %d points ending %+v", len(thin), thin[len(thin)-1])
	}
}

func TestPerDayFillsGapsAndCountsOutcomes(t *testing.T) {
	st := open(t)
	mk := func(status string, daysAgo int, gb float64) {
		id, _ := st.AddJob(Job{InstID: 1, SeriesID: daysAgo*10 + len(status), Season: 1, Title: "x", SizeGB: gb})
		_, _ = st.Exec(`update jobs set status=?, updated=? where id=?`, status, time.Now().AddDate(0, 0, -daysAgo).Unix(), id)
	}
	mk("done", 3, 10)
	mk("done", 3, 5)
	mk("failed", 3, 0)
	mk("done", 0, 2)
	mk("downloading", 1, 99) // unfinished jobs are not throughput
	days, err := st.PerDay(0)
	if err != nil || len(days) != 4 {
		t.Fatalf("want 4 consecutive days (3 days ago .. today), got %d err=%v: %+v", len(days), err, days)
	}
	if days[0].Done != 2 || days[0].Failed != 1 || days[0].GB != 15 || days[1].Done != 0 || days[2].Done != 0 || days[3].Done != 1 {
		t.Fatalf("unexpected per-day stats: %+v", days)
	}
}

func TestMessBucketsFlaggedSeasonsByReleaseGroups(t *testing.T) {
	st := open(t)
	mk := func(id, groups int, anime bool) audit.Season {
		g := make([]string, groups)
		for i := range g {
			g[i] = string(rune('a' + i))
		}
		ty := "standard"
		if anime {
			ty = "anime"
		}
		return audit.Season{InstID: 1, SeriesID: id, Season: 1, Title: "T", SeriesType: ty, Files: 8, Aired: 8, Single: 8, Groups: g, Qualities: []string{"q"}, Sources: []string{"web"}}
	}
	_ = st.ReplaceSeasons(1, 0, []audit.Season{mk(1, 1, false), mk(2, 3, false), mk(3, 4, false), mk(4, 9, false), mk(5, 9, true)})
	b, top, err := st.Mess(3, false, 2)
	if err != nil {
		t.Fatal(err)
	}
	if b[0].Count != 1 || b[1].Count != 2 || b[2].Count != 0 || b[3].Count != 1 {
		t.Fatalf("buckets wrong (anime hidden): %+v", b)
	}
	if len(top) != 2 || top[0].Groups != 9 {
		t.Fatalf("the messiest season must lead: %+v", top)
	}
	if b2, _, _ := st.Mess(3, true, 2); b2[3].Count != 2 {
		t.Fatalf("anime counted when included: %+v", b2)
	}
}

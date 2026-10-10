package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// fakeSonarr serves just enough of Sonarr's API for the job lifecycle and records destructive calls.
type fakeSonarr struct {
	mu         sync.Mutex
	deleted    [][]int
	imports    int
	packEps    []int // episodes the finished download contains
	seasonEps  map[int]int
	filesAfter []sonarr.EpisodeFile
	cmdStatus  string
	srv        *httptest.Server
}

func newFake(packEps []int) *fakeSonarr {
	f := &fakeSonarr{packEps: packEps, seasonEps: map[int]int{1: 11, 2: 12, 3: 13}, cmdStatus: "completed"}
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /api/v3/manualimport", func(w http.ResponseWriter, r *http.Request) {
		var items []map[string]any
		for _, e := range f.packEps {
			items = append(items, map[string]any{"path": "/dl/pack/e.mkv", "seasonNumber": 3, "episodes": []map[string]int{{"episodeNumber": e}}})
		}
		js(w, items)
	})
	mux.HandleFunc("GET /api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		var eps []map[string]int
		for n, id := range f.seasonEps {
			eps = append(eps, map[string]int{"episodeNumber": n, "episodeFileId": id})
		}
		js(w, eps)
	})
	mux.HandleFunc("DELETE /api/v3/episodefile/bulk", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			EpisodeFileIDs []int `json:"episodeFileIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.deleted = append(f.deleted, b.EpisodeFileIDs)
		f.mu.Unlock()
	})
	mux.HandleFunc("POST /api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.imports++
		f.mu.Unlock()
		js(w, map[string]int{"id": 42})
	})
	mux.HandleFunc("GET /api/v3/command/42", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"id": 42, "status": f.cmdStatus})
	})
	mux.HandleFunc("GET /api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) { js(w, f.filesAfter) })
	mux.HandleFunc("GET /api/v3/series", func(w http.ResponseWriter, r *http.Request) { js(w, []any{}) })
	f.srv = httptest.NewServer(mux)
	return f
}

type env struct {
	e    *Engine
	st   *store.Store
	c    *sonarr.Client
	inst store.Instance
	fake *fakeSonarr
}

func setup(t *testing.T, packEps []int) *env {
	t.Helper()
	st, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(packEps)
	t.Cleanup(f.srv.Close)
	probeFn = func(store.Instance, string) (bool, string) { return true, "ok" }
	t.Cleanup(func() { probeFn = probe })
	return &env{e: New(st), st: st, c: sonarr.New(f.srv.URL, "k"), inst: store.Instance{ID: 1, Name: "t"}, fake: f}
}

func (v *env) job(t *testing.T, created time.Time) store.Job {
	t.Helper()
	id, err := v.st.AddJob(store.Job{InstID: 1, SeriesID: 1, Season: 3, Title: "Show", OldFileIDs: []int{11, 12, 13}, Release: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !created.IsZero() {
		_, _ = v.st.Exec(`update jobs set created=? where id=?`, created.Unix(), id)
	}
	js, _ := v.st.Jobs(true, 10)
	for _, j := range js {
		if j.ID == id {
			return j
		}
	}
	t.Fatal("job missing")
	return store.Job{}
}

func done(size, left float64, status string) []sonarr.QueueItem {
	return []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Size: size, SizeLeft: left, Status: status, DownloadID: "d", OutputPath: "/dl/pack"}}
}

func live() store.Settings { s := store.DefaultSettings(); s.DryRun = false; return s }

func (v *env) status(t *testing.T, id int64) store.Job {
	t.Helper()
	js, _ := v.st.Jobs(false, 100)
	for _, j := range js {
		if j.ID == id {
			return j
		}
	}
	t.Fatal("job not found")
	return store.Job{}
}

func TestVerifiedPackDeletesOnlyOldFilesAndImports(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	got := v.status(t, j.ID)
	if got.Status != "importing" || got.ImportCmd != 42 {
		t.Fatalf("want importing/42, got %s %q", got.Status, got.Message)
	}
	if len(v.fake.deleted) != 1 || len(v.fake.deleted[0]) != 3 || v.fake.imports != 1 {
		t.Fatalf("deleted=%v imports=%d", v.fake.deleted, v.fake.imports)
	}
}

func TestPackMissingAnEpisodeNeverDeletes(t *testing.T) {
	v := setup(t, []int{1, 2}) // episode 3 is on disk but not in the pack
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "holding" {
		t.Fatalf("want holding, got %s %q", got.Status, got.Message)
	}
	if len(v.fake.deleted) != 0 || v.fake.imports != 0 {
		t.Fatal("must not delete or import when the pack lacks an episode")
	}
}

func TestFailedProbeNeverDeletes(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	probeFn = func(store.Instance, string) (bool, string) { return false, "e.mkv: unreadable or DRM-encrypted" }
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "holding" || got.Message == "" {
		t.Fatalf("want holding with reason, got %s %q", got.Status, got.Message)
	}
	if len(v.fake.deleted) != 0 {
		t.Fatal("must not delete when a probe fails")
	}
}

func TestDryRunAndManualApprovalNeverDelete(t *testing.T) {
	for name, set := range map[string]store.Settings{
		"dry run":        store.DefaultSettings(),
		"autoDelete off": func() store.Settings { s := live(); s.AutoDelete = false; return s }(),
	} {
		v := setup(t, []int{1, 2, 3})
		j := v.job(t, time.Time{})
		v.e.advance(context.Background(), v.inst, v.c, set, j, done(100, 0, "completed"))
		if got := v.status(t, j.ID); got.Status != "ready" {
			t.Fatalf("%s: want ready, got %s", name, got.Status)
		}
		if len(v.fake.deleted) != 0 {
			t.Fatalf("%s: deleted files without approval", name)
		}
	}
}

func TestStillDownloadingOnlyReportsProgress(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(200, 50, "downloading"))
	got := v.status(t, j.ID)
	if got.Status != "downloading" || got.Progress != 75 || len(v.fake.deleted) != 0 {
		t.Fatalf("got %s %d%% deleted=%v", got.Status, got.Progress, v.fake.deleted)
	}
}

func TestVanishedDownloadIsLostNotDeleted(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Now().Add(-time.Hour))
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "lost" || len(v.fake.deleted) != 0 {
		t.Fatalf("got %s deleted=%v", got.Status, v.fake.deleted)
	}
}

func TestRecentJobMissingFromQueueIsLeftAlone(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{}) // just created: the queue may not list it yet
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "downloading" || len(v.fake.deleted) != 0 {
		t.Fatalf("got %s deleted=%v", got.Status, v.fake.deleted)
	}
}

func TestImportCompletionMarksDoneOnlyWithFreshFiles(t *testing.T) {
	v := setup(t, nil)
	j := v.job(t, time.Time{})
	j.Status, j.ImportCmd = "importing", 42
	_ = v.st.UpdateJob(j)
	v.fake.filesAfter = []sonarr.EpisodeFile{{ID: 99, SeasonNumber: 3}, {ID: 98, SeasonNumber: 3}}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "done" {
		t.Fatalf("want done, got %s %q", got.Status, got.Message)
	}
	// an import that left the old files (or nothing) in place must not be reported as done
	v = setup(t, nil)
	j = v.job(t, time.Time{})
	j.Status, j.ImportCmd = "importing", 42
	_ = v.st.UpdateJob(j)
	v.fake.filesAfter = nil
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "failed" {
		t.Fatalf("empty season after import must be failed, got %s", got.Status)
	}
}

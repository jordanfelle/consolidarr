package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// removal records one bulk removal of download-queue items.
type removal struct {
	IDs   []int
	Query string
}

// fakeSonarr serves just enough of Sonarr's API for the job lifecycle and records destructive calls.
type fakeSonarr struct {
	mu            sync.Mutex
	deleted       [][]int
	imports       int
	grabs         int
	searches      [][2]int         // SeasonSearch commands (series, season)
	onDelete      func()           // called as a delete request arrives, before it is recorded
	failImport    int              // fail this many import commands with a 500
	failGrab      bool             // POST /release answers 500
	unaired       map[int]bool     // episodes whose air date is in the future
	noEpisodes    bool             // GET /episode returns nothing
	rejections    map[int][]string // manualimport rejection reasons by episode number
	removed       []removal        // queue removals
	series        []map[string]any // what GET /series reports (default: empty)
	cmds          []map[string]any // what GET /command reports
	itemPath      string           // path manualimport reports for every file (default inside the download folder)
	queueItems    []map[string]any // what GET /queue reports (default: empty)
	manualQueries []string         // raw query of every manualimport request
	packEps       []int            // episodes the finished download contains
	seasonEps     map[int]int
	filesAfter    []sonarr.EpisodeFile
	existing      map[int]bool // when set, GET /episodefile lists these and bulk delete 500s on unknown ids
	cmdStatus     string
	failFiles     bool // GET /episodefile answers 500
	srv           *httptest.Server
}

func newFake(packEps []int) *fakeSonarr {
	f := &fakeSonarr{packEps: packEps, seasonEps: map[int]int{1: 11, 2: 12, 3: 13}, cmdStatus: "completed",
		existing: map[int]bool{11: true, 12: true, 13: true}}
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /api/v3/manualimport", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.manualQueries = append(f.manualQueries, r.URL.RawQuery)
		f.mu.Unlock()
		var items []map[string]any
		for _, e := range f.packEps {
			rej := []map[string]string{}
			for _, reason := range f.rejections[e] {
				rej = append(rej, map[string]string{"reason": reason})
			}
			items = append(items, map[string]any{"path": f.itemPathOrDefault(), "seasonNumber": 3, "series": map[string]int{"id": 1},
				"episodes": []map[string]int{{"episodeNumber": e}}, "rejections": rej})
		}
		js(w, items)
	})
	mux.HandleFunc("GET /api/v3/episode", func(w http.ResponseWriter, r *http.Request) {
		if f.noEpisodes {
			js(w, []any{})
			return
		}
		var eps []map[string]any
		for n, id := range f.seasonEps {
			eps = append(eps, map[string]any{"seasonNumber": 3, "episodeNumber": n, "episodeFileId": id, "airDateUtc": airDate(f.unaired[n])})
		}
		js(w, eps)
	})
	mux.HandleFunc("DELETE /api/v3/episodefile/bulk", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			EpisodeFileIDs []int `json:"episodeFileIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		if f.onDelete != nil {
			f.onDelete()
		}
		f.deleted = append(f.deleted, b.EpisodeFileIDs)
		f.mu.Unlock()
	})
	mux.HandleFunc("POST /api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name         string `json:"name"`
			SeriesID     int    `json:"seriesId"`
			SeasonNumber int    `json:"seasonNumber"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		if b.Name != "SeasonSearch" && f.failImport > 0 {
			f.failImport--
			f.mu.Unlock()
			http.Error(w, "import refused", 500)
			return
		}
		if b.Name == "SeasonSearch" {
			f.searches = append(f.searches, [2]int{b.SeriesID, b.SeasonNumber})
		} else {
			f.imports++
		}
		f.mu.Unlock()
		js(w, map[string]int{"id": 42})
	})
	mux.HandleFunc("DELETE /api/v3/queue/bulk", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			IDs []int `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.removed = append(f.removed, removal{b.IDs, r.URL.RawQuery})
		f.mu.Unlock()
	})
	mux.HandleFunc("GET /api/v3/command/42", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"id": 42, "status": f.cmdStatus})
	})
	mux.HandleFunc("GET /api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		if f.failFiles {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if f.filesAfter != nil { // a test that scripts the post-import state
			js(w, f.filesAfter)
			return
		}
		fs := []sonarr.EpisodeFile{}
		for id := range f.existing {
			fs = append(fs, sonarr.EpisodeFile{ID: id, SeasonNumber: 3})
		}
		js(w, fs)
	})
	mux.HandleFunc("GET /api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		if f.cmds == nil {
			js(w, []any{})
			return
		}
		js(w, f.cmds)
	})
	mux.HandleFunc("GET /api/v3/series", func(w http.ResponseWriter, r *http.Request) {
		if f.series != nil {
			js(w, f.series)
			return
		}
		js(w, []any{})
	})
	mux.HandleFunc("GET /api/v3/health", func(w http.ResponseWriter, r *http.Request) { js(w, []any{}) })
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		if f.queueItems != nil {
			js(w, map[string]any{"records": f.queueItems})
			return
		}
		js(w, map[string]any{"records": []any{}})
	})
	mux.HandleFunc("GET /api/v3/release", func(w http.ResponseWriter, r *http.Request) {
		js(w, []map[string]any{{"guid": "g1", "indexerId": 1, "indexer": "BTN", "title": "Show.S03.1080p.WEB-DL", "size": 20e9,
			"seeders": 10, "fullSeason": true, "seasonNumber": 3, "rejected": false, "rejections": []string{},
			"quality": map[string]any{"quality": map[string]any{"id": 3, "name": "WEBDL-1080p", "source": "web", "resolution": 1080}}}})
	})
	mux.HandleFunc("POST /api/v3/release", func(w http.ResponseWriter, r *http.Request) {
		if f.failGrab {
			http.Error(w, "grab refused", 500)
			return
		}
		f.mu.Lock()
		f.grabs++
		f.mu.Unlock()
		js(w, map[string]bool{"ok": true})
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeSonarr) itemPathOrDefault() string {
	if f.itemPath != "" {
		return f.itemPath
	}
	return "/dl/pack/e.mkv"
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

func TestVanishedDownloadSonarrAlreadyImportedIsDone(t *testing.T) {
	grab := func(t *testing.T, oldIDs []int) (*env, store.Job) {
		v := setup(t, []int{1, 2, 3})
		j := v.job(t, time.Now().Add(-time.Hour))
		ids, _ := json.Marshal(oldIDs)
		if oldIDs == nil {
			ids = []byte("[]")
		}
		_, _ = v.st.Exec(`update jobs set release=?, old_file_ids=? where id=?`, "Show S03 1080p WEB x265-NOXXUS", string(ids), j.ID)
		j, _ = v.st.Job(j.ID)
		v.fake.seasonEps = map[int]int{1: 91, 2: 92, 3: 93}
		v.fake.filesAfter = []sonarr.EpisodeFile{{ID: 91, ReleaseGroup: "NOXXUS"}, {ID: 92, ReleaseGroup: "NOXXUS"}, {ID: 93, ReleaseGroup: "NOXXUS"}}
		return v, j
	}
	run := func(v *env, j store.Job) store.Job {
		v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
		return v.status(t, j.ID)
	}
	v, j := grab(t, []int{11, 12, 13})
	if got := run(v, j); got.Status != "done" || len(v.fake.deleted) != 0 {
		t.Fatalf("got %s %q deleted=%v", got.Status, got.Message, v.fake.deleted)
	}
	// new files from a different release group are someone else's upgrade, not this pack
	v, j = grab(t, []int{11, 12, 13})
	v.fake.filesAfter[1].ReleaseGroup = "OTHER"
	if got := run(v, j); got.Status != "lost" {
		t.Fatalf("a mixed-group season must stay lost, got %s", got.Status)
	}
	// a partial replacement is still lost, never done
	v, j = grab(t, []int{11, 12, 13})
	v.fake.seasonEps = map[int]int{1: 91, 2: 12, 3: 13}
	if got := run(v, j); got.Status != "lost" {
		t.Fatalf("partial replacement must stay lost, got %s", got.Status)
	}
	// no recorded old files: every file would look new, so claim nothing
	v, j = grab(t, nil)
	if got := run(v, j); got.Status != "lost" {
		t.Fatalf("empty OldFileIDs must stay lost, got %s", got.Status)
	}
	// hyphenated groups and trailing site tags still match
	v, j = grab(t, []int{11, 12, 13})
	_, _ = v.st.Exec(`update jobs set release=? where id=?`, "Show S03 1080p WEB-DL H.264-D-Z0N3[rartv]", j.ID)
	for i := range v.fake.filesAfter {
		v.fake.filesAfter[i].ReleaseGroup = "D-Z0N3"
	}
	j, _ = v.st.Job(j.ID)
	if got := run(v, j); got.Status != "done" {
		t.Fatalf("hyphenated group with a site tag should match, got %s %q", got.Status, got.Message)
	}
	// a quality token is never a group
	v, j = grab(t, []int{11, 12, 13})
	_, _ = v.st.Exec(`update jobs set release=? where id=?`, "Show S03 1080p WEB-DL", j.ID)
	for i := range v.fake.filesAfter {
		v.fake.filesAfter[i].ReleaseGroup = "DL"
	}
	j, _ = v.st.Job(j.ID)
	if got := run(v, j); got.Status != "lost" {
		t.Fatalf("WEB-DL must not parse as group DL, got %s", got.Status)
	}
	// a Sonarr error is not evidence either way: leave the job to be re-checked next cycle
	v, j = grab(t, []int{11, 12, 13})
	v.fake.srv.Close()
	if got := run(v, j); got.Status != "downloading" {
		t.Fatalf("a Sonarr error must not write a terminal state, got %s", got.Status)
	}
	// a failure of the second call (episode files) is handled the same, and a later good cycle recovers
	v, j = grab(t, []int{11, 12, 13})
	good := v.fake.filesAfter
	v.fake.filesAfter = nil
	v.fake.failFiles = true
	if got := run(v, j); got.Status != "downloading" {
		t.Fatalf("an episodefile error must not write a terminal state, got %s", got.Status)
	}
	v.fake.failFiles, v.fake.filesAfter = false, good
	j, _ = v.st.Job(j.ID)
	if got := run(v, j); got.Status != "done" {
		t.Fatalf("the next cycle should recover to done, got %s %q", got.Status, got.Message)
	}
	// errors that persist for a day end as lost instead of hanging forever
	v, j = grab(t, []int{11, 12, 13})
	v.fake.failFiles = true
	_, _ = v.st.Exec(`update jobs set created=? where id=?`, time.Now().Add(-25*time.Hour).Unix(), j.ID)
	j, _ = v.st.Job(j.ID)
	if got := run(v, j); got.Status != "lost" {
		t.Fatalf("a day of errors should fall through to lost, got %s", got.Status)
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
	v.fake.seasonEps = map[int]int{1: 99, 2: 98} // both episodes now point at new files
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "done" {
		t.Fatalf("want done, got %s %q", got.Status, got.Message)
	}
	// an import that left the old files (or nothing) in place must not be reported as done
	v = setup(t, nil)
	j = v.job(t, time.Time{})
	j.Status, j.ImportCmd = "importing", 42
	_ = v.st.UpdateJob(j)
	v.fake.filesAfter, v.fake.existing = nil, map[int]bool{}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "importing" {
		t.Fatalf("a fresh import gets time to settle before it is called failed, got %s", got.Status)
	}
	j = v.status(t, j.ID)
	j.Deleted = time.Now().Add(-time.Hour).Unix() // the old files were deleted an hour ago and nothing has appeared
	j.ImportAt = j.Deleted
	_ = v.st.UpdateJob(j)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "failed" {
		t.Fatalf("an empty season an hour after import must be failed, got %s", got.Status)
	}
}

func TestSonarrAlreadyReplacedSomeOldFiles(t *testing.T) {
	// Sonarr imported part of the pack as upgrades, so two of the three recorded old files are gone.
	v := setup(t, []int{1, 2, 3})
	v.fake.existing = map[int]bool{13: true}
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	got := v.status(t, j.ID)
	if got.Status != "importing" {
		t.Fatalf("a partially replaced season must still finish, got %s %q", got.Status, got.Message)
	}
	if len(v.fake.deleted) != 1 || len(v.fake.deleted[0]) != 1 || v.fake.deleted[0][0] != 13 {
		t.Fatalf("must delete only the file that still exists, got %v", v.fake.deleted)
	}
}

func TestNothingLeftToDeleteStillImports(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.existing = map[int]bool{} // every old file already replaced by Sonarr
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "importing" || len(v.fake.deleted) != 0 || v.fake.imports != 1 {
		t.Fatalf("got %s deleted=%v imports=%d", got.Status, v.fake.deleted, v.fake.imports)
	}
}

// seedSeasons stores n flagged seasons (all series have a season 3 so the fake's release search matches).
func seedSeasons(t *testing.T, v *env, n int) {
	t.Helper()
	var rows []audit.Season
	for i := 1; i <= n; i++ {
		rows = append(rows, audit.Season{InstID: 1, SeriesID: i, Season: 3, Title: fmt.Sprintf("Show %d", i), SeriesType: "standard",
			Files: 8, Aired: 8, Groups: []string{"a", "b"}, Qualities: []string{"WEBDL-1080p"}, Sources: []string{"web"}, CurRank: 10802})
	}
	if err := v.st.ReplaceSeasons(1, 0, rows); err != nil {
		t.Fatal(err)
	}
	_, _ = v.st.SaveInstance(store.Instance{Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true})
	v.st.SetKV("lastScan:1", strconv.FormatInt(time.Now().Unix(), 10)) // otherwise the cycle rescans and wipes the seeds
}

func TestOneOffRunWorksWithTheSchedulerOffAndHonoursItsLimit(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	seedSeasons(t, v, 5)
	set := live() // scheduler OFF, dry run off
	if shouldCycle(set, v.e.Once()) {
		t.Fatal("with the scheduler off and no one-off run, nothing may run")
	}
	if err := v.e.StartOnce(2); err != nil {
		t.Fatal(err)
	}
	if err := v.e.StartOnce(2); err == nil {
		t.Fatal("a second concurrent one-off run must be refused")
	}
	if !shouldCycle(set, v.e.Once()) {
		t.Fatal("an active one-off run must be allowed to cycle even with the scheduler off")
	}
	done := v.e.cycle(context.Background(), store.Instance{ID: 1, Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true}, set)
	if !done || v.fake.grabs != 2 || v.e.Once().Started != 2 {
		t.Fatalf("limit 2 of 5 candidates: done=%v grabs=%d started=%d", done, v.fake.grabs, v.e.Once().Started)
	}
	jobs, _ := v.st.Jobs(true, 10)
	if len(jobs) != 2 {
		t.Fatalf("want 2 tracked jobs, got %d", len(jobs))
	}
}

func TestOneOffDryRunGrabsNothing(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	seedSeasons(t, v, 3)
	set := store.DefaultSettings() // dry run ON
	_ = v.e.StartOnce(10)
	done := v.e.cycle(context.Background(), store.Instance{ID: 1, Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true}, set)
	if !done || v.fake.grabs != 0 || v.e.Once().Started != 3 {
		t.Fatalf("dry run: done=%v grabs=%d started=%d (want 3 picks logged, 0 grabs)", done, v.fake.grabs, v.e.Once().Started)
	}
	if j, _ := v.st.Jobs(true, 10); len(j) != 0 {
		t.Fatal("dry run must not create jobs")
	}
}

func TestCancelOnceStopsStartingNewSeasons(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	seedSeasons(t, v, 5)
	_ = v.e.StartOnce(5)
	v.e.CancelOnce()
	v.e.cycle(context.Background(), store.Instance{ID: 1, Name: "t", URL: v.fake.srv.URL, APIKey: "k", Enabled: true}, live())
	if v.fake.grabs != 0 {
		t.Fatalf("cancelled before any grab: want 0 grabs, got %d", v.fake.grabs)
	}
}

func TestOneOffFinishesOnlyWhenNothingIsRunning(t *testing.T) {
	v := setup(t, nil)
	_ = v.e.StartOnce(1)
	j := v.job(t, time.Time{}) // downloading
	if v.e.running() != 1 {
		t.Fatalf("a downloading job counts as running, got %d", v.e.running())
	}
	j.Status = "ready" // needs a human: must not keep the run alive
	_ = v.st.UpdateJob(j)
	if v.e.running() != 0 {
		t.Fatal("ready jobs must not keep a one-off run alive")
	}
	v.e.finishOnce()
	if o := v.e.Once(); o.Active || o.Finished == 0 {
		t.Fatalf("run should be finished: %+v", o)
	}
}

func TestUnverifiableFilesAreHeldUnlessAllowed(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	probeFn = func(store.Instance, string) (bool, string) { return true, "unverified" }
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	got := v.status(t, j.ID)
	if got.Status != "holding" || !strings.Contains(got.Message, "cannot verify") || len(v.fake.deleted) != 0 {
		t.Fatalf("unverifiable files must be held, got %s %q deleted=%v", got.Status, got.Message, v.fake.deleted)
	}
	set := live()
	set.AllowUnverified = true
	v.e.advance(context.Background(), v.inst, v.c, set, got, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "importing" || len(v.fake.deleted) != 1 {
		t.Fatalf("with AllowUnverified the job may proceed, got %s %q", got.Status, got.Message)
	}
}

func TestFilesOutsideTheDownloadFolderAreHeld(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.itemPath = "/mnt/Media2/TV Shows/Show/Season 03/e.mkv" // a library path, not the download
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	got := v.status(t, j.ID)
	if got.Status != "holding" || !strings.Contains(got.Message, "outside the download folder") || len(v.fake.deleted) != 0 {
		t.Fatalf("got %s %q deleted=%v", got.Status, got.Message, v.fake.deleted)
	}
}

func TestBackedUpSonarrDefersDeletionThenProceeds(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	old := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	v.fake.cmds = []map[string]any{{"id": 1, "name": "ProcessMonitoredDownloads", "status": "started", "started": old}}
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "holding" || !strings.Contains(got.Message, "waiting") || len(v.fake.deleted) != 0 {
		t.Fatalf("a stuck command must defer deletion, got %s %q", got.Status, got.Message)
	}
	// too many imports already queued
	var q []map[string]any
	for i := 0; i < 8; i++ {
		q = append(q, map[string]any{"id": 10 + i, "name": "DownloadedEpisodesScan", "status": "queued"})
	}
	v.fake.cmds = q
	j = v.status(t, j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "holding" || len(v.fake.deleted) != 0 {
		t.Fatalf("a deep import queue must defer deletion, got %s %q", got.Status, got.Message)
	}
	// a big backlog of completed downloads
	v.fake.cmds = nil
	backlog := done(100, 0, "completed")
	for i := 0; i < 150; i++ {
		backlog = append(backlog, sonarr.QueueItem{SeriesID: 99, SeasonNumber: 1, Status: "completed", TrackedState: "importPending"})
	}
	j = v.status(t, j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, backlog)
	if got := v.status(t, j.ID); got.Status != "holding" || len(v.fake.deleted) != 0 {
		t.Fatalf("an import backlog must defer deletion, got %s %q", got.Status, got.Message)
	}
	// healthy again: proceeds
	j = v.status(t, j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, j.ID); got.Status != "importing" || len(v.fake.deleted) != 1 {
		t.Fatalf("once Sonarr is healthy the job proceeds, got %s %q", got.Status, got.Message)
	}
}

func TestOnlyOneSeasonImportsAtATime(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	oid, _ := v.st.AddJob(store.Job{InstID: 1, SeriesID: 2, Season: 1, Title: "Other Show", OldFileIDs: []int{1}})
	other := store.Job{ID: oid, Status: "importing", ImportCmd: 42}
	_ = v.st.UpdateJob(other)
	id, _ := v.st.AddJob(store.Job{InstID: 1, SeriesID: 1, Season: 3, Title: "Show", OldFileIDs: []int{11, 12, 13}})
	js, _ := v.st.Jobs(true, 10)
	var j store.Job
	for _, x := range js {
		if x.ID == id {
			j = x
		}
	}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if got := v.status(t, id); got.Status != "holding" || !strings.Contains(got.Message, "Other Show") || len(v.fake.deleted) != 0 {
		t.Fatalf("must wait for the other season, got %s %q", got.Status, got.Message)
	}
}

func TestApproveRespectsTheSameGate(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	j.Status = "ready"
	_ = v.st.UpdateJob(j)
	v.fake.cmds = []map[string]any{{"id": 1, "name": "ProcessMonitoredDownloads", "status": "started",
		"started": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}
	v.fake.queueItems = []map[string]any{{"seriesId": 1, "seasonNumber": 3, "status": "completed", "downloadId": "d", "outputPath": "/dl/pack"}}
	inst := store.Instance{ID: 1, Name: "t", URL: v.fake.srv.URL, APIKey: "k"}
	err := v.e.Approve(context.Background(), inst, j)
	if err == nil || !strings.Contains(err.Error(), "not now") || len(v.fake.deleted) != 0 {
		t.Fatalf("approve must refuse while Sonarr looks stuck, err=%v deleted=%v", err, v.fake.deleted)
	}
}

func TestVerificationListsTheDownloadFolderNeverTheLibrary(t *testing.T) {
	// Passing seriesId makes real Sonarr list the series' existing library files and ignore the folder,
	// which silently turned "verify the pack" into "probe files you already have".
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if len(v.fake.manualQueries) == 0 {
		t.Fatal("no manualimport request was made")
	}
	for _, q := range v.fake.manualQueries {
		if strings.Contains(q, "seriesId") || strings.Contains(q, "seasonNumber") {
			t.Errorf("manualimport must not be filtered by series: %s", q)
		}
		if !strings.Contains(q, "folder=") {
			t.Errorf("manualimport must list the download's folder: %s", q)
		}
	}
}

func TestItemsOfOtherSeriesInTheFolderAreIgnored(t *testing.T) {
	v := setup(t, []int{1, 2, 3, 4, 5}) // fake attributes every item to series 1
	probed := 0
	probeFn = func(store.Instance, string) (bool, string) { probed++; return true, "ok" }
	j := v.job(t, time.Time{})
	j.SeriesID = 2 // a different series: none of the folder's items belong to this job
	v.e.advance(context.Background(), v.inst, v.c, live(), j, []sonarr.QueueItem{{SeriesID: 2, SeasonNumber: 3, Size: 100, Status: "completed", DownloadID: "d", OutputPath: "/dl/pack"}})
	if probed != 0 || len(v.fake.deleted) != 0 {
		t.Fatalf("files of another series must not count: probed=%d deleted=%v", probed, v.fake.deleted)
	}
}

func TestOneOffCannotStartWhileTheSchedulerIsOn(t *testing.T) {
	v := setup(t, nil)
	set := v.st.Settings()
	set.SchedulerEnabled = true
	_ = v.st.SaveSettings(set)
	if err := v.e.StartOnce(3); err == nil || !strings.Contains(err.Error(), "scheduler is on") {
		t.Fatalf("a one-off run alongside the scheduler is meaningless and must be refused, got %v", err)
	}
	if v.e.Once().Active {
		t.Fatal("a refused run must not be recorded as active")
	}
}

func airDate(future bool) string {
	if future {
		return time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	}
	return time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
}

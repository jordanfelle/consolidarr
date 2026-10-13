package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	history       []map[string]any // what GET /history/series reports (default: empty)
	historyCalls  atomic.Int32
	failHistory   bool
	manualQueries []string // raw query of every manualimport request
	packEps       []int    // episodes the finished download contains
	seasonEps     map[int]int
	filesAfter    []sonarr.EpisodeFile
	existing      map[int]bool // when set, GET /episodefile lists these and bulk delete 500s on unknown ids
	cmdStatus     string
	failFiles     bool // GET /episodefile answers 500
	realistic     bool // model Sonarr honestly: a delete removes files, an import is rejected while old files remain, scans are listed
	rejected      int  // imports Sonarr refused because old files were still present
	scans         []map[string]any
	scanStatus    string             // status new scans get in realistic mode (default completed)
	items         []map[string]any   // when set, GET /manualimport answers exactly this
	folderItems   []map[string]any   // when set, a folder-based GET /manualimport answers this instead
	failByID      bool               // GET /manualimport?downloadId=... answers 500
	episodes      []map[string]any   // when set, GET /episode answers exactly this
	mappedImports [][]map[string]any // the "files" of every ManualImport command
	srv           *httptest.Server
}

func (f *fakeSonarr) scanStatusOrDefault() string {
	if f.scanStatus != "" {
		return f.scanStatus
	}
	return "completed"
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
		if f.failByID && r.URL.Query().Get("downloadId") != "" {
			http.Error(w, "boom", 500)
			return
		}
		if f.folderItems != nil && r.URL.Query().Get("folder") != "" {
			js(w, f.folderItems)
			return
		}
		if f.items != nil {
			js(w, f.items)
			return
		}
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
		if f.episodes != nil {
			js(w, f.episodes)
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
		if f.realistic {
			for _, id := range b.EpisodeFileIDs {
				delete(f.existing, id)
				for n, fid := range f.seasonEps {
					if fid == id {
						f.seasonEps[n] = 0
					}
				}
			}
		}
		f.mu.Unlock()
	})
	mux.HandleFunc("POST /api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name         string           `json:"name"`
			SeriesID     int              `json:"seriesId"`
			SeasonNumber int              `json:"seasonNumber"`
			Path         string           `json:"path"`
			ClientID     string           `json:"downloadClientId"`
			Files        []map[string]any `json:"files"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		if b.Name == "ManualImport" {
			f.mappedImports = append(f.mappedImports, b.Files)
		}
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
			if f.realistic {
				id := 100 + f.imports
				f.scans = append(f.scans, map[string]any{"id": id, "name": b.Name, "status": f.scanStatusOrDefault(), "queued": time.Now().UTC().Format(time.RFC3339Nano), "body": map[string]string{"path": b.Path, "downloadClientId": b.ClientID}})
				if len(f.existing) > 0 {
					f.rejected++ // "not an upgrade": Sonarr will not import over files that are still there
				} else {
					for _, n := range f.packEps {
						f.seasonEps[n] = 900 + n
					}
				}
				f.mu.Unlock()
				js(w, map[string]int{"id": id})
				return
			}
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
	mux.HandleFunc("GET /api/v3/command/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, sc := range f.scans {
			if fmt.Sprint(sc["id"]) == r.PathValue("id") {
				js(w, sc)
				return
			}
		}
		http.NotFound(w, r)
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
		if f.realistic {
			f.mu.Lock()
			defer f.mu.Unlock()
			js(w, append([]map[string]any{}, f.scans...))
			return
		}
		if f.cmds == nil {
			// By default the fake serves what a real Sonarr serves (recorded payloads plus every known command shape),
			// not an empty list: one odd command in the real listing once held every replacement in production.
			js(w, realisticCommandListing())
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
	mux.HandleFunc("GET /api/v3/history/series", func(w http.ResponseWriter, r *http.Request) {
		f.historyCalls.Add(1)
		if f.failHistory {
			http.Error(w, "boom", 500)
			return
		}
		if f.history != nil {
			js(w, f.history)
			return
		}
		js(w, []any{})
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
	// error, a pass that is not an error, then another error: the timer restarted, so one blip never ends the job
	v, j = grab(t, []int{11, 12, 13})
	v.fake.failFiles = true
	_, _ = v.st.Exec(`update jobs set created=? where id=?`, time.Now().Add(-25*time.Hour).Unix(), j.ID)
	j, _ = v.st.Job(j.ID)
	run(v, j)
	v.e.mu.Lock()
	v.e.errSince[j.ID] = time.Now().Add(-25 * time.Hour) // stale timer from the first error
	v.e.mu.Unlock()
	q := []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Title: j.Release, DownloadID: "X", Size: 100, SizeLeft: 50, Status: "downloading"}}
	j.DownloadID = "X"
	_ = v.st.UpdateJob(j)
	j, _ = v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, q) // reappears in the queue: not an error
	v.e.mu.Lock()
	_, stale := v.e.errSince[j.ID]
	v.e.mu.Unlock()
	if stale {
		t.Fatal("a pass that is not an error must clear the timer")
	}
	j, _ = v.st.Job(j.ID)
	if got := run(v, j); got.Status != "downloading" {
		t.Fatalf("a fresh blip after a clean pass must not end the job, got %s", got.Status)
	}
	// errors that persist for a day end as lost instead of hanging forever
	v, j = grab(t, []int{11, 12, 13})
	v.fake.failFiles = true
	_, _ = v.st.Exec(`update jobs set created=? where id=?`, time.Now().Add(-25*time.Hour).Unix(), j.ID)
	j, _ = v.st.Job(j.ID)
	if got := run(v, j); got.Status != "downloading" {
		t.Fatalf("one blip on an old job must not end it, got %s", got.Status)
	}
	v.e.mu.Lock()
	first, recorded := v.e.errSince[j.ID]
	v.e.errSince[j.ID] = time.Now().Add(-25 * time.Hour)
	v.e.mu.Unlock()
	if !recorded || first.IsZero() {
		t.Fatal("the first error must be recorded")
	}
	if got := run(v, j); got.Status != "lost" {
		t.Fatalf("a day of errors in a row should fall through to lost, got %s", got.Status)
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
	for i := 0; i < maxImportBacklog+10; i++ { // distinct downloads: the gate counts downloads, not episode records
		backlog = append(backlog, sonarr.QueueItem{SeriesID: 99, SeasonNumber: 1, DownloadID: fmt.Sprintf("bk%d", i), Status: "completed", TrackedState: "importPending"})
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

func TestVerificationListsTheDownloadNeverTheLibrary(t *testing.T) {
	// Passing seriesId makes real Sonarr list the series' existing library files and ignore the folder,
	// which silently turned "verify the pack" into "probe files you already have".
	v := setup(t, []int{1, 2, 3})
	j := v.job(t, time.Time{})
	v.e.advance(context.Background(), v.inst, v.c, live(), j, done(100, 0, "completed"))
	if len(v.fake.manualQueries) == 0 {
		t.Fatal("no manualimport request was made")
	}
	if !strings.Contains(v.fake.manualQueries[0], "downloadId=") {
		t.Errorf("the download's own listing must be asked for first: %v", v.fake.manualQueries)
	}
	for _, q := range v.fake.manualQueries {
		if strings.Contains(q, "seriesId") || strings.Contains(q, "seasonNumber") {
			t.Errorf("manualimport must not be filtered by series: %s", q)
		}
		if !strings.Contains(q, "downloadId=") && !strings.Contains(q, "folder=") {
			t.Errorf("manualimport must list the download itself (by id or folder): %s", q)
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

func TestReconcileLostFixesOnlyVanishedJobsSonarrImported(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	lost := func(msg string) store.Job {
		j := v.job(t, time.Now().Add(-time.Hour))
		_, _ = v.st.Exec(`update jobs set status='lost', message=?, release=? where id=?`, msg, "Show S03 1080p WEB x265-NOXXUS", j.ID)
		j, _ = v.st.Job(j.ID)
		return j
	}
	v.fake.seasonEps = map[int]int{1: 91, 2: 92, 3: 93}
	v.fake.filesAfter = []sonarr.EpisodeFile{{ID: 91, ReleaseGroup: "NOXXUS"}, {ID: 92, ReleaseGroup: "NOXXUS"}, {ID: 93, ReleaseGroup: "NOXXUS"}}
	a := lost("no longer in Sonarr's queue; old files untouched")
	b := lost("the grab never completed; nothing was downloaded or changed") // a different lost reason: not ours to judge
	_, _ = v.st.Exec(`update jobs set season=4 where id=?`, b.ID)            // its own season, so it is not "later" than a
	v.e.reconcileLost(context.Background(), v.inst, v.c)
	if got := v.status(t, a.ID); got.Status != "done" || len(v.fake.deleted) != 0 {
		t.Fatalf("a vanished-lost job Sonarr imported must become done, got %s %q", got.Status, got.Message)
	}
	if got := v.status(t, b.ID); got.Status != "lost" {
		t.Fatalf("other lost jobs must be untouched, got %s", got.Status)
	}
	if v.st.KV("reconciled:vanished-lost:v1:1") == "" {
		t.Fatal("a clean pass must write the marker so it never runs again")
	}
	// a Sonarr error leaves the marker unset so the next cycle retries
	v2 := setup(t, []int{1, 2, 3})
	j := v2.job(t, time.Now().Add(-time.Hour))
	_, _ = v2.st.Exec(`update jobs set status='lost', message=?, release=? where id=?`, "no longer in Sonarr's queue; old files untouched", "Show S03 1080p WEB x265-NOXXUS", j.ID)
	v2.fake.failFiles = true
	v2.e.reconcileLost(context.Background(), v2.inst, v2.c)
	if v2.st.KV("reconciled:vanished-lost:v1:1") != "" || v2.status(t, j.ID).Status != "lost" {
		t.Fatal("an error must neither flip the job nor write the marker")
	}
}

func TestReconcileLostSkipsSeasonsWithALaterJobAndKeepsGoingPastErrors(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	mk := func(season int) store.Job {
		id, _ := v.st.AddJob(store.Job{InstID: 1, SeriesID: 1, Season: 3, Title: "Show", OldFileIDs: []int{11, 12, 13}, Release: "Show S03 1080p WEB x265-NOXXUS", Status: "lost"})
		_, _ = v.st.Exec(`update jobs set status='lost', message=? where id=?`, "no longer in Sonarr's queue; old files untouched", id)
		j, _ := v.st.Job(id)
		return j
	}
	v.fake.seasonEps = map[int]int{1: 91, 2: 92, 3: 93}
	v.fake.filesAfter = []sonarr.EpisodeFile{{ID: 91, ReleaseGroup: "NOXXUS"}, {ID: 92, ReleaseGroup: "NOXXUS"}, {ID: 93, ReleaseGroup: "NOXXUS"}}
	old, latest := mk(3), mk(3) // two past attempts at the same season
	v.e.reconcileLost(context.Background(), v.inst, v.c)
	if got := v.status(t, old.ID); got.Status != "lost" {
		t.Fatalf("an older job with a later one for the same season must be left alone, got %s", got.Status)
	}
	if got := v.status(t, latest.ID); got.Status != "done" {
		t.Fatalf("the season's latest job should be reconciled, got %s", got.Status)
	}
	// a job that moved on since it was listed (retried by the user) is not overwritten
	v2 := setup(t, []int{1, 2, 3})
	id, _ := v2.st.AddJob(store.Job{InstID: 1, SeriesID: 1, Season: 3, Title: "Show", OldFileIDs: []int{11}, Release: "x-NOXXUS", Status: "downloading"})
	if !v2.e.reconcileOne(context.Background(), v2.inst, v2.c, id) || v2.status(t, id).Status != "downloading" {
		t.Fatal("a job that is no longer lost must not be touched")
	}
}

func TestReconcileLostRetriesThenGivesUpWithoutBlockingOthers(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	mk := func(series, season int) store.Job {
		id, _ := v.st.AddJob(store.Job{InstID: 1, SeriesID: series, Season: season, Title: "Show", OldFileIDs: []int{11, 12, 13}, Release: "Show S03 1080p WEB x265-NOXXUS", Status: "lost"})
		_, _ = v.st.Exec(`update jobs set status='lost', message=? where id=?`, "no longer in Sonarr's queue; old files untouched", id)
		j, _ := v.st.Job(id)
		return j
	}
	j1 := mk(1, 3) // Sonarr's episodefile endpoint fails for every series in this fake, so every job errors
	j2 := mk(2, 3)
	v.fake.failFiles = true
	key := "reconciled:vanished-lost:v1:1"
	for i := 1; i <= 4; i++ {
		v.e.reconcileLost(context.Background(), v.inst, v.c)
		if v.st.KV(key) != "" {
			t.Fatalf("marker written after only %d tries", i)
		}
	}
	v.e.reconcileLost(context.Background(), v.inst, v.c)
	if v.st.KV(key) == "" {
		t.Fatal("the 5th failed try must stop the retries")
	}
	evs, _ := v.st.Events(20)
	gaveUp := false
	for _, e := range evs {
		if e.Level == "warn" && strings.Contains(e.Msg, "gave up re-checking") && strings.Contains(e.Msg, fmt.Sprint([]int64{j1.ID, j2.ID})) {
			gaveUp = true
		}
	}
	if !gaveUp {
		t.Fatal("giving up must be logged with the ids of the jobs it could not check")
	}
	for _, j := range []store.Job{j1, j2} {
		if got := v.status(t, j.ID); got.Status != "lost" {
			t.Fatalf("giving up must leave job %d lost, got %s", j.ID, got.Status)
		}
	}
}

func TestReconcileLostCountsTriesPerJobAndIgnoresShutdown(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	mk := func(series int) store.Job {
		id, _ := v.st.AddJob(store.Job{InstID: 1, SeriesID: series, Season: 3, Title: "Show", OldFileIDs: []int{11, 12, 13}, Release: "Show S03 1080p WEB x265-NOXXUS", Status: "lost"})
		_, _ = v.st.Exec(`update jobs set status='lost', message=? where id=?`, "no longer in Sonarr's queue; old files untouched", id)
		j, _ := v.st.Job(id)
		return j
	}
	a, b := mk(1), mk(2)
	v.fake.failFiles = true
	key := "reconciled:vanished-lost:v1:1"
	v.st.SetKV(fmt.Sprintf("%s:tries:%d", key, a.ID), "4") // a is one failure from exhausted, b is fresh
	v.e.reconcileLost(context.Background(), v.inst, v.c)
	if v.st.KV(key) != "" {
		t.Fatal("job b still has tries left, so the pass must not finish just because job a ran out")
	}
	if got := v.st.KV(fmt.Sprintf("%s:tries:%d", key, b.ID)); got != "1" {
		t.Fatalf("b must have exactly one try counted, got %q", got)
	}
	// a shutdown mid-pass burns no try and never finishes the pass
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v.e.reconcileLost(ctx, v.inst, v.c)
	if v.st.KV(key) != "" || v.st.KV(fmt.Sprintf("%s:tries:%d", key, b.ID)) != "1" {
		t.Fatal("a cancelled context must not count tries or write the marker")
	}
}

// realisticCommandListing is the recorded real listing plus the hand-written exotic shapes (internal/sonarr/testdata),
// with timestamps moved to now so the age-based checks (a command "running for 10+ minutes") see a healthy Sonarr.
func realisticCommandListing() []map[string]any {
	var all []map[string]any
	for _, name := range []string{"command.json", "command_exotic.json"} {
		b, err := os.ReadFile("../sonarr/testdata/" + name)
		if err != nil {
			panic(err)
		}
		var part []map[string]any
		if err := json.Unmarshal(b, &part); err != nil {
			panic(err)
		}
		all = append(all, part...)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, c := range all {
		for _, k := range []string{"queued", "started", "ended", "stateChangeTime", "lastExecutionTime", "lastStartTime"} {
			if v, ok := c[k]; ok && v != nil {
				c[k] = now
			}
		}
	}
	return all
}

func TestLostJobIsRevivedWhenItsPackAppearsInTheQueue(t *testing.T) {
	const rel = "Show.S03.1080p.WEB-DL.AAC2.0.x264-GRP"
	lose := func(v *env, msg string, age time.Duration) store.Job {
		j := v.job(t, time.Now().Add(-3*time.Hour))
		_, _ = v.st.Exec(`update jobs set status='lost', message=?, release=?, updated=? where id=?`, msg, rel, time.Now().Add(-age).Unix(), j.ID)
		j, _ = v.st.Job(j.ID)
		return j
	}
	pack := func(title, dl string) []sonarr.QueueItem {
		return []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Title: title, DownloadID: dl, Size: 5, SizeLeft: 0, Status: "completed"}}
	}
	vanished := "no longer in Sonarr's queue; old files untouched"

	// the exact release turns up: the job resumes bound to that download, nothing deleted
	v := setup(t, []int{1, 2, 3})
	j := lose(v, vanished, time.Hour)
	v.e.reviveLost(context.Background(), v.inst, v.c, pack(rel, "ABC"))
	got := v.status(t, j.ID)
	if got.Status != "downloading" || got.DownloadID != "ABC" || got.Message != "" || len(v.fake.deleted) != 0 {
		t.Fatalf("got %s dl=%q msg=%q deleted=%v", got.Status, got.DownloadID, got.Message, v.fake.deleted)
	}

	for name, tc := range map[string]struct {
		msg   string
		age   time.Duration
		queue []sonarr.QueueItem
	}{
		"a different release for the season is not ours": {vanished, time.Hour, pack("Show.S03.720p.HDTV-OTHER", "ABC")},
		"another season is not ours":                     {vanished, time.Hour, []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 4, Title: rel, DownloadID: "ABC"}}},
		"a lost job of another kind stays lost":          {"the grab never completed; nothing was downloaded or changed", time.Hour, pack(rel, "ABC")},
		"too long ago stays lost":                        {vanished, reviveWindow + time.Hour, pack(rel, "ABC")},
		"an item without a download id cannot be bound":  {vanished, time.Hour, pack(rel, "")},
		"not in the queue stays lost":                    {vanished, time.Hour, nil},
	} {
		v := setup(t, []int{1, 2, 3})
		j := lose(v, tc.msg, tc.age)
		v.e.reviveLost(context.Background(), v.inst, v.c, tc.queue)
		if got := v.status(t, j.ID); got.Status != "lost" {
			t.Errorf("%s: got %s", name, got.Status)
		}
	}

	// a download another job already owns is never adopted a second time, and two lost jobs cannot both take one
	v = setup(t, []int{1, 2, 3})
	a, b := lose(v, vanished, time.Hour), lose(v, vanished, time.Hour)
	_, _ = v.st.Exec(`update jobs set season=4 where id=?`, b.ID) // distinct seasons so the unique-active index is not what stops it
	v.e.reviveLost(context.Background(), v.inst, v.c, []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Title: rel, DownloadID: "abc"}, {SeriesID: 1, SeasonNumber: 4, Title: rel, DownloadID: "ABC"}})
	if v.status(t, a.ID).Status != "downloading" || v.status(t, b.ID).Status != "lost" {
		t.Fatalf("one download must end up with one job, got %s / %s", v.status(t, a.ID).Status, v.status(t, b.ID).Status)
	}

	// a season that already has a newer active job is left alone
	v = setup(t, []int{1, 2, 3})
	old := lose(v, vanished, time.Hour)
	newer := v.job(t, time.Time{})
	v.e.reviveLost(context.Background(), v.inst, v.c, pack(rel, "ABC"))
	if v.status(t, old.ID).Status != "lost" || v.status(t, newer.ID).Status == "lost" {
		t.Fatalf("old=%s newer=%s", v.status(t, old.ID).Status, v.status(t, newer.ID).Status)
	}
}

func TestReviveHonoursOwnBindingNewerJobsAndIsWiredIntoTheCycle(t *testing.T) {
	const rel = "Show.S03.1080p.WEB-DL.AAC2.0.x264-GRP"
	vanished := "no longer in Sonarr's queue; old files untouched"
	lose := func(v *env) store.Job {
		j := v.job(t, time.Now().Add(-3*time.Hour))
		_, _ = v.st.Exec(`update jobs set status='lost', message=?, release=?, updated=? where id=?`, vanished, rel, time.Now().Add(-time.Hour).Unix(), j.ID)
		j, _ = v.st.Job(j.ID)
		return j
	}
	pack := []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Title: rel, DownloadID: "ABC", Status: "completed"}}

	// a job that was bound to its download before it went lost may take that same download back
	v := setup(t, []int{1, 2, 3})
	j := lose(v)
	_, _ = v.st.Exec(`update jobs set download_id='ABC' where id=?`, j.ID)
	v.e.reviveLost(context.Background(), v.inst, v.c, pack)
	if got := v.status(t, j.ID); got.Status != "downloading" || got.DownloadID != "ABC" {
		t.Fatalf("its own recorded download must not block it, got %s %q", got.Status, got.DownloadID)
	}

	// a later job of any status (here a finished one) has the last word on the season
	v = setup(t, []int{1, 2, 3})
	old := lose(v)
	later := v.job(t, time.Time{})
	_, _ = v.st.Exec(`update jobs set status='done' where id=?`, later.ID)
	v.e.reviveLost(context.Background(), v.inst, v.c, pack)
	if v.status(t, old.ID).Status != "lost" || v.status(t, later.ID).Status != "done" {
		t.Fatalf("old=%s later=%s", v.status(t, old.ID).Status, v.status(t, later.ID).Status)
	}

	// two lost jobs for one season and release: only the latest is resumed
	v = setup(t, []int{1, 2, 3})
	a := lose(v)
	_, _ = v.st.Exec(`update jobs set status='lost' where id=?`, a.ID)
	b := lose(v)
	v.e.reviveLost(context.Background(), v.inst, v.c, pack)
	if v.status(t, a.ID).Status != "lost" || v.status(t, b.ID).Status != "downloading" {
		t.Fatalf("older=%s newer=%s", v.status(t, a.ID).Status, v.status(t, b.ID).Status)
	}

	// through the real cycle: the queue is fetched, the job revives, the next pass runs it, and nothing is deleted
	v = setup(t, []int{1, 2, 3})
	j = lose(v)
	v.fake.queueItems = []map[string]any{{"seriesId": 1, "seasonNumber": 3, "title": rel, "status": "completed", "downloadId": "ABC", "outputPath": "/dl/pack", "size": 5, "sizeleft": 0}}
	v.e.advanceJobs(context.Background(), v.inst, v.c, store.DefaultSettings())
	if got := v.status(t, j.ID); got.Status != "downloading" {
		t.Fatalf("advanceJobs must revive it, got %s %q", got.Status, got.Message)
	}
	v.e.advanceJobs(context.Background(), v.inst, v.c, store.DefaultSettings()) // dry run: verifies, deletes nothing
	if got := v.status(t, j.ID); got.Status == "lost" || got.Status == "failed" || len(v.fake.deleted) != 0 {
		t.Fatalf("after revival got %s %q deleted=%v", got.Status, got.Message, v.fake.deleted)
	}
}

func TestReviveMatchesARenamedQueueTitleThroughGrabHistory(t *testing.T) {
	const rel = "Prehistoric.Planet.2022.S03.1080p.ATVP.WEB-DL.DDPA5.1.H.264-FLUX"
	vanished := "no longer in Sonarr's queue; old files untouched"
	lose := func(v *env) store.Job {
		j := v.job(t, time.Now().Add(-3*time.Hour))
		_, _ = v.st.Exec(`update jobs set status='lost', message=?, release=?, updated=? where id=?`, vanished, rel, time.Now().Add(-time.Hour).Unix(), j.ID)
		j, _ = v.st.Job(j.ID)
		return j
	}
	// the queue shows a rewritten title (as on the real host); only the grab history ties it to our release
	queue := []sonarr.QueueItem{
		{SeriesID: 1, SeasonNumber: 3, Title: "Prehistoric.Planet.2022.S03.1080p.ATVP.WEB-DL.DDP5.1.Atmos.H.264-FLUX", DownloadID: "FA61", Status: "completed"},
		{SeriesID: 1, SeasonNumber: 3, Title: "Prehistoric.Planet.S03E01.720p.HDTV-OTHER", DownloadID: "BEEF", Status: "completed"},
	}
	grab := func(title, dl string) map[string]any {
		return map[string]any{"eventType": "grabbed", "sourceTitle": title, "downloadId": dl}
	}
	v := setup(t, []int{1, 2, 3})
	j := lose(v)
	v.fake.history = []map[string]any{grab("Prehistoric.Planet.S03E01.720p.HDTV-OTHER", "BEEF"), grab(rel, "fa61")}
	v.e.reviveLost(context.Background(), v.inst, v.c, queue)
	if got := v.status(t, j.ID); got.Status != "downloading" || got.DownloadID != "FA61" {
		t.Fatalf("got %s %q", got.Status, got.DownloadID)
	}

	// no grab of our release in the history: another download for the season is not ours
	v = setup(t, []int{1, 2, 3})
	j = lose(v)
	v.fake.history = []map[string]any{grab("Prehistoric.Planet.S03E01.720p.HDTV-OTHER", "BEEF")}
	v.e.reviveLost(context.Background(), v.inst, v.c, queue)
	if got := v.status(t, j.ID); got.Status != "lost" {
		t.Fatalf("got %s", got.Status)
	}

	// an import-failed or other event with the same title is not a grab
	v = setup(t, []int{1, 2, 3})
	j = lose(v)
	v.fake.history = []map[string]any{{"eventType": "downloadFailed", "sourceTitle": rel, "downloadId": "FA61"}}
	v.e.reviveLost(context.Background(), v.inst, v.c, queue)
	if got := v.status(t, j.ID); got.Status != "lost" {
		t.Fatalf("got %s", got.Status)
	}
}

func TestReviveAsksForHistoryOnlyWhenItCanChangeTheAnswer(t *testing.T) {
	const rel = "Show.S03.1080p.WEB-DL-GRP"
	vanished := "no longer in Sonarr's queue; old files untouched"
	lose := func(v *env, dl string) store.Job {
		j := v.job(t, time.Now().Add(-3*time.Hour))
		_, _ = v.st.Exec(`update jobs set status='lost', message=?, release=?, download_id=?, updated=? where id=?`, vanished, rel, dl, time.Now().Add(-time.Hour).Unix(), j.ID)
		j, _ = v.st.Job(j.ID)
		return j
	}
	renamed := func(dl string) []sonarr.QueueItem {
		return []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Title: "Show.S03.renamed", DownloadID: dl, Status: "completed"}}
	}
	run := func(v *env, q []sonarr.QueueItem) { v.e.reviveLost(context.Background(), v.inst, v.c, q) }
	grab := []map[string]any{{"eventType": "grabbed", "sourceTitle": rel, "downloadId": "ABC"}}

	// exact title: no history needed
	v := setup(t, []int{1, 2, 3})
	lose(v, "")
	run(v, []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Title: rel, DownloadID: "ABC"}})
	if n := v.fake.historyCalls.Load(); n != 0 {
		t.Fatalf("an exact title match made %d history calls", n)
	}

	// the download the job itself recorded: no history needed, even under a rewritten title
	v = setup(t, []int{1, 2, 3})
	j := lose(v, "ABC")
	run(v, renamed("abc"))
	if n := v.fake.historyCalls.Load(); n != 0 || v.status(t, j.ID).Status != "downloading" {
		t.Fatalf("own binding: %d history calls, status %s", n, v.status(t, j.ID).Status)
	}

	// a season with a later job can never revive this one: no history, however often it is asked
	v = setup(t, []int{1, 2, 3})
	old := lose(v, "")
	v.job(t, time.Time{})
	v.fake.history = grab
	for i := 0; i < 3; i++ {
		run(v, renamed("ABC"))
	}
	if n := v.fake.historyCalls.Load(); n != 0 || v.status(t, old.ID).Status != "lost" {
		t.Fatalf("later job: %d history calls, status %s", n, v.status(t, old.ID).Status)
	}

	// a Sonarr error leaves the job lost and is retried on the next pass
	v = setup(t, []int{1, 2, 3})
	j = lose(v, "")
	v.fake.history, v.fake.failHistory = grab, true
	run(v, renamed("ABC"))
	if v.status(t, j.ID).Status != "lost" {
		t.Fatal("a history error must not revive")
	}
	v.fake.failHistory = false
	run(v, renamed("ABC"))
	if v.status(t, j.ID).Status != "downloading" {
		t.Fatalf("the next pass must succeed, got %s", v.status(t, j.ID).Status)
	}

	// one history request per season per pass, and no more than the cap overall
	v = setup(t, []int{1, 2, 3})
	for s := 1; s <= maxHistoryPerPass+5; s++ {
		id := lose(v, "").ID
		_, _ = v.st.Exec(`update jobs set series_id=?, season=3 where id=?`, 100+s, id)
	}
	var q []sonarr.QueueItem
	for s := 1; s <= maxHistoryPerPass+5; s++ {
		q = append(q, sonarr.QueueItem{SeriesID: 100 + s, SeasonNumber: 3, Title: "x", DownloadID: fmt.Sprint("D", s)})
	}
	run(v, q)
	if n := int(v.fake.historyCalls.Load()); n != maxHistoryPerPass {
		t.Fatalf("expected the cap of %d history calls, got %d", maxHistoryPerPass, n)
	}
}

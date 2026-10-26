package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/qbit"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// fakeQbit is qBittorrent behind qui's proxy: one torrent, its files, and a log of what was changed.
type fakeQbit struct {
	srv      *httptest.Server
	mu       sync.Mutex
	torrent  map[string]any // nil = not in qBittorrent
	files    []string
	cats     map[string]bool
	changes  []string
	filesErr bool
}

func newFakeQbit(t *testing.T, category string, progress float64, files ...string) *fakeQbit {
	f := &fakeQbit{files: files, cats: map[string]bool{"btn": true, "seed-btn": true},
		torrent: map[string]any{"hash": "d", "category": category, "progress": progress, "content_path": "/dl/pack", "added_on": time.Now().Unix()}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.torrent == nil {
			w.Write([]byte(`[]`))
			return
		}
		_ = json.NewEncoder(w).Encode([]any{f.torrent})
	})
	mux.HandleFunc("/api/v2/torrents/files", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, n := range f.files {
			out = append(out, map[string]any{"name": n, "size": 1, "progress": 1})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/v2/torrents/categories", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[string]any{}
		for c := range f.cats {
			out[c] = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/v2/torrents/createCategory", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.cats[r.Form.Get("category")] = true
		f.changes = append(f.changes, "create:"+r.Form.Get("category"))
		f.mu.Unlock()
	})
	mux.HandleFunc("/api/v2/torrents/setCategory", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.torrent["category"] = r.Form.Get("category")
		f.changes = append(f.changes, "park:"+r.Form.Get("category"))
		f.mu.Unlock()
	})
	mux.HandleFunc("/api/v2/torrents/stop", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.changes = append(f.changes, "stop")
		f.mu.Unlock()
	})
	mux.HandleFunc("/api/v2/torrents/delete", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.torrent = nil
		f.changes = append(f.changes, "remove:"+r.Form.Get("deleteFiles"))
		f.mu.Unlock()
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeQbit) client() *qbit.Client { return qbit.New(f.srv.URL, "", "") }
func (f *fakeQbit) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.changes, " ")
}

// reviewEnv: a season 3 with three episodes of known titles, and a review job bound to the download "d".
func reviewEnv(t *testing.T, q *fakeQbit) (*env, store.Job) {
	v := setup(t, []int{1, 2, 3})
	v.e.Qbit = q.client()
	v.fake.episodes = []map[string]any{
		{"id": 601, "seasonNumber": 3, "episodeNumber": 1, "title": "Down Bad"},
		{"id": 602, "seasonNumber": 3, "episodeNumber": 2, "title": "Calculating"},
		{"id": 603, "seasonNumber": 3, "episodeNumber": 3, "title": "Correctness"},
		{"id": 501, "seasonNumber": 1, "episodeNumber": 1, "title": "Mother and Children"},
		{"id": 502, "seasonNumber": 1, "episodeNumber": 2, "title": "A Quiet Morning"}, {"id": 503, "seasonNumber": 1, "episodeNumber": 3, "title": "The Long Walk Home"}}
	j := reviewJob(t, v, time.Hour)
	_, _ = v.st.Exec(`update jobs set status='downloading' where id=?`, j.ID)
	j, _ = v.st.Job(j.ID)
	return v, j
}

func TestAReviewPackIsParkedOutOfTheArrsCategoryAndWaitsForAHuman(t *testing.T) {
	q := newFakeQbit(t, "btn", 1, "Show.S03/Show.S03E01.mkv", "Show.S03/Show.S03E02.mkv", "Show.S03/Show.S03E03.mkv")
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	got := v.status(t, j.ID)
	if got.Status != "holding" || !strings.Contains(got.Message, "awaiting your review") || !strings.Contains(got.Message, "/dl/pack") ||
		!strings.Contains(got.Message, "say it is this season") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
	if l := q.log(); !strings.Contains(l, "create:consolidarr-review") || !strings.Contains(l, "park:consolidarr-review") || strings.Contains(l, "seed-btn") {
		t.Fatalf("qBittorrent changes: %q", l)
	}
	if len(v.fake.deleted) != 0 || len(v.fake.removed) != 0 {
		t.Fatal("your library and Sonarr's queue must be untouched")
	}
}

// The Tenchi case: a pack of another season is thrown away as soon as its file list exists, before it finishes.
func TestAPackThatIsAnotherSeasonIsDiscardedWhileStillDownloading(t *testing.T) {
	q := newFakeQbit(t, "btn", 0.2, "Pack/Show S01 - 01 - Mother and Children.mkv.!qB", "Pack/Show S01 - 02 - A Quiet Morning.mkv.!qB", "Pack/Show S01 - 03 - The Long Walk Home.mkv.!qB")
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	got := v.status(t, j.ID)
	if got.Status != "failed" || !strings.Contains(got.Message, "not this season") || !strings.Contains(q.log(), "park:seed-btn") {
		t.Fatalf("got %s %q, qbit %q", got.Status, got.Message, q.log())
	}
	if !v.st.Skipped(skipKey(1, 1, 3)) || !v.st.Skipped("dub:"+skipKey(1, 1, 3)) {
		t.Fatal("the season must be left alone so the next wrong cour is not fetched")
	}
}

func TestAReviewPackStillDownloadingKeepsDownloadingAndNamesNothingUnknownYet(t *testing.T) {
	q := newFakeQbit(t, "consolidarr-review", 0.4, "p/01.mkv", "p/02.mkv", "p/03.mkv")
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "downloading" || got.Progress != 40 {
		t.Fatalf("got %s %d%%", got.Status, got.Progress)
	}
	if strings.Contains(q.log(), "park") {
		t.Fatal("already in the review category: nothing to move")
	}
	q.mu.Lock()
	q.torrent["progress"] = 1.0
	q.mu.Unlock()
	j, _ = v.st.Job(j.ID)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "holding" || !strings.Contains(got.Message, "could not settle which season") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
}

func TestAReviewPackRemovedFromQbittorrentByHandIsDone(t *testing.T) {
	q := newFakeQbit(t, "consolidarr-review", 1, "p/01.mkv", "p/02.mkv", "p/03.mkv")
	v, j := reviewEnv(t, q)
	q.mu.Lock()
	q.torrent = nil
	q.mu.Unlock()
	for i := 0; i < reviewGoneChecks-1; i++ { // one empty answer can be a proxy hiccup: not yet
		v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
		if got := v.status(t, j.ID); got.Status == "done" {
			t.Fatalf("closed after %d empty answers", i+1)
		}
	}
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "done" {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
	if !v.st.Skipped(skipKey(1, 1, 3)) {
		t.Fatal("left alone for a week afterwards")
	}
}

func TestRelabelledPacksAreNotFetchedWithoutQbittorrentAccess(t *testing.T) {
	grabPause = 0
	v := setup(t, []int{1, 2, 3})
	pack := dualRelease("ab", "[GRP] Show S01 [Blu-ray][1080p][Dual Audio]", "Japanese")
	pack["indexer"], pack["seasonNumber"] = "AnimeBytes (Prowlarr)", 1
	pack["publishDate"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	v.fake.releases = []map[string]any{pack}
	v.fake.episodes = []map[string]any{{"id": 601, "seasonNumber": 3, "episodeNumber": 1, "airDateUtc": time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)}}
	seedDubSeason(t, v, 0)
	set := live()
	set.SchedulerEnabled, set.DualAudio, set.RelabelAnimeBytes = true, true, true
	cycleOnce(v, set) // v.e.Qbit is nil
	if v.fake.grabs != 0 {
		t.Fatal("without CONSOLIDARR_QBIT_URL a relabelled pack could not be parked, so it must not be fetched")
	}
}

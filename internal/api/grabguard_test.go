package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/store"
)

type rel = map[string]any

func release(guid, title string, res int, source string, rejections ...string) rel {
	if rejections == nil {
		rejections = []string{}
	}
	return rel{"guid": guid, "indexerId": 7, "indexer": "BTN", "title": title, "size": 20e9, "seeders": 5, "fullSeason": true,
		"seasonNumber": 1, "quality": rel{"quality": rel{"name": source, "source": source, "resolution": res}}, "rejected": len(rejections) > 0, "rejections": rejections}
}

// grabEnv is an API with one instance, one season whose files are 1080p bluray, and a Sonarr that lists three releases.
func grabEnv(t *testing.T) (http.Handler, *int, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	grabbed := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/release", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]rel{
			release("good", "Show.S01.2160p.BluRay-GRP", 2160, "bluray"),
			release("worse", "Show.S01.720p.WEB-GRP", 720, "web"),
			release("blocked", "Show.S01.1080p.BluRay-BAD", 1080, "bluray", "Release is blocklisted"),
		})
	})
	mux.HandleFunc("GET /api/v3/episodefile", func(w http.ResponseWriter, r *http.Request) {
		// the season's current file is 1080p bluray: the fresh rank the guard compares against comes from here
		_ = json.NewEncoder(w).Encode([]rel{{"id": 1, "seriesId": 1, "seasonNumber": 1, "quality": rel{"quality": rel{"name": "Bluray-1080p", "source": "bluray", "resolution": 1080}}}})
	})
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rel{"records": []any{}})
	})
	mux.HandleFunc("POST /api/v3/release", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		grabbed++
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a, h := newAPI(t)
	a.AuthDisabled = true
	id, err := a.St.SaveInstance(store.Instance{Name: "t", URL: srv.URL, APIKey: "k", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.St.ReplaceSeasons(id, 1, []audit.Season{{InstID: int(id), SeriesID: 1, Season: 1, Title: "Show", Files: 5, Single: 5, CurRank: 0}}); err != nil { // the stored rank is stale (0): the guard must use the live files, not this
		t.Fatal(err)
	}
	return h, &grabbed, &mu
}

func grabReq(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/seasons/1/1/1/grab", strings.NewReader(body))
	r.Host = "consolidarr:7480"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGrabOnlyAcceptsListedNonDowngradesUnlessForced(t *testing.T) {
	h, grabbed, mu := grabEnv(t)
	count := func() int { mu.Lock(); defer mu.Unlock(); return *grabbed }

	if w := grabReq(h, `{"guid":"made-up-guid","indexerId":1}`); w.Code != 409 || count() != 0 {
		t.Fatalf("a guid Sonarr does not list must be refused: %d %s grabbed=%d", w.Code, w.Body.String(), count())
	}
	if w := grabReq(h, `{"guid":"worse"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "needs force") || count() != 0 {
		t.Fatalf("a lower-quality pack needs force: %d %s grabbed=%d", w.Code, w.Body.String(), count())
	}
	if w := grabReq(h, `{"guid":"blocked"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "needs force") || count() != 0 {
		t.Fatalf("a blocked pack needs force: %d %s grabbed=%d", w.Code, w.Body.String(), count())
	}
	if w := grabReq(h, `{"guid":"good","title":"attacker chosen title","sizeGB":0.001}`); w.Code != 201 || count() != 1 {
		t.Fatalf("a listed, equal-or-better pack is allowed: %d %s grabbed=%d", w.Code, w.Body.String(), count())
	}
}

func TestForcedGrabOfADowngradeUsesSonarrsRecordNotTheRequest(t *testing.T) {
	h, grabbed, mu := grabEnv(t)
	w := grabReq(h, `{"guid":"worse","force":true,"title":"attacker chosen title","sizeGB":0.001,"indexer":"evil"}`)
	if w.Code != 201 {
		t.Fatalf("force allows it: %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	n := *grabbed
	mu.Unlock()
	if n != 1 {
		t.Fatalf("grabbed=%d", n)
	}
	r := httptest.NewRequest("GET", "/api/jobs", nil)
	r.Host = "consolidarr:7480"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	body := rec.Body.String()
	if strings.Contains(body, "attacker chosen title") || strings.Contains(body, "evil") || !strings.Contains(body, "Show.S01.720p.WEB-GRP") {
		t.Fatalf("title, size and indexer must come from Sonarr's record: %s", body)
	}
}

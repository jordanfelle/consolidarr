package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/engine"
	"github.com/jordanfelle/consolidarr/internal/store"
)

func newAPI(t *testing.T) (*API, http.Handler) {
	t.Helper()
	st, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	a := &API{St: st, Eng: engine.New(st), Version: "test"}
	return a, a.Handler()
}

func do(h http.Handler, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEverythingRequiresLoginExceptAuthAndHealth(t *testing.T) {
	_, h := newAPI(t)
	for _, p := range []string{"/api/instances", "/api/seasons", "/api/jobs", "/api/settings", "/api/events", "/api/status"} {
		if w := do(h, "GET", p, ""); w.Code != 401 {
			t.Errorf("%s: want 401 unauthenticated, got %d", p, w.Code)
		}
	}
	if w := do(h, "POST", "/api/instances/1/scan", ""); w.Code != 401 {
		t.Errorf("scan must require login, got %d", w.Code)
	}
	if w := do(h, "GET", "/healthz", ""); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
}

func TestSetupOnceThenLoginAndSessions(t *testing.T) {
	_, h := newAPI(t)
	if w := do(h, "POST", "/api/auth/setup", `{"username":"a","password":"short"}`); w.Code != 400 {
		t.Fatalf("short password must be rejected, got %d", w.Code)
	}
	w := do(h, "POST", "/api/auth/setup", `{"username":"admin","password":"correct-horse"}`)
	if w.Code != 200 || len(w.Result().Cookies()) == 0 {
		t.Fatalf("setup: %d", w.Code)
	}
	if w := do(h, "POST", "/api/auth/setup", `{"username":"x","password":"another-password"}`); w.Code != 409 {
		t.Fatalf("setup must only work once, got %d", w.Code)
	}
	if w := do(h, "POST", "/api/auth/login", `{"username":"admin","password":"wrong-password"}`); w.Code != 401 {
		t.Fatalf("bad password: %d", w.Code)
	}
	l := do(h, "POST", "/api/auth/login", `{"username":"admin","password":"correct-horse"}`)
	if l.Code != 200 {
		t.Fatalf("login: %d", l.Code)
	}
	cookie := l.Result().Cookies()[0]
	if w := do(h, "GET", "/api/settings", "", cookie); w.Code != 200 {
		t.Fatalf("authed request: %d", w.Code)
	}
	do(h, "POST", "/api/auth/logout", "", cookie)
	if w := do(h, "GET", "/api/settings", "", cookie); w.Code != 401 {
		t.Fatalf("session must die on logout, got %d", w.Code)
	}
}

func TestAPIKeysNeverLeaveTheServer(t *testing.T) {
	a, h := newAPI(t)
	if _, err := a.St.SaveInstance(store.Instance{Name: "S", URL: "http://s", APIKey: "super-secret-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	w := do(h, "POST", "/api/auth/setup", `{"username":"admin","password":"correct-horse"}`)
	c := w.Result().Cookies()[0]
	got := do(h, "GET", "/api/instances", "", c)
	if got.Code != 200 || strings.Contains(got.Body.String(), "super-secret-key") || strings.Contains(strings.ToLower(got.Body.String()), "apikey\":\"s") {
		t.Fatalf("api key leaked: %s", got.Body.String())
	}
}

func TestAuthDisabledMode(t *testing.T) {
	a, h := newAPI(t)
	a.AuthDisabled = true
	if w := do(h, "GET", "/api/settings", ""); w.Code != 200 {
		t.Fatalf("auth-disabled mode must let requests through, got %d", w.Code)
	}
}

func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := do(h, "POST", "/api/auth/setup", `{"username":"admin","password":"correct-horse"}`)
	if w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	return w.Result().Cookies()[0]
}

func req(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAPIKeysAuthenticateAndCanBeRevoked(t *testing.T) {
	_, h := newAPI(t)
	c := login(t, h)
	w := do(h, "POST", "/api/keys", `{"name":"ci"}`, c)
	if w.Code != 201 {
		t.Fatalf("create key: %d %s", w.Code, w.Body)
	}
	var k struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &k)
	if !strings.HasPrefix(k.Key, "csl_") {
		t.Fatalf("unexpected key %q", k.Key)
	}
	for name, hdr := range map[string]map[string]string{
		"X-API-Key":     {"X-API-Key": k.Key},
		"Authorization": {"Authorization": "Bearer " + k.Key},
	} {
		if got := req(h, "GET", "/api/settings", "", hdr); got.Code != 200 {
			t.Errorf("%s: want 200 with a valid key, got %d", name, got.Code)
		}
	}
	if got := req(h, "GET", "/api/settings", "", map[string]string{"X-API-Key": "csl_nope"}); got.Code != 401 {
		t.Errorf("bogus key must be 401, got %d", got.Code)
	}
	// the list never reveals the secret or its hash
	list := do(h, "GET", "/api/keys", "", c).Body.String()
	if strings.Contains(list, k.Key) || strings.Contains(list, "hash") || !strings.Contains(list, `"name":"ci"`) {
		t.Fatalf("key list leaks or is wrong: %s", list)
	}
	if got := do(h, "DELETE", "/api/keys/"+strconv.FormatInt(k.ID, 10), "", c); got.Code != 200 {
		t.Fatalf("revoke: %d", got.Code)
	}
	if got := req(h, "GET", "/api/settings", "", map[string]string{"X-API-Key": k.Key}); got.Code != 401 {
		t.Fatalf("revoked key must stop working, got %d", got.Code)
	}
	if got := do(h, "DELETE", "/api/keys/9999", "", c); got.Code != 404 {
		t.Fatalf("revoking a missing key: %d", got.Code)
	}
}

func TestOpenAPIDocumentsEveryRoute(t *testing.T) {
	a, h := newAPI(t)
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(do(h, "GET", "/api/openapi.json", "").Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	have := map[string]bool{}
	for p, ops := range spec.Paths {
		for m := range ops {
			have[strings.ToUpper(m)+" "+p] = true
		}
	}
	for _, rt := range a.Routes() {
		if !have[rt.Method+" "+rt.Path] {
			t.Errorf("route %s %s is not documented in openapi.json", rt.Method, rt.Path)
		}
		delete(have, rt.Method+" "+rt.Path)
	}
	for extra := range have {
		t.Errorf("openapi.json documents %s, which is not a registered route", extra)
	}
}

func TestErrorsAreUniform(t *testing.T) {
	_, h := newAPI(t)
	c := login(t, h)
	for _, tc := range []struct {
		method, path, body string
		want               int
		code               string
	}{
		{"GET", "/api/instances/999", "", 404, "not_found"},
		{"POST", "/api/instances", `{"name":"x"}`, 422, "invalid"},
		{"PUT", "/api/settings", `{"nonsense":1}`, 400, "bad_request"},
		{"PUT", "/api/settings", `{"maxInflight":0}`, 422, "invalid"},
		{"GET", "/api/seasons?sort=bogus", "", 422, "invalid"},
		{"GET", "/api/jobs/12345", "", 404, "not_found"},
		{"POST", "/api/keys", `{"name":""}`, 422, "invalid"},
	} {
		w := do(h, tc.method, tc.path, tc.body, c)
		var e struct{ Error, Code string }
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != tc.want || e.Code != tc.code || e.Error == "" {
			t.Errorf("%s %s: want %d/%s with a message, got %d %s", tc.method, tc.path, tc.want, tc.code, w.Code, w.Body)
		}
	}
	if w := do(h, "GET", "/api/jobs", ""); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("401 must carry WWW-Authenticate, got %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}

func TestSeasonsPaginationAndSort(t *testing.T) {
	a, h := newAPI(t)
	c := login(t, h)
	var rows []audit.Season
	for i := 1; i <= 5; i++ {
		rows = append(rows, audit.Season{InstID: 1, SeriesID: i, Season: 1, Title: string(rune('A' + 5 - i)), SeriesType: "standard",
			Files: 5 + i, Aired: 5 + i, Groups: []string{"a"}, Qualities: []string{"q"}, Sources: []string{"web"}, GB: float64(i)})
	}
	if err := a.St.ReplaceSeasons(1, 0, rows); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Total, Offset, Limit int
		Seasons              []audit.Season
	}
	get := func(q string) {
		page = struct {
			Total, Offset, Limit int
			Seasons              []audit.Season
		}{}
		w := do(h, "GET", "/api/seasons?"+q, "", c)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
		_ = json.Unmarshal(w.Body.Bytes(), &page)
	}
	get("limit=2&offset=1&sort=title")
	if page.Total != 5 || page.Offset != 1 || page.Limit != 2 || len(page.Seasons) != 2 || page.Seasons[0].Title != "B" {
		t.Fatalf("pagination/sort wrong: %+v", page)
	}
	get("sort=files")
	if page.Seasons[0].Files != 10 {
		t.Fatalf("sort by files should put the biggest first: %+v", page.Seasons[0])
	}
}

func TestRunNowRefusesWhileSchedulerIsOff(t *testing.T) {
	a, h := newAPI(t)
	c := login(t, h)
	if w := do(h, "POST", "/api/engine/run", "", c); w.Code != 409 || !strings.Contains(w.Body.String(), "conflict") {
		t.Fatalf("scheduler off must be a 409, got %d %s", w.Code, w.Body)
	}
	set := a.St.Settings()
	set.SchedulerEnabled = true
	_ = a.St.SaveSettings(set)
	if w := do(h, "POST", "/api/engine/run", "", c); w.Code != 202 {
		t.Fatalf("scheduler on must be accepted, got %d %s", w.Code, w.Body)
	}
}

func TestOneOffRunAPI(t *testing.T) {
	a, h := newAPI(t)
	c := login(t, h)
	if w := do(h, "POST", "/api/engine/once", `{"limit":3}`, c); w.Code != 422 {
		t.Fatalf("no instance configured must be 422, got %d %s", w.Code, w.Body)
	}
	_, _ = a.St.SaveInstance(store.Instance{Name: "S", URL: "http://s", APIKey: "k", Enabled: true})
	for _, bad := range []string{`{"limit":0}`, `{"limit":501}`, `{}`, `{"limit":3,"extra":1}`} {
		if w := do(h, "POST", "/api/engine/once", bad, c); w.Code != 422 && w.Code != 400 {
			t.Errorf("%s must be rejected, got %d", bad, w.Code)
		}
	}
	if w := do(h, "POST", "/api/engine/once", `{"limit":3}`, c); w.Code != 202 || !strings.Contains(w.Body.String(), `"active":true`) {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/api/engine/once", `{"limit":3}`, c); w.Code != 409 {
		t.Fatalf("a second run must be 409, got %d", w.Code)
	}
	if w := do(h, "GET", "/api/status", "", c); !strings.Contains(w.Body.String(), `"once":{"active":true,"limit":3`) {
		t.Fatalf("status must report the run: %s", w.Body)
	}
	if w := do(h, "POST", "/api/engine/once/cancel", "", c); w.Code != 200 {
		t.Fatalf("cancel: %d", w.Code)
	}
}

func TestPackSizeLimitCanBeTurnedOff(t *testing.T) {
	_, h := newAPI(t)
	c := login(t, h)
	if w := do(h, "PUT", "/api/settings", `{"maxPackGB":0}`, c); w.Code != 200 || !strings.Contains(w.Body.String(), `"maxPackGB":0`) {
		t.Fatalf("0 (no limit) must be accepted: %d %s", w.Code, w.Body)
	}
	if w := do(h, "PUT", "/api/settings", `{"maxPackGB":-1}`, c); w.Code != 422 {
		t.Fatalf("negative must be rejected: %d", w.Code)
	}
}

func fakeSonarrIndexers(names ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for i, n := range names {
			out = append(out, map[string]any{"id": i + 1, "name": n, "protocol": "torrent", "enableInteractiveSearch": n != "Disabled (Prowlarr)"})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func TestIndexersAreListedFromSonarrAndDeduplicated(t *testing.T) {
	a, h := newAPI(t)
	c := login(t, h)
	s1 := fakeSonarrIndexers("TorrentLeech (Prowlarr)", "BroadcastheNet (Prowlarr)")
	s2 := fakeSonarrIndexers("BroadcastheNet (Prowlarr)", "Disabled (Prowlarr)")
	defer s1.Close()
	defer s2.Close()
	_, _ = a.St.SaveInstance(store.Instance{Name: "Main", URL: s1.URL, APIKey: "k", Enabled: true})
	_, _ = a.St.SaveInstance(store.Instance{Name: "4K", URL: s2.URL, APIKey: "k", Enabled: true})
	_, _ = a.St.SaveInstance(store.Instance{Name: "Off", URL: "http://127.0.0.1:1", APIKey: "k", Enabled: false})
	w := do(h, "GET", "/api/indexers", "", c)
	var got struct {
		Indexers []struct {
			Name       string
			Searchable bool
			Instances  []int
		}
		Errors []string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
	names := []string{}
	for _, i := range got.Indexers {
		names = append(names, i.Name)
		if i.Name == "BroadcastheNet (Prowlarr)" && len(i.Instances) != 2 {
			t.Errorf("shared indexer must list both instances, got %v", i.Instances)
		}
		if i.Name == "Disabled (Prowlarr)" && i.Searchable {
			t.Error("an indexer with interactive search off must not be marked searchable")
		}
	}
	if strings.Join(names, "|") != "BroadcastheNet (Prowlarr)|Disabled (Prowlarr)|TorrentLeech (Prowlarr)" {
		t.Fatalf("want 3 unique sorted names, got %v", names)
	}
	if len(got.Errors) != 0 {
		t.Fatalf("a disabled instance must be skipped silently, got %v", got.Errors)
	}
}

func TestIndexersToleratesAnUnreachableInstance(t *testing.T) {
	a, h := newAPI(t)
	c := login(t, h)
	ok1 := fakeSonarrIndexers("A (Prowlarr)")
	defer ok1.Close()
	_, _ = a.St.SaveInstance(store.Instance{Name: "Good", URL: ok1.URL, APIKey: "k", Enabled: true})
	_, _ = a.St.SaveInstance(store.Instance{Name: "Down", URL: "http://127.0.0.1:1", APIKey: "k", Enabled: true})
	w := do(h, "GET", "/api/indexers", "", c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "A (Prowlarr)") || !strings.Contains(w.Body.String(), "Down:") {
		t.Fatalf("partial results with the failure reported expected: %d %s", w.Code, w.Body)
	}
}

func TestInstanceURLsAreValidated(t *testing.T) {
	_, h := newAPI(t)
	c := login(t, h)
	for _, u := range []string{"http://169.254.169.254/latest/meta-data", "file:///etc/passwd", "http://u:p@host", "ftp://host"} {
		body := `{"name":"x","url":"` + u + `","apiKey":"k"}`
		if w := do(h, "POST", "/api/instances", body, c); w.Code != 422 {
			t.Errorf("save %s must be 422, got %d %s", u, w.Code, w.Body)
		}
		if w := do(h, "POST", "/api/instances/test", body, c); w.Code != 422 {
			t.Errorf("test %s must be 422, got %d %s", u, w.Code, w.Body)
		}
	}
	if w := do(h, "POST", "/api/instances", `{"name":"ok","url":"http://sonarr:8989/sonarr","apiKey":"k"}`, c); w.Code != 201 {
		t.Errorf("a normal URL must still be accepted, got %d %s", w.Code, w.Body)
	}
}

func TestMetricsEndpointShapeAndRangeValidation(t *testing.T) {
	a, h := newAPI(t)
	c := login(t, h)
	_ = a.St.ReplaceSeasons(1, 0, []audit.Season{{InstID: 1, SeriesID: 1, Season: 1, Title: "Show", SeriesType: "standard", Files: 8, Aired: 8, Single: 8,
		Groups: []string{"a", "b", "c"}, Qualities: []string{"q"}, Sources: []string{"web"}, GB: 2}})
	_ = a.St.RecordSnapshot(3)
	w := do(h, "GET", "/api/metrics?range=30d", "", c)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var m struct {
		Range     string
		Current   struct{ FlaggedTV, Files int }
		Snapshots []struct{ TS int64 }
		PerDay    []struct{ Day string }
		Top       []struct {
			Title  string
			Groups int
		}
		Totals struct{ Done, Inflight int }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m.Range != "30d" || m.Current.FlaggedTV != 1 || m.Current.Files != 8 ||
		len(m.Snapshots) != 1 || len(m.Top) != 1 || m.Top[0].Groups != 3 {
		t.Fatalf("unexpected metrics %s (err=%v)", w.Body, err)
	}
	if !strings.Contains(w.Body.String(), `"perDay":[]`) {
		t.Fatalf("empty lists must serialise as [] not null: %s", w.Body)
	}
	if w := do(h, "GET", "/api/metrics?range=forever", "", c); w.Code != 422 {
		t.Fatalf("unknown range must be 422, got %d", w.Code)
	}
}

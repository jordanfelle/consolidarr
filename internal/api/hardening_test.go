package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jordanfelle/consolidarr/internal/store"
)

func send(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "consolidarr.lan:7480"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCrossOriginWritesAreRefusedEvenWithAuthDisabled(t *testing.T) {
	a, h := newAPI(t)
	a.AuthDisabled = true
	body := `{"enabled":true,"cycleMinutes":5}`
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"hostile origin", map[string]string{"Origin": "https://evil.example"}, 403},
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site (sibling subdomain)", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"null origin", map[string]string{"Origin": "null"}, 403},
		{"same origin", map[string]string{"Origin": "http://consolidarr.lan:7480", "Sec-Fetch-Site": "same-origin"}, 200},
		{"proxied origin", map[string]string{"Origin": "https://obs.example", "X-Forwarded-Host": "obs.example"}, 200},
		{"script, no browser headers", nil, 200},
		{"api key header is not ambient", map[string]string{"Origin": "https://evil.example", "X-API-Key": "csl_whatever"}, 200},
	}
	for _, c := range cases {
		w := send(h, "PUT", "/api/settings", body, c.hdr)
		got := w.Code
		if c.want == 200 && got != 200 && got != 400 && got != 422 { // reached the handler
			t.Errorf("%s: want to reach the handler, got %d", c.name, got)
		}
		if c.want == 403 && got != 403 {
			t.Errorf("%s: want 403, got %d", c.name, got)
		}
	}
	if w := send(h, "GET", "/api/settings", "", map[string]string{"Origin": "https://evil.example"}); w.Code != 200 {
		t.Errorf("reads are not blocked by the origin guard: %d", w.Code)
	}
	_ = a
}

func TestAllowedHostsBlockRebinding(t *testing.T) {
	a, h := newAPI(t)
	a.AllowedHosts = ParseAllowedHosts("consolidarr.lan, 10.0.0.35")
	if w := send(h, "GET", "/api/me", "", nil); w.Code == 421 {
		t.Error("an allowed host (with port) must pass")
	}
	r := httptest.NewRequest("GET", "/api/auth/me", nil)
	r.Host = "attacker.example:7480"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 421 {
		t.Errorf("unknown host must get 421, got %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/healthz", nil)
	r.Host = "whatever"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("healthz must work for container health checks, got %d", w.Code)
	}
}

func TestStoredKeyIsNotSentToANewURL(t *testing.T) {
	a, h := newAPI(t)
	a.AuthDisabled = true
	id, _ := a.St.SaveInstance(store.Instance{Name: "s", URL: "http://sonarr:8989", APIKey: "real", Enabled: true})
	idp := "/api/instances/" + itoa(id)
	// changing the URL without resending a key must not carry the stored key along
	if w := send(h, "PUT", idp, `{"name":"s","url":"http://other:8989"}`, nil); w.Code != 422 {
		t.Errorf("save with new URL and no key: want 422, got %d %s", w.Code, w.Body.String())
	}
	if w := send(h, "POST", "/api/instances/test", `{"id":`+itoa(id)+`,"url":"http://127.0.0.1:1"}`, nil); w.Code != 422 {
		t.Errorf("test with new URL and no key: want 422, got %d", w.Code)
	}
	// the same URL (trailing slash aside) keeps the key
	if w := send(h, "PUT", idp, `{"name":"renamed","url":"http://sonarr:8989/"}`, nil); w.Code != 200 {
		t.Errorf("rename keeps the key: got %d %s", w.Code, w.Body.String())
	}
	if got, _ := a.St.Instance(id); got.APIKey != "real" || got.Name != "renamed" {
		t.Errorf("key lost or rename dropped: %+v", got)
	}
	for _, u := range []string{"http://sonarr:8989/?a=1", "http://sonarr:8989/#x"} {
		if w := send(h, "PUT", idp, `{"name":"s","url":"`+u+`","apiKey":"k"}`, nil); w.Code != 422 {
			t.Errorf("%s must be rejected, got %d", u, w.Code)
		}
	}
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func TestSetupPasswordLimitsAndRace(t *testing.T) {
	_, h := newAPI(t)
	long := strings.Repeat("a", 73)
	if w := send(h, "POST", "/api/auth/setup", `{"username":"u","password":"`+long+`"}`, nil); w.Code != 400 {
		t.Errorf("73-byte password: want 400, got %d", w.Code)
	}
	if w := send(h, "POST", "/api/auth/setup", `{"username":"u","password":"`+strings.Repeat("a", 72)+`"}`, nil); w.Code != 200 {
		t.Errorf("72-byte password is fine: got %d %s", w.Code, w.Body.String())
	}
	if w := send(h, "POST", "/api/auth/setup", `{"username":"v","password":"longenough1"}`, nil); w.Code != 409 {
		t.Errorf("second setup: want 409, got %d", w.Code)
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	_, h := newAPI(t)
	old := loginDelay
	loginDelay = 0
	defer func() { loginDelay = old }()
	send(h, "POST", "/api/auth/setup", `{"username":"admin","password":"correct-horse"}`, nil)
	for i := 0; i < loginPerUser; i++ {
		if w := send(h, "POST", "/api/auth/login", `{"username":"admin","password":"nope"}`, nil); w.Code != 401 {
			t.Fatalf("attempt %d: want 401, got %d", i, w.Code)
		}
	}
	if w := send(h, "POST", "/api/auth/login", `{"username":"admin","password":"correct-horse"}`, nil); w.Code != 429 {
		t.Errorf("after %d failures even the right password waits: got %d", loginPerUser, w.Code)
	}
	if w := send(h, "POST", "/api/auth/login", `{"username":"Admin ","password":"x"}`, nil); w.Code != 429 {
		t.Errorf("username case/space variants share the budget: got %d", w.Code)
	}
}

func TestSwaggerAssetsArePinnedWithSRI(t *testing.T) {
	_, h := newAPI(t)
	body := send(h, "GET", "/api/docs", "", nil).Body.String()
	if strings.Count(body, `integrity="sha384-`) != 2 || !strings.Contains(body, `crossorigin="anonymous"`) {
		t.Errorf("both swagger assets need an integrity hash:\n%s", body)
	}
}

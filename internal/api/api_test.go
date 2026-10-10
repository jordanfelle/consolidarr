package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

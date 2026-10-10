package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func cookieFrom(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cs := w.Result().Cookies()
	if len(cs) == 0 {
		t.Fatalf("no Set-Cookie header (status %d): %s", w.Code, w.Body)
	}
	return cs[0]
}

func TestSessionCookieAttributes(t *testing.T) {
	setupBody := `{"username":"admin","password":"correct-horse"}`
	cases := []struct {
		name       string
		hdr        map[string]string
		mode       string
		wantSecure bool
	}{
		{"plain HTTP on the LAN stays usable", nil, "", false},
		{"HTTPS via a proxy is Secure", map[string]string{"X-Forwarded-Proto": "https"}, "", true},
		{"forwarded proto is case-insensitive", map[string]string{"X-Forwarded-Proto": "HTTPS"}, "", true},
		{"explicit http forwarded proto is not Secure", map[string]string{"X-Forwarded-Proto": "http"}, "", false},
		{"always overrides detection", nil, "always", true},
		{"never overrides detection", map[string]string{"X-Forwarded-Proto": "https"}, "never", false},
	}
	for _, c := range cases {
		a, h := newAPI(t)
		a.CookieSecure = c.mode
		w := req(h, "POST", "/api/auth/setup", setupBody, c.hdr)
		ck := cookieFrom(t, w)
		if ck.Secure != c.wantSecure {
			t.Errorf("%s: Secure=%v want %v", c.name, ck.Secure, c.wantSecure)
		}
		if !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" {
			t.Errorf("%s: session cookie must always be HttpOnly, SameSite=Lax, Path=/: %+v", c.name, ck)
		}
	}
}

func TestLoginAndLogoutCookiesCarryTheSameFlags(t *testing.T) {
	a, h := newAPI(t)
	login(t, h)
	hdr := map[string]string{"X-Forwarded-Proto": "https"}
	l := req(h, "POST", "/api/auth/login", `{"username":"admin","password":"correct-horse"}`, hdr)
	lc := cookieFrom(t, l)
	if !lc.Secure || !lc.HttpOnly {
		t.Fatalf("login over HTTPS must be Secure+HttpOnly: %+v", lc)
	}
	r := httptest.NewRequest("POST", "/api/auth/logout", strings.NewReader(""))
	r.Header.Set("X-Forwarded-Proto", "https")
	r.AddCookie(lc)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	oc := cookieFrom(t, w)
	if oc.Value != "" || oc.MaxAge >= 0 || !oc.HttpOnly || !oc.Secure {
		t.Fatalf("the clearing cookie must expire the session and keep HttpOnly+Secure: %+v", oc)
	}
	_ = a
}

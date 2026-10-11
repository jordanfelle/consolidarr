package api

import (
	"errors"
	"golang.org/x/crypto/bcrypt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jordanfelle/consolidarr/internal/store"
)

func send(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "consolidarr:7480"
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
		{"same origin", map[string]string{"Origin": "http://consolidarr:7480", "Sec-Fetch-Site": "same-origin"}, 200},
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
	a.AllowedHosts = ParseAllowedHosts("consolidarr, 10.0.0.35")
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

func TestRebindingNameIsRefusedWhenLoginIsOff(t *testing.T) {
	a, h := newAPI(t)
	a.AuthDisabled = true
	get := func(host string) int {
		r := httptest.NewRequest("GET", "/api/instances", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, host := range []string{"10.0.0.35:7480", "localhost:7480", "prometheus:7480", "[::1]:7480"} {
		if c := get(host); c != 200 {
			t.Errorf("%s must work with login off, got %d", host, c)
		}
	}
	for _, host := range []string{"evil.example", "evil.example:7480", "a.b.c"} {
		if c := get(host); c != 421 {
			t.Errorf("%s is a dotted name: want 421, got %d", host, c)
		}
	}
	a.AllowedHosts = ParseAllowedHosts("obs.example.com")
	if c := get("obs.example.com"); c != 200 {
		t.Errorf("an explicitly allowed dotted name must work, got %d", c)
	}
	a.AllowedHosts = nil
	a.AuthDisabled = false
	if c := get("evil.example"); c != 401 {
		t.Errorf("with login on the host is not restricted (401 from auth), got %d", c)
	}
}

func TestOriginComparesPortAndTrustedOrigins(t *testing.T) {
	a, h := newAPI(t)
	a.AuthDisabled = true
	w := send(h, "PUT", "/api/settings", `{}`, map[string]string{"Origin": "http://consolidarr:9999"})
	if w.Code != 403 {
		t.Errorf("same host, other port is another service: want 403, got %d", w.Code)
	}
	a.TrustedOrigins = ParseAllowedHosts("public.example.com")
	w = send(h, "PUT", "/api/settings", `{}`, map[string]string{"Origin": "https://public.example.com"})
	if w.Code == 403 {
		t.Error("a trusted origin must be accepted when the proxy rewrites Host")
	}
}

func TestConcurrentLoginGuessesShareTheBudget(t *testing.T) {
	_, h := newAPI(t)
	old := loginDelay
	loginDelay = 0
	defer func() { loginDelay = old }()
	send(h, "POST", "/api/auth/setup", `{"username":"admin","password":"correct-horse"}`, nil)
	var wg sync.WaitGroup
	var limited, wrong atomic.Int32
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch send(h, "POST", "/api/auth/login", `{"username":"admin","password":"nope"}`, nil).Code {
			case 429:
				limited.Add(1)
			case 401:
				wrong.Add(1)
			}
		}()
	}
	wg.Wait()
	if wrong.Load() > loginPerUser {
		t.Errorf("a parallel burst got %d guesses through; the budget is %d", wrong.Load(), loginPerUser)
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
	for i := 0; i < loginPerUserAddr; i++ {
		if w := send(h, "POST", "/api/auth/login", `{"username":"admin","password":"nope"}`, nil); w.Code != 401 {
			t.Fatalf("attempt %d: want 401, got %d", i, w.Code)
		}
	}
	if w := send(h, "POST", "/api/auth/login", `{"username":"admin","password":"correct-horse"}`, nil); w.Code != 429 {
		t.Errorf("after %d failures even the right password waits: got %d", loginPerUserAddr, w.Code)
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

// One host hammering the admin's username must lock out only itself, not the admin signing in from elsewhere.
func TestAStrangerCannotLockTheAdminOut(t *testing.T) {
	a, h := newAPI(t)
	old := loginDelay
	loginDelay = 0
	defer func() { loginDelay = old }()
	_ = a
	send(h, "POST", "/api/auth/setup", `{"username":"admin","password":"correct-horse"}`, nil)
	from := func(addr, pw string) int {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+pw+`"}`))
		r.Host, r.RemoteAddr = "consolidarr:7480", addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < loginPerUserAddr+5; i++ {
		from("203.0.113.9:1111", "guess")
	}
	if got := from("203.0.113.9:1111", "correct-horse"); got != 429 {
		t.Fatalf("the attacker's own address stays locked, got %d", got)
	}
	if got := from("198.51.100.4:2222", "correct-horse"); got != 200 {
		t.Fatalf("the admin on another address must still be able to sign in, got %d", got)
	}
}

func TestSetupAndKeyCreationAreRefusedWhileLoginIsOff(t *testing.T) {
	a, h := newAPI(t)
	a.AuthDisabled = true
	if w := send(h, "POST", "/api/auth/setup", `{"username":"x","password":"longenough1"}`, nil); w.Code != 409 {
		t.Errorf("setup with login off must be refused, got %d", w.Code)
	}
	if w := send(h, "POST", "/api/keys", `{"name":"k"}`, nil); w.Code != 409 {
		t.Errorf("minting a key with login off must be refused, got %d", w.Code)
	}
}

func TestSessionsAreStoredHashedAndLogoutAllEndsThem(t *testing.T) {
	a, h := newAPI(t)
	send(h, "POST", "/api/auth/setup", `{"username":"admin","password":"correct-horse"}`, nil)
	w := send(h, "POST", "/api/auth/login", `{"username":"admin","password":"correct-horse"}`, nil)
	var raw string
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			raw = c.Value
		}
	}
	if raw == "" {
		t.Fatal("no session cookie")
	}
	var n int
	_ = a.St.QueryRow(`select count(*) from sessions where token=?`, raw).Scan(&n)
	if n != 0 {
		t.Fatal("the raw token must not be in the database")
	}
	cookie := map[string]string{"Cookie": cookieName + "=" + raw}
	if w := send(h, "GET", "/api/settings", "", cookie); w.Code != 200 {
		t.Fatalf("the hashed session must still authenticate, got %d", w.Code)
	}
	if w := send(h, "POST", "/api/auth/logout-all", "", cookie); w.Code != 200 {
		t.Fatalf("logout-all: %d", w.Code)
	}
	if w := send(h, "GET", "/api/settings", "", cookie); w.Code != 401 {
		t.Fatalf("every session must be dead after logout-all, got %d", w.Code)
	}
}

func TestSecurityHeadersAndCrossSiteGETs(t *testing.T) {
	a, h := newAPI(t)
	a.AuthDisabled = true
	w := send(h, "GET", "/api/status", "", nil)
	if w.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("framing/sniffing headers missing: %v", w.Header())
	}
	if w := send(h, "GET", "/api/status", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
		t.Errorf("a cross-site GET to the API must be refused, got %d", w.Code)
	}
	if w := send(h, "GET", "/api/status", "", map[string]string{"Sec-Fetch-Site": "same-origin"}); w.Code != 200 {
		t.Errorf("same-origin GET must work, got %d", w.Code)
	}
	if w := send(h, "GET", "/", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code == 403 {
		t.Errorf("following a link to the UI from another site must still work")
	}
}

func TestTestErrorDoesNotEchoRemoteBodies(t *testing.T) {
	for in, banned := range map[string]string{
		`sonarr GET /system/status: 404 {"secret":"internal-service-data"}`: "internal-service-data",
		`sonarr GET /system/status: 200 <html>admin panel</html>`:           "admin panel",
	} {
		if got := testError(errors.New(in)); strings.Contains(got, banned) || got == "" {
			t.Errorf("%q leaked: %q", in, got)
		}
	}
	if got := testError(errors.New("dial tcp 10.0.0.9:8989: connect: connection refused")); !strings.Contains(got, "connection refused") {
		t.Errorf("network errors stay informative: %q", got)
	}
}

// The per-address budget decides who is refused first; the per-user ceiling still bounds guessing spread over addresses.
func TestPerUserCeilingBoundsGuessingAcrossAddresses(t *testing.T) {
	a, h := newAPI(t)
	old := loginDelay
	loginDelay = 0
	defer func() { loginDelay = old }()
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-horse"), bcrypt.MinCost) // cheap hash: this test makes ~100 real attempts
	if _, err := a.St.CreateFirstUser("admin", string(hash)); err != nil {
		t.Fatal(err)
	}
	from := func(addr string) int {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"admin","password":"guess"}`))
		r.Host, r.RemoteAddr = "consolidarr:7480", addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	refused := 0
	for i := 0; i < loginPerUser/loginPerUserAddr+5; i++ { // more addresses than the ceiling allows, each within its own budget
		for j := 0; j < loginPerUserAddr; j++ {
			if from("198.51.100."+strconv.Itoa(i+1)+":1") == 429 {
				refused++
			}
		}
	}
	if refused == 0 {
		t.Fatal("guessing spread across many addresses must hit the per-user ceiling")
	}
	if got := from("203.0.113.77:1"); got != 429 {
		t.Fatalf("once the ceiling is reached a fresh address is refused too, got %d", got)
	}
}

func TestPublicPagesOpenFromAnotherSite(t *testing.T) {
	a, h := newAPI(t)
	a.AuthDisabled = true
	for _, p := range []string{"/api/docs", "/api/openapi.json", "/api/auth/me"} {
		if w := send(h, "GET", p, "", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 200 {
			t.Errorf("%s should open from a link on another site, got %d", p, w.Code)
		}
	}
}

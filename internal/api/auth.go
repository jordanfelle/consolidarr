package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jordanfelle/consolidarr/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const cookieName = "consolidarr_session"

func (a *API) userCount() int {
	var n int
	_ = a.St.QueryRow(`select count(*) from users`).Scan(&n)
	return n
}

// apiKey extracts a key from `X-API-Key` or `Authorization: Bearer`.
func apiKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func (a *API) authed(r *http.Request) bool {
	if a.AuthDisabled {
		return true
	}
	if k := apiKey(r); k != "" {
		return a.St.KeyValid(k)
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	var exp int64
	if a.St.QueryRow(`select expires from sessions where token=?`, c.Value).Scan(&exp) != nil {
		return false
	}
	return exp > time.Now().Unix()
}

// secureRequest reports whether the client reached us over HTTPS, directly or through a TLS-terminating
// proxy (Cloudflare, nginx). The header can be forged by a client, but it only decides the Secure flag of a
// cookie sent back to that same client, so spoofing it gains nothing.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionCookie builds the session cookie. It is always HttpOnly and SameSite=Lax. Secure follows the
// connection: set whenever the request came over HTTPS, so it works unchanged behind Cloudflare Access, but
// not forced on plain-HTTP LAN use where browsers would silently drop a Secure cookie and break login.
// CONSOLIDARR_COOKIE_SECURE=always|never overrides the detection.
func (a *API) sessionCookie(r *http.Request, value string) *http.Cookie {
	c := &http.Cookie{Name: cookieName, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
	switch strings.ToLower(a.CookieSecure) {
	case "always", "true":
		c.Secure = true
	case "never", "false":
	default:
		if secureRequest(r) {
			c.Secure = true
		}
	}
	return c
}

func (a *API) startSession(w http.ResponseWriter, r *http.Request, uid int64) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	exp := time.Now().Add(30 * 24 * time.Hour)
	_, _ = a.St.Exec(`insert into sessions(token,user_id,expires) values(?,?,?)`, tok, uid, exp.Unix())
	c := a.sessionCookie(r, tok)
	c.Expires = exp
	http.SetCookie(w, c)
}

type creds struct{ Username, Password string }

// bcrypt silently ignores everything past 72 bytes (and newer versions refuse such input), so longer passwords
// are rejected up front instead of being quietly truncated.
const maxPassword = 72

// loginDelay is the pause after a failed login (a variable so tests need not wait).
var loginDelay = 500 * time.Millisecond

// dummyHash is compared against when the username does not exist, so a miss costs the same bcrypt time as a
// wrong password and does not reveal which usernames exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("consolidarr-dummy-password"), bcrypt.DefaultCost)

func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	if a.userCount() > 0 {
		fail(w, 409, "already set up")
		return
	}
	var c creds
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&c) != nil || strings.TrimSpace(c.Username) == "" || len(c.Password) < 8 {
		fail(w, 400, "username required and password must be at least 8 characters")
		return
	}
	if len(c.Password) > maxPassword {
		fail(w, 400, "password must be at most 72 bytes")
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "could not hash password")
		return
	}
	id, err := a.St.CreateFirstUser(c.Username, string(h))
	if errors.Is(err, store.ErrSetupDone) {
		fail(w, 409, "already set up")
		return
	}
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.startSession(w, r, id)
	ok(w, map[string]any{"ok": true})
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var c creds
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&c)
	user, addr := strings.ToLower(strings.TrimSpace(c.Username)), remoteAddr(r)
	if a.limiter.blocked(user, addr) {
		w.Header().Set("Retry-After", "600")
		fail(w, 429, "too many failed logins; try again in a few minutes")
		return
	}
	var id int64
	h := string(dummyHash)
	found := a.St.QueryRow(`select id,pw_hash from users where username=?`, c.Username).Scan(&id, &h) == nil
	if bcrypt.CompareHashAndPassword([]byte(h), []byte(c.Password)) != nil || !found {
		a.limiter.fail(user, addr)
		time.Sleep(loginDelay)
		fail(w, 401, "invalid credentials")
		return
	}
	a.limiter.ok(user)
	a.startSession(w, r, id)
	ok(w, map[string]any{"ok": true})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_, _ = a.St.Exec(`delete from sessions where token=?`, c.Value)
	}
	c := a.sessionCookie(r, "")
	c.MaxAge = -1
	http.SetCookie(w, c)
	ok(w, map[string]any{"ok": true})
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"setupRequired": a.userCount() == 0 && !a.AuthDisabled, "authenticated": a.authed(r), "version": a.Version})
}

func (a *API) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="consolidarr"`)
			fail(w, 401, "unauthorized: log in or send a valid API key in X-API-Key")
			return
		}
		h(w, r)
	}
}

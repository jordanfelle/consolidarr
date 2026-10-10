package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const cookieName = "consolidarr_session"

func (a *API) userCount() int {
	var n int
	_ = a.St.QueryRow(`select count(*) from users`).Scan(&n)
	return n
}

func (a *API) authed(r *http.Request) bool {
	if a.AuthDisabled {
		return true
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

func (a *API) startSession(w http.ResponseWriter, uid int64) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	exp := time.Now().Add(30 * 24 * time.Hour)
	_, _ = a.St.Exec(`insert into sessions(token,user_id,expires) values(?,?,?)`, tok, uid, exp.Unix())
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", Expires: exp, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

type creds struct{ Username, Password string }

func (a *API) setup(w http.ResponseWriter, r *http.Request) {
	if a.userCount() > 0 {
		fail(w, 409, "already set up")
		return
	}
	var c creds
	if json.NewDecoder(r.Body).Decode(&c) != nil || strings.TrimSpace(c.Username) == "" || len(c.Password) < 8 {
		fail(w, 400, "username required and password must be at least 8 characters")
		return
	}
	h, _ := bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
	var id int64
	if err := a.St.QueryRow(`insert into users(username,pw_hash) values(?,?) returning id`, c.Username, string(h)).Scan(&id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	a.startSession(w, id)
	ok(w, map[string]any{"ok": true})
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var c creds
	_ = json.NewDecoder(r.Body).Decode(&c)
	var id int64
	var h string
	if a.St.QueryRow(`select id,pw_hash from users where username=?`, c.Username).Scan(&id, &h) != nil ||
		bcrypt.CompareHashAndPassword([]byte(h), []byte(c.Password)) != nil {
		time.Sleep(500 * time.Millisecond)
		fail(w, 401, "invalid credentials")
		return
	}
	a.startSession(w, id)
	ok(w, map[string]any{"ok": true})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_, _ = a.St.Exec(`delete from sessions where token=?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	ok(w, map[string]any{"ok": true})
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"setupRequired": a.userCount() == 0 && !a.AuthDisabled, "authenticated": a.authed(r), "version": a.Version})
}

func (a *API) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			fail(w, 401, "unauthorized")
			return
		}
		h(w, r)
	}
}

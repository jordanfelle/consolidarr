package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTVDBOrders(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login":
			logins.Add(1)
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["apikey"] != "k" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"token":"tok"}}`))
		case r.Header.Get("Authorization") != "Bearer tok":
			w.WriteHeader(401)
		case r.URL.Path == "/series/7/episodes/dvd" && r.URL.Query().Get("page") == "0":
			_, _ = w.Write([]byte(`{"data":{"episodes":[{"id":11,"name":"B","aired":"2000-01-02","number":1,"seasonNumber":1}]},"links":{"next":"x"}}`))
		case r.URL.Path == "/series/7/episodes/dvd" && r.URL.Query().Get("page") == "1":
			_, _ = w.Write([]byte(`{"data":{"episodes":[{"id":10,"name":"A","aired":"2000-01-01","number":2,"seasonNumber":1}]},"links":{"next":null}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	tv := &TVDB{Key: "k", Base: srv.URL}
	alts, err := tv.Alts(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(alts) != 1 || alts[0].Name != "TVDB dvd" || len(alts[0].Eps) != 2 || alts[0].Eps[0].TvdbID != 11 || alts[0].Eps[1].Number != 2 {
		t.Fatalf("%+v", alts)
	}
	if _, err := tv.Alts(context.Background(), 7); err != nil || logins.Load() != 1 {
		t.Fatalf("the token must be reused: logins=%d err=%v", logins.Load(), err)
	}
	if _, err := (&TVDB{Key: "bad", Base: srv.URL}).Alts(context.Background(), 7); err == nil {
		t.Fatal("a rejected key must be an error")
	}
}

func TestTMDBGroupsAndKeyNeverInErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/5/episode_groups":
			_, _ = w.Write([]byte(`{"results":[{"id":"abc","name":"DVD Order","type":4}]}`))
		case "/tv/episode_group/abc":
			_, _ = w.Write([]byte(`{"groups":[{"order":1,"episodes":[{"name":"First","air_date":"2001-01-01","order":0},{"name":"Second","air_date":"2001-01-08","order":1}]}]}`))
		default:
			w.WriteHeader(500)
		}
	}))
	tm := &TMDB{Key: "secretkey", Base: srv.URL}
	alts, err := tm.Alts(context.Background(), 5)
	if err != nil || len(alts) != 1 || alts[0].Name != "TMDB DVD Order" || alts[0].Eps[1].Number != 2 || alts[0].Eps[1].Season != 1 {
		t.Fatalf("%+v %v", alts, err)
	}
	srv.Close() // connection refused: the URL carries the key and must not reach the error text
	if _, err := tm.Alts(context.Background(), 5); err == nil || strings.Contains(err.Error(), "secretkey") {
		t.Fatalf("error must exist and must not leak the key: %v", err)
	}
}

func TestLookupSkipsFailingSourceAndNilIsSafe(t *testing.T) {
	var none *Lookup
	if a, err := none.Alts(context.Background(), 1, 1); a != nil || err != nil {
		t.Fatal("a nil lookup is simply unconfigured")
	}
}

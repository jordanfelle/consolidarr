package qbit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInfoFilesAndPark(t *testing.T) {
	var posted []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hashes") == "abc" {
			w.Write([]byte(`[{"hash":"abc","category":"btn","progress":0.5,"content_path":"/d/x"}]`))
			return
		}
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/api/v2/torrents/files", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"name":"x/E01.mkv","size":5,"progress":1}]`))
	})
	mux.HandleFunc("/api/v2/torrents/categories", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"btn":{}}`)) })
	for _, p := range []string{"setCategory", "createCategory"} {
		p := p
		mux.HandleFunc("/api/v2/torrents/"+p, func(w http.ResponseWriter, r *http.Request) {
			r.ParseForm()
			posted = append(posted, p+"?"+r.Form.Encode())
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL+"/", "", "")
	ctx := context.Background()
	if tr, err := c.Info(ctx, "ABC"); err != nil || tr.Category != "btn" || tr.ContentPath != "/d/x" {
		t.Fatalf("%+v %v", tr, err)
	}
	if _, err := c.Info(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("err = %v", err)
	}
	if fs, err := c.Files(ctx, "ABC"); err != nil || len(fs) != 1 || fs[0].Name != "x/E01.mkv" {
		t.Fatalf("%+v %v", fs, err)
	}
	if err := c.EnsureCategory(ctx, "btn"); err != nil || len(posted) != 0 {
		t.Fatalf("existing category must be left alone: %v %v", err, posted)
	}
	_ = c.EnsureCategory(ctx, "review")
	_ = c.Park(ctx, "ABC", "review")
	if !c.HasCategory(ctx, "btn") || c.HasCategory(ctx, "seed-btn") {
		t.Fatal("HasCategory")
	}
	got := strings.Join(posted, " ")
	for _, want := range []string{"createCategory?category=review", "setCategory?category=review&hashes=abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

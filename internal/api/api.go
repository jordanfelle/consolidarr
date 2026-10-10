// Package api serves the JSON API and the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jordanfelle/consolidarr/internal/audit"
	"github.com/jordanfelle/consolidarr/internal/engine"
	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
)

type API struct {
	St           *store.Store
	Eng          *engine.Engine
	Web          fs.FS // built SPA (may be nil in dev)
	AuthDisabled bool
	Version      string
}

func ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (a *API) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/auth/me", a.me)
	m.HandleFunc("POST /api/auth/setup", a.setup)
	m.HandleFunc("POST /api/auth/login", a.login)
	m.HandleFunc("POST /api/auth/logout", a.logout)

	m.HandleFunc("GET /api/instances", a.guard(a.listInstances))
	m.HandleFunc("POST /api/instances", a.guard(a.saveInstance))
	m.HandleFunc("PUT /api/instances/{id}", a.guard(a.saveInstance))
	m.HandleFunc("DELETE /api/instances/{id}", a.guard(a.deleteInstance))
	m.HandleFunc("POST /api/instances/test", a.guard(a.testInstance))
	m.HandleFunc("POST /api/instances/{id}/scan", a.guard(a.scan))

	m.HandleFunc("GET /api/seasons", a.guard(a.seasons))
	m.HandleFunc("GET /api/seasons/{inst}/{series}/{season}/candidates", a.guard(a.candidates))
	m.HandleFunc("POST /api/seasons/{inst}/{series}/{season}/grab", a.guard(a.grab))

	m.HandleFunc("GET /api/jobs", a.guard(a.jobs))
	m.HandleFunc("POST /api/jobs/{id}/approve", a.guard(a.approve))
	m.HandleFunc("DELETE /api/jobs/{id}", a.guard(a.deleteJob))

	m.HandleFunc("GET /api/settings", a.guard(func(w http.ResponseWriter, r *http.Request) { ok(w, a.St.Settings()) }))
	m.HandleFunc("PUT /api/settings", a.guard(a.putSettings))
	m.HandleFunc("GET /api/events", a.guard(a.events))
	m.HandleFunc("GET /api/status", a.guard(a.status))
	m.HandleFunc("POST /api/engine/run", a.guard(func(w http.ResponseWriter, r *http.Request) { a.Eng.RunNow(); ok(w, map[string]bool{"ok": true}) }))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { ok(w, map[string]string{"status": "ok"}) })

	if a.Web != nil {
		m.Handle("/", spa(a.Web))
	}
	return m
}

// spa serves static files and falls back to index.html for client-side routes.
func spa(root fs.FS) http.Handler {
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(root, p); err != nil {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

func (a *API) listInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := a.St.Instances()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	type view struct {
		store.Instance
		Scanning bool   `json:"scanning"`
		LastScan int64  `json:"lastScan"`
		Health   string `json:"health"`
	}
	out := []view{}
	for _, i := range insts {
		i.APIKey = "" // never send keys back to the browser
		ls, _ := strconv.ParseInt(a.St.KV("lastScan:"+strconv.FormatInt(i.ID, 10)), 10, 64)
		out = append(out, view{i, a.Eng.Scanning(i.ID), ls, a.Eng.HealthOf(i.ID)})
	}
	ok(w, out)
}

func (a *API) saveInstance(w http.ResponseWriter, r *http.Request) {
	var i store.Instance
	if json.NewDecoder(r.Body).Decode(&i) != nil || i.Name == "" || i.URL == "" {
		fail(w, 400, "name and url required")
		return
	}
	if id := r.PathValue("id"); id != "" {
		i.ID, _ = strconv.ParseInt(id, 10, 64)
	} else if i.APIKey == "" {
		fail(w, 400, "apiKey required")
		return
	}
	id, err := a.St.SaveInstance(i)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]int64{"id": id})
}

func (a *API) deleteInstance(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.St.DeleteInstance(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (a *API) testInstance(w http.ResponseWriter, r *http.Request) {
	var i store.Instance
	_ = json.NewDecoder(r.Body).Decode(&i)
	if i.APIKey == "" && i.ID != 0 {
		if old, err := a.St.Instance(i.ID); err == nil {
			i.APIKey = old.APIKey
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	v, err := sonarr.New(i.URL, i.APIKey).Status(ctx)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	ok(w, map[string]string{"version": v})
}

func (a *API) scan(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	inst, err := a.St.Instance(id)
	if err != nil {
		fail(w, 404, "no such instance")
		return
	}
	if a.Eng.Scanning(id) {
		fail(w, 409, "scan already running")
		return
	}
	go func() {
		if err := a.Eng.ScanInstance(context.Background(), inst); err != nil {
			a.St.Event("error", "scan "+inst.Name+": "+err.Error())
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	ok(w, map[string]bool{"started": true})
}

func (a *API) seasons(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	inst, _ := strconv.ParseInt(qv.Get("inst"), 10, 64)
	set := a.St.Settings()
	minFiles, _ := strconv.Atoi(qv.Get("minFiles"))
	if minFiles == 0 {
		minFiles = set.MinFiles
	}
	anime := qv.Get("anime")
	if anime == "" && !set.IncludeAnime {
		anime = "hide"
	}
	rows, err := a.St.Seasons(store.SeasonQuery{InstID: inst, FlaggedOnly: qv.Get("flagged") != "0", Anime: anime, Search: qv.Get("q"), MinFiles: minFiles})
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	sortSeasons(rows)
	total := len(rows)
	if lim, _ := strconv.Atoi(qv.Get("limit")); lim > 0 && len(rows) > lim {
		rows = rows[:lim]
	}
	ok(w, map[string]any{"total": total, "seasons": rows})
}

func (a *API) lookup(r *http.Request) (store.Instance, audit.Season, error) {
	iid, _ := strconv.ParseInt(r.PathValue("inst"), 10, 64)
	sid, _ := strconv.Atoi(r.PathValue("series"))
	sn, _ := strconv.Atoi(r.PathValue("season"))
	inst, err := a.St.Instance(iid)
	if err != nil {
		return inst, audit.Season{}, err
	}
	rows, err := a.St.Seasons(store.SeasonQuery{InstID: iid, MinFiles: 1})
	if err != nil {
		return inst, audit.Season{}, err
	}
	for _, s := range rows {
		if s.SeriesID == sid && s.Season == sn {
			return inst, s, nil
		}
	}
	return inst, audit.Season{}, store.ErrNotFound
}

func (a *API) candidates(w http.ResponseWriter, r *http.Request) {
	inst, s, err := a.lookup(r)
	if err != nil {
		fail(w, 404, "season not found")
		return
	}
	c, err := a.Eng.Candidates(r.Context(), inst, s)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	ok(w, c)
}

func (a *API) grab(w http.ResponseWriter, r *http.Request) {
	inst, s, err := a.lookup(r)
	if err != nil {
		fail(w, 404, "season not found")
		return
	}
	var b struct {
		GUID      string  `json:"guid"`
		IndexerID int     `json:"indexerId"`
		Title     string  `json:"title"`
		SizeGB    float64 `json:"sizeGB"`
		Indexer   string  `json:"indexer"`
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil || b.GUID == "" {
		fail(w, 400, "guid required")
		return
	}
	id, err := a.Eng.Grab(r.Context(), inst, s, b.GUID, b.IndexerID, b.Title, b.SizeGB, b.Indexer)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	ok(w, map[string]int64{"jobId": id})
}

func (a *API) jobs(w http.ResponseWriter, r *http.Request) {
	js, err := a.St.Jobs(r.URL.Query().Get("active") == "1", 500)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, js)
}

func (a *API) approve(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	js, _ := a.St.Jobs(false, 5000)
	for _, j := range js {
		if j.ID != id {
			continue
		}
		if j.Status != "ready" {
			fail(w, 409, "only verified 'ready' jobs can be approved")
			return
		}
		inst, err := a.St.Instance(j.InstID)
		if err != nil {
			fail(w, 404, "instance gone")
			return
		}
		if err := a.Eng.Approve(r.Context(), inst, j); err != nil {
			fail(w, 502, err.Error())
			return
		}
		ok(w, map[string]bool{"ok": true})
		return
	}
	fail(w, 404, "no such job")
}

func (a *API) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.St.DeleteJob(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	s := a.St.Settings()
	if json.NewDecoder(r.Body).Decode(&s) != nil {
		fail(w, 400, "bad json")
		return
	}
	s.MaxInflight, s.CycleMinutes, s.ScanHours, s.MinFiles = max(s.MaxInflight, 1), max(s.CycleMinutes, 1), max(s.ScanHours, 1), max(s.MinFiles, 2)
	if err := a.St.SaveSettings(s); err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, s)
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	ev, err := a.St.Events(300)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, ev)
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	act, _ := a.St.Jobs(true, 1000)
	all, _ := a.St.Seasons(store.SeasonQuery{FlaggedOnly: true, MinFiles: a.St.Settings().MinFiles, Anime: "hide"})
	ani, _ := a.St.Seasons(store.SeasonQuery{FlaggedOnly: true, MinFiles: a.St.Settings().MinFiles, Anime: "only"})
	ok(w, map[string]any{"active": len(act), "flaggedTV": len(all), "flaggedAnime": len(ani), "settings": a.St.Settings()})
}

func sortSeasons(rows []audit.Season) {
	sort.SliceStable(rows, func(i, j int) bool {
		ai, bj := rows[i].SeriesType == "anime", rows[j].SeriesType == "anime"
		if ai != bj {
			return !ai
		}
		return rows[i].Score > rows[j].Score
	})
}

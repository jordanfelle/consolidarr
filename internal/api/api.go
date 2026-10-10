// Package api serves the JSON API and the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"io"
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

func created(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v)
}

var codes = map[int]string{400: "bad_request", 401: "unauthorized", 404: "not_found", 409: "conflict", 422: "invalid", 502: "upstream_error", 500: "internal"}

// fail writes the one error shape every endpoint uses: {"error": "...", "code": "..."}.
func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	c := codes[code]
	if c == "" {
		c = "error"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": c})
}

// decode reads a JSON body (max 1 MiB), rejecting unknown fields so typos are not silently ignored.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

type route struct {
	Method, Path string
	H            http.HandlerFunc
	Public       bool
}

// Routes is the single source of truth for the API surface; openapi.json must document every entry
// (enforced by a test).
func (a *API) Routes() []route {
	return []route{
		{"GET", "/api/auth/me", a.me, true},
		{"POST", "/api/auth/setup", a.setup, true},
		{"POST", "/api/auth/login", a.login, true},
		{"POST", "/api/auth/logout", a.logout, true},
		{"GET", "/healthz", func(w http.ResponseWriter, r *http.Request) { ok(w, map[string]string{"status": "ok"}) }, true},
		{"GET", "/api/openapi.json", a.openapi, true},
		{"GET", "/api/docs", a.docs, true},

		{"GET", "/api/instances", a.listInstances, false},
		{"POST", "/api/instances", a.saveInstance, false},
		{"POST", "/api/instances/test", a.testInstance, false},
		{"GET", "/api/instances/{id}", a.getInstance, false},
		{"PUT", "/api/instances/{id}", a.saveInstance, false},
		{"DELETE", "/api/instances/{id}", a.deleteInstance, false},
		{"POST", "/api/instances/{id}/scan", a.scan, false},
		{"POST", "/api/scan", a.scanAll, false},

		{"GET", "/api/seasons", a.seasons, false},
		{"GET", "/api/seasons/{inst}/{series}/{season}", a.season, false},
		{"GET", "/api/seasons/{inst}/{series}/{season}/candidates", a.candidates, false},
		{"POST", "/api/seasons/{inst}/{series}/{season}/grab", a.grab, false},

		{"GET", "/api/jobs", a.jobs, false},
		{"GET", "/api/jobs/{id}", a.job, false},
		{"POST", "/api/jobs/{id}/approve", a.approve, false},
		{"DELETE", "/api/jobs/{id}", a.deleteJob, false},

		{"GET", "/api/settings", func(w http.ResponseWriter, r *http.Request) { ok(w, a.St.Settings()) }, false},
		{"PUT", "/api/settings", a.putSettings, false},
		{"GET", "/api/events", a.events, false},
		{"GET", "/api/status", a.status, false},
		{"POST", "/api/engine/run", a.runNow, false},

		{"GET", "/api/keys", a.listKeys, false},
		{"POST", "/api/keys", a.createKey, false},
		{"DELETE", "/api/keys/{id}", a.deleteKey, false},
	}
}

func (a *API) Handler() http.Handler {
	m := http.NewServeMux()
	for _, rt := range a.Routes() {
		h := rt.H
		if !rt.Public {
			h = a.guard(h)
		}
		m.HandleFunc(rt.Method+" "+rt.Path, h)
	}
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

type instanceView struct {
	store.Instance
	Scanning bool   `json:"scanning"`
	LastScan int64  `json:"lastScan"`
	Health   string `json:"health"`
}

func (a *API) listInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := a.St.Instances()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	out := []instanceView{}
	for _, i := range insts {
		i.APIKey = "" // never send keys back to the browser
		ls, _ := strconv.ParseInt(a.St.KV("lastScan:"+strconv.FormatInt(i.ID, 10)), 10, 64)
		out = append(out, instanceView{i, a.Eng.Scanning(i.ID), ls, a.Eng.HealthOf(i.ID)})
	}
	ok(w, out)
}

// instanceInput is the writable subset of an instance (read-only fields like health are ignored by design).
type instanceInput struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	APIKey   string `json:"apiKey"`
	PathFrom string `json:"pathFrom"`
	PathTo   string `json:"pathTo"`
	Enabled  *bool  `json:"enabled"`
}

func (a *API) saveInstance(w http.ResponseWriter, r *http.Request) {
	var in instanceInput
	raw, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if json.Unmarshal(raw, &in) != nil { // lenient: the UI echoes back server-computed fields on edit
		fail(w, 400, "invalid JSON body")
		return
	}
	if strings.TrimSpace(in.Name) == "" || !strings.HasPrefix(in.URL, "http") {
		fail(w, 422, "name and an http(s) url are required")
		return
	}
	i := store.Instance{Name: in.Name, URL: strings.TrimRight(in.URL, "/"), APIKey: in.APIKey, PathFrom: in.PathFrom, PathTo: in.PathTo, Enabled: in.Enabled == nil || *in.Enabled}
	isNew := r.PathValue("id") == ""
	if !isNew {
		i.ID, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
		if _, err := a.St.Instance(i.ID); err != nil {
			fail(w, 404, "no such instance")
			return
		}
	} else if i.APIKey == "" {
		fail(w, 422, "apiKey required")
		return
	}
	id, err := a.St.SaveInstance(i)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if isNew {
		created(w, map[string]int64{"id": id})
		return
	}
	ok(w, map[string]int64{"id": id})
}

func (a *API) getInstance(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	i, err := a.St.Instance(id)
	if err != nil {
		fail(w, 404, "no such instance")
		return
	}
	i.APIKey = ""
	ls, _ := strconv.ParseInt(a.St.KV("lastScan:"+r.PathValue("id")), 10, 64)
	ok(w, instanceView{i, a.Eng.Scanning(id), ls, a.Eng.HealthOf(id)})
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
	anime := qv.Get("anime") // "" = settings default, "all", "only", "hide"
	switch anime {
	case "":
		if !set.IncludeAnime {
			anime = "hide"
		}
	case "all":
		anime = ""
	case "only", "hide":
	default:
		fail(w, 422, "anime must be one of: all, only, hide")
		return
	}
	rows, err := a.St.Seasons(store.SeasonQuery{InstID: inst, FlaggedOnly: qv.Get("flagged") != "0", Anime: anime, Search: qv.Get("q"), MinFiles: minFiles})
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	switch qv.Get("sort") {
	case "", "score":
		sortSeasons(rows)
	case "title":
		sort.SliceStable(rows, func(i, j int) bool { return strings.ToLower(rows[i].Title) < strings.ToLower(rows[j].Title) })
	case "files":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Files > rows[j].Files })
	case "size":
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].GB > rows[j].GB })
	default:
		fail(w, 422, "sort must be one of: score, title, files, size")
		return
	}
	total := len(rows)
	off, _ := strconv.Atoi(qv.Get("offset"))
	lim, _ := strconv.Atoi(qv.Get("limit"))
	if off < 0 || off > total {
		off = 0
	}
	rows = rows[off:]
	if lim <= 0 || lim > 1000 {
		lim = 100
	}
	if len(rows) > lim {
		rows = rows[:lim]
	}
	ok(w, map[string]any{"total": total, "offset": off, "limit": lim, "seasons": rows})
}

func (a *API) season(w http.ResponseWriter, r *http.Request) {
	_, s, err := a.lookup(r)
	if err != nil {
		fail(w, 404, "season not found")
		return
	}
	ok(w, s)
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
	if !decode(w, r, &b) {
		return
	}
	if b.GUID == "" {
		fail(w, 422, "guid required")
		return
	}
	id, err := a.Eng.Grab(r.Context(), inst, s, b.GUID, b.IndexerID, b.Title, b.SizeGB, b.Indexer)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	created(w, map[string]int64{"jobId": id})
}

func (a *API) jobs(w http.ResponseWriter, r *http.Request) {
	js, err := a.St.Jobs(r.URL.Query().Get("active") == "1", 500)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, js)
}

// runNow triggers a scheduler cycle immediately. It refuses (409) while the scheduler is off instead of
// reporting success for something that would do nothing.
func (a *API) runNow(w http.ResponseWriter, r *http.Request) {
	if !a.St.Settings().SchedulerEnabled {
		fail(w, 409, "the scheduler is off: enable it in Settings and save first")
		return
	}
	a.Eng.RunNow()
	w.WriteHeader(http.StatusAccepted)
	ok(w, map[string]bool{"started": true})
}

func (a *API) job(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	js, _ := a.St.Jobs(false, 100000)
	for _, j := range js {
		if j.ID == id {
			ok(w, j)
			return
		}
	}
	fail(w, 404, "no such job")
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
	if !decode(w, r, &s) {
		return
	}
	if s.MaxInflight < 1 || s.MaxInflight > 200 || s.CycleMinutes < 1 || s.ScanHours < 1 || s.MinFiles < 2 || s.MaxPackGB <= 0 {
		fail(w, 422, "maxInflight 1-200, cycleMinutes >= 1, scanHours >= 1, minFiles >= 2 and maxPackGB > 0 are required")
		return
	}
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

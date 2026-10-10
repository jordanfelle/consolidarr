package api

import (
	"context"
	_ "embed"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
)

//go:embed openapi.json
var openapiSpec []byte

func (a *API) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openapiSpec)
}

// docs serves a small interactive reference (Swagger UI from a pinned CDN build) for /api/openapi.json.
func (a *API) docs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>consolidarr API</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css"></head>
<body><div id="ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url:"/api/openapi.json",dom_id:"#ui",persistAuthorization:true})</script>
</body></html>`))
}

func (a *API) scanAll(w http.ResponseWriter, r *http.Request) {
	insts, err := a.St.Instances()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	started := []int64{}
	for _, inst := range insts {
		if !inst.Enabled || a.Eng.Scanning(inst.ID) {
			continue
		}
		started = append(started, inst.ID)
		go func() {
			if err := a.Eng.ScanInstance(context.Background(), inst); err != nil {
				a.St.Event("error", "scan "+inst.Name+": "+err.Error())
			}
		}()
	}
	w.WriteHeader(http.StatusAccepted)
	ok(w, map[string]any{"started": started})
}

func (a *API) listKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := a.St.Keys()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, ks)
}

func (a *API) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 80 {
		fail(w, 422, "name required (max 80 characters)")
		return
	}
	k, plain, err := a.St.CreateKey(strings.TrimSpace(in.Name))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// The plaintext key is returned here and never again.
	created(w, map[string]any{"id": k.ID, "name": k.Name, "prefix": k.Prefix, "key": plain})
}

func (a *API) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.St.DeleteKey(id); err != nil {
		fail(w, 404, "no such key")
		return
	}
	ok(w, map[string]bool{"ok": true})
}

type indexerView struct {
	Name       string  `json:"name"`
	Protocol   string  `json:"protocol"`
	Searchable bool    `json:"searchable"` // enabled for interactive search on at least one instance
	Instances  []int64 `json:"instances"`
}

// indexers lists the indexers configured across every enabled Sonarr instance, de-duplicated by name.
// Names are what releases report as their indexer, so they are what "last resort" matches against.
// An unreachable instance is skipped (reported in `errors`) rather than failing the whole call.
func (a *API) indexers(w http.ResponseWriter, r *http.Request) {
	insts, err := a.St.Instances()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	byName := map[string]*indexerView{}
	errs := []string{}
	for _, inst := range insts {
		if !inst.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		list, err := sonarr.New(inst.URL, inst.APIKey).Indexers(ctx)
		cancel()
		if err != nil {
			errs = append(errs, inst.Name+": "+err.Error())
			continue
		}
		for _, ix := range list {
			v := byName[ix.Name]
			if v == nil {
				v = &indexerView{Name: ix.Name, Protocol: ix.Protocol}
				byName[ix.Name] = v
			}
			v.Searchable = v.Searchable || ix.EnableInteractiveSearch
			v.Instances = append(v.Instances, inst.ID)
		}
	}
	out := make([]indexerView, 0, len(byName))
	for _, v := range byName {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	ok(w, map[string]any{"indexers": out, "errors": errs})
}

package api

import (
	"context"
	_ "embed"
	"net/http"
	"strconv"
	"strings"
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

package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jordanfelle/consolidarr/internal/engine"
	"github.com/jordanfelle/consolidarr/internal/store"
)

// attention lists packs stuck in Sonarr's queue that no active job owns.
func (a *API) attention(w http.ResponseWriter, r *http.Request) {
	groups, errs := a.Eng.Attention(r.Context())
	if groups == nil {
		groups = []engine.Attention{}
	}
	ok(w, map[string]any{"items": groups, "errors": errs})
}

func (a *API) attentionTarget(w http.ResponseWriter, r *http.Request) (store.Instance, int, int, bool) {
	iid, _ := strconv.ParseInt(r.PathValue("inst"), 10, 64)
	sid, _ := strconv.Atoi(r.PathValue("series"))
	sn, _ := strconv.Atoi(r.PathValue("season"))
	inst, err := a.St.Instance(iid)
	if err != nil {
		fail(w, 404, "no such instance")
		return inst, 0, 0, false
	}
	return inst, sid, sn, true
}

func (a *API) adopt(w http.ResponseWriter, r *http.Request) {
	inst, sid, sn, good := a.attentionTarget(w, r)
	if !good {
		return
	}
	j, err := a.Eng.Adopt(r.Context(), inst, sid, sn, r.URL.Query().Get("force") == "1")
	if errors.Is(err, engine.ErrDowngrade) {
		fail(w, 409, err.Error())
		return
	}
	if err == store.ErrNotFound {
		fail(w, 404, "no stuck download for that season (it may already be handled)")
		return
	}
	if err == store.ErrDuplicate {
		fail(w, 409, err.Error())
		return
	}
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	created(w, j)
}

func (a *API) retryImport(w http.ResponseWriter, r *http.Request) {
	inst, sid, sn, good := a.attentionTarget(w, r)
	if !good {
		return
	}
	id, err := a.Eng.RetryImport(r.Context(), inst, sid, sn)
	if err == store.ErrNotFound {
		fail(w, 404, "no stuck download for that season")
		return
	}
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
	ok(w, map[string]int{"commandId": id})
}

func (a *API) removePack(w http.ResponseWriter, r *http.Request) {
	inst, sid, sn, good := a.attentionTarget(w, r)
	if !good {
		return
	}
	var o engine.RemoveOpts
	if !decode(w, r, &o) {
		return
	}
	n, searched, err := a.Eng.RemovePack(r.Context(), inst, sid, sn, o)
	if err == store.ErrNotFound {
		fail(w, 404, "no stuck download for that season")
		return
	}
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	ok(w, map[string]any{"removed": n, "searched": searched})
}

// retryJob re-evaluates a held/failed/lost job right now instead of waiting for the next cycle.
func (a *API) retryJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	j, err := a.Eng.AdvanceJob(r.Context(), id)
	if err == store.ErrNotFound {
		fail(w, 404, "no such job")
		return
	}
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	ok(w, j)
}

// rejectJob discards a job's bad pack (remove, blocklist) and by default searches again.
func (a *API) rejectJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	in := struct {
		Research bool `json:"research"`
	}{Research: true}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	j, err := a.Eng.RejectJob(r.Context(), id, in.Research)
	if err == store.ErrNotFound {
		fail(w, 404, "no such job")
		return
	}
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	ok(w, j)
}

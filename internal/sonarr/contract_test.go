package sonarr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// These tests decode REAL Sonarr responses (testdata/, see its README) through the client. A hand-made fake only
// ever returns the shapes its author thought of; the bug that held every replacement in production was a command whose
// body had a field of another type, which no handmade fake contained.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serve answers every request with the body chosen by the request path (without /api/v3).
func serve(t *testing.T, bodies map[string][]byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/v3")
		b, ok := bodies[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "k")
}

func TestRealCommandListingDecodes(t *testing.T) {
	var want []json.RawMessage
	_ = json.Unmarshal(fixture(t, "command.json"), &want)
	cmds, err := serve(t, map[string][]byte{"/command": fixture(t, "command.json")}).Commands(context.Background())
	if err != nil || len(cmds) != len(want) || len(want) == 0 {
		t.Fatalf("real listing: %d of %d decoded, err=%v", len(cmds), len(want), err)
	}
	for _, c := range cmds {
		if c.ID == 0 || c.Name == "" || c.Status == "" {
			t.Fatalf("a real command lost its basics: %+v", c)
		}
	}
}

func TestEveryKnownCommandShapeDecodesAndYieldsWhatWeUse(t *testing.T) {
	cmds, err := serve(t, map[string][]byte{"/command": fixture(t, "command_exotic.json")}).Commands(context.Background())
	if err != nil || len(cmds) != 9 {
		t.Fatalf("exotic listing: %d decoded, err=%v", len(cmds), err)
	}
	by := map[string]Command{}
	for _, c := range cmds {
		by[c.Name] = c
	}
	if f := by["ManualImport"].Body.Files; len(f) != 1 || f[0] != "/downloads/completed/btn/Pack/ep1.mkv" {
		t.Fatalf("a ManualImport must still yield its file paths: %v", f)
	}
	if len(by["RenameFiles"].Body.Files) != 0 {
		t.Fatalf("numeric file ids are not paths: %v", by["RenameFiles"].Body.Files)
	}
	if b := by["DownloadedEpisodesScan"].Body; b.Path != "/downloads/completed/btn/Pack" || b.DownloadClientID != "ABC" {
		t.Fatalf("a scan keeps its path and client id: %+v", b)
	}
}

// Whatever single field of a command Sonarr sends in an unexpected type, the listing as a whole must still decode:
// at worst that one entry is dropped.
func TestAnySingleFieldOfAnyCommandMayHaveTheWrongTypeWithoutLosingTheListing(t *testing.T) {
	odd := []string{`"text"`, `7`, `true`, `null`, `{}`, `[]`, `[1,2]`, `{"path":3}`}
	var mu sync.Mutex
	var current []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // one server for all variations
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write(current)
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	for _, file := range []string{"command.json", "command_exotic.json"} {
		var list []map[string]any
		if err := json.Unmarshal(fixture(t, file), &list); err != nil {
			t.Fatal(err)
		}
		n := len(list)
		seen := map[string]bool{} // many real commands share a name and shape: one variation of each is enough
		try := func(i int, path []string, v string) {
			key := file + "|" + list[i]["name"].(string) + "|" + strings.Join(path, ".") + "|" + v
			if seen[key] {
				return
			}
			seen[key] = true
			// rebuild the listing with one value replaced
			var cp []map[string]any
			_ = json.Unmarshal(fixture(t, file), &cp)
			cur := cp[i]
			for _, k := range path[:len(path)-1] {
				next, ok := cur[k].(map[string]any)
				if !ok {
					return
				}
				cur = next
			}
			var val any
			_ = json.Unmarshal([]byte(v), &val)
			cur[path[len(path)-1]] = val
			b, _ := json.Marshal(cp)
			mu.Lock()
			current = b
			mu.Unlock()
			got, err := c.Commands(context.Background())
			name, _ := list[i]["name"].(string)
			switch {
			case len(path) > 1:
				// a problem in a body must never cost us an entry (bodies decode tolerantly)
				if err != nil || len(got) != n {
					t.Fatalf("%s %s body %v = %s: err=%v, kept %d of %d", file, name, path, v, err, len(got), n)
				}
			case criticalCommands[name] || path[0] == "name":
				// an unreadable entry we act on must fail the call (fail closed), or be kept; never silently vanish
				if err == nil && len(got) != n {
					t.Fatalf("%s %s field %v = %s was silently dropped (kept %d of %d)", file, name, path, v, len(got), n)
				}
			default:
				if err != nil || len(got) < n-1 {
					t.Fatalf("%s %s field %v = %s: err=%v, kept %d of %d", file, name, path, v, err, len(got), n)
				}
			}
		}
		for i, c := range list {
			for k, v := range c {
				for _, o := range odd {
					try(i, []string{k}, o)
				}
				if body, ok := v.(map[string]any); ok && k == "body" {
					for bk := range body {
						for _, o := range odd {
							try(i, []string{"body", bk}, o)
						}
					}
				}
			}
		}
	}
}

func TestRealQueueStatusAndHealthDecode(t *testing.T) {
	c := serve(t, map[string][]byte{"/queue": fixture(t, "queue.json"), "/system/status": fixture(t, "system_status.json"), "/health": fixture(t, "health.json")})
	q, err := c.Queue(context.Background(), 0)
	if err != nil || len(q) == 0 {
		t.Fatalf("real queue: %d records, err=%v", len(q), err)
	}
	for _, r := range q {
		if r.ID == 0 || r.DownloadID == "" || r.Status == "" {
			t.Fatalf("a real queue record lost its basics: %+v", r)
		}
	}
	v, err := c.Status(context.Background())
	if err != nil || !strings.HasPrefix(v, "4.") {
		t.Fatalf("status version %q err=%v", v, err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

// The queue stays strict ON PURPOSE: a record that cannot be read must not silently vanish, because the scheduler
// would then think the season is not downloading and grab it again.
func TestQueueRefusesAnUndecodableRecordInsteadOfDroppingIt(t *testing.T) {
	b := []byte(`{"totalRecords":1,"records":[{"id":"not a number","seriesId":1}]}`)
	if _, err := serve(t, map[string][]byte{"/queue": b}).Queue(context.Background(), 0); err == nil {
		t.Fatal("a queue record of the wrong shape must be an error, never skipped")
	}
}

// The gate that runs before old files are deleted reads the import and monitor commands. If one of those cannot be
// read, Commands must say so (the job is then held) instead of returning a list without it ("nothing is running").
func TestAnUnreadableImportCommandFailsTheListingClosed(t *testing.T) {
	body := []byte(`[{"id":1,"name":"RssSync","status":"completed","body":{}},
	 {"id":2,"name":"DownloadedEpisodesScan","status":"started","started":"2026-10-12T08:00:00Z","message":5,"body":{"path":"/dl/x"}}]`)
	if _, err := serve(t, map[string][]byte{"/command": body}).Commands(context.Background()); err == nil {
		t.Fatal("an unreadable DownloadedEpisodesScan must fail the listing, not vanish from it")
	}
	body = []byte(`[{"id":1,"name":"RssSync","status":"completed","message":5,"body":{}},{"id":2,"name":"Backup","status":"completed"}]`)
	got, err := serve(t, map[string][]byte{"/command": body}).Commands(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("an unreadable command we never act on is skipped: %d, %v", len(got), err)
	}
	if _, err := serve(t, map[string][]byte{"/command": []byte(`[{"id":1,"name":7},{"id":2,"name":8}]`)}).Commands(context.Background()); err == nil {
		t.Fatal("a listing where nothing decodes must be an error")
	}
}

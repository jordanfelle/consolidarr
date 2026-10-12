package sonarr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A real Sonarr command list: ManualImport names its files as objects, RenameFiles/others as plain numbers, and some
// commands carry no body fields at all. One odd body must never make the whole listing unreadable (that held every
// replacement behind "cannot read Sonarr's command queue").
func TestCommandListDecodesWhateverTheBodyShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
		 {"id":1,"name":"ManualImport","status":"started","body":{"files":[{"path":"/dl/a.mkv","episodeIds":[1]},{"path":"/dl/b.mkv"}]}},
		 {"id":2,"name":"RenameFiles","status":"completed","body":{"seriesId":4,"files":[678249,678254]}},
		 {"id":3,"name":"DownloadedEpisodesScan","status":"queued","body":{"path":"/dl/x","downloadClientId":"abc"}},
		 {"id":4,"name":"Backup","status":"completed","body":null},
		 {"id":5,"name":"Odd","status":"completed","body":"not an object"},
		 {"id":6,"name":"Odd2","status":"completed","body":{"path":12,"downloadClientId":false,"files":"x"}}
		]`))
	}))
	defer srv.Close()
	cmds, err := New(srv.URL, "k").Commands(context.Background())
	if err != nil {
		t.Fatalf("the listing must decode: %v", err)
	}
	if len(cmds) != 6 {
		t.Fatalf("got %d commands", len(cmds))
	}
	if got := cmds[0].Body.Files; len(got) != 2 || got[0] != "/dl/a.mkv" || got[1] != "/dl/b.mkv" {
		t.Fatalf("ManualImport files: %v", got)
	}
	if len(cmds[1].Body.Files) != 0 {
		t.Fatalf("numeric file ids are not paths: %v", cmds[1].Body.Files)
	}
	if cmds[2].Body.Path != "/dl/x" || cmds[2].Body.DownloadClientID != "abc" {
		t.Fatalf("scan body: %+v", cmds[2].Body)
	}
}
